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
