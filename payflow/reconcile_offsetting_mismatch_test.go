package payflow

import (
	"bytes"
	"os"
	"reflect"
	"testing"
)

// 本文件为离线流水对账（Ledger.ReconcileFlow 全量入口）补充“汇总对平但逐笔
// 存在差异”的回归保障：用户看到净扣款差额为零时，报告仍必须保留每笔实际存在
// 的差异，不能把汇总对平当成所有记录已经匹配。锁定的行为：
//
//  1. 付款方向：同一账户、同一资产下两笔不同编号的成功付款，外部流水编号正确，
//     却把一笔多记、另一笔等额少记，使总扣款与账本完全相等。两笔都必须是
//     field_mismatch，分别带出实际不同字段的账本值与流水值，记录详情仍指向
//     各自原付款；该组合的扣款合计、净扣款与净差额如实反映总额相等（净差额
//     为十进制字符串零）。已通过编号找到的付款不得再出现在账本缺失列表，
//     逐条结果保持外部流水的输入顺序。
//  2. 退款方向：两笔等额付款分别已全额退款，流水的退款编号、账户、资产、金额
//     均正确，却交换了各自引用的原付款编号。即使退款合计与净扣款差额完全对平，
//     两笔退款仍必须是 field_mismatch，差异只落在 settlement_id 并携带账本中
//     真正的原付款关联。付款曾经退款不影响原付款的核对：付款与退款各按自己的
//     编号和记录判断，不因金额相同而互相替代。
//  3. 上述输入字段与数值都合法：差异作为正常对账报告返回，不能变成
//     invalid_parameter，也不能省略不一致的结果。把同一批流水中的偏差改回
//     账本原值后，对应结果恢复 matched，汇总金额保持正确；金额与手续费分项
//     不同但扣款总额相同时，分项差异必须保留。
//  4. 对账只读：对账前后余额、付款与退款历史、账本文件内容保持一致。
//
// 核对规则、状态名称、报告结构与 ReconcileFlow 公开入口均不在本文件变更。

// offsettingLedger 构造回归用账本：账户 aa、资产 usdc，初始余额 1000，
// 30 基点费率下两笔成功付款 p1=500（费 1、扣 501，seq 1）、
// p2=300（费 0、扣 300，seq 2），扣款合计 801。refunded 为 true 时再
// 分别全额退款：r1 退 p1（501，seq 1）、r2 退 p2（300，seq 2）。
func offsettingLedger(t *testing.T, refunded bool) (*Ledger, string) {
	t.Helper()
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)
	if r := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 500)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}
	if r := mustSettle(t, l, 30, intent("p2", "aa", "usdc", 300)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}
	if refunded {
		if r := mustRefundOne(t, l, "r1", "p1", "cancel p1"); r.Status != StatusRefundSuccess {
			t.Fatalf("refund r1: %+v", r)
		}
		if r := mustRefundOne(t, l, "r2", "p2", "cancel p2"); r.Status != StatusRefundSuccess {
			t.Fatalf("refund r2: %+v", r)
		}
	}
	return l, path
}

// assertFieldDiffs 断言一条结果的字段差异与期望完全一致（字段、账本值、流水值、顺序）。
func assertFieldDiffs(t *testing.T, got, want []FieldDiff, label string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s diffs:\n got %+v\nwant %+v", label, got, want)
	}
}

// assertOnlyCombo 断言报告只含 aa/usdc 一个组合，且账本侧、流水侧汇总与净差额
// 逐字段等于期望（金额均为十进制字符串）。
func assertOnlyCombo(t *testing.T, rep *ReconReport, wantLedger, wantFlow ReconTotalRow, wantDiff ReconNetDiff) {
	t.Helper()
	if !reflect.DeepEqual(rep.Totals.Ledger, []ReconTotalRow{wantLedger}) {
		t.Fatalf("ledger totals:\n got %+v\nwant %+v", rep.Totals.Ledger, wantLedger)
	}
	if !reflect.DeepEqual(rep.Totals.Flow, []ReconTotalRow{wantFlow}) {
		t.Fatalf("flow totals:\n got %+v\nwant %+v", rep.Totals.Flow, wantFlow)
	}
	if !reflect.DeepEqual(rep.NetDiff, []ReconNetDiff{wantDiff}) {
		t.Fatalf("net diff:\n got %+v\nwant %+v", rep.NetDiff, wantDiff)
	}
}

