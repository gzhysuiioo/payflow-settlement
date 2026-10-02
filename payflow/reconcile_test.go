package payflow

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---- 输入解析 ----

func TestParseReconcileRequestValid(t *testing.T) {
	raw := []byte(`{"entries":[
	  {"kind":"payment","id":"p1","account":"aa","asset":"usdc","amount":10,"fee":3,"charged":13},
	  {"kind":"refund","id":"r1","account":"aa","asset":"usdc","amount":10,"fee":3,"charged":13,"settlement_id":"p1"}
	]}`)
	entries, err := ParseReconcileRequest(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Index != 0 || entries[0].Kind != "payment" || entries[0].Charged != 13 {
		t.Fatalf("entry0=%+v", entries[0])
	}
	if entries[1].Kind != "refund" || entries[1].SettlementID != "p1" || entries[1].Index != 1 {
		t.Fatalf("entry1=%+v", entries[1])
	}

	// 边界值合法：amount/charged 为正、fee 为 0 或 MaxInt64。
	raw = []byte(`{"entries":[{"kind":"payment","id":"p","account":"a","asset":"z","amount":1,"fee":9223372036854775807,"charged":9223372036854775807}]}`)
	if _, err := ParseReconcileRequest(raw); err != nil {
		t.Fatalf("boundary values: %v", err)
	}
}

func TestParseReconcileRequestInvalid(t *testing.T) {
	bad := []string{
		`{bad`,                    // 非法 JSON
		`{}`,                      // 缺 entries
		`{"entries":[]} trailing`, // 尾随内容
		`{"entries":[{"kind":"transfer","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":1}]}`, // 未知 kind
		`{"entries":[{"kind":"payment","id":"","account":"a","asset":"z","amount":1,"fee":0,"charged":1}]}`,   // 空 id
		`{"entries":[{"kind":"payment","id":"x","account":"","asset":"z","amount":1,"fee":0,"charged":1}]}`,   // 空 account
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"","amount":1,"fee":0,"charged":1}]}`,   // 空 asset
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","fee":0,"charged":1}]}`,             // 缺 amount
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1,"charged":1}]}`,          // 缺 fee
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1,"fee":0}]}`,              // 缺 charged
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":0,"fee":0,"charged":1}]}`,  // amount 非正
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":-1,"fee":0,"charged":1}]}`,
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1,"fee":-1,"charged":1}]}`,                   // fee 负
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":0}]}`,                    // charged 非正
		`{"entries":[{"kind":"refund","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":1}]}`,                     // 退款缺 settlement_id
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":"1","fee":0,"charged":1}]}`,                  // 字符串金额
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1.5,"fee":0,"charged":1}]}`,                  // 小数
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1e3,"fee":0,"charged":1}]}`,                  // 科学计数
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":true,"fee":0,"charged":1}]}`,                 // 布尔
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":null,"fee":0,"charged":1}]}`,                 // null
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":9223372036854775808,"fee":0,"charged":1}]}`,  // 上越界
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":-9223372036854775809}]}`, // 下越界
		`{"entries":"x"}`, // 类型不符
	}
	for i, in := range bad {
		if _, err := ParseReconcileRequest([]byte(in)); err == nil || KindOf(err) != ErrInvalid {
			t.Fatalf("case %d must be invalid_parameter, got %v: %s", i, err, in)
		}
	}
}

// ---- 对账主流程 ----

func reconLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa-1", Asset: "usdc", Balance: math.MaxInt64},
		{Account: "aa-2", Asset: "eth", Balance: 100},
	})
	l := openOrFail(t, path)
	// p1: aa-1/usdc amount=1000 fee=3 charged=1003；p2: aa-2/eth amount=10 fee=0 charged=10。
	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		{ID: "p1", Account: "aa-1", Paymaster: "pm", Asset: "usdc", Amount: i64p(1000), Nonce: 1, State: "pending"},
		{ID: "p2", Account: "aa-2", Asset: "eth", Amount: i64p(10), State: "pending"},
	}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := statuses(res); got[0] != StatusSettled || got[1] != StatusSettled {
		t.Fatalf("setup statuses=%v", got)
	}
	// r1 退回 p1。
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r1", SettlementID: "p1", Reason: "cancel"},
	}})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if rr.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("setup refund=%s", rr.Results[0].Status)
	}
	return l, path
}

func TestReconcileFlowStatusesAndDiffs(t *testing.T) {
	l, _ := reconLedger(t)

	entries, err := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":1,"charged":11},
	  {"kind":"payment","id":"ghost","account":"aa-1","asset":"usdc","amount":5,"fee":0,"charged":5},
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"},
	  {"kind":"refund","id":"rX","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p-wrong"},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":0,"charged":10}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	wantStatus := []string{
		ReconMatched,         // 0: p1 已退款仍按原扣款核对
		ReconDuplicate,       // 1: p2 与索引 5 同编号，整组重复（不判字段差异）
		ReconMissingInLedger, // 2: ghost
		ReconMatched,         // 3: r1
		ReconMissingInLedger, // 4: rX 在账本退款编号中不存在
		ReconDuplicate,       // 5: p2 重复组
	}
	for i, r := range rep.Results {
		if r.Status != wantStatus[i] {
			t.Fatalf("result %d: got %s want %s (%+v)", i, r.Status, wantStatus[i], r)
		}
	}

	// 重复组两条都带全部输入位置 [1,5]。
	for _, i := range []int{1, 5} {
		pos := rep.Results[i].Positions
		if len(pos) != 2 || pos[0] != 1 || pos[1] != 5 {
			t.Fatalf("result %d positions=%v", i, pos)
		}
		if len(rep.Results[i].Diffs) != 0 {
			t.Fatalf("duplicate must carry no diffs: %+v", rep.Results[i].Diffs)
		}
	}

	// p2 是重复组，账本记录不列入缺失；ghost 是流水外项，只出现在逐条结果。
	// 唯一未覆盖的账本记录：无（p1/r1 命中，p2 重复抑制）。
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("want no missing, got payments=%+v refunds=%+v", rep.MissingPayments, rep.MissingRefunds)
	}

	// 已退款付款的匹配结果带退款关联。
	if rep.Results[0].Record == nil || rep.Results[0].Record.RefundID != "r1" {
		t.Fatalf("matched p1 must link refund r1: %+v", rep.Results[0].Record)
	}
	// 退款匹配结果带原付款编号。
	if rep.Results[3].Record == nil || rep.Results[3].Record.SettlementID != "p1" {
		t.Fatalf("matched r1 must link settlement p1: %+v", rep.Results[3].Record)
	}
}

