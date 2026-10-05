package payflow

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 本文件为离线对账报告与账本、以及报告内部各条目之间的独立性提供回归保障：
//
//  1. 组内独立：同一 (kind,id) 在输入中出现多次时，整组逐项返回且各项都携带
//     完整输入位置与对应成功记录；调用者改动其中一项的记录编号、账户、金额、
//     付款/退款关联或位置列表，同组其他项保持原样。范围外记录被多条流水引用
//     时同样如此（out_of_scope 优先于整组重复的判定不变）。
//  2. 类别独立：付款编号与退款编号即使相同也是两类独立记录，编辑一类结果
//     不影响另一类。
//  3. 报告与账本独立：改动报告的逐项详情、字段差异、缺失记录与金额汇总后，
//     查询到的余额与成功历史不变；用原流水原范围重新对账仍得到依据真实账本
//     计算的结果，不沿用调用者改过的数据。对账与编辑返回值都不改写账本文件。
//  4. 时效独立：取得报告后账本再发生付款或退款，新报告反映真实变化，旧报告
//     保留取得时的记录、缺失列表、金额汇总、最大成功序号与实际范围；原先未
//     退款的付款后来退款时，旧报告里的付款不会自动出现新退款关联，新报告则
//     带上该关联（即使关联退款不在选定范围内）。空流水的缺失报告同样如此。
//
// 核对规则、报告格式与公开入口（ReconcileFlow / ReconcileFlowRanged）不在
// 本文件改动范围内，这里只锁定既有返回行为的隔离性。

// marshalReport 把报告序列化为 JSON，用于整体比对“重新对账是否得到逐字相同
// 的真实计算结果”以及“旧报告是否逐字冻结”。
func marshalReport(t *testing.T, rep *ReconReport) []byte {
	t.Helper()
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	return raw
}