// assertNoMissing 断言账本缺失列表为空：流水已按编号覆盖全部入选记录。
func assertNoMissing(t *testing.T, rep *ReconReport) {
	t.Helper()
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("covered payments must not be listed missing: %+v", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("covered refunds must not be listed missing: %+v", rep.MissingRefunds)
	}
}

// assertLedgerUnchanged 断言对账只读：余额、付款与退款历史、账本文件内容
// 与对账前完全一致。
func assertLedgerUnchanged(t *testing.T, l *Ledger, path string, beforeSnap *Snapshot, beforeFile []byte) {
	t.Helper()
	assertSnap(t, mustQuery(t, l), beforeSnap, "ledger changed by reconcile")
	afterFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeFile, afterFile) {
		t.Fatalf("ledger file rewritten by reconcile:\nbefore=%s\nafter =%s", beforeFile, afterFile)
	}
}

// TestReconcileOffsettingPaymentMismatchNetDiffZero 锁定付款方向的核心场景：
// 流水编号正确但把 p1 多记 100、p2 等额少记 100，总扣款与账本完全相等。
// 两笔仍必须各自报 field_mismatch 并指向各自原付款；汇总如实对平，
// 净差额为十进制字符串零；两笔付款不得进入缺失列表。
func TestReconcileOffsettingPaymentMismatchNetDiffZero(t *testing.T) {
	l, path := offsettingLedger(t, false)
	beforeSnap := mustQuery(t, l)
	beforeFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 输入顺序故意与账本成功顺序相反（p2 在前、p1 在后），
	// 逐条结果必须保持外部流水的输入顺序。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p2", "aa", "usdc", 200, 0, 200, ""), // 少记 100
		flowEntry(1, "payment", "p1", "aa", "usdc", 600, 1, 601, ""), // 多记 100
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("合法输入的偏差是对账结论，不能是 invalid_parameter: %v", err)
	}

	// 逐条结果不省略、保持输入顺序，两笔都是 field_mismatch。
	if len(rep.Results) != 2 {
		t.Fatalf("results=%d want 2 (no entry may be dropped): %+v", len(rep.Results), rep.Results)
	}
	if rep.Results[0].Index != 0 || rep.Results[0].ID != "p2" ||
		rep.Results[1].Index != 1 || rep.Results[1].ID != "p1" {
		t.Fatalf("results must follow flow input order: %+v", rep.Results)
	}
	for i := range rep.Results {
		if rep.Results[i].Status != ReconFieldMismatch {
			t.Fatalf("result %d (%s) status=%s want field_mismatch",
				i, rep.Results[i].ID, rep.Results[i].Status)
		}
	}

	// 各笔只带出自己实际不同的字段：账本值与流水值逐字并列。
	assertFieldDiffs(t, rep.Results[0].Diffs, []FieldDiff{
		{Field: "amount", Ledger: "300", Flow: "200"},
		{Field: "charged", Ledger: "300", Flow: "200"},
	}, "p2 under-recorded")
	assertFieldDiffs(t, rep.Results[1].Diffs, []FieldDiff{
		{Field: "amount", Ledger: "500", Flow: "600"},
		{Field: "charged", Ledger: "501", Flow: "601"},
	}, "p1 over-recorded")

	// 记录详情仍指向各自原付款（编号、金额、序号不被对调的流水带偏）。
	assertRefUntouched(t, *rep.Results[0].Record, ReconRecordRef{
		Kind: "payment", ID: "p2", Account: "aa", Asset: "usdc",
		Amount: 300, Fee: 0, Charged: 300, Seq: 2,
	}, "p2 record")
	assertRefUntouched(t, *rep.Results[1].Record, ReconRecordRef{
		Kind: "payment", ID: "p1", Account: "aa", Asset: "usdc",
		Amount: 500, Fee: 1, Charged: 501, Seq: 1,
	}, "p1 record")

	// 两笔都已通过编号找到：不得再出现在账本缺失列表。
	assertNoMissing(t, rep)

	// 汇总如实反映总额相等：两侧扣款合计、净扣款都是 "801"，
	// 净差额为十进制字符串零——对平不代表两笔已经匹配。
	assertOnlyCombo(t, rep,
		ReconTotalRow{Account: "aa", Asset: "usdc", ChargedTotal: "801", RefundedTotal: "0", NetCharged: "801"},
		ReconTotalRow{Account: "aa", Asset: "usdc", ChargedTotal: "801", RefundedTotal: "0", NetCharged: "801"},
		ReconNetDiff{Account: "aa", Asset: "usdc", LedgerNetCharged: "801", FlowNetCharged: "801", NetChargedDiff: "0"},
	)

	// 对账只读：余额、付款历史与账本文件不变。
	assertLedgerUnchanged(t, l, path, beforeSnap, beforeFile)
}