func TestReconcileFieldDiffsListEachField(t *testing.T) {
	l, _ := reconLedger(t)
	entries, _ := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"p1","account":"OTHER","asset":"usdc","amount":999,"fee":2,"charged":1003}
	]}`))
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	r := rep.Results[0]
	if r.Status != ReconFieldMismatch {
		t.Fatalf("status=%s", r.Status)
	}
	got := map[string][2]string{}
	for _, d := range r.Diffs {
		got[d.Field] = [2]string{d.Ledger, d.Flow}
	}
	want := map[string][2]string{
		"account": {"aa-1", "OTHER"},
		"amount":  {"1000", "999"},
		"fee":     {"3", "2"},
	}
	if len(got) != len(want) {
		t.Fatalf("diffs=%v want %v", got, want)
	}
	for f, v := range want {
		if got[f] != v {
			t.Fatalf("diff %s=%v want %v", f, got[f], v)
		}
	}
}

func TestReconcileRefundSettlementIDMismatch(t *testing.T) {
	l, _ := reconLedger(t)
	// r1 存在且原付款是 p1；流水声称原付款是 p2 -> 字段不符，且只报 settlement_id。
	entries, _ := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p2"}
	]}`))
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	r := rep.Results[0]
	if r.Status != ReconFieldMismatch || len(r.Diffs) != 1 || r.Diffs[0].Field != "settlement_id" {
		t.Fatalf("want only settlement_id diff, got %+v", r)
	}
	if r.Diffs[0].Ledger != "p1" || r.Diffs[0].Flow != "p2" {
		t.Fatalf("diff values=%+v", r.Diffs[0])
	}
}

func TestReconcileMissingListsInSuccessOrder(t *testing.T) {
	l, _ := reconLedger(t)
	// 空列表：全部账本成功记录列入缺失，先付款（成功顺序 p1,p2）再退款（r1）。
	rep, err := l.ReconcileFlow(nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	gotP := []string{}
	for _, p := range rep.MissingPayments {
		gotP = append(gotP, p.ID)
		if p.Seq <= 0 {
			t.Fatalf("missing payment seq: %+v", p)
		}
	}
	gotR := []string{}
	for _, rf := range rep.MissingRefunds {
		gotR = append(gotR, rf.ID)
	}
	if strings.Join(gotP, ",") != "p1,p2" || strings.Join(gotR, ",") != "r1" {
		t.Fatalf("missing payments=%v refunds=%v", gotP, gotR)
	}
	// p1 已退款，缺失项也必须带原付款/退款关联。
	if rep.MissingPayments[0].RefundID != "r1" {
		t.Fatalf("missing p1 must link refund r1: %+v", rep.MissingPayments[0])
	}
	if rep.MissingRefunds[0].SettlementID != "p1" {
		t.Fatalf("missing r1 must link p1: %+v", rep.MissingRefunds[0])
	}
	if rep.MaxPaymentSeq != 2 || rep.MaxRefundSeq != 1 {
		t.Fatalf("max seqs=%d/%d", rep.MaxPaymentSeq, rep.MaxRefundSeq)
	}
}

func TestReconcileSameIDInDifferentKinds(t *testing.T) {
	l, _ := reconLedger(t)
	// 同名编号分属付款/退款空间：付款 "r1" 账本无此付款 -> 缺失；
	// 退款 "r1" 命中 -> 匹配；互不影响。
	entries, _ := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"r1","account":"aa-1","asset":"usdc","amount":1,"fee":0,"charged":1},
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"}
	]}`))
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconMissingInLedger {
		t.Fatalf("payment r1 must not match refund r1: %s", rep.Results[0].Status)
	}
	if rep.Results[1].Status != ReconMatched {
		t.Fatalf("refund r1 must match: %s", rep.Results[1].Status)
	}
	// 付款 r1 缺失是流水外项；退款 r1 已覆盖，账本无缺失记录。
	if len(rep.MissingPayments) != 2 {
		var ids []string
		for _, p := range rep.MissingPayments {
			ids = append(ids, p.ID)
		}
		t.Fatalf("p1,p2 still missing (r1 payment is flow-only), got %v", ids)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("refund r1 covered, got %+v", rep.MissingRefunds)
	}
}

func TestReconcileTotalsAndNetDiff(t *testing.T) {
	l, _ := reconLedger(t)
	// 账本：usdc 扣款 1003/退款 1003/净 0；eth 扣款 10/退款 0/净 10。
	// 流水：usdc 扣款 1003（p1 匹配），退款只来一笔 ghost 500（账本外）；
	//       eth 扣款 10（p2）。再加一笔账本外资产 jpy。
	entries, _ := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":0,"charged":10},
	  {"kind":"refund","id":"g1","account":"aa-1","asset":"usdc","amount":500,"fee":0,"charged":500,"settlement_id":"p9"},
	  {"kind":"payment","id":"x1","account":"aa-9","asset":"jpy","amount":7,"fee":0,"charged":7}
	]}`))
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	findRow := func(rows []ReconTotalRow, acct, asset string) ReconTotalRow {
		t.Helper()
		for _, r := range rows {
			if r.Account == acct && r.Asset == asset {
				return r
			}
		}
		t.Fatalf("row %s/%s not found in %+v", acct, asset, rows)
		return ReconTotalRow{}
	}
	findDiff := func(acct, asset string) ReconNetDiff {
		t.Helper()
		for _, r := range rep.NetDiff {
			if r.Account == acct && r.Asset == asset {
				return r
			}
		}
		t.Fatalf("diff %s/%s not found in %+v", acct, asset, rep.NetDiff)
		return ReconNetDiff{}
	}

	// 账本侧。
	usdcL := findRow(rep.Totals.Ledger, "aa-1", "usdc")
	if usdcL.ChargedTotal != "1003" || usdcL.RefundedTotal != "1003" || usdcL.NetCharged != "0" {
		t.Fatalf("ledger usdc=%+v", usdcL)
	}
	ethL := findRow(rep.Totals.Ledger, "aa-2", "eth")
	if ethL.ChargedTotal != "10" || ethL.RefundedTotal != "0" || ethL.NetCharged != "10" {
		t.Fatalf("ledger eth=%+v", ethL)
	}
	// 流水侧：jpy 只在流水出现；usdc 净扣款 = 1003-500 = 503。
	usdcF := findRow(rep.Totals.Flow, "aa-1", "usdc")
	if usdcF.ChargedTotal != "1003" || usdcF.RefundedTotal != "500" || usdcF.NetCharged != "503" {
		t.Fatalf("flow usdc=%+v", usdcF)
	}
	jpyF := findRow(rep.Totals.Flow, "aa-9", "jpy")
	if jpyF.NetCharged != "7" {
		t.Fatalf("flow jpy=%+v", jpyF)
	}
	if len(rep.Totals.Ledger) != 2 || len(rep.Totals.Flow) != 3 {
		t.Fatalf("row counts ledger=%d flow=%d", len(rep.Totals.Ledger), len(rep.Totals.Flow))
	}
	// 差额方向为流水减账本。
	if d := findDiff("aa-1", "usdc"); d.NetChargedDiff != "503" || d.LedgerNetCharged != "0" || d.FlowNetCharged != "503" {
		t.Fatalf("usdc diff=%+v", d)
	}
	if d := findDiff("aa-2", "eth"); d.NetChargedDiff != "0" {
		t.Fatalf("eth diff=%+v", d)
	}
	if d := findDiff("aa-9", "jpy"); d.NetChargedDiff != "7" || d.LedgerNetCharged != "0" {
		t.Fatalf("jpy diff=%+v", d)
	}
	// 行按账户、资产排序。
	if rep.Totals.Flow[0].Account != "aa-1" || rep.Totals.Flow[1].Account != "aa-2" || rep.Totals.Flow[2].Account != "aa-9" {
		t.Fatalf("flow rows not sorted: %+v", rep.Totals.Flow)
	}
	if rep.NetDiff[0].Account != "aa-1" || rep.NetDiff[len(rep.NetDiff)-1].Account != "aa-9" {
		t.Fatalf("net_diff not sorted: %+v", rep.NetDiff)
	}
}

