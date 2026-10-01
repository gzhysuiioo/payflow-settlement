package payflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func stmtPayment(id, account, asset string, amount, fee, charged int64) StatementEntry {
	return StatementEntry{
		Kind: KindPayment, ID: id, Account: account, Asset: asset,
		Amount: &amount, Fee: &fee, Charged: &charged,
	}
}

func stmtRefund(id, settlementID, account, asset string, amount, fee, charged int64) StatementEntry {
	return StatementEntry{
		Kind: KindRefund, ID: id, SettlementID: settlementID,
		Account: account, Asset: asset,
		Amount: &amount, Fee: &fee, Charged: &charged,
	}
}

func openReconcileLedger(t *testing.T, path string) *Ledger {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// 基础匹配：付款与退款全部一致，报告 matched，无缺失，汇总差额为零。
func TestReconcileMatched(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 1500)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "cancel")}}); err != nil {
		t.Fatal(err)
	}

	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 1500, 4, 1504),
		stmtRefund("r1", "p1", "aa", "usdc", 1500, 4, 1504),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 2 {
		t.Fatalf("entries=%d want 2", len(rep.Entries))
	}
	for i, e := range rep.Entries {
		if e.Status != ReconcileMatched {
			t.Fatalf("entry %d status=%s want matched: %+v", i, e.Status, e)
		}
		if e.Ledger == nil {
			t.Fatalf("entry %d missing ledger side", i)
		}
	}
	// 付款记录附带退款记录。
	if len(rep.Entries[0].Ledger.Refunds) != 1 || rep.Entries[0].Ledger.Refunds[0].ID != "r1" {
		t.Fatalf("payment should carry related refund: %+v", rep.Entries[0].Ledger)
	}
	// 退款记录附带原付款结算记录。
	if rep.Entries[1].Ledger.Settlement == nil || rep.Entries[1].Ledger.Settlement.ID != "p1" {
		t.Fatalf("refund should carry original settlement: %+v", rep.Entries[1].Ledger)
	}
	// 无缺失。
	if len(rep.Missing) != 0 {
		t.Fatalf("missing=%+v want empty", rep.Missing)
	}
	// 汇总：账本与流水完全一致，净差额为零。
	if len(rep.Summary) != 1 {
		t.Fatalf("summary=%+v", rep.Summary)
	}
	s := rep.Summary[0]
	if s.NetDiff != "0" {
		t.Fatalf("net_diff=%s want 0", s.NetDiff)
	}
	if s.Ledger.Charges != "1504" || s.Ledger.Refunds != "1504" || s.Ledger.Net != "0" {
		t.Fatalf("ledger totals=%+v", s.Ledger)
	}
	if s.Statement.Charges != "1504" || s.Statement.Refunds != "1504" || s.Statement.Net != "0" {
		t.Fatalf("statement totals=%+v", s.Statement)
	}
	// 最大序号。
	if rep.MaxSeq.Payments != 1 || rep.MaxSeq.Refunds != 1 {
		t.Fatalf("max_seq=%+v", rep.MaxSeq)
	}
}

// 字段不符：列出每个不同字段及账本值、流水值。
func TestReconcileFieldMismatch(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 1000)}}); err != nil {
		t.Fatal(err)
	}

	// 流水篡改 account、asset、amount、fee、charged 全部五个字段。
	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "bb", "eth", 1001, 5, 1006),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := rep.Entries[0]
	if e.Status != ReconcileFieldMismatch {
		t.Fatalf("status=%s want field_mismatch", e.Status)
	}
	wantDiffs := map[string][2]any{
		"account": {"aa", "bb"},
		"asset":   {"usdc", "eth"},
		"amount":  {int64(1000), int64(1001)},
		"fee":     {int64(0), int64(5)},
		"charged": {int64(1000), int64(1006)},
	}
	if len(e.Fields) != len(wantDiffs) {
		t.Fatalf("diffs=%+v", e.Fields)
	}
	for _, d := range e.Fields {
		want, ok := wantDiffs[d.Field]
		if !ok {
			t.Fatalf("unexpected diff field %q", d.Field)
		}
		if d.Ledger != want[0] || d.Statement != want[1] {
			t.Fatalf("diff %s: ledger=%v statement=%v want %v/%v", d.Field, d.Ledger, d.Statement, want[0], want[1])
		}
	}
}