// TestReconcileDuplicateGroupEditsStayPrivate 覆盖范围内重复组：同一付款编号、
// 同一退款编号各自在输入中出现多次，整组逐项返回 duplicate，各项携带全部输入
// 位置与对应成功记录；改动其中一项的记录字段或位置列表不波及其他项。
func TestReconcileDuplicateGroupEditsStayPrivate(t *testing.T) {
	l := rangeLedger(t) // p1,p2(usdc)、p3(eth)；r1 退 p1、r2 退 p2

	entries := []FlowEntry{
		flowEntry(0, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),
		flowEntry(1, "payment", "p2", "aa-1", "usdc", 1, 0, 1, ""), // 内容不同也整组重复
		flowEntry(2, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),
		flowEntry(3, "refund", "r2", "aa-1", "usdc", 2000, 6, 2006, "p2"),
		flowEntry(4, "refund", "r2", "aa-1", "usdc", 5, 0, 5, "p9"),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 整组各项都是 duplicate，且都携带完整输入位置与对应成功记录。
	wantPos := [][]int{{0, 1, 2}, {0, 1, 2}, {0, 1, 2}, {3, 4}, {3, 4}}
	for i, r := range rep.Results {
		if r.Status != ReconDuplicate {
			t.Fatalf("result %d status=%s want duplicate", i, r.Status)
		}
		if !reflect.DeepEqual(r.Positions, wantPos[i]) {
			t.Fatalf("result %d positions=%v want %v", i, r.Positions, wantPos[i])
		}
		if r.Record == nil {
			t.Fatalf("result %d must carry the ledger record", i)
		}
	}
	// 付款项记录指向 p2 并携带其退款关联；退款项记录指向 r2 并携带原付款编号。
	for _, i := range []int{0, 1, 2} {
		rec := rep.Results[i].Record
		if rec.Kind != "payment" || rec.ID != "p2" || rec.Seq != 2 ||
			rec.Charged != 2006 || rec.RefundID != "r2" || rec.RefundSeq != 2 {
			t.Fatalf("result %d payment record=%+v", i, rec)
		}
	}
	for _, i := range []int{3, 4} {
		rec := rep.Results[i].Record
		if rec.Kind != "refund" || rec.ID != "r2" || rec.Seq != 2 ||
			rec.Charged != 2006 || rec.SettlementID != "p2" {
			t.Fatalf("result %d refund record=%+v", i, rec)
		}
	}

	// 先保留同组其他项的原始内容，再对第一项做破坏性改写。
	payOrig1 := *rep.Results[1].Record
	payOrig2 := *rep.Results[2].Record
	posOrig1 := append([]int(nil), rep.Results[1].Positions...)
	posOrig2 := append([]int(nil), rep.Results[2].Positions...)
	refOrig4 := *rep.Results[4].Record
	posOrig4 := append([]int(nil), rep.Results[4].Positions...)

	// 调用者改动第一项：记录编号、账户、金额、付款与退款关联、位置列表。
	rec0 := rep.Results[0].Record
	rec0.ID = "HACK"
	rec0.Account = "zz"
	rec0.Asset = "btc"
	rec0.Amount = 1
	rec0.Fee = 1
	rec0.Charged = 2
	rec0.Seq = 99
	rec0.SettlementID = "forged"
	rec0.RefundID = "HACK-R"
	rec0.RefundSeq = 99
	rep.Results[0].Positions[0] = 999
	rep.Results[0].Positions = append(rep.Results[0].Positions, 12345)

	// 同组其他付款项的记录与位置列表保持原样。
	if got := rep.Results[1].Record; *got != payOrig1 {
		t.Fatalf("result 1 record polluted by editing result 0:\n got %+v\nwant %+v", got, payOrig1)
	}
	if got := rep.Results[2].Record; *got != payOrig2 {
		t.Fatalf("result 2 record polluted by editing result 0:\n got %+v\nwant %+v", got, payOrig2)
	}
	if !reflect.DeepEqual(rep.Results[1].Positions, posOrig1) ||
		!reflect.DeepEqual(rep.Results[2].Positions, posOrig2) {
		t.Fatalf("payment positions polluted: %v / %v", rep.Results[1].Positions, rep.Results[2].Positions)
	}
	// 退款组也不受付款组编辑影响。
	if got := rep.Results[4].Record; *got != refOrig4 {
		t.Fatalf("refund result 4 polluted by editing payment result 0:\n got %+v\nwant %+v", got, refOrig4)
	}

	// 改动退款组第一项的原付款关联与位置，退款组第二项保持原样。
	rec3 := rep.Results[3].Record
	rec3.SettlementID = "ghost"
	rec3.ID = "HACK-REF"
	rec3.Amount = 7
	rep.Results[3].Positions[1] = 888
	if got := rep.Results[4].Record; *got != refOrig4 {
		t.Fatalf("result 4 record polluted by editing result 3:\n got %+v\nwant %+v", got, refOrig4)
	}
	if !reflect.DeepEqual(rep.Results[4].Positions, posOrig4) {
		t.Fatalf("result 4 positions polluted: %v want %v", rep.Results[4].Positions, posOrig4)
	}

	// 用同一流水重新对账，仍得到依据真实账本计算的原始内容。
	fresh, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if got := fresh.Results[0].Record; got == nil || got.ID != "p2" || got.RefundID != "r2" ||
		got.Charged != 2006 || got.Seq != 2 {
		t.Fatalf("fresh report reused caller edits: %+v", got)
	}
	if !reflect.DeepEqual(fresh.Results[0].Positions, []int{0, 1, 2}) {
		t.Fatalf("fresh positions=%v", fresh.Results[0].Positions)
	}
}

// TestReconcileSameIDPaymentAndRefundEditsIndependent 覆盖编号相同的付款与退款：
// 两类记录各自独立匹配，编辑其中一类结果的记录不影响另一类。
func TestReconcileSameIDPaymentAndRefundEditsIndependent(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)
	// 付款编号与退款编号都是 "same"：退款 "same" 退回付款 "same"。
	if r := mustSettle(t, l, 0, intent("same", "aa", "usdc", 100)); r.Status != StatusSettled {
		t.Fatalf("payment: %+v", r)
	}
	if r := mustRefundOne(t, l, "same", "same", "take it back"); r.Status != StatusRefundSuccess {
		t.Fatalf("refund: %+v", r)
	}

	entries := []FlowEntry{
		flowEntry(0, "payment", "same", "aa", "usdc", 100, 0, 100, ""),
		flowEntry(1, "refund", "same", "aa", "usdc", 100, 0, 100, "same"),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconMatched || rep.Results[1].Status != ReconMatched {
		t.Fatalf("statuses=%s,%s want both matched", rep.Results[0].Status, rep.Results[1].Status)
	}
	payRec, refRec := rep.Results[0].Record, rep.Results[1].Record
	if payRec == nil || refRec == nil {
		t.Fatalf("both results must carry records: %+v / %+v", payRec, refRec)
	}
	// 付款记录携带退款关联（同名），退款记录携带原付款编号（同名）。
	if payRec.RefundID != "same" || payRec.RefundSeq != 1 || refRec.SettlementID != "same" {
		t.Fatalf("cross links: payment=%+v refund=%+v", payRec, refRec)
	}
	payOrig, refOrig := *payRec, *refRec

	// 编辑付款类结果的记录：退款类结果的记录逐字保持原样。
	payRec.ID = "HACK"
	payRec.Account = "zz"
	payRec.Amount = 1
	payRec.Charged = 1
	payRec.Seq = 77
	payRec.RefundID = "HACK-R"
	payRec.RefundSeq = 77
	payRec.SettlementID = "forged"
	if got := rep.Results[1].Record; *got != refOrig {
		t.Fatalf("refund record polluted by editing same-ID payment record:\n got %+v\nwant %+v", got, refOrig)
	}

	// 编辑退款类结果的记录（含原付款关联）：付款类结果不被连带。
	refRec.SettlementID = "ghost"
	refRec.ID = "HACK-REF"
	refRec.Account = "zz"
	if got := rep.Results[0].Record; got.ID != "HACK" || got.RefundID != "HACK-R" {
		// 付款记录仍是调用者自己改过的样子，没有被退款记录的编辑覆盖或联动。
		t.Fatalf("payment record unexpectedly changed by editing refund record: %+v", got)
	}

	// 重新对账：两类记录都恢复为账本真实内容，互不污染。
	fresh, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if got := fresh.Results[0].Record; got == nil || *got != payOrig {
		t.Fatalf("fresh payment record:\n got %+v\nwant %+v", got, payOrig)
	}
	if got := fresh.Results[1].Record; got == nil || *got != refOrig {
		t.Fatalf("fresh refund record:\n got %+v\nwant %+v", got, refOrig)
	}
}

// TestReconcileOutOfScopeSharedRecordEditsStayPrivate 覆盖范围外记录被多条流水
// 引用：各条结果都按 out_of_scope 返回并携带同一条账本记录（含原有退款关联），
// 修改其中一条结果的记录不连带改变其他条目。
func TestReconcileOutOfScopeSharedRecordEditsStayPrivate(t *testing.T) {
	l := rangeLedger(t)
	pa, pt := int64(1), int64(2) // p1(seq 1) 在范围外，且已被 r1 退款
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
		flowEntry(1, "payment", "p1", "aa-1", "usdc", 999, 0, 999, ""), // 内容不同也按范围外处理
		flowEntry(2, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
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
		if r.Record == nil || r.Record.ID != "p1" || r.Record.RefundID != "r1" ||
			r.Record.RefundSeq != 1 || r.Record.Charged != 1003 {
			t.Fatalf("result %d must carry the out-of-scope record with its refund link: %+v", i, r.Record)
		}
	}
	orig1, orig2 := *rep.Results[1].Record, *rep.Results[2].Record

	// 修改第一条结果的记录（编号、账户、金额、退款关联）。
	rec0 := rep.Results[0].Record
	rec0.ID = "HACK"
	rec0.Account = "zz"
	rec0.Amount = 1
	rec0.Charged = 1
	rec0.Seq = 42
	rec0.RefundID = "HACK-R"
	rec0.RefundSeq = 42

	// 引用同一范围外记录的其他条目保持原样。
	if got := rep.Results[1].Record; *got != orig1 {
		t.Fatalf("result 1 polluted by editing result 0:\n got %+v\nwant %+v", got, orig1)
	}
	if got := rep.Results[2].Record; *got != orig2 {
		t.Fatalf("result 2 polluted by editing result 0:\n got %+v\nwant %+v", got, orig2)
	}

	// 重新对账仍返回真实记录与关联。
	fresh, err := l.ReconcileFlowRanged(entries, ReconcileBounds{
		PaymentAfter: &pa, PaymentThrough: &pt,
	})
	if err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if got := fresh.Results[0].Record; got == nil || got.ID != "p1" || got.RefundID != "r1" ||
		got.Charged != 1003 || got.Seq != 1 {
		t.Fatalf("fresh out-of-scope record reused caller edits: %+v", got)
	}
}

// TestReconcileReportEditsDoNotTouchLedger 覆盖报告与账本之间的隔离：调用者改动
// 报告的逐项详情、字段差异、缺失记录与金额汇总后，查询到的余额与成功历史保持
// 原样，重新对账仍得到依据真实账本计算的整份报告。
func TestReconcileReportEditsDoNotTouchLedger(t *testing.T) {
	l := rangeLedger(t)
	// p1 匹配；p2 字段不符（fee）；ghost 账本外。缺失：付款 p3，退款 r1、r2。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
		flowEntry(1, "payment", "p2", "aa-1", "usdc", 2000, 9, 2009, ""),
		flowEntry(2, "payment", "ghost", "aa-1", "usdc", 5, 0, 5, ""),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconMatched || rep.Results[1].Status != ReconFieldMismatch ||
		rep.Results[2].Status != ReconMissingInLedger {
		t.Fatalf("statuses=%s,%s,%s", rep.Results[0].Status, rep.Results[1].Status, rep.Results[2].Status)
	}
	if len(rep.MissingPayments) != 1 || len(rep.MissingRefunds) != 2 {
		t.Fatalf("missing payments=%+v refunds=%+v", rep.MissingPayments, rep.MissingRefunds)
	}

	before, err := l.Query()
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	beforeJSON, _ := json.Marshal(before)
	reportJSON := marshalReport(t, rep)

	// 逐项详情：命中记录、字段差异、缺失记录全部改写。
	rec := rep.Results[0].Record
	rec.ID, rec.Account, rec.Asset = "HACK", "zz", "btc"
	rec.Amount, rec.Fee, rec.Charged, rec.Seq = 1, 1, 2, 99
	rec.RefundID, rec.RefundSeq = "HACK-R", 99
	rep.Results[1].Diffs[0].Field = "forged"
	rep.Results[1].Diffs[0].Ledger = "forged-ledger"
	rep.Results[1].Diffs[0].Flow = "forged-flow"
	rep.Results[1].Diffs = append(rep.Results[1].Diffs, FieldDiff{Field: "x", Ledger: "y", Flow: "z"})
	rep.MissingPayments[0].ID = "HACK-P"
	rep.MissingPayments[0].Charged = 1
	rep.MissingPayments[0].Seq = 99
	rep.MissingRefunds[0].ID = "HACK-R"
	rep.MissingRefunds[0].SettlementID = "ghost"
	rep.MissingRefunds = append(rep.MissingRefunds, ReconRecordRef{ID: "fake-refund"})

	// 金额汇总与差额：账本侧、流水侧、净差额、序号与范围全部改写。
	rep.Totals.Ledger[0].ChargedTotal = "0"
	rep.Totals.Ledger[0].RefundedTotal = "0"
	rep.Totals.Ledger[0].NetCharged = "0"
	rep.Totals.Flow[0].ChargedTotal = "999999999"
	rep.Totals.Ledger = append(rep.Totals.Ledger, ReconTotalRow{Account: "fake", Asset: "fake", NetCharged: "7"})
	rep.NetDiff[0].LedgerNetCharged = "0"
	rep.NetDiff[0].FlowNetCharged = "0"
	rep.NetDiff[0].NetChargedDiff = "0"
	rep.MaxPaymentSeq, rep.MaxRefundSeq = 99, 99
	rep.PaymentAfter, rep.PaymentThrough = 99, 99
	rep.RefundAfter, rep.RefundThrough = 99, 99

	// 余额与成功历史保持原样。
	after, err := l.Query()
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("ledger changed by editing the report:\nbefore=%s\nafter =%s", beforeJSON, afterJSON)
	}
	if bal, _ := l.Balance("aa-1", "usdc"); bal != 1000000-1003-2006+1003+2006 {
		t.Fatalf("balance changed: %d", bal)
	}

	// 用原来的流水重新对账：整份报告与改动前逐字一致，不沿用调用者改过的数据。
	fresh, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if got := marshalReport(t, fresh); !bytes.Equal(got, reportJSON) {
		t.Fatalf("fresh report reused caller edits:\nwant %s\ngot  %s", reportJSON, got)
	}
}

