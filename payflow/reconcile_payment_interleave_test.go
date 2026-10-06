package payflow

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 本文件为按成功序号对账（Ledger.ReconcileFlowRanged）补充交错使用下的回归
// 保障：同一进程中两个句柄指向同一本账本时，一个句柄正在提交付款批次（批次
// 仍在逐项保存、账本互斥锁尚未释放），另一个句柄发起范围对账。对账必须等该
// 付款批次整批处理完毕，再依据同一个完整账本状态决定默认上界、逐条结果与
// 金额汇总：
//   - 不能读到“p1 已落账、p2 尚未确定”的中途状态而提前返回只含前项成功的
//     报告（默认上界、最大成功序号都必须反映批次结束后的完整历史）；
//   - 前项成功、后项保存失败是允许的完整状态：失败项已回滚、不留成功记录，
//     报告不得把失败付款算成成功记录（该流水为 missing_in_ledger，付款最大
//     成功序号只计成功项），但账本外流水仍计入流水侧汇总与净扣款差额；
//   - 对账是只读操作：不新增付款或退款、不改变余额、不改写账本文件。
//
// 场景固定为：一个账户 aa、一种资产 usdc，初始余额 1000，无付款与退款历史，
// 费率 30 基点。句柄 A 顺序提交 p1=500（手续费 1，扣款 501）与 p2=300
// （手续费 0，扣款 300），两笔合法且互不重名；在 p2 的“保存点”用故障注入门
// 把批次停住（此时 p1 已完整落账、锁仍被 A 持有）。句柄 B 就在此刻发起范围
// 对账：付款排除下界 1、上界缺省，退款范围沿用缺省值；流水依次提供 p1、p2，
// 账户、资产、金额都与对应付款一致。随后放行 p2 的保存，分别覆盖“两笔付款
// 都成功”和“p1 成功、p2 保存失败”两种完整结局。
//
// 付款规则、对账核对规则、报告格式与公开接口均不在本文件变更。