func TestReconcileFlowTotalsOverflowInt64(t *testing.T) {
	l, _ := reconLedger(t)
	// 流水侧 10 笔 MaxInt64 扣款，累计远超 int64；十进制字符串仍须精确。
	entries := make([]FlowEntry, 0, 10)
	for i := 0; i < 10; i++ {
		entries = append(entries, FlowEntry{
			Index: i, Kind: "payment", ID: "big" + string(rune('a'+i)),
			Account: "aa-1", Asset: "usdc",
			Amount: math.MaxInt64, Fee: 0, Charged: math.MaxInt64,
		})
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var row ReconTotalRow
	for _, r := range rep.Totals.Flow {
		if r.Account == "aa-1" && r.Asset == "usdc" {
			row = r
		}
	}
	want := new(big.Int).Mul(big.NewInt(math.MaxInt64), big.NewInt(10))
	if row.ChargedTotal != want.String() || row.NetCharged != want.String() {
		t.Fatalf("charged=%s want %s", row.ChargedTotal, want.String())
	}
}

func TestReconcileEmptyHistorySeqsAreZero(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1}})
	l := openOrFail(t, path)
	rep, err := l.ReconcileFlow(nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.MaxPaymentSeq != 0 || rep.MaxRefundSeq != 0 {
		t.Fatalf("empty history seqs=%d/%d", rep.MaxPaymentSeq, rep.MaxRefundSeq)
	}
	if len(rep.Results) != 0 || len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 ||
		len(rep.Totals.Ledger) != 0 || len(rep.NetDiff) != 0 {
		t.Fatalf("empty history report not empty: %+v", rep)
	}
}

func TestReconcileLegacyLedgerWithoutRefunds(t *testing.T) {
	// 旧版无 refunds 字段的账本：构造带合法 checksum 的文档。
	type doc struct {
		Version     int           `json:"version"`
		Initial     []BalanceView `json:"initial_balances"`
		Balances    []BalanceView `json:"balances"`
		Settlements []Record      `json:"settlements"`
	}
	rec := Record{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 300, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 300, Seq: 1}
	payload, err := json.Marshal(doc{
		Version:     ledgerVersion,
		Initial:     []BalanceView{{Account: "aa", Asset: "usdc", Balance: 1000}},
		Balances:    []BalanceView{{Account: "aa", Asset: "usdc", Balance: 700}},
		Settlements: []Record{rec},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := checksumHex(payload)
	data := append(append(payload[:len(payload)-1], []byte(`,"checksum":"`+sum+`"}`)...), '\n')
	path := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	l := openOrFail(t, path)
	entries, err := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"p1","account":"aa","asset":"usdc","amount":300,"fee":0,"charged":300}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile legacy: %v", err)
	}
	if rep.Results[0].Status != ReconMatched {
		t.Fatalf("status=%s", rep.Results[0].Status)
	}
	if rep.MaxRefundSeq != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("legacy must have no refunds: seq=%d missing=%+v", rep.MaxRefundSeq, rep.MissingRefunds)
	}
	// 对账不得给旧文件补写 refunds 字段。
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte(`"refunds"`)) {
		t.Fatalf("reconcile rewrote legacy file:\n%s", onDisk)
	}
}