// 退款字段不符：包括 settlement_id 核对。
func TestReconcileRefundMismatch(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 1000)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "x")}}); err != nil {
		t.Fatal(err)
	}

	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtRefund("r1", "p2", "aa", "usdc", 1000, 0, 1000), // settlement_id 错误
	})
	if err != nil {
		t.Fatal(err)
	}
	e := rep.Entries[0]
	if e.Status != ReconcileFieldMismatch {
		t.Fatalf("status=%s want field_mismatch", e.Status)
	}
	found := false
	for _, d := range e.Fields {
		if d.Field == "settlement_id" {
			found = true
			if d.Ledger != "p1" || d.Statement != "p2" {
				t.Fatalf("settlement_id diff: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("settlement_id not checked: %+v", e.Fields)
	}
}

// 账本不存在该笔。
func TestReconcileMissingInLedger(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openReconcileLedger(t, path)

	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("ghost", "aa", "usdc", 100, 0, 100),
		stmtRefund("rghost", "ghost", "aa", "usdc", 100, 0, 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range rep.Entries {
		if e.Status != ReconcileMissingStatus {
			t.Fatalf("entry %d status=%s want missing_in_ledger", i, e.Status)
		}
		if e.Ledger != nil {
			t.Fatalf("entry %d should not have ledger side", i)
		}
		if e.Statement == nil {
			t.Fatalf("entry %d should have statement side", i)
		}
	}
}

// 重复流水：同类型同编号多次出现，整组标为重复，附全部输入位置，
// 不判断字段差异，账本记录不列为缺失。
func TestReconcileDuplicate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 1000)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "x")}}); err != nil {
		t.Fatal(err)
	}

	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 1000, 0, 1000),
		stmtPayment("p1", "bb", "eth", 999, 9, 999), // 同编号，内容不同
		stmtRefund("r1", "p1", "aa", "usdc", 1000, 0, 1000),
		stmtRefund("r1", "p1", "aa", "usdc", 1, 0, 1), // 同编号，内容不同
	})
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses := []string{ReconcileDuplicate, ReconcileDuplicate, ReconcileDuplicate, ReconcileDuplicate}
	for i, e := range rep.Entries {
		if e.Status != wantStatuses[i] {
			t.Fatalf("entry %d status=%s want %s", i, e.Status, wantStatuses[i])
		}
		if len(e.Positions) != 2 {
			t.Fatalf("entry %d positions=%v want 2", i, e.Positions)
		}
		if e.Fields != nil {
			t.Fatalf("entry %d duplicate should not judge fields", i)
		}
	}
	// 付款组位置为 [0,1]，退款组位置为 [2,3]。
	if rep.Entries[0].Positions[0] != 0 || rep.Entries[0].Positions[1] != 1 {
		t.Fatalf("payment positions=%v", rep.Entries[0].Positions)
	}
	if rep.Entries[2].Positions[0] != 2 || rep.Entries[2].Positions[1] != 3 {
		t.Fatalf("refund positions=%v", rep.Entries[2].Positions)
	}
	// 账本记录不列为缺失。
	if len(rep.Missing) != 0 {
		t.Fatalf("missing=%+v want empty (duplicates cover ledger records)", rep.Missing)
	}
}

// 缺失列表：账本中有但流水未覆盖的记录，先付款后退款，按成功顺序。
func TestReconcileMissingRecords(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 1000),
		intent("p2", "aa", "usdc", 2000),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", "x"),
		refundReq("r2", "p2", "x"),
	}}); err != nil {
		t.Fatal(err)
	}

	// 流水只覆盖 p1 与 r1，其余缺失。
	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 1000, 0, 1000),
		stmtRefund("r1", "p1", "aa", "usdc", 1000, 0, 1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantMissing := []struct {
		kind string
		id   string
	}{
		{KindPayment, "p2"},
		{KindRefund, "r2"},
	}
	if len(rep.Missing) != len(wantMissing) {
		t.Fatalf("missing=%+v", rep.Missing)
	}
	for i, w := range wantMissing {
		m := rep.Missing[i]
		if m.Kind != w.kind || m.ID != w.id {
			t.Fatalf("missing %d: kind=%s id=%s want %s/%s", i, m.Kind, m.ID, w.kind, w.id)
		}
	}
}

// 空列表：报告全部账本记录缺失。
func TestReconcileEmptyStatement(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)}}); err != nil {
		t.Fatal(err)
	}

	rep, err := l.ReconcileStatement(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 0 {
		t.Fatalf("entries=%d want 0", len(rep.Entries))
	}
	if len(rep.Missing) != 1 || rep.Missing[0].ID != "p1" {
		t.Fatalf("missing=%+v want [p1]", rep.Missing)
	}
	// 流水侧合计为零。
	if len(rep.Summary) != 1 {
		t.Fatalf("summary=%+v", rep.Summary)
	}
	if rep.Summary[0].Statement.Charges != "0" || rep.Summary[0].Statement.Refunds != "0" {
		t.Fatalf("statement totals should be zero: %+v", rep.Summary[0].Statement)
	}
	// 净差额 = 0 - 100 = -100。
	if rep.Summary[0].NetDiff != "-100" {
		t.Fatalf("net_diff=%s want -100", rep.Summary[0].NetDiff)
	}
}

