package payflow

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
)

// 本文件为“净扣款差额为零不等于逐笔全部匹配”提供回归保障。
//
// 对账报告同时承载四种互相不可替代的表达：
//   - results：逐条核对结论（按外部流水输入顺序）；
//   - missing_payments / missing_refunds：入选但流水未覆盖的账本记录；
//   - totals：按账户、资产的扣款/退款/净扣款合计（账本侧与流水侧各一份）；
//   - net_diff：逐组合的净扣款差额（流水减账本）。
//
// 以下场景中外部流水的总额与账本完全对平（net_charged_diff 为十进制 "0"），
// 但偏差跨记录分布：一条多记、另一条等额少记，或退款交换了各自引用的原付款
// 编号。报告必须逐笔保留 field_mismatch 与具体字段两侧取值，命中编号的记录
// 不得再进缺失列表，同时汇总如实反映“总额相等”。修复偏差后对应结果恢复
// matched；金额与手续费分项不同但 charged 相同也必须保留分项差异。整个过程
// 沿用全量入口 ReconcileFlow 与既有状态名称，且对账只读。

// regNetZeroLedger 构造同一账户/资产下两笔等额成功付款 p1、p2（aa-1/usdc，
// amount=1000、fee=3、charged=1003，30bps），并分别全额退款 r1（退 p1）、
// r2（退 p2）。账本扣款合计 2006、退款合计 2006、净扣款 0。
func regNetZeroLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa-1", Asset: "usdc", Balance: math.MaxInt64},
	})
	l := openOrFail(t, path)
	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		{ID: "p1", Account: "aa-1", Paymaster: "pm", Asset: "usdc", Amount: i64p(1000), Nonce: 1, State: "pending"},
		{ID: "p2", Account: "aa-1", Paymaster: "pm", Asset: "usdc", Amount: i64p(1000), Nonce: 2, State: "pending"},
	}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := statuses(res); got[0] != StatusSettled || got[1] != StatusSettled {
		t.Fatalf("setup statuses=%v", got)
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
	return l, path
}

// regRow 在单侧汇总中定位 (account, asset) 行。
func regRow(t *testing.T, rows []ReconTotalRow, acct, asset string) ReconTotalRow {
	t.Helper()
	return findTotalRow(t, rows, acct, asset)
}

// regDiff 在 net_diff 中定位 (account, asset) 行。
func regDiff(t *testing.T, rep *ReconReport, acct, asset string) ReconNetDiff {
	t.Helper()
	for _, d := range rep.NetDiff {
		if d.Account == acct && d.Asset == asset {
			return d
		}
	}
	t.Fatalf("net_diff row %s/%s not found in %+v", acct, asset, rep.NetDiff)
	return ReconNetDiff{}
}

// regDiffMap 把一条结果的字段差异整理成 field -> {ledger, flow}。
func regDiffMap(r ReconResult) map[string][2]string {
	out := make(map[string][2]string, len(r.Diffs))
	for _, d := range r.Diffs {
		out[d.Field] = [2]string{d.Ledger, d.Flow}
	}
	return out
}

// regAssertBalancedTotals 校验该组合两侧汇总与净差额如实反映“总额相等”：
// 扣款合计与退款合计两侧各自相等，净扣款与净差额都是十进制字符串零。
func regAssertBalancedTotals(t *testing.T, rep *ReconReport, chargedTotal, refundedTotal string) {
	t.Helper()
	lg := regRow(t, rep.Totals.Ledger, "aa-1", "usdc")
	fl := regRow(t, rep.Totals.Flow, "aa-1", "usdc")
	if lg.ChargedTotal != chargedTotal || lg.RefundedTotal != refundedTotal || lg.NetCharged != "0" {
		t.Fatalf("ledger totals=%+v want charged=%s refunded=%s net=0", lg, chargedTotal, refundedTotal)
	}
	if fl.ChargedTotal != chargedTotal || fl.RefundedTotal != refundedTotal || fl.NetCharged != "0" {
		t.Fatalf("flow totals=%+v want charged=%s refunded=%s net=0", fl, chargedTotal, refundedTotal)
	}
	if d := regDiff(t, rep, "aa-1", "usdc"); d.LedgerNetCharged != "0" ||
		d.FlowNetCharged != "0" || d.NetChargedDiff != "0" {
		t.Fatalf("net_diff=%+v want all decimal zero", d)
	}
}