func TestReconcileIsReadOnly(t *testing.T) {
	l, path := reconLedger(t)
	before, err := l.Query()
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	entries, _ := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1,"fee":0,"charged":1},
	  {"kind":"payment","id":"zzz","account":"aa-1","asset":"jpy","amount":1,"fee":0,"charged":1}
	]}`))
	if _, err := l.ReconcileFlow(entries); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after, err := l.Query()
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(after)
	if string(bj) != string(aj) {
		t.Fatalf("ledger changed by reconcile:\nbefore=%s\nafter =%s", bj, aj)
	}
	// 重新打开仍能读到完全相同的状态文件。
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	rq, err := l2.Query()
	if err != nil {
		t.Fatalf("query reopened: %v", err)
	}
	rj, _ := json.Marshal(rq)
	if string(rj) != string(aj) {
		t.Fatalf("ledger file changed by reconcile:\ndisk=%s\nmem =%s", rj, aj)
	}
}

func TestReconcileDuplicatesCountedInFlowTotals(t *testing.T) {
	l, _ := reconLedger(t)
	// p2 出现两次（内容相同也仍是重复）；两条都计入流水扣款合计。
	entries, _ := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":0,"charged":10},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":0,"charged":10}
	]}`))
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconDuplicate || rep.Results[1].Status != ReconDuplicate {
		t.Fatalf("statuses=%s,%s", rep.Results[0].Status, rep.Results[1].Status)
	}
	var ethFlow, ethLedger ReconTotalRow
	for _, r := range rep.Totals.Flow {
		if r.Account == "aa-2" && r.Asset == "eth" {
			ethFlow = r
		}
	}
	for _, r := range rep.Totals.Ledger {
		if r.Account == "aa-2" && r.Asset == "eth" {
			ethLedger = r
		}
	}
	if ethFlow.ChargedTotal != "20" || ethFlow.NetCharged != "20" {
		t.Fatalf("flow eth totals=%+v", ethFlow)
	}
	if ethLedger.ChargedTotal != "10" {
		t.Fatalf("ledger eth totals=%+v", ethLedger)
	}
	var diff ReconNetDiff
	for _, d := range rep.NetDiff {
		if d.Account == "aa-2" && d.Asset == "eth" {
			diff = d
		}
	}
	if diff.NetChargedDiff != "10" {
		t.Fatalf("eth diff=%+v", diff)
	}
}

// TestReconcileConcurrentWithWriters 在并发付款/退款压力下反复对账，
// 每份报告都必须自洽：max seq 与记录条数一致，合计可由逐笔重算得到。
func TestReconcileConcurrentWithWriters(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: math.MaxInt64},
	})
	l := openOrFail(t, path)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写入方：不断用新编号提交 1 美元付款并退款其中一部分。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := "p-" + strconv.Itoa(i)
			res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{{
				ID: id, Account: "aa", Asset: "usdc", Amount: i64p(1), State: "pending",
			}}})
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			if res.Results[0].Status == StatusSettled && i%3 == 0 {
				_, _ = l.Refund(RefundBatch{Refunds: []RefundRequest{{
					ID: "r-" + strconv.Itoa(i), SettlementID: id, Reason: "x",
				}}})
			}
		}
	}()

	// 对账方：空列表报告（全部缺失）必须始终是某个完整状态的一致快照。
	for i := 0; i < 200; i++ {
		rep, err := l.ReconcileFlow(nil)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if int64(len(rep.MissingPayments)) != rep.MaxPaymentSeq {
			t.Fatalf("payments %d != max seq %d", len(rep.MissingPayments), rep.MaxPaymentSeq)
		}
		if int64(len(rep.MissingRefunds)) != rep.MaxRefundSeq {
			t.Fatalf("refunds %d != max seq %d", len(rep.MissingRefunds), rep.MaxRefundSeq)
		}
		// 账本侧净扣款 = 成功付款数 - 成功退款数。
		var net int64
		for _, row := range rep.Totals.Ledger {
			n := new(big.Int)
			if _, ok := n.SetString(row.NetCharged, 10); !ok {
				t.Fatalf("bad net %q", row.NetCharged)
			}
			net = n.Int64()
		}
		if net != rep.MaxPaymentSeq-rep.MaxRefundSeq {
			t.Fatalf("net=%d want %d", net, rep.MaxPaymentSeq-rep.MaxRefundSeq)
		}
	}
	close(stop)
	wg.Wait()
}

// ---- 按成功序号限定核对范围 ----

// rangeLedger 构造含 3 笔付款、2 笔退款的账本：
// p1(usdc,1003)、p2(usdc,2006)、p3(eth,10)；r1 退 p1、r2 退 p2。
func rangeLedger(t *testing.T) *Ledger {
	t.Helper()
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa-1", Asset: "usdc", Balance: 1000000},
		{Account: "aa-2", Asset: "eth", Balance: 100},
	})
	l := openOrFail(t, path)
	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		{ID: "p1", Account: "aa-1", Paymaster: "pm", Asset: "usdc", Amount: i64p(1000), Nonce: 1, State: "pending"},
		{ID: "p2", Account: "aa-1", Paymaster: "pm", Asset: "usdc", Amount: i64p(2000), Nonce: 2, State: "pending"},
		{ID: "p3", Account: "aa-2", Asset: "eth", Amount: i64p(10), State: "pending"},
	}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	for i, want := range []string{StatusSettled, StatusSettled, StatusSettled} {
		if res.Results[i].Status != want {
			t.Fatalf("setup payment %d status=%s", i, res.Results[i].Status)
		}
	}
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r1", SettlementID: "p1", Reason: "cancel"},
		{ID: "r2", SettlementID: "p2", Reason: "cancel"},
	}})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	for i, want := range []string{StatusRefundSuccess, StatusRefundSuccess} {
		if rr.Results[i].Status != want {
			t.Fatalf("setup refund %d status=%s", i, rr.Results[i].Status)
		}
	}
	return l
}

func flowEntry(idx int, kind, id, acct, asset string, amount, fee, charged int64, settle string) FlowEntry {
	return FlowEntry{
		Index: idx, Kind: kind, ID: id, Account: acct, Asset: asset,
		Amount: amount, Fee: fee, Charged: charged, SettlementID: settle,
	}
}