// TestReconcileRefundsSwappedSettlementIDNetDiffZero 锁定退款方向的对应场景：
// 两笔付款分别已全额退款，流水的退款编号、账户、资产、金额均正确，却交换了
// 各自引用的原付款编号。退款合计与净扣款差额完全对平，两笔退款仍必须报
// field_mismatch，差异只落在 settlement_id 并携带账本中真正的原付款关联；
// 付款与退款各按自己的编号核对，付款结果不受退款影响。
func TestReconcileRefundsSwappedSettlementIDNetDiffZero(t *testing.T) {
	l, path := offsettingLedger(t, true)
	beforeSnap := mustQuery(t, l)
	beforeFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 付款与退款交错排列，验证逐条结果保持输入顺序、两类记录各自独立核对。
	entries := []FlowEntry{
		flowEntry(0, "refund", "r2", "aa", "usdc", 300, 0, 300, "p1"), // 原付款被换成 p1
		flowEntry(1, "payment", "p1", "aa", "usdc", 500, 1, 501, ""),  // 付款本身无偏差
		flowEntry(2, "refund", "r1", "aa", "usdc", 500, 1, 501, "p2"), // 原付款被换成 p2
		flowEntry(3, "payment", "p2", "aa", "usdc", 300, 0, 300, ""),  // 付款本身无偏差
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("合法输入的偏差是对账结论，不能是 invalid_parameter: %v", err)
	}

	// 逐条结果不省略、保持输入顺序：退款 field_mismatch，付款 matched。
	wantStatus := []string{ReconFieldMismatch, ReconMatched, ReconFieldMismatch, ReconMatched}
	wantIDs := []string{"r2", "p1", "r1", "p2"}
	if len(rep.Results) != 4 {
		t.Fatalf("results=%d want 4 (no entry may be dropped): %+v", len(rep.Results), rep.Results)
	}
	for i := range rep.Results {
		r := rep.Results[i]
		if r.Index != i || r.ID != wantIDs[i] {
			t.Fatalf("result %d out of input order: %+v", i, r)
		}
		if r.Status != wantStatus[i] {
			t.Fatalf("result %d (%s) status=%s want %s", i, r.ID, r.Status, wantStatus[i])
		}
	}

	// 两笔退款的差异只落在 settlement_id，账本值是真正的原付款关联。
	assertFieldDiffs(t, rep.Results[0].Diffs, []FieldDiff{
		{Field: "settlement_id", Ledger: "p2", Flow: "p1"},
	}, "r2 swapped settlement")
	assertFieldDiffs(t, rep.Results[2].Diffs, []FieldDiff{
		{Field: "settlement_id", Ledger: "p1", Flow: "p2"},
	}, "r1 swapped settlement")

	// 退款记录详情携带账本中真正的原付款关联，不被流水对调带偏。
	assertRefUntouched(t, *rep.Results[0].Record, ReconRecordRef{
		Kind: "refund", ID: "r2", Account: "aa", Asset: "usdc",
		Amount: 300, Fee: 0, Charged: 300, Seq: 2, SettlementID: "p2",
	}, "r2 record")
	assertRefUntouched(t, *rep.Results[2].Record, ReconRecordRef{
		Kind: "refund", ID: "r1", Account: "aa", Asset: "usdc",
		Amount: 500, Fee: 1, Charged: 501, Seq: 1, SettlementID: "p1",
	}, "r1 record")

	// 付款曾经退款不影响原付款的核对：两笔付款按自己的编号 matched，
	// 记录详情仍携带各自真实的退款关联，不因金额相同而互相替代。
	assertRefUntouched(t, *rep.Results[1].Record, ReconRecordRef{
		Kind: "payment", ID: "p1", Account: "aa", Asset: "usdc",
		Amount: 500, Fee: 1, Charged: 501, Seq: 1, RefundID: "r1", RefundSeq: 1,
	}, "p1 record")
	assertRefUntouched(t, *rep.Results[3].Record, ReconRecordRef{
		Kind: "payment", ID: "p2", Account: "aa", Asset: "usdc",
		Amount: 300, Fee: 0, Charged: 300, Seq: 2, RefundID: "r2", RefundSeq: 2,
	}, "p2 record")

	// 全部记录均已按编号覆盖：缺失列表为空。
	assertNoMissing(t, rep)

	// 退款合计与净扣款差额完全对平：两侧扣款/退款各 "801"、净扣款 "0"，
	// 净差额为十进制字符串零——对平不代表两笔退款已经匹配。
	assertOnlyCombo(t, rep,
		ReconTotalRow{Account: "aa", Asset: "usdc", ChargedTotal: "801", RefundedTotal: "801", NetCharged: "0"},
		ReconTotalRow{Account: "aa", Asset: "usdc", ChargedTotal: "801", RefundedTotal: "801", NetCharged: "0"},
		ReconNetDiff{Account: "aa", Asset: "usdc", LedgerNetCharged: "0", FlowNetCharged: "0", NetChargedDiff: "0"},
	)

	// 对账只读：余额、付款与退款历史、账本文件不变。
	assertLedgerUnchanged(t, l, path, beforeSnap, beforeFile)
}

