package payflow

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 本文件为离线流水对账（Ledger.ReconcileFlow）补充交错使用下的回归保障：
// 同一进程中两个句柄指向同一本账本时，一个句柄正在提交退款批次（批次仍在
// 逐项保存、账本互斥锁尚未释放），另一个句柄发起全量对账。对账必须等该退款
// 批次整批处理完毕，再依据完整账本生成报告：
//   - 不能读到“第一笔退款已落账、第二笔退款尚未确定”的中途状态（不能提前返回）；
//   - 前项成功、后项保存失败是允许的完整状态：失败项已回滚、不留退款记录，
//     报告不得把失败退款算成成功记录（该流水为 missing_in_ledger，退款最大
//     成功序号只计成功项）；
//   - 对账是只读操作：不新增退款、不改变余额、不改写账本文件。
//
// 场景固定为：一个账户 aa、一种资产 usdc，初始余额 1000，费率 30 基点。
// 先成功支付 p1=500（手续费 1，扣款 501）与 p2=300（手续费 0，扣款 300）。
// 退款批次依次用 r1 退回 p1、用 r2 退回 p2，原因均为非空字符串；在 r2 的
// “保存点”用故障注入门把批次停住（此时 r1 已完整落账、锁仍被退款句柄持有），
// 另一句柄就在此刻发起全量对账，流水按 p1、p2、r1、r2 排列，账户、资产、
// 金额都与对应原结算一致，退款引用各自原付款。随后放行 r2 的保存，分别覆盖
// “两笔退款都成功”和“r1 成功、r2 保存失败”两种完整结局。
//
// 对账核对规则、报告格式与公开接口均不在本文件变更。