// TestReconcileAndReportEditsDoNotRewriteLedgerFile 验证对账本身与对返回报告的
// 任意编辑都不改写账本文件，已有付款、退款行为不受影响。
func TestReconcileAndReportEditsDoNotRewriteLedgerFile(t *testing.T) {
	l, path := reconLedger(t) // p1(usdc,1003,已退 r1)、p2(eth,10)；r1 退 p1

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 全量与限定范围各对账一次，随后对两份报告做破坏性编辑。
	full, err := l.ReconcileFlow([]FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
	})
	if err != nil {
		t.Fatalf("reconcile full: %v", err)
	}
	z := int64(0)
	ranged, err := l.ReconcileFlowRanged(nil, ReconcileBounds{RefundThrough: &z})
	if err != nil {
		t.Fatalf("reconcile ranged: %v", err)
	}
	for _, rep := range []*ReconReport{full, ranged} {
		for i := range rep.Results {
			if rep.Results[i].Record != nil {
				rep.Results[i].Record.ID = "MUTATED"
				rep.Results[i].Record.Charged = -1
			}
		}
		for i := range rep.MissingPayments {
			rep.MissingPayments[i].ID = "MUTATED"
			rep.MissingPayments[i].Seq = -1
		}
		for i := range rep.MissingRefunds {
			rep.MissingRefunds[i].SettlementID = "ghost"
		}
		for i := range rep.Totals.Ledger {
			rep.Totals.Ledger[i].NetCharged = "-999"
		}
		rep.MaxPaymentSeq = -1
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file changed by reconcile or report edits:\nbefore=%s\nafter =%s", before, after)
	}

	// 已有付款行为不变：p1 按原请求重提仍是 duplicate 并携带原记录。
	dup := mustSettle(t, l, 30, PaymentIntent{
		ID: "p1", Account: "aa-1", Paymaster: "pm", Asset: "usdc",
		Amount: i64p(1000), Nonce: 1, State: "pending",
	})
	if dup.Status != StatusDuplicate || dup.Record == nil || dup.Record.Seq != 1 || dup.Record.Charged != 1003 {
		t.Fatalf("p1 retry after reconcile/edits: %+v", dup)
	}
	// 已有退款行为不变：r1 按原请求重提仍是 duplicate。
	rdup := mustRefundOne(t, l, "r1", "p1", "cancel")
	if rdup.Status != StatusDuplicate || rdup.Record == nil || rdup.Record.Seq != 1 {
		t.Fatalf("r1 retry after reconcile/edits: %+v", rdup)
	}
}