func findTotalRow(t *testing.T, rows []ReconTotalRow, acct, asset string) ReconTotalRow {
	t.Helper()
	for _, r := range rows {
		if r.Account == acct && r.Asset == asset {
			return r
		}
	}
	t.Fatalf("row %s/%s not found in %+v", acct, asset, rows)
	return ReconTotalRow{}
}

func TestReconcileFlowRangedDefaultsFull(t *testing.T) {
	l := rangeLedger(t)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
		flowEntry(1, "refund", "r1", "aa-1", "usdc", 1000, 3, 1003, "p1"),
	}
	full, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	ranged, err := l.ReconcileFlowRanged(entries, ReconcileBounds{})
	if err != nil {
		t.Fatalf("ranged: %v", err)
	}
	fj, _ := json.Marshal(full)
	rj, _ := json.Marshal(ranged)
	if !bytes.Equal(fj, rj) {
		t.Fatalf("full and ranged-empty differ:\n%s\n%s", fj, rj)
	}
	// 空边界 = 全量：报告边界取完整历史最大序号。
	if ranged.PaymentAfter != 0 || ranged.PaymentThrough != 3 ||
		ranged.RefundAfter != 0 || ranged.RefundThrough != 2 {
		t.Fatalf("bounds=%d,%d,%d,%d", ranged.PaymentAfter, ranged.PaymentThrough,
			ranged.RefundAfter, ranged.RefundThrough)
	}
}

func TestReconcileFlowRangedPaymentSelection(t *testing.T) {
	l := rangeLedger(t)
	// 只选付款 seq (1,2]：p2 入选；p1、p3 不入选。退款全量入选。
	a, th := int64(1), int64(2)
	rep, err := l.ReconcileFlowRanged(nil, ReconcileBounds{
		PaymentAfter: &a, PaymentThrough: &th,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rep.MissingPayments) != 1 || rep.MissingPayments[0].ID != "p2" {
		t.Fatalf("missing payments=%+v", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 2 {
		t.Fatalf("missing refunds=%+v", rep.MissingRefunds)
	}
	// 账本侧：usdc 扣款只有 p2=2006；退款 r1+r2=3009；eth 无入选记录，不出行。
	if len(rep.Totals.Ledger) != 1 {
		t.Fatalf("ledger rows=%+v", rep.Totals.Ledger)
	}
	usdc := findTotalRow(t, rep.Totals.Ledger, "aa-1", "usdc")
	if usdc.ChargedTotal != "2006" || usdc.RefundedTotal != "3009" {
		t.Fatalf("usdc ledger=%+v", usdc)
	}
	if rep.PaymentAfter != 1 || rep.PaymentThrough != 2 {
		t.Fatalf("payment bounds=%d,%d", rep.PaymentAfter, rep.PaymentThrough)
	}
}

func TestReconcileFlowRangedOutOfScope(t *testing.T) {
	l := rangeLedger(t)
	// 付款 (1,2]，退款 (0,1]。
	pa, pt, ra, rt := int64(1), int64(2), int64(0), int64(1)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),    // seq 1 范围外
		flowEntry(1, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),    // 入选
		flowEntry(2, "refund", "r2", "aa-1", "usdc", 2000, 6, 2006, "p2"),   // seq 2 范围外
	}
	rep, err := l.ReconcileFlowRanged(entries, ReconcileBounds{
		PaymentAfter: &pa, PaymentThrough: &pt, RefundAfter: &ra, RefundThrough: &rt,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := []string{ReconOutOfScope, ReconMatched, ReconOutOfScope}
	for i, r := range rep.Results {
		if r.Status != want[i] {
			t.Fatalf("result %d=%s want %s", i, r.Status, want[i])
		}
	}
	// out_of_scope 携带账本记录与关联（不切断）。
	if rep.Results[0].Record == nil || rep.Results[0].Record.RefundID != "r1" {
		t.Fatalf("p1 out_of_scope record link: %+v", rep.Results[0].Record)
	}
	if rep.Results[2].Record == nil || rep.Results[2].Record.SettlementID != "p2" {
		t.Fatalf("r2 out_of_scope record link: %+v", rep.Results[2].Record)
	}
	// 流水侧只计入 p2：usdc 扣款 2006，退款 0。
	if len(rep.Totals.Flow) != 1 {
		t.Fatalf("flow rows=%+v", rep.Totals.Flow)
	}
	usdc := findTotalRow(t, rep.Totals.Flow, "aa-1", "usdc")
	if usdc.ChargedTotal != "2006" || usdc.RefundedTotal != "0" {
		t.Fatalf("flow usdc=%+v", usdc)
	}
	// 缺失：退款 r1 入选且未覆盖；付款无缺失（p2 覆盖，p1/p3 范围外）。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 1 || rep.MissingRefunds[0].ID != "r1" {
		t.Fatalf("missing refunds=%+v", rep.MissingRefunds)
	}
}

func TestReconcileFlowRangedDuplicateOutOfScope(t *testing.T) {
	l := rangeLedger(t)
	pa, pt := int64(1), int64(2)
	// p1 出现两次，记录在范围外 -> 全部 out_of_scope，不是 duplicate。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
		flowEntry(1, "payment", "p1", "aa-1", "usdc", 999, 0, 999, ""),
	}
	rep, err := l.ReconcileFlowRanged(entries, ReconcileBounds{
		PaymentAfter: &pa, PaymentThrough: &pt,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for i, r := range rep.Results {
		if r.Status != ReconOutOfScope {
			t.Fatalf("result %d=%s want out_of_scope", i, r.Status)
		}
		if r.Record == nil || r.Record.ID != "p1" {
			t.Fatalf("result %d record=%+v", i, r.Record)
		}
	}
	// 流水侧不计入。
	if len(rep.Totals.Flow) != 0 {
		t.Fatalf("flow totals should be empty: %+v", rep.Totals.Flow)
	}
}

func TestReconcileFlowRangedOnlyRefunds(t *testing.T) {
	l := rangeLedger(t)
	// 付款范围 (0,0] 全空；退款全量入选。
	z := int64(0)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""), // 范围外
	}
	rep, err := l.ReconcileFlowRanged(entries, ReconcileBounds{PaymentThrough: &z})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconOutOfScope {
		t.Fatalf("p1=%s want out_of_scope", rep.Results[0].Status)
	}
	// 账本侧：扣款合计为零，退款合计 3009，净扣款 -3009。
	if len(rep.Totals.Ledger) != 1 {
		t.Fatalf("ledger rows=%+v", rep.Totals.Ledger)
	}
	row := rep.Totals.Ledger[0]
	if row.ChargedTotal != "0" || row.RefundedTotal != "3009" || row.NetCharged != "-3009" {
		t.Fatalf("ledger row=%+v", row)
	}
	// 缺失付款为空；缺失退款 r1,r2。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 2 {
		t.Fatalf("missing refunds=%+v", rep.MissingRefunds)
	}
}

