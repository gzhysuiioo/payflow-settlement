package payflow

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 本文件为离线对账报告（ReconcileFlow / ReconcileFlowRanged 返回的 *ReconReport）
// 与真实账本之间的独立性提供回归保障。Go 调用者拿到全量或按成功序号限定范围的
// 报告后，可能修改其中的记录详情、差异说明和重复位置供自己继续处理；本文件锁定：
//
//  1. 结果之间彼此独立：同一付款/退款编号在输入中多次出现时，范围内重复组逐项
//     返回，整组各项都携带完整输入位置及对应成功记录的独立副本；调用者改动其中
//     一项的编号、账户、金额、付款与退款关联或位置列表，其他项保留原内容。
//     付款编号与退款编号各自独立，同名编号编辑一类结果不能影响另一类。
//  2. 范围外优先且彼此独立：记录在范围外时继续优先报告 out_of_scope 并保留关联
//     信息；同一范围外记录被多条流水引用时，编辑其中一条结果不连带改变其他条目。
//  3. 报告与账本空间独立：调用者改动逐项详情、字段差异、缺失记录和金额汇总后，
//     查询到的余额与成功历史保持原样；用原流水与原范围重新对账，仍得到依据真实
//     账本计算的状态、关联、位置及汇总，不沿用调用者改过的数据。
//  4. 只读：对账与编辑返回值都不改写账本文件，已有付款、退款行为不受影响。
//  5. 时效独立：取得报告后账本再发生付款或退款，新报告反映真实变化，旧报告仍保留
//     取得时的记录、缺失列表、金额汇总、最大成功序号与实际范围；原先未退款的付款
//     后来退款时，旧报告付款不得自动出现新退款关联，新报告必须带上该关联，即使
//     关联退款不在选定范围内。空流水正常返回的缺失报告遵守同样的独立性。
//
// 核对规则、报告格式与 ReconcileFlow/ReconcileFlowRanged 公开入口均不在本文件变更。

// reconBounds 是构造核对范围的便捷函数：after < seq <= through。
func reconBounds(pa, pt, ra, rt int64) ReconcileBounds {
	return ReconcileBounds{
		PaymentAfter: &pa, PaymentThrough: &pt,
		RefundAfter: &ra, RefundThrough: &rt,
	}
}

// mustReconcile 执行一次对账，失败即终止用例。
func mustReconcile(t *testing.T, l *Ledger, entries []FlowEntry, bounds ReconcileBounds) *ReconReport {
	t.Helper()
	rep, err := l.ReconcileFlowRanged(entries, bounds)
	if err != nil {
		t.Fatalf("ReconcileFlowRanged: %v", err)
	}
	return rep
}

// cloneReconReport 深拷贝一份对账报告，作为“调用者取得时”的冻结基线：
// ReconRecordRef 是指针字段，逐项结果必须连同其 *Record 一起复制，否则后续比较
// 会被调用者对自己那份报告的编辑污染。
func cloneReconReport(r *ReconReport) *ReconReport {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	var cp ReconReport
	if err := json.Unmarshal(b, &cp); err != nil {
		panic(err)
	}
	return &cp
}

// corruptReconResult 像调用者为自己继续处理所做的那样，破坏性改写一条逐项结果：
// 顶层编号/状态/位置列表、差异说明，以及命中记录的编号、账户、资产、金额、
// 手续费、扣款总额、序号、原付款编号与退款关联一律换成伪造值。
func corruptReconResult(r *ReconResult) {
	r.ID = "HACK-ID"
	r.Kind = "forged-kind"
	r.Status = "forged-status"
	if r.Positions != nil {
		r.Positions[0] = 4242
		r.Positions = append(r.Positions, 999)
	} else {
		r.Positions = []int{7, 7}
	}
	if r.Diffs != nil {
		r.Diffs[0] = FieldDiff{Field: "forged", Ledger: "HACK-L", Flow: "HACK-F"}
	} else {
		r.Diffs = []FieldDiff{{Field: "injected", Ledger: "x", Flow: "y"}}
	}
	if r.Record == nil {
		return
	}
	r.Record.Kind = "forged-kind"
	r.Record.ID = "HACK-REC"
	r.Record.Account = "zz"
	r.Record.Asset = "btc"
	r.Record.Amount = 1
	r.Record.Fee = 1
	r.Record.Charged = 2
	r.Record.Seq = 77
	r.Record.SettlementID = "p-fake"
	r.Record.RefundID = "r-fake"
	r.Record.RefundSeq = 88
}