// TestReconcileOldReportFrozenAcrossLedgerChanges 覆盖旧报告的时效独立：取得报告
// 后账本再发生付款与退款，新报告反映真实变化，旧报告保留取得时的全部内容；
// 原先未退款的付款后来退款时，旧报告里的付款不会自动出现新退款关联。
func TestReconcileOldReportFrozenAcrossLedgerChanges(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100000}})
	l := openOrFail(t, path)
	if r := mustSettle(t, l, 0, intent("p1", "aa", "usdc", 100)); r.Status != StatusSettled {
		t.Fatalf("p1: %+v", r)
	}
	if r := mustSettle(t, l, 0, intent("p2", "aa", "usdc", 200)); r.Status != StatusSettled {
		t.Fatalf("p2: %+v", r)
	}

	// 旧报告 1：空流水的缺失报告（全部账本记录缺失，此时 p1 尚未退款）。
	missingRep, err := l.ReconcileFlow(nil)
	if err != nil {
		t.Fatalf("reconcile empty: %v", err)
	}
	if len(missingRep.MissingPayments) != 2 || len(missingRep.MissingRefunds) != 0 {
		t.Fatalf("missing=%+v/%+v", missingRep.MissingPayments, missingRep.MissingRefunds)
	}
	if missingRep.MissingPayments[0].RefundID != "" || missingRep.MissingPayments[0].RefundSeq != 0 {
		t.Fatalf("p1 not refunded yet, must carry no refund link: %+v", missingRep.MissingPayments[0])
	}
	if missingRep.MaxPaymentSeq != 2 || missingRep.MaxRefundSeq != 0 ||
		missingRep.PaymentThrough != 2 || missingRep.RefundThrough != 0 {
		t.Fatalf("old report seqs/bounds: %+v", missingRep)
	}
	missingJSON := marshalReport(t, missingRep)

	// 旧报告 2：p1 逐条匹配，同样尚无退款关联。
	flowRep, err := l.ReconcileFlow([]FlowEntry{
		flowEntry(0, "payment", "p1", "aa", "usdc", 100, 0, 100, ""),
	})
	if err != nil {
		t.Fatalf("reconcile flow: %v", err)
	}
	if flowRep.Results[0].Status != ReconMatched || flowRep.Results[0].Record.RefundID != "" {
		t.Fatalf("flow report before refund: %+v", flowRep.Results[0])
	}
	flowJSON := marshalReport(t, flowRep)

	// 账本发生变化：p1 被 r1 退款，另发生新付款 p3。
	if r := mustRefundOne(t, l, "r1", "p1", "cancel"); r.Status != StatusRefundSuccess {
		t.Fatalf("r1: %+v", r)
	}
	if r := mustSettle(t, l, 0, intent("p3", "aa", "usdc", 50)); r.Status != StatusSettled {
		t.Fatalf("p3: %+v", r)
	}

	// 两份旧报告逐字冻结：记录、缺失列表、金额汇总、最大成功序号与实际范围不变；
	// p1 不会自动出现新退款关联。
	if got := marshalReport(t, missingRep); !bytes.Equal(got, missingJSON) {
		t.Fatalf("old missing report changed after ledger writes:\nwant %s\ngot  %s", missingJSON, got)
	}
	if missingRep.MissingPayments[0].RefundID != "" || missingRep.MissingPayments[0].RefundSeq != 0 {
		t.Fatalf("old report gained a refund link for p1: %+v", missingRep.MissingPayments[0])
	}
	if got := marshalReport(t, flowRep); !bytes.Equal(got, flowJSON) {
		t.Fatalf("old flow report changed after ledger writes:\nwant %s\ngot  %s", flowJSON, got)
	}
	if flowRep.Results[0].Record.RefundID != "" {
		t.Fatalf("old matched result gained a refund link: %+v", flowRep.Results[0].Record)
	}

	// 新报告反映真实变化：最大序号、缺失列表与汇总都按新账本计算，
	// p1 的缺失记录带上 r1 关联。
	fresh, err := l.ReconcileFlow(nil)
	if err != nil {
		t.Fatalf("fresh reconcile: %v", err)
	}
	if fresh.MaxPaymentSeq != 3 || fresh.MaxRefundSeq != 1 ||
		fresh.PaymentThrough != 3 || fresh.RefundThrough != 1 {
		t.Fatalf("fresh seqs/bounds: %+v", fresh)
	}
	if ids := refs(fresh.MissingPayments); ids != "p1,p2,p3" {
		t.Fatalf("fresh missing payments=%s want p1,p2,p3", ids)
	}
	if fresh.MissingPayments[0].RefundID != "r1" || fresh.MissingPayments[0].RefundSeq != 1 {
		t.Fatalf("fresh p1 must link refund r1: %+v", fresh.MissingPayments[0])
	}
	if ids := refs(fresh.MissingRefunds); ids != "r1" || fresh.MissingRefunds[0].SettlementID != "p1" {
		t.Fatalf("fresh missing refunds=%+v", fresh.MissingRefunds)
	}
	usdc := findTotalRow(t, fresh.Totals.Ledger, "aa", "usdc")
	if usdc.ChargedTotal != "350" || usdc.RefundedTotal != "100" || usdc.NetCharged != "250" {
		t.Fatalf("fresh ledger totals=%+v", usdc)
	}

	// 即使关联退款不在选定范围内，新报告的入选付款仍携带该关联。
	z := int64(0)
	ranged, err := l.ReconcileFlowRanged(nil, ReconcileBounds{RefundThrough: &z})
	if err != nil {
		t.Fatalf("ranged reconcile: %v", err)
	}
	if len(ranged.MissingRefunds) != 0 {
		t.Fatalf("refund range is empty, want no missing refunds: %+v", ranged.MissingRefunds)
	}
	if ranged.MissingPayments[0].ID != "p1" || ranged.MissingPayments[0].RefundID != "r1" ||
		ranged.MissingPayments[0].RefundSeq != 1 {
		t.Fatalf("selected payment must carry the out-of-range refund link: %+v", ranged.MissingPayments[0])
	}

	// 新报告不影响旧报告：再次确认旧报告内容仍未被连带更新。
	if got := marshalReport(t, missingRep); !bytes.Equal(got, missingJSON) {
		t.Fatalf("old missing report changed after new reconciles:\nwant %s\ngot  %s", missingJSON, got)
	}
	if got := marshalReport(t, flowRep); !bytes.Equal(got, flowJSON) {
		t.Fatalf("old flow report changed after new reconciles:\nwant %s\ngot  %s", flowJSON, got)
	}
}