func TestReconcileFlowRangedAssociationsPreserved(t *testing.T) {
	l := rangeLedger(t)
	z := int64(0)
	// 只选退款：入选退款 r1 仍指向范围外付款 p1。
	rep, err := l.ReconcileFlowRanged(nil, ReconcileBounds{PaymentThrough: &z})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.MissingRefunds[0].SettlementID != "p1" {
		t.Fatalf("selected refund r1 must link out-of-range payment p1: %+v", rep.MissingRefunds[0])
	}
	// 只选付款：入选付款 p1 仍携带范围外退款 r1。
	rep2, err := l.ReconcileFlowRanged(nil, ReconcileBounds{RefundThrough: &z})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep2.MissingPayments[0].RefundID != "r1" {
		t.Fatalf("selected payment p1 must carry out-of-range refund r1: %+v", rep2.MissingPayments[0])
	}
}

func TestReconcileFlowRangedInvalidBounds(t *testing.T) {
	l := rangeLedger(t)
	neg, zero, one, two, three, four := int64(-1), int64(0), int64(1), int64(2), int64(3), int64(4)
	bad := []ReconcileBounds{
		{PaymentAfter: &neg},                      // after 负
		{PaymentAfter: &one, PaymentThrough: &zero}, // after > through
		{PaymentThrough: &four},                   // through > maxPay(3)
		{RefundAfter: &neg},                       // refund after 负
		{RefundAfter: &two, RefundThrough: &one},  // refund after > through
		{RefundThrough: &three},                   // through > maxRef(2)
	}
	for i, b := range bad {
		if _, err := l.ReconcileFlowRanged(nil, b); err == nil || KindOf(err) != ErrInvalid {
			t.Fatalf("case %d: want invalid_parameter, got %v", i, err)
		}
	}
	// 非法边界即使流水有条目也整次拒绝。
	if _, err := l.ReconcileFlowRanged([]FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
	}, ReconcileBounds{PaymentThrough: &four}); err == nil || KindOf(err) != ErrInvalid {
		t.Fatalf("invalid bounds with entries: %v", err)
	}
}