// corruptReconRef 破坏性改写一条缺失/汇总中携带的记录引用（全部字段，含两类关联）。
func corruptReconRef(r *ReconRecordRef) {
	r.Kind = "forged-kind"
	r.ID = "HACK-MISSING"
	r.Account = "zz"
	r.Asset = "btc"
	r.Amount = 11
	r.Fee = 22
	r.Charged = 33
	r.Seq = 66
	r.SettlementID = "p-fake"
	r.RefundID = "r-fake"
	r.RefundSeq = 55
}

// corruptReconTotals 破坏性改写单侧汇总的每一行（金额换成伪造的大数字符串）。
func corruptReconTotals(rows []ReconTotalRow) {
	for i := range rows {
		rows[i].Account = "zz-" + rows[i].Account
		rows[i].ChargedTotal = "999999"
		rows[i].RefundedTotal = "888888"
		rows[i].NetCharged = "111111"
	}
}

// assertReconReportDeepEqual 逐字段断言两份报告一致（含指针记录与各汇总）。
func assertReconReportDeepEqual(t *testing.T, got, want *ReconReport, label string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Fatalf("%s:\n got %s\nwant %s", label, gj, wj)
	}
}

// assertResultUntouched 断言一条逐项结果仍与冻结基线完全一致。
func assertResultUntouched(t *testing.T, got, want ReconResult, label string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Fatalf("%s:\n got %s\nwant %s", label, gj, wj)
	}
}

// assertRefUntouched 断言一条记录引用逐字段保持原值（缺失列表与命中记录共用）。
func assertRefUntouched(t *testing.T, got, want ReconRecordRef, label string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s:\n got %+v\nwant %+v", label, got, want)
	}
}

// ---- 1. 范围内重复组：逐项独立副本 ----

// TestReconcileDuplicateResultsAreIndependent 锁定同一付款编号在输入中多次出现时，
// 范围内重复组逐项返回：每项都携带完整输入位置 [0,1,2] 与同一条成功记录的独立
// 副本；调用者逐项破坏改写其中一条（编号、账户、金额、关联、位置列表），其余
// 尚未被改的项必须保留原来的状态、位置与记录详情。
func TestReconcileDuplicateResultsAreIndependent(t *testing.T) {
	l := rangeLedger(t)
	// 付款范围选 (0,3] 全量；p2(seq=2) 在范围内，出现三次，内容各不相同也仍是
	// 整组重复（不判字段差异）。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),
		flowEntry(1, "payment", "p2", "aa-1", "usdc", 999, 0, 999, ""),
		flowEntry(2, "payment", "p2", "OTHER", "eth", 7, 7, 14, ""),
	}
	rep := mustReconcile(t, l, entries, reconBounds(0, 3, 0, 2))

	wantRec := ReconRecordRef{
		Kind: "payment", ID: "p2", Account: "aa-1", Asset: "usdc",
		Amount: 2000, Fee: 6, Charged: 2006, Seq: 2,
		RefundID: "r2", RefundSeq: 2, // 范围内重复项仍携带该付款的退款关联
	}
	for i, r := range rep.Results {
		if r.Status != ReconDuplicate {
			t.Fatalf("result %d status=%s want duplicate", i, r.Status)
		}
		if !reflect.DeepEqual(r.Positions, []int{0, 1, 2}) {
			t.Fatalf("result %d positions=%v want [0 1 2]", i, r.Positions)
		}
		if len(r.Diffs) != 0 {
			t.Fatalf("duplicate must carry no diffs, result %d: %+v", i, r.Diffs)
		}
		if r.Record == nil {
			t.Fatalf("result %d must carry the ledger record", i)
		}
		assertRefUntouched(t, *r.Record, wantRec, "duplicate record")
	}

	// 三个命中记录必须是各自独立的分配，两两不共享。
	for i := 0; i < len(rep.Results); i++ {
		for j := i + 1; j < len(rep.Results); j++ {
			if rep.Results[i].Record == rep.Results[j].Record {
				t.Fatalf("duplicate results %d and %d share one *ReconRecordRef", i, j)
			}
		}
	}
	// 位置列表也必须是每条结果各自的底层数组。
	if &rep.Results[0].Positions[0] == &rep.Results[1].Positions[0] {
		t.Fatal("duplicate results share the positions backing array")
	}

	// 冻结逐项基线后逐项破坏：每改一项，其余尚未被改的项保持原状态/位置/记录。
	saved := make([]ReconResult, len(rep.Results))
	copy(saved, rep.Results)
	corrupted := map[int]bool{}
	for i := range rep.Results {
		corruptReconResult(&rep.Results[i])
		corrupted[i] = true
		for j := range rep.Results {
			if corrupted[j] {
				continue
			}
			assertResultUntouched(t, rep.Results[j], saved[j],
				"other duplicate entry changed after editing one")
		}
	}
}