// 汇总金额用十进制整数字符串，累计超过 int64 仍精确。
// 账本侧重放校验限制单账户累计扣款不超过 int64，因此用流水侧的大额累计验证 big.Int 路径。
func TestReconcileSummaryBigInt(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: math.MaxInt64}})
	l := openReconcileLedger(t, path)
	half := int64(math.MaxInt64 / 2)
	// 账本侧 2 笔各 MaxInt64/2，累计 = MaxInt64-1，不超过 int64。
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", half),
		intent("p2", "aa", "usdc", half),
	}}); err != nil {
		t.Fatal(err)
	}

	// 流水侧 3 笔各 MaxInt64/2，累计超过 int64。
	stmt := []StatementEntry{
		stmtPayment("p1", "aa", "usdc", half, 0, half),
		stmtPayment("p2", "aa", "usdc", half, 0, half),
		stmtPayment("p3", "aa", "usdc", half, 0, half),
	}
	rep, err := l.ReconcileStatement(stmt)
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Summary[0]
	// 账本侧累计 = 2 * (MaxInt64/2) = MaxInt64-1。
	wantLedger := new(big.Int).Mul(big.NewInt(2), big.NewInt(half))
	if s.Ledger.Charges != wantLedger.String() {
		t.Fatalf("ledger charges=%s want %s", s.Ledger.Charges, wantLedger.String())
	}
	// 流水侧累计 = 3 * (MaxInt64/2)，超过 int64。
	wantStmt := new(big.Int).Mul(big.NewInt(3), big.NewInt(half))
	if s.Statement.Charges != wantStmt.String() {
		t.Fatalf("statement charges=%s want %s", s.Statement.Charges, wantStmt.String())
	}
	// 净差额 = 流水 - 账本 = MaxInt64/2。
	wantDiff := new(big.Int).Sub(wantStmt, wantLedger)
	if s.NetDiff != wantDiff.String() {
		t.Fatalf("net_diff=%s want %s", s.NetDiff, wantDiff.String())
	}
	// 验证流水侧累计确实超过 int64。
	if wantStmt.IsInt64() {
		t.Fatalf("statement total %s should exceed int64", wantStmt.String())
	}
}

// 流水侧包括重复项和账本外的项。
func TestReconcileSummaryIncludesAllEntries(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 1000)}}); err != nil {
		t.Fatal(err)
	}

	// 流水：p1 匹配（1000），p1 重复（500），ghost 账本外（300）。
	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 1000, 0, 1000),
		stmtPayment("p1", "aa", "usdc", 500, 0, 500),
		stmtPayment("ghost", "aa", "usdc", 300, 0, 300),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Summary[0]
	// 流水侧合计 = 1000 + 500 + 300 = 1800。
	if s.Statement.Charges != "1800" {
		t.Fatalf("statement charges=%s want 1800 (includes duplicates and out-of-ledger)", s.Statement.Charges)
	}
	// 账本侧合计 = 1000。
	if s.Ledger.Charges != "1000" {
		t.Fatalf("ledger charges=%s want 1000", s.Ledger.Charges)
	}
	// 净差额 = 1800 - 1000 = 800。
	if s.NetDiff != "800" {
		t.Fatalf("net_diff=%s want 800", s.NetDiff)
	}
}

// 不同资产不相加，总额相等也不能取消逐笔差异。
func TestReconcileSummaryByAsset(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 1000},
		{Account: "aa", Asset: "eth", Balance: 1000},
	})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 100),
		intent("p2", "aa", "eth", 200),
	}}); err != nil {
		t.Fatal(err)
	}

	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 100, 0, 100),
		stmtPayment("p2", "aa", "eth", 200, 0, 200),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Summary) != 2 {
		t.Fatalf("summary=%+v want 2 rows (usdc, eth)", rep.Summary)
	}
	// 按账户、资产排序：eth 在 usdc 前。
	if rep.Summary[0].Asset != "eth" || rep.Summary[1].Asset != "usdc" {
		t.Fatalf("summary order=%+v", rep.Summary)
	}
}