// TestReconcileEmptyFlowMissingReportEditsStayPrivate 覆盖空流水缺失报告的隔离：
// 调用者改动缺失列表与汇总后，账本不变，重新对账仍得到真实缺失内容。
func TestReconcileEmptyFlowMissingReportEditsStayPrivate(t *testing.T) {
	l, _ := reconLedger(t) // p1,p2；r1 退 p1

	rep, err := l.ReconcileFlow(nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ids := refs(rep.MissingPayments); ids != "p1,p2" {
		t.Fatalf("missing payments=%s", ids)
	}
	if ids := refs(rep.MissingRefunds); ids != "r1" {
		t.Fatalf("missing refunds=%s", ids)
	}
	reportJSON := marshalReport(t, rep)
	before, err := l.Query()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	beforeJSON, _ := json.Marshal(before)

	// 破坏性编辑缺失报告：缺失记录、追加伪造记录、汇总与序号。
	rep.MissingPayments[0].ID = "HACK"
	rep.MissingPayments[0].RefundID = "HACK-R"
	rep.MissingPayments[0].Charged = 1
	rep.MissingPayments = append(rep.MissingPayments, ReconRecordRef{ID: "fake"})
	rep.MissingRefunds[0].SettlementID = "ghost"
	rep.Totals.Ledger[0].NetCharged = "0"
	rep.MaxPaymentSeq = 0
	rep.MaxRefundSeq = 0

	// 账本不变。
	after, err := l.Query()
	if err != nil {
		t.Fatalf("query after edits: %v", err)
	}
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("ledger changed by editing missing report:\nbefore=%s\nafter =%s", beforeJSON, afterJSON)
	}

	// 重新空流水对账，仍得到逐字相同的真实缺失报告。
	fresh, err := l.ReconcileFlow(nil)
	if err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if got := marshalReport(t, fresh); !bytes.Equal(got, reportJSON) {
		t.Fatalf("fresh missing report reused caller edits:\nwant %s\ngot  %s", reportJSON, got)
	}
}