// runReconcileDuringSubmitInterleave 执行交错用例。failSecond=false 时 p1、p2
// 都真实成功；failSecond=true 时 p1 成功、p2 在保存点失败（storage_error，回滚）。
func runReconcileDuringSubmitInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const acct, asset = "aa", "usdc"

	// 成功结算记录（30 基点：500→费 1 扣 501，300→费 0 扣 300）。
	wantP1 := Record{ID: "p1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 500, Nonce: 1, FeeBps: 30, Fee: 1, Charged: 501, Seq: 1}
	wantP2 := Record{ID: "p2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 300, Nonce: 1, FeeBps: 30, Fee: 0, Charged: 300, Seq: 2}
	// 对账报告中命中记录应携带的内容（无退款历史，付款不带退款关联）。
	wantP1Ref := ReconRecordRef{
		Kind: "payment", ID: "p1", Account: acct, Asset: asset,
		Amount: 500, Fee: 1, Charged: 501, Seq: 1,
	}
	wantP2Ref := ReconRecordRef{
		Kind: "payment", ID: "p2", Account: acct, Asset: asset,
		Amount: 300, Fee: 0, Charged: 300, Seq: 2,
	}

	// 两种结局下付款批次自身与对账报告的期望。
	wantBatchStatuses := []string{StatusSettled, StatusSettled}
	wantStatuses := []string{ReconOutOfScope, ReconMatched}
	wantMaxPaySeq := int64(2)
	wantLedgerRows := []ReconTotalRow{
		{Account: acct, Asset: asset, ChargedTotal: "300", RefundedTotal: "0", NetCharged: "300"},
	}
	wantNetDiff := []ReconNetDiff{
		{Account: acct, Asset: asset, LedgerNetCharged: "300", FlowNetCharged: "300", NetChargedDiff: "0"},
	}
	wantBalance := int64(199) // 1000-501-300
	wantRecords := []Record{wantP1, wantP2}
	if failSecond {
		// p1 成功、p2 保存失败：批次保留 p1、p2 报 storage_error。报告中付款
		// 最大成功序号与默认上界均为 1，范围 (1,1] 为空：p1 仍 out_of_scope，
		// p2 为 missing_in_ledger 且不携带成功记录；账本侧没有入选付款（无汇总
		// 行），流水侧仍计入账本外 p2 的 300，净扣款差额按流水减账本为 300。
		wantBatchStatuses = []string{StatusSettled, StatusStorage}
		wantStatuses = []string{ReconOutOfScope, ReconMissingInLedger}
		wantMaxPaySeq = 1
		wantLedgerRows = []ReconTotalRow{}
		wantNetDiff = []ReconNetDiff{
			{Account: acct, Asset: asset, LedgerNetCharged: "0", FlowNetCharged: "300", NetChargedDiff: "300"},
		}
		wantBalance = 499 // 1000-501
		wantRecords = []Record{wantP1}
	}
	// 流水侧两种结局都只计入 p2 的 300：p1 命中范围外记录不计入，
	// 失败结局下 p2 是账本外流水仍逐条计入。
	wantFlowRows := []ReconTotalRow{
		{Account: acct, Asset: asset, ChargedTotal: "300", RefundedTotal: "0", NetCharged: "300"},
	}

	path := newTestLedger(t, []BalanceInit{{Account: acct, Asset: asset, Balance: 1000}})
	lSubmit := openOrFail(t, path) // 句柄 A：提交付款批次
	lRecon := openOrFail(t, path)  // 句柄 B：范围对账（同一进程、同一本账本）

	// 对账输入按 p1、p2 排列，账户、资产、金额与对应付款一致。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", acct, asset, 500, 1, 501, ""),
		flowEntry(1, "payment", "p2", acct, asset, 300, 0, 300, ""),
	}
	// 付款排除下界 1、上界缺省（取批次完成后的最大成功序号）；退款范围缺省。
	payAfter := int64(1)
	bounds := ReconcileBounds{PaymentAfter: &payAfter}

	// 故障注入门：第 1 次持久化（p1）正常通过，第 2 次持久化（p2 的保存点）
	// 停住，放行后按 failSecond 决定成败。
	gate := &secondPersistGate{
		reached: make(chan struct{}),
		release: make(chan struct{}),
		fail:    failSecond,
	}
	SetFailHook(lSubmit, gate)

	var (
		wg sync.WaitGroup

		submitRes    *BatchResult
		submitErr    error
		reconStarted = make(chan struct{})
		reconDone    = make(chan struct{}) // 对账返回（无论成功失败）后关闭
		reconRes     *ReconReport
		reconErr     error
	)

	// 句柄 A：顺序提交 p1、p2。p2 的保存会停在 gate，整批持锁不释放。
	wg.Add(1)
	go func() {
		defer wg.Done()
		submitRes, submitErr = lSubmit.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
			intent("p1", acct, asset, 500),
			intent("p2", acct, asset, 300),
		}})
	}()

	// 等 A 把 p1 完整落账、停在 p2 的保存点（批次仍在保存、锁仍被持有）。
	<-gate.reached

	// 句柄 B：就在该付款批次仍在保存时发起范围对账。它应当在同一把账本锁上
	// 排队，直到 A 整批处理完毕，而不是读到 p1 已扣、p2 未定的中途状态。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(reconDone)
		close(reconStarted)
		reconRes, reconErr = lRecon.ReconcileFlowRanged(entries, bounds)
	}()

	<-reconStarted
	// 让出 P，令对账 goroutine 实际运行到争夺账本锁并排队；A 仍停在 p2 保存点。
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
	// 付款批次仍在保存、锁仍被 A 持有：对账绝不可能在此刻返回。正确实现下对账
	// 线程正阻塞在同一把账本锁上，因此该非阻塞检查不会误报；它直接锁定“对账
	// 等待在途付款批次整批完成”这一性质（仅凭结果无法区分等待与否——若对账
	// 提前返回，其默认上界与最大序号只反映中途状态，本检查补上这重保障）。
	select {
	case <-reconDone:
		t.Fatal("reconcile returned while the payment batch was still saving; it must wait for the whole batch")
	default:
	}
	close(gate.release) // 放行 p2 保存（成功或失败），随后整批结束并释放锁
	wg.Wait()

	SetFailHook(lSubmit, nil) // 用例结束，拆卸故障注入

	if submitErr != nil {
		t.Fatalf("submit batch: %v", submitErr)
	}
	if reconErr != nil {
		t.Fatalf("reconcile: %v", reconErr)
	}

	// ---- 付款批次逐项结果：与输入顺序一一对应，各自独立处理 ----
	if got := statuses(submitRes); !reflect.DeepEqual(got, wantBatchStatuses) {
		t.Fatalf("submit statuses=%v want %v", got, wantBatchStatuses)
	}
	if submitRes.Results[0].ID != "p1" || submitRes.Results[1].ID != "p2" {
		t.Fatalf("submit results out of order: %+v", submitRes.Results)
	}
	assertRecord(t, "submit p1", submitRes.Results[0].Record, wantP1)
	if failSecond {
		// p2 保存失败：报告 storage_error、无结算记录，前项 p1 保留不回滚。
		if r := submitRes.Results[1]; r.Status != StatusStorage || r.Record != nil {
			t.Fatalf("failed submit p2: %+v", r)
		}
	} else {
		assertRecord(t, "submit p2", submitRes.Results[1].Record, wantP2)
	}

	// ---- 对账报告：整份基于付款批次结束后的同一个完整账本状态 ----
	rep := reconRes
	if len(rep.Results) != 2 {
		t.Fatalf("report results=%d want 2: %+v", len(rep.Results), rep.Results)
	}
	wantIDs := []string{"p1", "p2"}
	for i := range rep.Results {
		r := rep.Results[i]
		if r.Index != i || r.ID != wantIDs[i] || r.Kind != "payment" {
			t.Fatalf("result %d out of order: %+v", i, r)
		}
		if r.Status != wantStatuses[i] {
			t.Fatalf("result %d (%s) status=%s want %s", i, r.ID, r.Status, wantStatuses[i])
		}
		if len(r.Diffs) != 0 {
			t.Fatalf("result %d (%s) unexpected diffs: %+v", i, r.ID, r.Diffs)
		}
	}
	// p1 的成功序号 1 不在范围 (1,through] 内：out_of_scope 并携带原记录。
	if rep.Results[0].Record == nil {
		t.Fatalf("p1 out_of_scope must carry the ledger record")
	}
	assertRefUntouched(t, *rep.Results[0].Record, wantP1Ref, "p1 record")
	if failSecond {
		// 失败的 p2 不能被算成成功记录：missing_in_ledger 且不携带账本记录。
		if rep.Results[1].Record != nil {
			t.Fatalf("failed p2 must not carry a ledger record: %+v", rep.Results[1].Record)
		}
	} else {
		// p2 的成功序号 2 入选：matched 并携带原记录。
		if rep.Results[1].Record == nil {
			t.Fatalf("p2 matched must carry the ledger record")
		}
		assertRefUntouched(t, *rep.Results[1].Record, wantP2Ref, "p2 record")
	}

	// 两种结局下付款与退款缺失列表都为空：范围内记录都被流水覆盖（或范围为空）。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v want empty", rep.MissingRefunds)
	}

	// 最大成功序号与本次边界：付款只计成功项（2 或 1），默认上界随之确定；
	// 下界仍为显式的 1；无退款历史，退款序号与边界均为 0。
	if rep.MaxPaymentSeq != wantMaxPaySeq || rep.MaxRefundSeq != 0 {
		t.Fatalf("max seqs=%d/%d want %d/0", rep.MaxPaymentSeq, rep.MaxRefundSeq, wantMaxPaySeq)
	}
	if rep.PaymentAfter != 1 || rep.PaymentThrough != wantMaxPaySeq ||
		rep.RefundAfter != 0 || rep.RefundThrough != 0 {
		t.Fatalf("bounds=%d,%d,%d,%d want 1,%d,0,0",
			rep.PaymentAfter, rep.PaymentThrough, rep.RefundAfter, rep.RefundThrough, wantMaxPaySeq)
	}

	// 金额汇总（十进制字符串）：账本侧只含入选记录，流水侧只含计入条目；
	// p1 的 501 在任何结局下都不得混入本次范围。
	if !reflect.DeepEqual(rep.Totals.Ledger, wantLedgerRows) {
		t.Fatalf("ledger totals:\n got %+v\nwant %+v", rep.Totals.Ledger, wantLedgerRows)
	}
	if !reflect.DeepEqual(rep.Totals.Flow, wantFlowRows) {
		t.Fatalf("flow totals:\n got %+v\nwant %+v", rep.Totals.Flow, wantFlowRows)
	}
	if !reflect.DeepEqual(rep.NetDiff, wantNetDiff) {
		t.Fatalf("net diff:\n got %+v\nwant %+v", rep.NetDiff, wantNetDiff)
	}

	// ---- 对账只读：不新增付款或退款、不改变余额或账本文件 ----
	snap := mustQuery(t, lRecon)
	if !reflect.DeepEqual(snap.Settlements, wantRecords) {
		t.Fatalf("settlements after reconcile:\n got %+v\nwant %+v (reconcile must not add payments)", snap.Settlements, wantRecords)
	}
	if len(snap.Refunds) != 0 {
		t.Fatalf("refunds after reconcile=%+v want empty (reconcile must not add refunds)", snap.Refunds)
	}
	if bal, _ := lRecon.Balance(acct, asset); bal != wantBalance {
		t.Fatalf("balance after reconcile=%d want %d (reconcile must not move balances)", bal, wantBalance)
	}

	// 账本文件不被对账改写：再对账一次，文件字节与报告都保持不变。
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := lRecon.ReconcileFlowRanged(entries, bounds)
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