// TestReconcileDuplicateSameIDPaymentAndRefundStayIndependent 付款编号与退款编号
// 即使相同也仍是两类独立记录：同名重复组分别按付款、退款核对；编辑付款类结果的
// 记录编号/账户/金额/关联/位置，不能改变退款类各项，反之亦然。
func TestReconcileDuplicateSameIDPaymentAndRefundStayIndependent(t *testing.T) {
	l := rangeLedger(t)
	// 编号 "x1"：账本无付款 x1（重复组，无命中记录）；退款侧没有 x1 也无记录。
	// 同时构造命中记录的同名编号：用 "r1" —— 付款 r1 账本无此付款（无记录），
	// 退款 r1 命中 seq=1（重复两次）。这样同名编号横跨两类且一类带记录。
	entries := []FlowEntry{
		flowEntry(0, "payment", "r1", "aa-1", "usdc", 5, 0, 5, ""),
		flowEntry(1, "payment", "r1", "aa-1", "usdc", 6, 0, 6, ""),
		flowEntry(2, "refund", "r1", "aa-1", "usdc", 1000, 3, 1003, "p1"),
		flowEntry(3, "refund", "r1", "aa-1", "usdc", 1000, 3, 1003, "p1"),
	}
	rep := mustReconcile(t, l, entries, reconBounds(0, 3, 0, 2))

	wantStatus := []string{ReconDuplicate, ReconDuplicate, ReconDuplicate, ReconDuplicate}
	for i, s := range wantStatus {
		if rep.Results[i].Status != s {
			t.Fatalf("result %d status=%s want %s", i, rep.Results[i].Status, s)
		}
	}
	// 付款 r1 在付款空间不存在 -> 无记录；退款 r1 命中 -> 带退款记录与原付款 p1。
	for _, i := range []int{0, 1} {
		if rep.Results[i].Record != nil {
			t.Fatalf("payment r1 duplicate must have no record, result %d: %+v", i, rep.Results[i].Record)
		}
		if !reflect.DeepEqual(rep.Results[i].Positions, []int{0, 1}) {
			t.Fatalf("payment r1 positions=%v want [0 1]", rep.Results[i].Positions)
		}
	}
	wantRefundRec := ReconRecordRef{
		Kind: "refund", ID: "r1", Account: "aa-1", Asset: "usdc",
		Amount: 1000, Fee: 3, Charged: 1003, Seq: 1, SettlementID: "p1",
	}
	for _, i := range []int{2, 3} {
		if !reflect.DeepEqual(rep.Results[i].Positions, []int{2, 3}) {
			t.Fatalf("refund r1 positions=%v want [2 3]", rep.Results[i].Positions)
		}
		if rep.Results[i].Record == nil {
			t.Fatalf("refund r1 duplicate must carry record, result %d", i)
		}
		assertRefUntouched(t, *rep.Results[i].Record, wantRefundRec, "refund r1 record")
	}

	saved := make([]ReconResult, len(rep.Results))
	copy(saved, rep.Results)

	// 编辑付款类两条结果（含其空记录情形与位置列表）：退款类各项保持原值。
	corruptReconResult(&rep.Results[0])
	corruptReconResult(&rep.Results[1])
	assertResultUntouched(t, rep.Results[2], saved[2], "refund entry changed by payment edits")
	assertResultUntouched(t, rep.Results[3], saved[3], "refund entry changed by payment edits")

	// 再编辑退款类两条（含原付款关联 SettlementID）：已被改的付款项不再要求，
	// 但退款类内部另一条在被改之前保持原值；且付款侧原本就无记录这一事实不变。
	corruptReconResult(&rep.Results[2])
	if rep.Results[3].Record == nil || rep.Results[3].Record.SettlementID != "p1" {
		t.Fatalf("sibling refund result altered by editing one: %+v", rep.Results[3].Record)
	}
	corruptReconResult(&rep.Results[3])
}