func TestReconcileFlowRangedEmptyRange(t *testing.T) {
	l := rangeLedger(t)
	// after == through 表示范围为空。
	z := int64(0)
	rep, err := l.ReconcileFlowRanged(nil, ReconcileBounds{
		PaymentAfter: &z, PaymentThrough: &z, RefundAfter: &z, RefundThrough: &z,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("empty range missing=%+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}
	if len(rep.Totals.Ledger) != 0 {
		t.Fatalf("empty range ledger totals=%+v", rep.Totals.Ledger)
	}
}

func TestReconcileFlowRangedEmptyHistory(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1}})
	l := openOrFail(t, path)
	rep, err := l.ReconcileFlowRanged(nil, ReconcileBounds{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.PaymentAfter != 0 || rep.PaymentThrough != 0 ||
		rep.RefundAfter != 0 || rep.RefundThrough != 0 {
		t.Fatalf("empty history bounds=%d,%d,%d,%d", rep.PaymentAfter, rep.PaymentThrough,
			rep.RefundAfter, rep.RefundThrough)
	}
	if rep.MaxPaymentSeq != 0 || rep.MaxRefundSeq != 0 {
		t.Fatalf("max seqs=%d/%d", rep.MaxPaymentSeq, rep.MaxRefundSeq)
	}
}

func TestReconcileFlowRangedDuplicatesCounted(t *testing.T) {
	l := rangeLedger(t)
	pa, pt := int64(1), int64(2)
	// p2 两次（都在范围内）-> duplicate，两条都计入流水侧。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),
		flowEntry(1, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),
	}
	rep, err := l.ReconcileFlowRanged(entries, ReconcileBounds{
		PaymentAfter: &pa, PaymentThrough: &pt,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconDuplicate || rep.Results[1].Status != ReconDuplicate {
		t.Fatalf("statuses=%s,%s", rep.Results[0].Status, rep.Results[1].Status)
	}
	usdc := findTotalRow(t, rep.Totals.Flow, "aa-1", "usdc")
	if usdc.ChargedTotal != "4012" {
		t.Fatalf("flow usdc=%+v", usdc)
	}
}

// ---- Go 入口直接接收流水的参数校验 ----

// TestReconcileFlowDirectEntriesRejected 覆盖全部取值维度：两种 Go 入口对
// 非法流水都返回 invalid_parameter 且报告为空，不因调用方直接构造条目而绕过。
func TestReconcileFlowDirectEntriesRejected(t *testing.T) {
	l, _ := reconLedger(t)
	base := func() FlowEntry {
		return FlowEntry{Kind: "payment", ID: "x", Account: "aa-1", Asset: "usdc", Amount: 1, Fee: 0, Charged: 1}
	}
	bad := []struct {
		name   string
		mutate func(*FlowEntry)
	}{
		{"unknown kind", func(e *FlowEntry) { e.Kind = "transfer" }},
		{"uppercase kind", func(e *FlowEntry) { e.Kind = "PAYMENT" }},
		{"empty id", func(e *FlowEntry) { e.ID = "" }},
		{"empty account", func(e *FlowEntry) { e.Account = "" }},
		{"empty asset", func(e *FlowEntry) { e.Asset = "" }},
		{"zero amount", func(e *FlowEntry) { e.Amount = 0 }},
		{"negative amount", func(e *FlowEntry) { e.Amount = -1 }},
		{"negative fee", func(e *FlowEntry) { e.Fee = -1 }},
		{"zero charged", func(e *FlowEntry) { e.Charged = 0 }},
		{"negative charged", func(e *FlowEntry) { e.Charged = -1 }},
		{"refund missing settlement_id", func(e *FlowEntry) { e.Kind = "refund"; e.SettlementID = "" }},
		{"refund empty asset", func(e *FlowEntry) { e.Kind = "refund"; e.Asset = ""; e.SettlementID = "p1" }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			entry := base()
			tc.mutate(&entry)
			for _, call := range []struct {
				name string
				fn   func() (*ReconReport, error)
			}{
				{"full", func() (*ReconReport, error) { return l.ReconcileFlow([]FlowEntry{entry}) }},
				{"ranged", func() (*ReconReport, error) { return l.ReconcileFlowRanged([]FlowEntry{entry}, ReconcileBounds{}) }},
			} {
				rep, err := call.fn()
				if err == nil || KindOf(err) != ErrInvalid {
					t.Fatalf("%s/%s: want invalid_parameter, got %v", tc.name, call.name, err)
				}
				if rep != nil {
					t.Fatalf("%s/%s: report must be nil on parameter error, got %+v", tc.name, call.name, rep)
				}
			}
		})
	}
}

// TestReconcileFlowDirectFirstIllegalEntryPosition 要求按传入列表顺序指出
// 第一条非法流水的位置与出错字段；前面的合法条目不产生部分结果，
// 且传入条目不被改写。
func TestReconcileFlowDirectFirstIllegalEntryPosition(t *testing.T) {
	l, _ := reconLedger(t)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
		flowEntry(1, "payment", "ok", "aa-1", "usdc", 1, 0, 1, ""),
		flowEntry(2, "payment", "bad", "aa-1", "usdc", -5, 0, 1, ""),
	}
	before := append([]FlowEntry(nil), entries...)
	rep, err := l.ReconcileFlow(entries)
	if err == nil || KindOf(err) != ErrInvalid {
		t.Fatalf("want invalid_parameter, got %v", err)
	}
	if rep != nil {
		t.Fatalf("report must be nil, got %+v", rep)
	}
	if !strings.Contains(err.Error(), "entry 2") || !strings.Contains(err.Error(), "amount") {
		t.Fatalf("error must point at list position 2 and field amount: %v", err)
	}
	for i := range entries {
		if entries[i] != before[i] {
			t.Fatalf("entry %d mutated: %+v -> %+v", i, before[i], entries[i])
		}
	}

	// 第一条非法条目在位置 0 时同样指向 0 及对应字段。
	entries = []FlowEntry{
		flowEntry(0, "refund", "r1", "aa-1", "usdc", 1, 0, 1, ""),
		flowEntry(1, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
	}
	_, err = l.ReconcileFlow(entries)
	if err == nil || KindOf(err) != ErrInvalid {
		t.Fatalf("want invalid_parameter, got %v", err)
	}
	if !strings.Contains(err.Error(), "entry 0") || !strings.Contains(err.Error(), "settlement_id") {
		t.Fatalf("error must point at list position 0 and field settlement_id: %v", err)
	}
}

// TestReconcileFlowDirectIllegalEntryNotMasked 要求非法条目即使命中范围外
// 记录或参与重复编号组，也必须按参数错误整次拒绝，不能被 out_of_scope 或
// duplicate 状态掩盖。
func TestReconcileFlowDirectIllegalEntryNotMasked(t *testing.T) {
	l := rangeLedger(t)
	pa, pt := int64(1), int64(2) // p1 在付款范围 (1,2] 之外。

	// 非法条目命中范围外记录：仍是 invalid_parameter，不是 out_of_scope。
	outOfRange := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", -1, 0, 1, ""),
	}
	if _, err := l.ReconcileFlowRanged(outOfRange, ReconcileBounds{
		PaymentAfter: &pa, PaymentThrough: &pt,
	}); err == nil || KindOf(err) != ErrInvalid {
		t.Fatalf("out-of-range illegal entry must be invalid_parameter, got %v", err)
	}

	// 非法条目与其他条目组成重复编号组：仍是 invalid_parameter，不是 duplicate。
	dup := []FlowEntry{
		flowEntry(0, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),
		flowEntry(1, "payment", "p2", "aa-1", "usdc", 2000, -1, 2006, ""),
	}
	if _, err := l.ReconcileFlow(dup); err == nil || KindOf(err) != ErrInvalid {
		t.Fatalf("duplicate-group illegal entry must be invalid_parameter, got %v", err)
	}

	// 非法条目在列表末位、且前面有合法条目：同样整次拒绝。
	last := []FlowEntry{
		flowEntry(0, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),
		flowEntry(1, "payment", "p3", "aa-2", "eth", 10, 0, 10, ""),
		flowEntry(2, "payment", "x", "aa-1", "usdc", 1, 0, 0, ""),
	}
	if _, err := l.ReconcileFlow(last); err == nil || KindOf(err) != ErrInvalid {
		t.Fatalf("last-position illegal entry must be invalid_parameter, got %v", err)
	}
}

// TestReconcileFlowDirectMatchesJSONValidation 要求相同流水内容经 JSON 解析
// 入口与 Go 直接入口给出一致的判断（相同分类与消息）。
func TestReconcileFlowDirectMatchesJSONValidation(t *testing.T) {
	l, _ := reconLedger(t)
	cases := []struct {
		name string
		raw  string
		goIn FlowEntry
	}{
		{
			"unknown kind",
			`{"entries":[{"kind":"transfer","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":1}]}`,
			FlowEntry{Kind: "transfer", ID: "x", Account: "a", Asset: "z", Amount: 1, Fee: 0, Charged: 1},
		},
		{
			"negative amount",
			`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":-1,"fee":0,"charged":1}]}`,
			FlowEntry{Kind: "payment", ID: "x", Account: "a", Asset: "z", Amount: -1, Fee: 0, Charged: 1},
		},
		{
			"refund missing settlement_id",
			`{"entries":[{"kind":"refund","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":1}]}`,
			FlowEntry{Kind: "refund", ID: "x", Account: "a", Asset: "z", Amount: 1, Fee: 0, Charged: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, jsonErr := ParseReconcileRequest([]byte(tc.raw))
			if jsonErr == nil || KindOf(jsonErr) != ErrInvalid {
				t.Fatalf("json: %v", jsonErr)
			}
			_, goErr := l.ReconcileFlow([]FlowEntry{tc.goIn})
			if goErr == nil || KindOf(goErr) != ErrInvalid {
				t.Fatalf("go: %v", goErr)
			}
			if jsonErr.Error() != goErr.Error() {
				t.Fatalf("json and go must agree:\njson=%q\ngo  =%q", jsonErr, goErr)
			}
		})
	}
}

// TestReconcileFlowDirectValidInputsStillReconcile 要求金额与账本不一致
// （amount 加 fee 不等于 charged、退款引用的原付款编号与账本不同）仍按现有
// 核对规则给出字段差异，不是参数错误；正数金额达到 int64 上界仍是合法输入。
func TestReconcileFlowDirectValidInputsStillReconcile(t *testing.T) {
	l, _ := reconLedger(t)

	// amount+fee != charged：字段不符。
	rep, err := l.ReconcileFlow([]FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 999, ""),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconFieldMismatch {
		t.Fatalf("charged mismatch must be field_mismatch, got %s", rep.Results[0].Status)
	}

	// 退款引用原付款编号与账本不同：字段不符。
	rep, err = l.ReconcileFlow([]FlowEntry{
		flowEntry(0, "refund", "r1", "aa-1", "usdc", 1000, 3, 1003, "p2"),
	})
	if err != nil {
		t.Fatalf("reconcile refund: %v", err)
	}
	if rep.Results[0].Status != ReconFieldMismatch {
		t.Fatalf("settlement mismatch must be field_mismatch, got %s", rep.Results[0].Status)
	}

	// 正数金额达到 int64 上界：合法输入，累计超过 int64 仍输出精确十进制字符串。
	entries := make([]FlowEntry, 0, 10)
	for i := 0; i < 10; i++ {
		entries = append(entries, FlowEntry{
			Index: i, Kind: "payment", ID: "big" + string(rune('a'+i)),
			Account: "aa-1", Asset: "usdc",
			Amount: math.MaxInt64, Fee: 0, Charged: math.MaxInt64,
		})
	}
	rep, err = l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile max int64: %v", err)
	}
	var row ReconTotalRow
	for _, r := range rep.Totals.Flow {
		if r.Account == "aa-1" && r.Asset == "usdc" {
			row = r
		}
	}
	want := new(big.Int).Mul(big.NewInt(math.MaxInt64), big.NewInt(10))
	if row.ChargedTotal != want.String() {
		t.Fatalf("charged=%s want %s", row.ChargedTotal, want.String())
	}
}