// TestReconcileCorrectedFlowRestoresMatched 同一批流水先把付款一多一少、
// 退款原付款编号对调（全部 field_mismatch、汇总仍对平），再把偏差改回账本
// 原值重新对账：对应结果全部恢复 matched，汇总金额保持正确。差异是逐次的
// 核对结论，不是参数错误，也不是滞留状态。
func TestReconcileCorrectedFlowRestoresMatched(t *testing.T) {
	l, _ := offsettingLedger(t, true)

	deviated := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa", "usdc", 600, 1, 601, ""),  // 多记 100
		flowEntry(1, "payment", "p2", "aa", "usdc", 200, 0, 200, ""),  // 少记 100
		flowEntry(2, "refund", "r1", "aa", "usdc", 500, 1, 501, "p2"), // 原付款对调
		flowEntry(3, "refund", "r2", "aa", "usdc", 300, 0, 300, "p1"), // 原付款对调
	}
	rep, err := l.ReconcileFlow(deviated)
	if err != nil {
		t.Fatalf("deviated flow must be a normal report, not invalid_parameter: %v", err)
	}
	for i := range rep.Results {
		if rep.Results[i].Status != ReconFieldMismatch {
			t.Fatalf("deviated result %d (%s) status=%s want field_mismatch",
				i, rep.Results[i].ID, rep.Results[i].Status)
		}
	}
	if d := rep.NetDiff[0]; d.NetChargedDiff != "0" {
		t.Fatalf("deviated flow still balances in total, diff=%+v", d)
	}

	// 偏差全部改回账本原值：同一入口、同一账本，结果恢复 matched。
	corrected := []FlowEntry{
		flowEntry(0, "payment", "p1", "aa", "usdc", 500, 1, 501, ""),
		flowEntry(1, "payment", "p2", "aa", "usdc", 300, 0, 300, ""),
		flowEntry(2, "refund", "r1", "aa", "usdc", 500, 1, 501, "p1"),
		flowEntry(3, "refund", "r2", "aa", "usdc", 300, 0, 300, "p2"),
	}
	rep, err = l.ReconcileFlow(corrected)
	if err != nil {
		t.Fatalf("corrected flow: %v", err)
	}
	if len(rep.Results) != 4 {
		t.Fatalf("results=%d want 4: %+v", len(rep.Results), rep.Results)
	}
	for i := range rep.Results {
		r := rep.Results[i]
		if r.Status != ReconMatched {
			t.Fatalf("corrected result %d (%s) status=%s want matched", i, r.ID, r.Status)
		}
		if len(r.Diffs) != 0 {
			t.Fatalf("corrected result %d (%s) unexpected diffs: %+v", i, r.ID, r.Diffs)
		}
	}
	assertNoMissing(t, rep)
	// 汇总金额保持正确：两侧扣款/退款各 "801"、净扣款 "0"，净差额 "0"。
	assertOnlyCombo(t, rep,
		ReconTotalRow{Account: "aa", Asset: "usdc", ChargedTotal: "801", RefundedTotal: "801", NetCharged: "0"},
		ReconTotalRow{Account: "aa", Asset: "usdc", ChargedTotal: "801", RefundedTotal: "801", NetCharged: "0"},
		ReconNetDiff{Account: "aa", Asset: "usdc", LedgerNetCharged: "0", FlowNetCharged: "0", NetChargedDiff: "0"},
	)
}