// ---- 2. 范围外优先：多条流水引用同一范围外记录，结果彼此独立并保留关联 ----

// TestReconcileOutOfScopeResultsIndependentAndAssociationsKept 锁定记录在范围外时
// 优先报告 out_of_scope（优先于整组重复），各条都携带完整记录与跨范围关联；
// 同一范围外记录被多条流水引用时，编辑其中一条结果不能连带改变其他条目。
func TestReconcileOutOfScopeResultsIndependentAndAssociationsKept(t *testing.T) {
	l := rangeLedger(t)
	// 付款范围 (1,3]：p1(seq=1) 在范围外；p1 仍携带其退款 r1 的关联（不切断）。
	// 退款范围 (1,2]：r1(seq=1) 在范围外，仍指向 p1。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),
		flowEntry(1, "payment", "p1", "aa-1", "usdc", 999, 0, 999, ""), // 内容不同也优先 out_of_scope
		flowEntry(2, "refund", "r1", "aa-1", "usdc", 1000, 3, 1003, "p1"),
		flowEntry(3, "refund", "r1", "aa-1", "usdc", 1, 0, 1, "p-other"),
	}
	rep := mustReconcile(t, l, entries, reconBounds(1, 3, 1, 2))

	for i := range rep.Results {
		if rep.Results[i].Status != ReconOutOfScope {
			t.Fatalf("result %d status=%s want out_of_scope", i, rep.Results[i].Status)
		}
	}
	wantPay := ReconRecordRef{
		Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc",
		Amount: 1000, Fee: 3, Charged: 1003, Seq: 1,
		RefundID: "r1", RefundSeq: 1, // 范围外付款仍携带退款关联
	}
	wantRefund := ReconRecordRef{
		Kind: "refund", ID: "r1", Account: "aa-1", Asset: "usdc",
		Amount: 1000, Fee: 3, Charged: 1003, Seq: 1, SettlementID: "p1", // 范围外退款仍指向原付款
	}
	assertRefUntouched(t, *rep.Results[0].Record, wantPay, "out-of-scope payment record")
	assertRefUntouched(t, *rep.Results[1].Record, wantPay, "out-of-scope payment record")
	assertRefUntouched(t, *rep.Results[2].Record, wantRefund, "out-of-scope refund record")
	assertRefUntouched(t, *rep.Results[3].Record, wantRefund, "out-of-scope refund record")

	// out_of_scope 不附位置列表（只有 duplicate 才整组标位置），也不附差异。
	for i := range rep.Results {
		if rep.Results[i].Positions != nil {
			t.Fatalf("out_of_scope result %d must not carry positions: %v", i, rep.Results[i].Positions)
		}
		if len(rep.Results[i].Diffs) != 0 {
			t.Fatalf("out_of_scope result %d must not carry diffs: %+v", i, rep.Results[i].Diffs)
		}
	}
	for i := 0; i < len(rep.Results); i++ {
		for j := i + 1; j < len(rep.Results); j++ {
			if rep.Results[i].Record == rep.Results[j].Record {
				t.Fatalf("out_of_scope results %d and %d share one *ReconRecordRef", i, j)
			}
		}
	}

	// 逐条引用同一范围外记录：编辑其中一条，其余条目（含另一类的同名编号）保持原值。
	saved := make([]ReconResult, len(rep.Results))
	copy(saved, rep.Results)
	corrupted := map[int]bool{}
	for i := range rep.Results {
		corruptReconResult(&rep.Results[i])
		corrupted[i] = true
		for j := range rep.Results {
			if corrupted[j] {
				continue
			}
			assertResultUntouched(t, rep.Results[j], saved[j],
				"other out_of_scope entry changed after editing one")
		}
	}
}

// ---- 3. 报告改动与账本空间独立：账本、再对账都不沿用被改数据 ----