// 最大成功序号：空历史为零。
func TestReconcileMaxSeqEmpty(t *testing.T) {
	path := newTestLedger(t, nil)
	l := openReconcileLedger(t, path)

	rep, err := l.ReconcileStatement(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.MaxSeq.Payments != 0 || rep.MaxSeq.Refunds != 0 {
		t.Fatalf("max_seq=%+v want 0/0 for empty history", rep.MaxSeq)
	}
}

// 校验：空字段、未知 kind、金额缺失或越界都返回 invalid_parameter，
// 不输出部分报告。
func TestReconcileValidation(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openReconcileLedger(t, path)

	bad := []struct {
		name string
		e    StatementEntry
	}{
		{"empty id", StatementEntry{Kind: KindPayment, ID: "", Account: "aa", Asset: "usdc", Amount: i64p(1), Fee: i64p(0), Charged: i64p(1)}},
		{"empty account", StatementEntry{Kind: KindPayment, ID: "p", Account: "", Asset: "usdc", Amount: i64p(1), Fee: i64p(0), Charged: i64p(1)}},
		{"empty asset", StatementEntry{Kind: KindPayment, ID: "p", Account: "aa", Asset: "", Amount: i64p(1), Fee: i64p(0), Charged: i64p(1)}},
		{"refund empty settlement_id", StatementEntry{Kind: KindRefund, ID: "r", SettlementID: "", Account: "aa", Asset: "usdc", Amount: i64p(1), Fee: i64p(0), Charged: i64p(1)}},
		{"unknown kind", StatementEntry{Kind: "transfer", ID: "t", Account: "aa", Asset: "usdc", Amount: i64p(1), Fee: i64p(0), Charged: i64p(1)}},
		{"missing amount", StatementEntry{Kind: KindPayment, ID: "p", Account: "aa", Asset: "usdc", Amount: nil, Fee: i64p(0), Charged: i64p(1)}},
		{"missing fee", StatementEntry{Kind: KindPayment, ID: "p", Account: "aa", Asset: "usdc", Amount: i64p(1), Fee: nil, Charged: i64p(1)}},
		{"missing charged", StatementEntry{Kind: KindPayment, ID: "p", Account: "aa", Asset: "usdc", Amount: i64p(1), Fee: i64p(0), Charged: nil}},
		{"amount zero", StatementEntry{Kind: KindPayment, ID: "p", Account: "aa", Asset: "usdc", Amount: i64p(0), Fee: i64p(0), Charged: i64p(0)}},
		{"amount negative", StatementEntry{Kind: KindPayment, ID: "p", Account: "aa", Asset: "usdc", Amount: i64p(-1), Fee: i64p(0), Charged: i64p(1)}},
		{"charged zero", StatementEntry{Kind: KindPayment, ID: "p", Account: "aa", Asset: "usdc", Amount: i64p(1), Fee: i64p(0), Charged: i64p(0)}},
		{"fee negative", StatementEntry{Kind: KindPayment, ID: "p", Account: "aa", Asset: "usdc", Amount: i64p(1), Fee: i64p(-1), Charged: i64p(1)}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := l.ReconcileStatement([]StatementEntry{tc.e})
			if err == nil || KindOf(err) != ErrInvalid {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

// 兼容旧版无退款账本。
func TestReconcileLegacyLedger(t *testing.T) {
	legacy := func(balance int64, records ...Record) []byte {
		type doc struct {
			Version     int           `json:"version"`
			Initial     []BalanceView `json:"initial_balances"`
			Balances    []BalanceView `json:"balances"`
			Settlements []Record      `json:"settlements"`
		}
		d := doc{
			Version:     1,
			Initial:     []BalanceView{{Account: "aa", Asset: "usdc", Balance: 1000}},
			Balances:    []BalanceView{{Account: "aa", Asset: "usdc", Balance: balance}},
			Settlements: records,
		}
		payload, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		sum := checksumHex(payload)
		return append(append(payload[:len(payload)-1], []byte(`,"checksum":"`+sum+`"}`)...), '\n')
	}
	rec := Record{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 300, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 300, Seq: 1}
	path := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(path, legacy(700, rec), 0o600); err != nil {
		t.Fatal(err)
	}

	l := openReconcileLedger(t, path)
	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 300, 0, 300),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries[0].Status != ReconcileMatched {
		t.Fatalf("legacy reconcile: %+v", rep.Entries[0])
	}
	// 无退款记录。
	if len(rep.Missing) != 0 {
		t.Fatalf("missing=%+v", rep.Missing)
	}
}

// 同一进程并发付款、退款时，整份报告对应同一完整账本状态。
func TestReconcileConcurrentConsistentSnapshot(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openReconcileLedger(t, path)

	// 先写入一批记录。
	const n = 20
	entries := make([]PaymentIntent, n)
	for i := 0; i < n; i++ {
		entries[i] = intent("p"+string(rune('a'+i)), "aa", "usdc", 1000)
	}
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: entries}); err != nil {
		t.Fatal(err)
	}

	// 并发：一边对账，一边付款。对账必须看到一致的完整状态。
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			rep, err := l.ReconcileStatement(nil)
			if err != nil {
				errs <- err
				return
			}
			// 缺失列表必须与账本状态一致：要么 20 条，要么更多（并发付款后）。
			if len(rep.Missing) < n {
				errs <- fmt.Errorf("missing=%d want >= %d", len(rep.Missing), n)
				return
			}
			// 缺失列表中付款序号必须连续（1..len）。
			for j, m := range rep.Missing {
				if m.Kind != KindPayment || m.Seq != int64(j+1) {
					errs <- fmt.Errorf("missing %d: kind=%s seq=%d", j, m.Kind, m.Seq)
					return
				}
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			h, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			if _, err := h.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent(fmt.Sprintf("g-%d", i), "aa", "usdc", 1)}}); err != nil {
				errs <- err
				h.Close()
				return
			}
			h.Close()
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// 对账不改变余额、历史或账本文件。
func TestReconcileDoesNotMutateLedger(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 100, 0, 100),
		stmtPayment("ghost", "aa", "usdc", 1, 0, 1),
	}); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("reconcile mutated the ledger file")
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 900 {
		t.Fatalf("balance changed: %d want 900", bal)
	}
}