// TestReconcileComponentDiffsKeptWhenChargedEqual 金额与手续费分项不同但扣款
// 总额相同：扣款合计与净差额照常对平，amount 与 fee 的分项差异必须保留，
// 不能因 charged 相等就当成 matched。
func TestReconcileComponentDiffsKeptWhenChargedEqual(t *testing.T) {
	l, _ := offsettingLedger(t, false)

	entries := []FlowEntry{
		// p1 的 amount/fee 被重新拆分（501+0 而非 500+1），charged 总额相同。
		flowEntry(0, "payment", "p1", "aa", "usdc", 501, 0, 501, ""),
		flowEntry(1, "payment", "p2", "aa", "usdc", 300, 0, 300, ""),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("component mismatch is a normal report, not invalid_parameter: %v", err)
	}

	if rep.Results[0].Status != ReconFieldMismatch {
		t.Fatalf("p1 status=%s want field_mismatch", rep.Results[0].Status)
	}
	// 分项差异逐字保留，且不出现 charged 差异。
	assertFieldDiffs(t, rep.Results[0].Diffs, []FieldDiff{
		{Field: "amount", Ledger: "500", Flow: "501"},
		{Field: "fee", Ledger: "1", Flow: "0"},
	}, "p1 component")
	if rep.Results[1].Status != ReconMatched {
		t.Fatalf("p2 status=%s want matched", rep.Results[1].Status)
	}

	assertNoMissing(t, rep)
	// 扣款总额相同：汇总与净差额照常对平。
	assertOnlyCombo(t, rep,
		ReconTotalRow{Account: "aa", Asset: "usdc", ChargedTotal: "801", RefundedTotal: "0", NetCharged: "801"},
		ReconTotalRow{Account: "aa", Asset: "usdc", ChargedTotal: "801", RefundedTotal: "0", NetCharged: "801"},
		ReconNetDiff{Account: "aa", Asset: "usdc", LedgerNetCharged: "801", FlowNetCharged: "801", NetChargedDiff: "0"},
	)
}