// TestRangedReconcileWaitsForInFlightSubmitBatchBothSettled：两笔付款都成功
// （余额剩 199）。与付款批次并发发起的范围对账（付款排除下界 1、上界缺省）
// 必须等整批结束：付款最大成功序号与本次上界均为 2，p1 为 out_of_scope 并
// 携带原记录，p2 为 matched；两侧扣款合计都只计 p2 的 "300"，净扣款差额
// 为 "0"，p1 的 501 不混入本次范围。
func TestRangedReconcileWaitsForInFlightSubmitBatchBothSettled(t *testing.T) {
	runReconcileDuringSubmitInterleave(t, false)
}

// TestRangedReconcileWaitsForInFlightSubmitBatchSecondSaveFails：p1 成功、
// p2 保存失败。付款批次保留 p1、p2 报 storage_error、余额 499；并发范围对账
// 基于该完整状态：付款最大成功序号与本次上界均为 1，付款范围为空；p1 仍
// out_of_scope，p2 为 missing_in_ledger 且不携带成功记录；账本侧没有入选
// 付款（无汇总行），流水侧仍计入账本外 p2 的 "300"，净扣款差额按流水减账本
// 为 "300"，不把账本外流水误判为范围外而忽略。
func TestRangedReconcileWaitsForInFlightSubmitBatchSecondSaveFails(t *testing.T) {
	runReconcileDuringSubmitInterleave(t, true)
}