// 付款与退款编号分别匹配，同名也不能混为一笔。
func TestReconcilePaymentAndRefundIDsSeparate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("same", "aa", "usdc", 100)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("same", "same", "x")}}); err != nil {
		t.Fatal(err)
	}

	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("same", "aa", "usdc", 100, 0, 100),
		stmtRefund("same", "same", "aa", "usdc", 100, 0, 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range rep.Entries {
		if e.Status != ReconcileMatched {
			t.Fatalf("entry %d status=%s want matched", i, e.Status)
		}
	}
}

// 已退款的付款仍核对原扣款，退款单独核对。
func TestReconcileRefundedPaymentChecksOriginal(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "x")}}); err != nil {
		t.Fatal(err)
	}

	// 付款流水仍按原扣款核对（charged=100），退款流水单独核对。
	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 100, 0, 100),
		stmtRefund("r1", "p1", "aa", "usdc", 100, 0, 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries[0].Status != ReconcileMatched {
		t.Fatalf("refunded payment should still match original charge: %+v", rep.Entries[0])
	}
	if rep.Entries[1].Status != ReconcileMatched {
		t.Fatalf("refund should match separately: %+v", rep.Entries[1])
	}
}

func TestReconcileReportJSONShape(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openReconcileLedger(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)}}); err != nil {
		t.Fatal(err)
	}

	rep, err := l.ReconcileStatement([]StatementEntry{
		stmtPayment("p1", "aa", "usdc", 100, 0, 100),
		stmtPayment("p1", "aa", "usdc", 50, 0, 50),
		stmtPayment("ghost", "aa", "usdc", 1, 0, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	// 报告必须是合法 JSON 且包含顶层键。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	for _, key := range []string{"entries", "missing", "summary", "max_seq"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("report missing top-level key %q: %s", key, data)
		}
	}
	// 金额必须是字符串。
	var summary []map[string]any
	if err := json.Unmarshal(raw["summary"], &summary); err != nil {
		t.Fatal(err)
	}
	for _, row := range summary {
		for _, side := range []string{"ledger", "statement"} {
			totals := row[side].(map[string]any)
			for _, k := range []string{"charges", "refunds", "net"} {
				if _, ok := totals[k].(string); !ok {
					t.Fatalf("summary %s.%s is not a string: %v", side, k, totals[k])
				}
			}
		}
		if _, ok := row["net_difference"].(string); !ok {
			t.Fatalf("net_difference is not a string: %v", row["net_difference"])
		}
	}
	// max_seq 必须是数字。
	var maxSeq map[string]any
	if err := json.Unmarshal(raw["max_seq"], &maxSeq); err != nil {
		t.Fatal(err)
	}
	if _, ok := maxSeq["payments"].(float64); !ok {
		t.Fatalf("max_seq.payments is not a number: %v", maxSeq["payments"])
	}
}