// TestReconcileOffsettingPaymentMismatchesNetZero 两笔不同编号的成功付款，
// 流水用正确编号定位，却一笔多记 20、另一笔等额少记 20：整批流水（连同两笔
// 正确的退款）扣款合计与账本完全相等，但两笔付款都必须是 field_mismatch，
// 且记录详情仍指向各自原付款。
func TestReconcileOffsettingPaymentMismatchesNetZero(t *testing.T) {
	l, _ := regNetZeroLedger(t)

	// p1 多记 20（amount/charged 1000/1003 -> 1020/1023），p2 等额少记 20
	// （-> 980/983）；r1、r2 按账本原值正确呈现。账户、资产、fee 一致。
	// 付款与退款交错排列，验证逐条结果严格保持外部流水输入顺序。
	entries, err := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":980,"fee":3,"charged":983},
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"},
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1020,"fee":3,"charged":1023},
	  {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p2"}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 逐条结果保持外部流水输入顺序：两笔偏差付款是字段不符，两笔正确退款匹配。
	wantOrder := []struct {
		id     string
		kind   string
		status string
	}{
		{"p2", "payment", ReconFieldMismatch},
		{"r1", "refund", ReconMatched},
		{"p1", "payment", ReconFieldMismatch},
		{"r2", "refund", ReconMatched},
	}
	if len(rep.Results) != len(wantOrder) {
		t.Fatalf("want %d results, got %d", len(wantOrder), len(rep.Results))
	}
	for i, w := range wantOrder {
		r := rep.Results[i]
		if r.Index != i || r.ID != w.id || r.Kind != w.kind {
			t.Fatalf("result %d not in input order: %+v want %s/%s", i, r, w.kind, w.id)
		}
		if r.Status != w.status {
			t.Fatalf("result %d (%s) status=%s want %s; totals balancing must not mask per-record diffs",
				i, r.ID, r.Status, w.status)
		}
	}

	// p2 少记：差异落在 amount 与 charged，逐字保留两侧取值。
	d2 := regDiffMap(rep.Results[0])
	if len(d2) != 2 {
		t.Fatalf("p2 diffs=%v want exactly amount,charged", d2)
	}
	if d2["amount"] != [2]string{"1000", "980"} {
		t.Fatalf("p2 amount diff=%v want ledger 1000 flow 980", d2["amount"])
	}
	if d2["charged"] != [2]string{"1003", "983"} {
		t.Fatalf("p2 charged diff=%v want ledger 1003 flow 983", d2["charged"])
	}
	// p1 多记。
	d1 := regDiffMap(rep.Results[2])
	if len(d1) != 2 {
		t.Fatalf("p1 diffs=%v want exactly amount,charged", d1)
	}
	if d1["amount"] != [2]string{"1000", "1020"} {
		t.Fatalf("p1 amount diff=%v want ledger 1000 flow 1020", d1["amount"])
	}
	if d1["charged"] != [2]string{"1003", "1023"} {
		t.Fatalf("p1 charged diff=%v want ledger 1003 flow 1023", d1["charged"])
	}

	// 记录详情仍指向各自原付款（包括退款关联），不得因金额串号而互换。
	r2 := rep.Results[0].Record
	if r2 == nil || r2.Kind != "payment" || r2.ID != "p2" || r2.Seq != 2 ||
		r2.Amount != 1000 || r2.Fee != 3 || r2.Charged != 1003 || r2.RefundID != "r2" {
		t.Fatalf("p2 record must point to original payment: %+v", r2)
	}
	r1 := rep.Results[2].Record
	if r1 == nil || r1.Kind != "payment" || r1.ID != "p1" || r1.Seq != 1 ||
		r1.Amount != 1000 || r1.Fee != 3 || r1.Charged != 1003 || r1.RefundID != "r1" {
		t.Fatalf("p1 record must point to original payment: %+v", r1)
	}

	// 四条记录全部通过各自编号找到：缺失列表（付款与退款）必须为空，
	// 命中编号的记录不能因为字段不符而被当成缺失。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("matched-by-id payments must not be missing: %+v", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("covered refunds must not be missing: %+v", rep.MissingRefunds)
	}

	// 扣款合计 1023+983=2006 与账本相等，净扣款、净差额如实为零。
	regAssertBalancedTotals(t, rep, "2006", "2006")

	// 把偏差改回账本原值：四条恢复 matched，缺失列表仍为空，汇总保持相等。
	corrected := []FlowEntry{
		{Index: 0, Kind: "payment", ID: "p2", Account: "aa-1", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
		{Index: 1, Kind: "refund", ID: "r1", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p1"},
		{Index: 2, Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
		{Index: 3, Kind: "refund", ID: "r2", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p2"},
	}
	rep2, err := l.ReconcileFlow(corrected)
	if err != nil {
		t.Fatalf("reconcile corrected: %v", err)
	}
	for i, r := range rep2.Results {
		if r.Status != ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("corrected result %d (%s)=%+v want matched", i, r.ID, r)
		}
	}
	if len(rep2.MissingPayments) != 0 || len(rep2.MissingRefunds) != 0 {
		t.Fatalf("corrected missing=%+v/%+v", rep2.MissingPayments, rep2.MissingRefunds)
	}
	regAssertBalancedTotals(t, rep2, "2006", "2006")
}

// TestReconcilePaymentFeeBreakdownDiffChargedEqual amount 与 fee 分项互换、
// charged 总额相同：净差额为零也必须保留两个分项差异，不能因总额一致而匹配。
func TestReconcilePaymentFeeBreakdownDiffChargedEqual(t *testing.T) {
	l, _ := regNetZeroLedger(t)
	// 账本 p1：amount=1000 fee=3 charged=1003；流水：amount=990 fee=13
	// charged=1003。分项不同、扣款总额相同。只核对 p1。
	entries := []FlowEntry{
		{Index: 0, Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc",
			Amount: 990, Fee: 13, Charged: 1003},
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	r := rep.Results[0]
	if r.Status != ReconFieldMismatch {
		t.Fatalf("status=%s want field_mismatch: equal charged must not hide amount/fee diffs", r.Status)
	}
	d := regDiffMap(r)
	if len(d) != 2 {
		t.Fatalf("diffs=%v want exactly amount,fee", d)
	}
	if d["amount"] != [2]string{"1000", "990"} {
		t.Fatalf("amount diff=%v", d["amount"])
	}
	if d["fee"] != [2]string{"3", "13"} {
		t.Fatalf("fee diff=%v", d["fee"])
	}
	if _, ok := d["charged"]; ok {
		t.Fatalf("charged must not be listed as diff: %v", d["charged"])
	}
	if r.Record == nil || r.Record.ID != "p1" {
		t.Fatalf("record detail missing: %+v", r.Record)
	}
	// 只来一笔 1003 的扣款：流水侧 charged_total=1003，与账本侧 2006 不等，
	// 净差额按真实总额给出（不允许因为 p1 自身 charged 相同而置零）。
	lg := regRow(t, rep.Totals.Ledger, "aa-1", "usdc")
	fl := regRow(t, rep.Totals.Flow, "aa-1", "usdc")
	if lg.ChargedTotal != "2006" || fl.ChargedTotal != "1003" {
		t.Fatalf("totals ledger=%+v flow=%+v", lg, fl)
	}
	if d := regDiff(t, rep, "aa-1", "usdc"); d.NetChargedDiff != "1003" {
		t.Fatalf("net_charged_diff=%s want 1003 (flow 1003-2006 vs ledger 0)", d.NetChargedDiff)
	}
}

// TestReconcileSwappedRefundSettlementsNetZero 两笔等额付款均已全额退款；
// 流水的退款编号、账户、资产、金额全部正确，只交换了各自引用的原付款编号。
// 整批流水（连同两笔正确的原付款）退款合计与净扣款差额完全对平，两笔退款
// 仍必须报告 settlement_id 字段不符，并携带账本中真正的原付款关联。
func TestReconcileSwappedRefundSettlementsNetZero(t *testing.T) {
	l, _ := regNetZeroLedger(t)

	// r1/r2 编号、金额均正确，仅 settlement_id 互换；p1/p2 按原扣款正确。
	// 付款与退款交错排列，验证逐条结果严格保持外部流水输入顺序。
	entries, err := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p2"},
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"},
	  {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	wantOrder := []struct {
		id     string
		kind   string
		status string
	}{
		{"r1", "refund", ReconFieldMismatch},
		{"p1", "payment", ReconMatched},
		{"r2", "refund", ReconFieldMismatch},
		{"p2", "payment", ReconMatched},
	}
	if len(rep.Results) != len(wantOrder) {
		t.Fatalf("want %d results, got %d", len(wantOrder), len(rep.Results))
	}
	// 逐条结果保持输入顺序：两笔退款都是 field_mismatch，差异只在 settlement_id，
	// 付款曾经退款不影响原付款按自己的编号 matched。
	for i, w := range wantOrder {
		r := rep.Results[i]
		if r.Index != i || r.ID != w.id || r.Kind != w.kind {
			t.Fatalf("result %d not in input order: %+v want %s/%s", i, r, w.kind, w.id)
		}
		if r.Status != w.status {
			t.Fatalf("result %d (%s) status=%s want %s; swapped settlement_id must be reported even when totals net zero",
				i, r.ID, r.Status, w.status)
		}
	}
	// r1 账本真正指向 p1，流水误填 p2；r2 反之。差异值逐字并列。
	if got := regDiffMap(rep.Results[0])["settlement_id"]; got != [2]string{"p1", "p2"} {
		t.Fatalf("r1 settlement_id diff=%v want ledger p1 flow p2", got)
	}
	if len(regDiffMap(rep.Results[0])) != 1 {
		t.Fatalf("r1 must differ only on settlement_id: %+v", rep.Results[0].Diffs)
	}
	if got := regDiffMap(rep.Results[2])["settlement_id"]; got != [2]string{"p2", "p1"} {
		t.Fatalf("r2 settlement_id diff=%v want ledger p2 flow p1", got)
	}
	if len(regDiffMap(rep.Results[2])) != 1 {
		t.Fatalf("r2 must differ only on settlement_id: %+v", rep.Results[2].Diffs)
	}
	// 记录详情携带账本中真正的原付款关联（以及退款自身序号），不能互相替代。
	rec1 := rep.Results[0].Record
	if rec1 == nil || rec1.Kind != "refund" || rec1.ID != "r1" || rec1.Seq != 1 ||
		rec1.SettlementID != "p1" || rec1.Amount != 1000 || rec1.Fee != 3 || rec1.Charged != 1003 {
		t.Fatalf("r1 record must carry true origin payment p1: %+v", rec1)
	}
	rec2 := rep.Results[2].Record
	if rec2 == nil || rec2.Kind != "refund" || rec2.ID != "r2" || rec2.Seq != 2 ||
		rec2.SettlementID != "p2" {
		t.Fatalf("r2 record must carry true origin payment p2: %+v", rec2)
	}
	// 匹配的付款仍带各自退款关联，不因退款串号而丢失。
	if rep.Results[1].Record == nil || rep.Results[1].Record.RefundID != "r1" {
		t.Fatalf("p1 must still link refund r1: %+v", rep.Results[1].Record)
	}
	if rep.Results[3].Record == nil || rep.Results[3].Record.RefundID != "r2" {
		t.Fatalf("p2 must still link refund r2: %+v", rep.Results[3].Record)
	}

	// 四条记录全部通过各自编号找到：缺失列表（付款与退款）必须为空。
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("matched-by-id refunds must not be missing: %+v", rep.MissingRefunds)
	}
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("covered payments must not be missing: %+v", rep.MissingPayments)
	}

	// 两侧扣款合计 2006、退款合计 2006、净扣款为零、净差额为十进制零。
	regAssertBalancedTotals(t, rep, "2006", "2006")

	// 交换回正确的原付款编号后四条全部恢复 matched；金额字段本就一致。
	corrected := []FlowEntry{
		{Index: 0, Kind: "refund", ID: "r1", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p1"},
		{Index: 1, Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
		{Index: 2, Kind: "refund", ID: "r2", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p2"},
		{Index: 3, Kind: "payment", ID: "p2", Account: "aa-1", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
	}
	rep2, err := l.ReconcileFlow(corrected)
	if err != nil {
		t.Fatalf("reconcile corrected: %v", err)
	}
	for i, r := range rep2.Results {
		if r.Status != ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("corrected result %d (%s)=%+v want matched", i, r.ID, r)
		}
	}
	if len(rep2.MissingPayments) != 0 || len(rep2.MissingRefunds) != 0 {
		t.Fatalf("corrected missing=%+v/%+v", rep2.MissingPayments, rep2.MissingRefunds)
	}
	regAssertBalancedTotals(t, rep2, "2006", "2006")
}

// TestReconcileRefundedPaymentStillCheckedOnItsOwn 付款曾经退款不影响原付款的
// 核对：同一批流水里一笔退款 settlement_id 串号，另一笔原付款按自己的编号与
// 金额判断；两类记录各按自己的编号空间匹配，不能因为金额相同就互相替代。
func TestReconcileRefundedPaymentStillCheckedOnItsOwn(t *testing.T) {
	l, _ := regNetZeroLedger(t)
	// r1 编号命中、金额正确，但原付款误填成 p2 -> field_mismatch(settlement_id)；
	// p1 按原扣款如实呈现 -> matched。两笔金额相同（1003）不允许互相顶替。
	entries := []FlowEntry{
		{Index: 0, Kind: "refund", ID: "r1", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p2"},
		{Index: 1, Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003},
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconFieldMismatch {
		t.Fatalf("r1=%s want field_mismatch", rep.Results[0].Status)
	}
	if d := regDiffMap(rep.Results[0]); len(d) != 1 || d["settlement_id"] != [2]string{"p1", "p2"} {
		t.Fatalf("r1 diffs=%v", d)
	}
	if rep.Results[1].Status != ReconMatched {
		t.Fatalf("p1=%s want matched even though it was refunded and r1 mismatches", rep.Results[1].Status)
	}
	// 已退款的付款匹配时仍带退款关联；退款记录带真正的原付款编号。
	if rep.Results[1].Record == nil || rep.Results[1].Record.RefundID != "r1" {
		t.Fatalf("p1 must still link refund r1: %+v", rep.Results[1].Record)
	}
	if rep.Results[0].Record == nil || rep.Results[0].Record.SettlementID != "p1" {
		t.Fatalf("r1 record must carry true settlement p1: %+v", rep.Results[0].Record)
	}
	// p1、r1 均已覆盖；缺失只剩 p2、r2。
	if ids := refs(rep.MissingPayments); ids != "p2" {
		t.Fatalf("missing payments=%s want p2", ids)
	}
	if ids := refs(rep.MissingRefunds); ids != "r2" {
		t.Fatalf("missing refunds=%s want r2", ids)
	}
}

// TestReconcileNetZeroDeviationsAndRecoveryAcrossSameBatch 同一批流水先呈现
// 跨记录偏差（净额对平），再把偏差全部改回账本原值：偏差状态、缺失列表、
// 汇总金额与恢复后的 matched 必须在同一份公开报告结构里自洽表达同一批流水。
func TestReconcileNetZeroDeviationsAndRecoveryAcrossSameBatch(t *testing.T) {
	l, _ := regNetZeroLedger(t)

	// 混合一批：p1 多记 20、p2 少记 20（付款净额对平）；r1 串号到 p2
	// （退款净额因 r2 缺席不再对平，但 r1 自身仍是 settlement_id 不符）。
	bad := []FlowEntry{
		{Index: 0, Kind: "payment", ID: "p2", Account: "aa-1", Asset: "usdc", Amount: 980, Fee: 3, Charged: 983},
		{Index: 1, Kind: "refund", ID: "r1", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p2"},
		{Index: 2, Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc", Amount: 1020, Fee: 3, Charged: 1023},
	}
	rep, err := l.ReconcileFlow(bad)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	wantStatus := []string{ReconFieldMismatch, ReconFieldMismatch, ReconFieldMismatch}
	for i, r := range rep.Results {
		if r.Status != wantStatus[i] {
			t.Fatalf("bad batch result %d (%s)=%s want %s", i, r.ID, r.Status, wantStatus[i])
		}
	}
	// 输入顺序同时保留（付款与退款交错，不重排）。
	wantOrder := []string{"p2", "r1", "p1"}
	for i, r := range rep.Results {
		if r.ID != wantOrder[i] || r.Index != i {
			t.Fatalf("result %d id=%s index=%d want %s", i, r.ID, r.Index, wantOrder[i])
		}
	}
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("bad batch missing payments=%+v", rep.MissingPayments)
	}
	if ids := refs(rep.MissingRefunds); ids != "r2" {
		t.Fatalf("bad batch missing refunds=%s want r2", ids)
	}
	// 付款扣款合计仍为 2006（983+1023），但流水只来一笔退款 1003：
	// 净扣款 = 1003，而账本净扣款为 0，净差额 +1003——逐项偏差与汇总并存。
	fl := regRow(t, rep.Totals.Flow, "aa-1", "usdc")
	if fl.ChargedTotal != "2006" || fl.RefundedTotal != "1003" || fl.NetCharged != "1003" {
		t.Fatalf("flow totals=%+v", fl)
	}
	if d := regDiff(t, rep, "aa-1", "usdc"); d.NetChargedDiff != "1003" {
		t.Fatalf("net diff=%+v want 1003", d)
	}

	// 全部偏差改回账本原值（r1 指回 p1），并补齐 r2 与正确的 p1/p2：
	// 四条全部 matched，两侧合计 2006/2006、净差额恢复为零。
	good := []FlowEntry{
		{Index: 0, Kind: "payment", ID: "p2", Account: "aa-1", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
		{Index: 1, Kind: "refund", ID: "r1", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p1"},
		{Index: 2, Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
		{Index: 3, Kind: "refund", ID: "r2", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p2"},
	}
	rep2, err := l.ReconcileFlow(good)
	if err != nil {
		t.Fatalf("reconcile good: %v", err)
	}
	for i, r := range rep2.Results {
		if r.Status != ReconMatched {
			t.Fatalf("good batch result %d (%s)=%s want matched", i, r.ID, r.Status)
		}
	}
	if len(rep2.MissingPayments) != 0 || len(rep2.MissingRefunds) != 0 {
		t.Fatalf("good batch missing=%+v/%+v", rep2.MissingPayments, rep2.MissingRefunds)
	}
	regAssertBalancedTotals(t, rep2, "2006", "2006")
}

// TestReconcileNetZeroDeviationsAreValidReportsNotErrors 这些场景的输入字段与
// 数值全部合法：跨记录偏差必须作为正常对账报告返回（退出语义层面无错误），
// 不能变成 invalid_parameter，也不能省略不一致的逐条结果。
func TestReconcileNetZeroDeviationsAreValidReportsNotErrors(t *testing.T) {
	l, _ := regNetZeroLedger(t)

	cases := []struct {
		name    string
		raw     string
		badID   string // 必然带字段差异的编号
		diffFld string
	}{
		{
			"offsetting payment amounts",
			`{"entries":[
			 {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1020,"fee":3,"charged":1023},
			 {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":980,"fee":3,"charged":983}
			]}`,
			"p1", "amount",
		},
		{
			"swapped refund settlements",
			`{"entries":[
			 {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p2"},
			 {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"}
			]}`,
			"r1", "settlement_id",
		},
		{
			"amount/fee breakdown shift with equal charged",
			`{"entries":[
			 {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":990,"fee":13,"charged":1003}
			]}`,
			"p1", "amount",
		},
	}
	for _, c := range cases {
		entries, err := ParseReconcileRequest([]byte(c.raw))
		if err != nil {
			t.Fatalf("%s: valid input must parse: %v", c.name, err)
		}
		rep, err := l.ReconcileFlow(entries)
		if err != nil || rep == nil {
			t.Fatalf("%s: deviations are a normal report, not invalid_parameter: rep=%+v err=%v", c.name, rep, err)
		}
		found := false
		for _, r := range rep.Results {
			if r.ID != c.badID {
				continue
			}
			found = true
			if r.Status != ReconFieldMismatch {
				t.Fatalf("%s: %s status=%s want field_mismatch", c.name, r.ID, r.Status)
			}
			if _, ok := regDiffMap(r)[c.diffFld]; !ok {
				t.Fatalf("%s: %s missing diff on %s: %+v", c.name, r.ID, c.diffFld, r.Diffs)
			}
		}
		if !found {
			t.Fatalf("%s: inconsistent result for %s must not be omitted: %+v", c.name, c.badID, rep.Results)
		}
	}
}

// TestReconcileNetZeroDeviationsAreReadOnly 带跨记录偏差的对账前后，余额、
// 付款与退款历史以及账本文件内容必须逐字节保持一致（偏差只存在于报告中）。
func TestReconcileNetZeroDeviationsAreReadOnly(t *testing.T) {
	l, path := regNetZeroLedger(t)

	before, err := l.Query()
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	bj, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("marshal before: %v", err)
	}
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger before: %v", err)
	}

	deviating := []FlowEntry{
		{Index: 0, Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc", Amount: 1020, Fee: 3, Charged: 1023},
		{Index: 1, Kind: "payment", ID: "p2", Account: "aa-1", Asset: "usdc", Amount: 980, Fee: 3, Charged: 983},
		{Index: 2, Kind: "refund", ID: "r1", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p2"},
		{Index: 3, Kind: "refund", ID: "r2", Account: "aa-1", Asset: "usdc",
			Amount: 1000, Fee: 3, Charged: 1003, SettlementID: "p1"},
	}
	rep, err := l.ReconcileFlow(deviating)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for i, r := range rep.Results {
		if r.Status != ReconFieldMismatch {
			t.Fatalf("setup: result %d (%s)=%s want field_mismatch", i, r.ID, r.Status)
		}
	}

	after, err := l.Query()
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	aj, err := json.Marshal(after)
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("ledger state changed by reconcile:\nbefore=%s\nafter =%s", bj, aj)
	}
	diskAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger after: %v", err)
	}
	if string(diskBefore) != string(diskAfter) {
		t.Fatalf("ledger file changed by reconcile:\nbefore=%s\nafter =%s", diskBefore, diskAfter)
	}

	// 重新打开同一文件，余额与成功历史仍与对账前完全一致。
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	reopened, err := l2.Query()
	if err != nil {
		t.Fatalf("query reopened: %v", err)
	}
	if !reflect.DeepEqual(before, reopened) {
		rj, _ := json.Marshal(reopened)
		t.Fatalf("reopened ledger differs:\ndisk=%s\nmem =%s", rj, bj)
	}
}