// runReconcileDuringRefundInterleave 执行交错用例。failSecond=false 时 r1、r2
// 都真实成功；failSecond=true 时 r1 成功、r2 在保存点失败（storage_error，回滚）。
func runReconcileDuringRefundInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const acct, asset = "aa", "usdc"

	// 退款记录（落账时已有两笔结算：after_seq=2）。
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "cancel p1",
		Account: acct, Asset: asset, Amount: 500, Fee: 1, Charged: 501,
		AfterSeq: 2, Seq: 1,
	}
	wantR2 := RefundRecord{
		ID: "r2", SettlementID: "p2", Reason: "cancel p2",
		Account: acct, Asset: asset, Amount: 300, Fee: 0, Charged: 300,
		AfterSeq: 2, Seq: 2,
	}
	// 对账报告中命中记录应携带的关联信息。
	wantP1Ref := ReconRecordRef{
		Kind: "payment", ID: "p1", Account: acct, Asset: asset,
		Amount: 500, Fee: 1, Charged: 501, Seq: 1,
		RefundID: "r1", RefundSeq: 1,
	}
	wantP2Ref := ReconRecordRef{
		Kind: "payment", ID: "p2", Account: acct, Asset: asset,
		Amount: 300, Fee: 0, Charged: 300, Seq: 2,
	}
	wantR1Ref := ReconRecordRef{
		Kind: "refund", ID: "r1", Account: acct, Asset: asset,
		Amount: 500, Fee: 1, Charged: 501, Seq: 1, SettlementID: "p1",
	}
	wantR2Ref := ReconRecordRef{
		Kind: "refund", ID: "r2", Account: acct, Asset: asset,
		Amount: 300, Fee: 0, Charged: 300, Seq: 2, SettlementID: "p2",
	}

	// 两种结局下退款批次自身与对账报告的期望。
	wantRefundStatuses := []string{StatusRefundSuccess, StatusRefundSuccess}
	wantStatuses := []string{ReconMatched, ReconMatched, ReconMatched, ReconMatched}
	wantMaxRefundSeq := int64(2)
	wantLedgerRow := ReconTotalRow{Account: acct, Asset: asset, ChargedTotal: "801", RefundedTotal: "801", NetCharged: "0"}
	wantNetDiff := ReconNetDiff{Account: acct, Asset: asset, LedgerNetCharged: "0", FlowNetCharged: "0", NetChargedDiff: "0"}
	wantBalance := int64(1000) // 1000-801+801
	wantRefunds := []RefundRecord{wantR1, wantR2}
	if failSecond {
		// r1 成功、r2 保存失败：批次保留 r1、r2 报 storage_error；
		// 报告中 r2 为 missing_in_ledger，p2 没有退款关联，退款最大成功序号为 1。
		wantRefundStatuses = []string{StatusRefundSuccess, StatusStorage}
		wantStatuses = []string{ReconMatched, ReconMatched, ReconMatched, ReconMissingInLedger}
		wantMaxRefundSeq = 1
		wantLedgerRow = ReconTotalRow{Account: acct, Asset: asset, ChargedTotal: "801", RefundedTotal: "501", NetCharged: "300"}
		wantNetDiff = ReconNetDiff{Account: acct, Asset: asset, LedgerNetCharged: "300", FlowNetCharged: "0", NetChargedDiff: "-300"}
		wantBalance = 700 // 1000-801+501
		wantRefunds = []RefundRecord{wantR1}
	} else {
		wantP2Ref.RefundID = "r2"
		wantP2Ref.RefundSeq = 2
	}
	// 流水侧两种结局都包含输入的两笔退款：扣款 801、退款 801、净扣款 0。
	wantFlowRow := ReconTotalRow{Account: acct, Asset: asset, ChargedTotal: "801", RefundedTotal: "801", NetCharged: "0"}

	path := newTestLedger(t, []BalanceInit{{Account: acct, Asset: asset, Balance: 1000}})
	lRefund := openOrFail(t, path) // 句柄 A：提交退款批次
	lRecon := openOrFail(t, path)  // 句柄 B：全量对账（同一进程、同一本账本）

	// 先顺序成功支付 p1、p2（30 基点：500→费 1 扣 501，300→费 0 扣 300）。
	if r := mustSettle(t, lRefund, 30, intent("p1", acct, asset, 500)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}
	if r := mustSettle(t, lRefund, 30, intent("p2", acct, asset, 300)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}
	if bal, _ := lRefund.Balance(acct, asset); bal != 199 {
		t.Fatalf("balance after payments=%d want 199", bal)
	}

	// 对账输入按 p1、p2、r1、r2 排列，账户、资产、金额与对应原结算一致，
	// 退款引用各自原付款。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", acct, asset, 500, 1, 501, ""),
		flowEntry(1, "payment", "p2", acct, asset, 300, 0, 300, ""),
		flowEntry(2, "refund", "r1", acct, asset, 500, 1, 501, "p1"),
		flowEntry(3, "refund", "r2", acct, asset, 300, 0, 300, "p2"),
	}

	// 故障注入门安装在付款完成之后：第 1 次持久化（r1）正常通过，
	// 第 2 次持久化（r2 的保存点）停住，放行后按 failSecond 决定成败。
	gate := &secondPersistGate{
		reached: make(chan struct{}),
		release: make(chan struct{}),
		fail:    failSecond,
	}
	SetFailHook(lRefund, gate)

	var (
		wg sync.WaitGroup

		refundRes    *RefundBatchResult
		refundErr    error
		reconStarted = make(chan struct{})
		reconDone    = make(chan struct{}) // 对账返回（无论成功失败）后关闭
		reconRes     *ReconReport
		reconErr     error
	)

	// 句柄 A：顺序提交退款 r1、r2。r2 的保存会停在 gate，整批持锁不释放。
	wg.Add(1)
	go func() {
		defer wg.Done()
		refundRes, refundErr = lRefund.Refund(RefundBatch{Refunds: []RefundRequest{
			refundReq("r1", "p1", "cancel p1"),
			refundReq("r2", "p2", "cancel p2"),
		}})
	}()

	// 等 A 把 r1 完整落账、停在 r2 的保存点（批次仍在保存、锁仍被持有）。
	<-gate.reached

	// 句柄 B：就在该退款批次仍在保存时发起全量对账。它应当在同一把账本锁上
	// 排队，直到 A 整批处理完毕，而不是读到 r1 已退、r2 未定的中途状态。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(reconDone)
		close(reconStarted)
		reconRes, reconErr = lRecon.ReconcileFlow(entries)
	}()

	<-reconStarted
	// 让出 P，令对账 goroutine 实际运行到争夺账本锁并排队；A 仍停在 r2 保存点。
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
	// 退款批次仍在保存、锁仍被 A 持有：对账绝不可能在此刻返回。正确实现下对账
	// 线程正阻塞在同一把账本锁上，因此该非阻塞检查不会误报；它直接锁定“对账
	// 等待在途退款批次整批完成”这一性质（仅凭结果无法区分等待与否——中途状态
	// 与最终状态可能恰好一致，本检查补上这重保障）。
	select {
	case <-reconDone:
		t.Fatal("reconcile returned while the refund batch was still saving; it must wait for the whole batch")
	default:
	}
	close(gate.release) // 放行 r2 保存（成功或失败），随后整批结束并释放锁
	wg.Wait()

	SetFailHook(lRefund, nil) // 用例结束，拆卸故障注入

	if refundErr != nil {
		t.Fatalf("refund batch: %v", refundErr)
	}
	if reconErr != nil {
		t.Fatalf("reconcile: %v", reconErr)
	}

	// ---- 退款批次逐项结果：与输入顺序一一对应，各自独立处理 ----
	if got := refundStatuses(refundRes); !reflect.DeepEqual(got, wantRefundStatuses) {
		t.Fatalf("refund statuses=%v want %v", got, wantRefundStatuses)
	}
	if refundRes.Results[0].ID != "r1" || refundRes.Results[1].ID != "r2" {
		t.Fatalf("refund results out of order: %+v", refundRes.Results)
	}
	assertRefundResultRecord(t, refundRes.Results[0], StatusRefundSuccess, wantR1)
	if failSecond {
		// r2 保存失败：报告 storage_error、无退款记录，前项 r1 保留不回滚。
		if r := refundRes.Results[1]; r.Status != StatusStorage || r.Record != nil {
			t.Fatalf("failed refund r2: %+v", r)
		}
	} else {
		assertRefundResultRecord(t, refundRes.Results[1], StatusRefundSuccess, wantR2)
	}

	// ---- 对账报告：整份基于退款批次结束后的同一个完整账本状态 ----
	rep := reconRes
	if len(rep.Results) != 4 {
		t.Fatalf("report results=%d want 4: %+v", len(rep.Results), rep.Results)
	}
	wantIDs := []string{"p1", "p2", "r1", "r2"}
	wantKinds := []string{"payment", "payment", "refund", "refund"}
	for i := range rep.Results {
		r := rep.Results[i]
		if r.Index != i || r.ID != wantIDs[i] || r.Kind != wantKinds[i] {
			t.Fatalf("result %d out of order: %+v", i, r)
		}
		if r.Status != wantStatuses[i] {
			t.Fatalf("result %d (%s) status=%s want %s", i, r.ID, r.Status, wantStatuses[i])
		}
		if len(r.Diffs) != 0 {
			t.Fatalf("result %d (%s) unexpected diffs: %+v", i, r.ID, r.Diffs)
		}
	}
	// 关联：p1 关联 r1；p2 仅在 r2 成功时关联 r2，失败结局下不得出现退款关联。
	assertRefUntouched(t, *rep.Results[0].Record, wantP1Ref, "p1 record")
	assertRefUntouched(t, *rep.Results[1].Record, wantP2Ref, "p2 record")
	assertRefUntouched(t, *rep.Results[2].Record, wantR1Ref, "r1 record")
	if failSecond {
		// 失败的 r2 不能被算成成功记录：missing_in_ledger 且不携带账本记录。
		if rep.Results[3].Record != nil {
			t.Fatalf("failed r2 must not carry a ledger record: %+v", rep.Results[3].Record)
		}
	} else {
		assertRefUntouched(t, *rep.Results[3].Record, wantR2Ref, "r2 record")
	}

	// 两种结局下账本缺失列表都为空：流水完整覆盖了入选的成功记录。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v want empty", rep.MissingRefunds)
	}

	// 最大成功序号：付款恒为 2；退款只计成功项（2 或 1），失败退款不占序号。
	if rep.MaxPaymentSeq != 2 || rep.MaxRefundSeq != wantMaxRefundSeq {
		t.Fatalf("max seqs=%d/%d want 2/%d", rep.MaxPaymentSeq, rep.MaxRefundSeq, wantMaxRefundSeq)
	}
	if rep.PaymentAfter != 0 || rep.PaymentThrough != 2 ||
		rep.RefundAfter != 0 || rep.RefundThrough != wantMaxRefundSeq {
		t.Fatalf("full-reconcile bounds=%d,%d,%d,%d want 0,2,0,%d",
			rep.PaymentAfter, rep.PaymentThrough, rep.RefundAfter, rep.RefundThrough, wantMaxRefundSeq)
	}

	// 金额汇总（十进制字符串）：账本侧按完整账本，流水侧按输入流水。
	if got := findTotalRow(t, rep.Totals.Ledger, acct, asset); got != wantLedgerRow {
		t.Fatalf("ledger totals:\n got %+v\nwant %+v", got, wantLedgerRow)
	}
	if len(rep.Totals.Ledger) != 1 {
		t.Fatalf("ledger totals rows=%d want 1: %+v", len(rep.Totals.Ledger), rep.Totals.Ledger)
	}
	if got := findTotalRow(t, rep.Totals.Flow, acct, asset); got != wantFlowRow {
		t.Fatalf("flow totals:\n got %+v\nwant %+v", got, wantFlowRow)
	}
	if len(rep.Totals.Flow) != 1 {
		t.Fatalf("flow totals rows=%d want 1: %+v", len(rep.Totals.Flow), rep.Totals.Flow)
	}
	if !reflect.DeepEqual(rep.NetDiff, []ReconNetDiff{wantNetDiff}) {
		t.Fatalf("net diff:\n got %+v\nwant %+v", rep.NetDiff, []ReconNetDiff{wantNetDiff})
	}

	// ---- 对账只读：不新增退款、不改变余额或账本文件 ----
	snap := mustQuery(t, lRecon)
	if !reflect.DeepEqual(snap.Refunds, wantRefunds) {
		t.Fatalf("refunds after reconcile:\n got %+v\nwant %+v (reconcile must not add refunds)", snap.Refunds, wantRefunds)
	}
	if len(snap.Settlements) != 2 {
		t.Fatalf("settlements after reconcile=%d want 2", len(snap.Settlements))
	}
	if bal, _ := lRecon.Balance(acct, asset); bal != wantBalance {
		t.Fatalf("balance after reconcile=%d want %d (reconcile must not move balances)", bal, wantBalance)
	}

	// 账本文件不被对账改写：再对账一次，文件字节与报告都保持不变。
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := lRecon.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if !reflect.DeepEqual(again, rep) {
		t.Fatalf("re-reconcile not deterministic:\n first %+v\nagain %+v", rep, again)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file rewritten by reconcile:\nbefore=%s\nafter =%s", before, after)
	}
}

// TestReconcileWaitsForInFlightRefundBatchBothRefunded：两笔退款都成功
// （余额回到 1000）。与退款批次并发发起的全量对账必须等整批结束：四条流水
// 全部 matched，p1、p2 分别关联 r1、r2，付款与退款最大成功序号均为 2，
// 两侧扣款/退款合计均为 "801"，净扣款与净差额均为 "0"。
func TestReconcileWaitsForInFlightRefundBatchBothRefunded(t *testing.T) {
	runReconcileDuringRefundInterleave(t, false)
}

// TestReconcileWaitsForInFlightRefundBatchSecondSaveFails：r1 成功、r2 保存失败。
// 退款批次保留 r1、r2 报 storage_error、余额 700；并发对账基于该完整状态：
// 前三条流水 matched，r2 为 missing_in_ledger；p1 关联 r1，p2 无退款关联，
// 退款最大成功序号为 1；账本侧退款合计 "501"、净扣款 "300"，流水侧净扣款
// 仍为 "0"，净差额 "-300"。失败的 r2 不被算成成功记录。
func TestReconcileWaitsForInFlightRefundBatchSecondSaveFails(t *testing.T) {
	runReconcileDuringRefundInterleave(t, true)
}