// TestReconcileFlowDirectNilAndEmptyStillMissing 要求 Go 的 nil 或空条目列表
// 仍表示没有外部流水：正常列出核对范围内的账本缺失记录。
func TestReconcileFlowDirectNilAndEmptyStillMissing(t *testing.T) {
	l, _ := reconLedger(t)
	for _, entries := range [][]FlowEntry{nil, {}} {
		rep, err := l.ReconcileFlow(entries)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if len(rep.Results) != 0 {
			t.Fatalf("results=%v", rep.Results)
		}
		if len(rep.MissingPayments) != 2 || len(rep.MissingRefunds) != 1 {
			t.Fatalf("missing payments=%+v refunds=%+v", rep.MissingPayments, rep.MissingRefunds)
		}
	}
}

// TestReconcileFlowDirectHandleReusableAfterError 要求参数错误返回后，
// 同一句柄能继续接受后续合法对账；参数错误不改变账本状态。
func TestReconcileFlowDirectHandleReusableAfterError(t *testing.T) {
	l, _ := reconLedger(t)
	before, err := l.Query()
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	if _, err := l.ReconcileFlow([]FlowEntry{
		flowEntry(0, "payment", "bad", "aa-1", "usdc", -1, 0, 1, ""),
	}); err == nil || KindOf(err) != ErrInvalid {
		t.Fatalf("bad call: %v", err)
	}
	after, err := l.Query()
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(after)
	if string(bj) != string(aj) {
		t.Fatalf("ledger changed by parameter error:\nbefore=%s\nafter =%s", bj, aj)
	}

	// 同一句柄继续合法对账：结果与 Index 正常返回。
	rep, err := l.ReconcileFlow([]FlowEntry{
		flowEntry(7, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
	})
	if err != nil {
		t.Fatalf("good call after error: %v", err)
	}
	if rep.Results[0].Status != ReconMatched || rep.Results[0].Index != 7 {
		t.Fatalf("result=%+v", rep.Results[0])
	}
}

// TestReconcileFlowDirectSameIDInDifferentKinds 要求付款和退款使用同一编号
// 时仍分别匹配（与 JSON 入口行为一致）。
func TestReconcileFlowDirectSameIDInDifferentKinds(t *testing.T) {
	l, _ := reconLedger(t)
	entries := []FlowEntry{
		flowEntry(0, "payment", "r1", "aa-1", "usdc", 1, 0, 1, ""),
		flowEntry(1, "refund", "r1", "aa-1", "usdc", 1000, 3, 1003, "p1"),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconMissingInLedger {
		t.Fatalf("payment r1 must not match refund r1: %s", rep.Results[0].Status)
	}
	if rep.Results[1].Status != ReconMatched {
		t.Fatalf("refund r1 must match: %s", rep.Results[1].Status)
	}
}