// TestReconcileReportEditsDoNotReachLedger 调用者破坏性改写手里的整份报告
// （逐项详情、字段差异、缺失记录、金额与净额汇总），账本查询与用原流水原范围的
// 重新对账都必须仍依据真实账本计算，状态、关联、位置、汇总逐字段不变。
func TestReconcileReportEditsDoNotReachLedger(t *testing.T) {
	l, path := reconLedger(t)

	// 构造覆盖全部状态的流水：matched、field_mismatch、missing_in_ledger、duplicate。
	entries, err := ParseReconcileRequest([]byte(`{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":1,"charged":11},
	  {"kind":"payment","id":"ghost","account":"aa-1","asset":"usdc","amount":5,"fee":0,"charged":5},
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":0,"charged":10}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rep := mustReconcile(t, l, entries, ReconcileBounds{})
	saved := cloneReconReport(rep) // 冻结：调用者取得时应得的完整报告

	// 账本查询基线。
	snapBefore := mustQuery(t, l)

	// 破坏性改写整份报告：所有逐项结果（含记录与差异）、缺失记录、金额与净额汇总、
	// 以及最大序号与范围字段。
	for i := range rep.Results {
		corruptReconResult(&rep.Results[i])
	}
	for i := range rep.MissingPayments {
		corruptReconRef(&rep.MissingPayments[i])
	}
	for i := range rep.MissingRefunds {
		corruptReconRef(&rep.MissingRefunds[i])
	}
	rep.MissingPayments = append(rep.MissingPayments, ReconRecordRef{ID: "fake-missing-pay"})
	rep.MissingRefunds = append(rep.MissingRefunds, ReconRecordRef{ID: "fake-missing-ref"})
	corruptReconTotals(rep.Totals.Ledger)
	corruptReconTotals(rep.Totals.Flow)
	for i := range rep.NetDiff {
		rep.NetDiff[i].LedgerNetCharged = "1"
		rep.NetDiff[i].FlowNetCharged = "2"
		rep.NetDiff[i].NetChargedDiff = "999"
	}
	rep.NetDiff = append(rep.NetDiff, ReconNetDiff{Account: "zz", Asset: "gold", NetChargedDiff: "7"})
	rep.MaxPaymentSeq = 4242
	rep.MaxRefundSeq = 4242
	rep.PaymentAfter = 4242
	rep.PaymentThrough = 4242
	rep.RefundAfter = 4242
	rep.RefundThrough = 4242

	// 1. 账本余额与成功历史完全不变。
	snapAfter := mustQuery(t, l)
	if !reflect.DeepEqual(snapAfter, snapBefore) {
		t.Fatalf("ledger changed by report edit:\nbefore=%+v\nafter =%+v", snapBefore, snapAfter)
	}

	// 2. 用原流水与原范围重新对账：完整报告仍依据真实账本，逐字段等于冻结基线。
	again := mustReconcile(t, l, entries, ReconcileBounds{})
	assertReconReportDeepEqual(t, again, saved, "re-reconcile must not reuse caller edits")

	// 3. 报告改动不改变业务行为：已有付款编号重提仍 duplicate 携带真实原记录，
	//    原付款仍只被原来的退款退回。
	dup := mustSettle(t, l, 30, PaymentIntent{
		ID: "p1", Account: "aa-1", Paymaster: "pm", Asset: "usdc",
		Amount: i64p(1000), Nonce: 1, State: "pending",
	})
	if dup.Status != StatusDuplicate || dup.Record == nil ||
		dup.Record.Charged != 1003 || dup.Record.Seq != 1 {
		t.Fatalf("payment behavior changed by report edit: %+v", dup)
	}
	already := mustRefundOne(t, l, "r-again", "p1", "x")
	if already.Status != StatusAlreadyRefunded {
		t.Fatalf("refund behavior changed by report edit: %+v", already)
	}

	// 4. 反复对账与编辑报告都不改写账本文件。
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte("HACK")) || bytes.Contains(onDisk, []byte("ghost")) ||
		bytes.Contains(onDisk, []byte("4242")) {
		t.Fatalf("reconcile/report edit leaked into the ledger file:\n%s", onDisk)
	}
}

// TestReconcileReportEditsDoNotReachRangedReconcile 在限定范围报告上重复空间独立：
// 编辑一份范围报告（含范围外记录与缺失）不能影响同范围或全量的再次对账。
func TestReconcileReportEditsDoNotReachRangedReconcile(t *testing.T) {
	l := rangeLedger(t)
	// 付款 (1,3]：p2、p3 入选，p1 范围外；退款 (0,1]：r1 入选，r2 范围外。
	bounds := reconBounds(1, 3, 0, 1)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""),  // out_of_scope
		flowEntry(1, "payment", "p2", "aa-1", "usdc", 2000, 6, 2006, ""),  // matched
		flowEntry(2, "refund", "r2", "aa-1", "usdc", 2000, 6, 2006, "p2"), // out_of_scope
	}
	rep := mustReconcile(t, l, entries, bounds)
	saved := cloneReconReport(rep)

	// 先确认范围与关联符合预期，再整体破坏。
	if rep.Results[0].Status != ReconOutOfScope || rep.Results[1].Status != ReconMatched ||
		rep.Results[2].Status != ReconOutOfScope {
		t.Fatalf("unexpected statuses: %s/%s/%s",
			rep.Results[0].Status, rep.Results[1].Status, rep.Results[2].Status)
	}
	for i := range rep.Results {
		corruptReconResult(&rep.Results[i])
	}
	for i := range rep.MissingPayments {
		corruptReconRef(&rep.MissingPayments[i])
	}
	for i := range rep.MissingRefunds {
		corruptReconRef(&rep.MissingRefunds[i])
	}
	corruptReconTotals(rep.Totals.Ledger)
	corruptReconTotals(rep.Totals.Flow)

	// 同范围重新对账，逐字段恢复冻结基线（状态、跨范围关联、范围边界都来自真实账本）。
	again := mustReconcile(t, l, entries, bounds)
	assertReconReportDeepEqual(t, again, saved, "ranged re-reconcile must ignore edits")
	if again.Results[0].Record == nil || again.Results[0].Record.RefundID != "r1" {
		t.Fatalf("out-of-range payment must still link r1: %+v", again.Results[0].Record)
	}
	if again.Results[2].Record == nil || again.Results[2].Record.SettlementID != "p2" {
		t.Fatalf("out-of-range refund must still link p2: %+v", again.Results[2].Record)
	}
	// 全量对账也不被范围报告的编辑污染。
	full := mustReconcile(t, l, entries, ReconcileBounds{})
	if full.Results[0].Status != ReconMatched || full.Results[2].Status != ReconMatched {
		t.Fatalf("full reconcile tainted by ranged report edit: %s/%s",
			full.Results[0].Status, full.Results[2].Status)
	}
}

// ---- 4. 时效独立：旧报告在账本后续付款/退款后仍冻结在取得时刻 ----

// TestReconcileOldReportFreezesAcrossLaterPaymentAndRefund 取得报告后账本再发生
// 一笔付款与一笔退款：新报告反映真实变化；旧报告保留取得时的逐项结果、缺失列表、
// 金额汇总、最大成功序号与实际范围。尤其是原先未退款的付款后来被退款时，旧报告
// 里的付款不能自动出现新退款关联；新报告必须带上该关联，即使关联退款不在选定范围内。
func TestReconcileOldReportFreezesAcrossLaterPaymentAndRefund(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 1_000_000},
	})
	l := openOrFail(t, path)

	// 初始只有一笔未退款的付款 p1。
	if r := mustSettle(t, l, 0, intent("p1", "aa", "usdc", 100)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}

	// 选定付款范围 (0,1]：只含 p1（显式固定，账本后续增长也不改变选定范围）；
	// 退款范围为空 (0,0]，此时没有任何退款。
	emptyRange := reconBounds(0, 1, 0, 0)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa", "usdc", 100, 0, 100, ""),
	}
	old := mustReconcile(t, l, entries, emptyRange)
	if old.Results[0].Status != ReconMatched {
		t.Fatalf("old report p1 status=%s", old.Results[0].Status)
	}
	if old.Results[0].Record == nil || old.Results[0].Record.RefundID != "" {
		t.Fatalf("p1 must initially have no refund link: %+v", old.Results[0].Record)
	}
	if old.MaxPaymentSeq != 1 || old.MaxRefundSeq != 0 ||
		old.PaymentAfter != 0 || old.PaymentThrough != 1 ||
		old.RefundAfter != 0 || old.RefundThrough != 0 {
		t.Fatalf("old report bounds/seqs: max=%d/%d bounds=%d,%d,%d,%d",
			old.MaxPaymentSeq, old.MaxRefundSeq,
			old.PaymentAfter, old.PaymentThrough, old.RefundAfter, old.RefundThrough)
	}
	oldSaved := cloneReconReport(old)

	// 账本再发生一笔新付款 p2（落在选定范围之外），随后退还原先未退款的 p1。
	if r := mustSettle(t, l, 0, intent("p2", "aa", "usdc", 200)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}
	ref := mustRefundOne(t, l, "r1", "p1", "later refund")
	if ref.Status != StatusRefundSuccess {
		t.Fatalf("refund p1: %+v", ref)
	}

	// 1. 旧报告完全冻结：p1 不自动出现新退款关联，缺失、汇总、max seq、范围都不变。
	assertReconReportDeepEqual(t, old, oldSaved,
		"old report must freeze across later payment/refund")
	if old.Results[0].Record.RefundID != "" {
		t.Fatalf("old report p1 acquired a refund link retroactively: %+v",
			old.Results[0].Record)
	}

	// 2. 用原流水、原范围重新对账：新报告反映真实变化。p1 现在带上退款关联 r1，
	//    即使退款范围仍为空、r1 不在选定范围内（范围不切断关联）。
	fresh := mustReconcile(t, l, entries, emptyRange)
	if fresh.Results[0].Status != ReconMatched {
		t.Fatalf("fresh p1 status=%s", fresh.Results[0].Status)
	}
	if fresh.Results[0].Record == nil ||
		fresh.Results[0].Record.RefundID != "r1" || fresh.Results[0].Record.RefundSeq != 1 {
		t.Fatalf("fresh report must carry the new refund link even if refund out of range: %+v",
			fresh.Results[0].Record)
	}
	// 新报告的最大序号与范围反映新账本：付款 2、退款 1；选定范围仍是 (0,1]/(0,0]。
	if fresh.MaxPaymentSeq != 2 || fresh.MaxRefundSeq != 1 {
		t.Fatalf("fresh max seqs=%d/%d want 2/1", fresh.MaxPaymentSeq, fresh.MaxRefundSeq)
	}
	if fresh.PaymentThrough != 1 || fresh.RefundThrough != 0 {
		t.Fatalf("fresh selected bounds changed: %d,%d", fresh.PaymentThrough, fresh.RefundThrough)
	}
	// p2 在选定付款范围之外；流水未引用它，但它属于真实账本（全量报告可见）。
	full := mustReconcile(t, l, entries, ReconcileBounds{})
	if full.MaxPaymentSeq != 2 || full.MaxRefundSeq != 1 {
		t.Fatalf("full max seqs=%d/%d want 2/1", full.MaxPaymentSeq, full.MaxRefundSeq)
	}
	if full.Results[0].Record == nil || full.Results[0].Record.RefundID != "r1" {
		t.Fatalf("full report p1 must link r1: %+v", full.Results[0].Record)
	}

	// 3. 再次核对旧报告未受第二次对账影响。
	assertReconReportDeepEqual(t, old, oldSaved, "old report changed after re-reconcile")
}

// TestReconcileOldMissingReportFreezesAcrossLaterPayment 空流水正常返回的缺失报告
// 同样遵守时效独立：取得“全部缺失”报告后新增付款/退款，旧缺失报告仍保留取得时的
// 记录、关联、金额汇总、最大成功序号与范围；新报告只反映新账本。
func TestReconcileOldMissingReportFreezesAcrossLaterPayment(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 1_000_000},
	})
	l := openOrFail(t, path)
	if r := mustSettle(t, l, 0, intent("p1", "aa", "usdc", 100)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}

	// 空流水：p1 列入缺失，且此时没有退款关联。
	old := mustReconcile(t, l, nil, ReconcileBounds{})
	if len(old.Results) != 0 {
		t.Fatalf("empty flow must have no per-entry results: %+v", old.Results)
	}
	if len(old.MissingPayments) != 1 || old.MissingPayments[0].ID != "p1" {
		t.Fatalf("old missing payments=%+v want [p1]", old.MissingPayments)
	}
	if old.MissingPayments[0].RefundID != "" {
		t.Fatalf("p1 must initially have no refund link: %+v", old.MissingPayments[0])
	}
	if len(old.MissingRefunds) != 0 || old.MaxPaymentSeq != 1 || old.MaxRefundSeq != 0 {
		t.Fatalf("old missing report seqs/counts: %+v", old)
	}
	oldSaved := cloneReconReport(old)

	// 账本再付款 p2 并退回 p1。
	if r := mustSettle(t, l, 0, intent("p2", "aa", "usdc", 200)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}
	if r := mustRefundOne(t, l, "r1", "p1", "later"); r.Status != StatusRefundSuccess {
		t.Fatalf("refund p1: %+v", r)
	}

	// 旧缺失报告冻结：仍是仅 p1、无退款关联、max seq=1/0、汇总不变。
	assertReconReportDeepEqual(t, old, oldSaved, "old missing report must freeze")
	if old.MissingPayments[0].RefundID != "" {
		t.Fatalf("old missing p1 acquired refund link retroactively: %+v",
			old.MissingPayments[0])
	}

	// 新缺失报告反映真实账本：p1 现带退款关联 r1；p2 仍缺失；r1 进入缺失退款。
	fresh := mustReconcile(t, l, nil, ReconcileBounds{})
	if ids := refs(fresh.MissingPayments); ids != "p1,p2" {
		t.Fatalf("fresh missing payments=%s want p1,p2", ids)
	}
	if fresh.MissingPayments[0].RefundID != "r1" || fresh.MissingPayments[0].RefundSeq != 1 {
		t.Fatalf("fresh missing p1 must link later refund r1: %+v", fresh.MissingPayments[0])
	}
	if ids := refs(fresh.MissingRefunds); ids != "r1" {
		t.Fatalf("fresh missing refunds=%s want r1", ids)
	}
	if fresh.MaxPaymentSeq != 2 || fresh.MaxRefundSeq != 1 {
		t.Fatalf("fresh max seqs=%d/%d want 2/1", fresh.MaxPaymentSeq, fresh.MaxRefundSeq)
	}

	// 编辑旧报告同样不能污染这份新报告：再取一次仍与 fresh 一致。
	corruptReconRef(&old.MissingPayments[0])
	again := mustReconcile(t, l, nil, ReconcileBounds{})
	assertReconReportDeepEqual(t, again, fresh, "new report tainted by editing old report")
}

// ---- 5. 只读：对账本身不改写账本文件 ----

// TestReconcileRangedAndEditsDoNotRewriteLedgerFile 在限定范围对账上锁定只读：
// 仅对账（含范围外命中与缺失）以及编辑返回报告，账本文件字节保持不变。
func TestReconcileRangedAndEditsDoNotRewriteLedgerFile(t *testing.T) {
	l := rangeLedger(t)
	path := l.path
	bounds := reconBounds(1, 2, 0, 1)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa-1", "usdc", 1000, 3, 1003, ""), // 范围外
		flowEntry(1, "refund", "r2", "aa-1", "usdc", 2000, 6, 2006, "p2"),
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rep := mustReconcile(t, l, entries, bounds)
		for j := range rep.Results {
			corruptReconResult(&rep.Results[j])
		}
		for j := range rep.MissingPayments {
			corruptReconRef(&rep.MissingPayments[j])
		}
		corruptReconTotals(rep.Totals.Ledger)
		corruptReconTotals(rep.Totals.Flow)
		if _, err := json.Marshal(rep); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file rewritten by ranged reconcile/edits:\nbefore=%s\nafter =%s",
			before, after)
	}

	// 已有付款与退款记录数量不变，余额仍可正常支持后续付款（对账未扣加余额）。
	snap := mustQuery(t, l)
	if len(snap.Settlements) != 3 || len(snap.Refunds) != 2 {
		t.Fatalf("history counts changed: settlements=%d refunds=%d",
			len(snap.Settlements), len(snap.Refunds))
	}
	if r := mustSettle(t, l, 0, intent("p9", "aa-1", "usdc", 100)); r.Status != StatusSettled {
		t.Fatalf("payment after reconciles must behave normally: %+v", r)
	}
	if r := mustRefundOne(t, l, "r9", "p9", "x"); r.Status != StatusRefundSuccess {
		t.Fatalf("refund after reconciles must behave normally: %+v", r)
	}
}
