package payflow

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 本文件为“按成功序号的范围对账”补充交错使用下的回归保障：同一进程中两个
// 句柄指向同一本账本时，一个句柄正在提交付款批次（批次仍在逐项保存、账本
// 互斥锁尚未释放），另一个句柄在批次尚未完成时发起按范围对账。对账必须等
// 整个付款批次处理完，再用同一个完整账本状态决定默认上界、逐条结果和金额
// 汇总：
//   - 不能在 p1 已落账、p2 尚未确定时提前返回仅含前项成功的“半批报告”
//     （默认上界不能停在前项序号，范围外判定、缺失判定与两侧合计都必须基于
//     批次结束后的完整历史）；
//   - 前项成功、后项保存失败是允许的完整状态：失败项已回滚、不留成功记录，
//     默认上界只计成功项，该流水为 missing_in_ledger 且不携带成功记录，
//     账本侧没有入选付款、流水侧仍逐条计入账本外流水，净差额不得把它误判为
//     范围外而忽略；
//   - 对账是只读操作：不新增付款或退款、不改变批次完成后的余额与账本文件。
//
// 场景固定为：没有任何付款和退款历史的账本，账户 aa、资产 usdc、初始余额
// 1000，费率 30 基点。句柄 A 在一个批次中依次提交 p1=500（手续费 1、扣款
// 501）、p2=300（手续费 0、扣款 300），两笔合法且互不重名；在 p2 的“保存
// 点”用故障注入门把批次停住（此时 p1 已完整落账、锁仍被 A 持有）。句柄 B
// 就在此刻发起范围对账：付款排除下界 payment_after=1、上界不提供（缺省取
// 批次结束后所见的最大付款成功序号），退款范围沿用缺省值（空历史，(0,0]
// 为空）；外部流水依次给出 p1、p2，账户、资产、手续费与扣款总额与各自付款
// 一致。随后放行 p2 的保存，分别覆盖“两笔都成功”和“p1 成功、p2 保存失败”
// 两种完整结局。
//
// 付款规则、按范围对账规则与公开报告格式均不在本文件变更。

// runRangedReconcileDuringPaymentInterleave 执行交错用例。failSecond=false 时
// p1、p2 都真实成功；failSecond=true 时 p1 成功、p2 在保存点失败
// （storage_error，回滚）。
func runRangedReconcileDuringPaymentInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const acct, asset = "aa", "usdc"

	wantP1 := Record{ID: "p1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 500, Nonce: 1, FeeBps: 30, Fee: 1, Charged: 501, Seq: 1}
	wantP2 := Record{ID: "p2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 300, Nonce: 1, FeeBps: 30, Fee: 0, Charged: 300, Seq: 2}

	// 范围外的 p1 在报告中应携带原记录（不比较字段、不计入流水侧汇总）。
	wantP1Ref := ReconRecordRef{
		Kind: "payment", ID: "p1", Account: acct, Asset: asset,
		Amount: 500, Fee: 1, Charged: 501, Seq: 1,
	}
	wantP2Ref := ReconRecordRef{
		Kind: "payment", ID: "p2", Account: acct, Asset: asset,
		Amount: 300, Fee: 0, Charged: 300, Seq: 2,
	}

	// 两种结局下付款批次自身与对账报告的期望。
	wantSubmitStatuses := []string{StatusSettled, StatusSettled}
	wantStatuses := []string{ReconOutOfScope, ReconMatched}
	wantMaxPaySeq := int64(2)
	wantPayThrough := int64(2)
	// 两侧扣款合计都只计范围内 p2 的 300，净扣款差额为零——p1 的 501 是
	// 范围外项，绝不允许混入本次范围的任一侧合计。
	wantLedgerRow := ReconTotalRow{Account: acct, Asset: asset, ChargedTotal: "300", RefundedTotal: "0", NetCharged: "300"}
	wantFlowRow := ReconTotalRow{Account: acct, Asset: asset, ChargedTotal: "300", RefundedTotal: "0", NetCharged: "300"}
	wantNetDiff := ReconNetDiff{Account: acct, Asset: asset, LedgerNetCharged: "300", FlowNetCharged: "300", NetChargedDiff: "0"}
	wantBalance := int64(199) // 1000-501-300
	wantSettlements := []Record{wantP1, wantP2}
	wantLedgerRowCount := 1
	if failSecond {
		// p1 成功、p2 保存失败：批次保留 p1、p2 报 storage_error，余额 499。
		wantSubmitStatuses = []string{StatusSettled, StatusStorage}
		// 对账仍基于完整状态正常返回报告：默认上界随成功历史落到 1，付款范围
		// (1,1] 为空。p1 仍是范围外；p2 账本无成功记录，为 missing_in_ledger。
		wantStatuses = []string{ReconOutOfScope, ReconMissingInLedger}
		wantMaxPaySeq = 1
		wantPayThrough = 1
		// 账本侧没有入选付款（合计没有任何行）；流水侧仍逐条计入账本外 p2 的
		// 300，净扣款差额按流水减账本为 300，不能把 p2 当范围外忽略。
		wantLedgerRow = ReconTotalRow{}
		wantNetDiff = ReconNetDiff{Account: acct, Asset: asset, LedgerNetCharged: "0", FlowNetCharged: "300", NetChargedDiff: "300"}
		wantBalance = 499 // 1000-501
		wantSettlements = []Record{wantP1}
		wantLedgerRowCount = 0
	}

	path := newTestLedger(t, []BalanceInit{{Account: acct, Asset: asset, Balance: 1000}})
	lSubmit := openOrFail(t, path) // 句柄 A：提交付款批次
	lRecon := openOrFail(t, path) // 句柄 B：按范围对账（同一进程、同一本账本）

	// 句柄 B 的范围：付款排除下界为 1、上界不提供（缺省取完整历史的最大成功
	// 序号）；退款边界全部缺省（空历史，范围 (0,0] 为空）。
	payAfter := int64(1)
	bounds := ReconcileBounds{PaymentAfter: &payAfter}

	// 外部流水依次给出 p1、p2，账户、资产、手续费与扣款总额与各自付款一致。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", acct, asset, 500, 1, 501, ""),
		flowEntry(1, "payment", "p2", acct, asset, 300, 0, 300, ""),
	}

	// 故障注入门：第 1 次持久化（p1）正常通过，第 2 次持久化（p2 的保存点）
	// 停住，放行后按 failSecond 决定成败。整个等待期间 A 仍持账本互斥锁。
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

	// 句柄 A：在一个批次内顺序提交 p1、p2。p2 的保存会停在 gate，整批持锁不释放。
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
	// 排队，直到 A 整批处理完毕，而不是基于“p1 已成功、p2 未定”的中途状态
	// 提前返回仅含前项成功的报告。
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
	// 等待在途付款批次整批完成”这一性质（仅凭结果无法区分等待与否——中途状态
	// 与失败结局的报告可能恰好一致，本检查补上这重保障）。
	select {
	case <-reconDone:
		t.Fatal("ranged reconcile returned while the payment batch was still saving; it must wait for the whole batch")
	default:
	}
	close(gate.release) // 放行 p2 保存（成功或失败），随后整批结束并释放锁
	wg.Wait()

	SetFailHook(lSubmit, nil) // 用例结束，拆卸故障注入

	if submitErr != nil {
		t.Fatalf("submit batch: %v", submitErr)
	}
	if reconErr != nil {
		t.Fatalf("ranged reconcile: %v", reconErr)
	}

	// ---- 付款批次逐项结果：与输入顺序一一对应，各自独立处理 ----
	if got := statuses(submitRes); !reflect.DeepEqual(got, wantSubmitStatuses) {
		t.Fatalf("submit statuses=%v want %v", got, wantSubmitStatuses)
	}
	if submitRes.Results[0].ID != "p1" || submitRes.Results[1].ID != "p2" {
		t.Fatalf("submit results out of order: %+v", submitRes.Results)
	}
	assertRecord(t, "p1 submit record", submitRes.Results[0].Record, wantP1)
	if failSecond {
		// p2 保存失败：报告 storage_error、无成功记录，前项 p1 保留不回滚。
		if r := submitRes.Results[1]; r.Status != StatusStorage || r.Record != nil {
			t.Fatalf("failed p2: %+v", r)
		}
	} else {
		assertRecord(t, "p2 submit record", submitRes.Results[1].Record, wantP2)
	}

	// ---- 对账报告：整份基于付款批次结束后的同一个完整账本状态 ----
	rep := reconRes
	if len(rep.Results) != 2 {
		t.Fatalf("report results=%d want 2: %+v", len(rep.Results), rep.Results)
	}
	for i, wantID := range []string{"p1", "p2"} {
		r := rep.Results[i]
		if r.Index != i || r.ID != wantID || r.Kind != "payment" {
			t.Fatalf("result %d out of order: %+v", i, r)
		}
		if r.Status != wantStatuses[i] {
			t.Fatalf("result %d (%s) status=%s want %s", i, r.ID, r.Status, wantStatuses[i])
		}
		if len(r.Diffs) != 0 {
			t.Fatalf("result %d (%s) unexpected diffs: %+v", i, r.ID, r.Diffs)
		}
	}
	// p1 两种结局下都是范围外（seq=1 不满足 after=1 < seq），携带原记录、
	// 不比较字段；p2 成功时为 matched 并携带记录。
	assertRefUntouched(t, *rep.Results[0].Record, wantP1Ref, "p1 out_of_scope record")
	if failSecond {
		// 失败的 p2 不能被算成成功记录：missing_in_ledger 且不携带成功记录。
		if rep.Results[1].Record != nil {
			t.Fatalf("failed p2 must not carry a ledger record: %+v", rep.Results[1].Record)
		}
	} else {
		assertRefUntouched(t, *rep.Results[1].Record, wantP2Ref, "p2 matched record")
	}

	// 两种结局下付款与退款缺失列表都为空：成功时范围内只有 p2 且已被流水
	// 覆盖；失败时付款范围为空，根本没有入选记录；退款历史恒为空。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v want empty", rep.MissingRefunds)
	}

	// 最大成功序号与本次实际采用的四个边界：上界缺省，必须取批次结束后完整
	// 历史的最大付款序号（2 或仅成功项的 1），不能提前返回停在前项的半批值；
	// 退款没有历史，最大序号与上下界均为零（范围为空）。
	if rep.MaxPaymentSeq != wantMaxPaySeq || rep.MaxRefundSeq != 0 {
		t.Fatalf("max seqs=%d/%d want %d/0", rep.MaxPaymentSeq, rep.MaxRefundSeq, wantMaxPaySeq)
	}
	if rep.PaymentAfter != 1 || rep.PaymentThrough != wantPayThrough ||
		rep.RefundAfter != 0 || rep.RefundThrough != 0 {
		t.Fatalf("bounds payment=%d,%d refund=%d,%d want payment=1,%d refund=0,0",
			rep.PaymentAfter, rep.PaymentThrough, rep.RefundAfter, rep.RefundThrough, wantPayThrough)
	}

	// 金额汇总（十进制字符串）：
	// 成功——账本侧只计入选的 p2（300），流水侧排除范围外 p1 后也只计 p2
	// （300），净扣款差额为零，p1 的 501 不混入任一侧；
	// 失败——账本侧无入选付款（无汇总行），流水侧仍逐条计入账本外 p2 的 300，
	// 净差额按流水减账本为 300。
	if len(rep.Totals.Ledger) != wantLedgerRowCount {
		t.Fatalf("ledger totals rows=%d want %d: %+v", len(rep.Totals.Ledger), wantLedgerRowCount, rep.Totals.Ledger)
	}
	if !failSecond {
		if got := findTotalRow(t, rep.Totals.Ledger, acct, asset); got != wantLedgerRow {
			t.Fatalf("ledger totals:\n got %+v\nwant %+v", got, wantLedgerRow)
		}
	}
	if got := findTotalRow(t, rep.Totals.Flow, acct, asset); got != wantFlowRow {
		t.Fatalf("flow totals:\n got %+v\nwant %+v", got, wantFlowRow)
	}
	if len(rep.Totals.Flow) != 1 {
		t.Fatalf("flow totals rows=%d want 1: %+v", len(rep.Totals.Flow), rep.Totals.Flow)
	}
	if len(rep.NetDiff) != 1 {
		t.Fatalf("net diff rows=%d want 1: %+v", len(rep.NetDiff), rep.NetDiff)
	}
	if rep.NetDiff[0] != wantNetDiff {
		t.Fatalf("net diff:\n got %+v\nwant %+v", rep.NetDiff[0], wantNetDiff)
	}

	// ---- 对账只读：不新增付款或退款，不改变批次完成后的余额与账本文件 ----
	snap := mustQuery(t, lRecon)
	if !reflect.DeepEqual(snap.Settlements, wantSettlements) {
		t.Fatalf("settlements after reconcile:\n got %+v\nwant %+v (reconcile must not add payments)", snap.Settlements, wantSettlements)
	}
	if len(snap.Refunds) != 0 {
		t.Fatalf("refunds after reconcile=%+v want empty", snap.Refunds)
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

// TestRangedReconcileWaitsForInFlightPaymentBatchBothSettled：p1、p2 都保存
// 成功（余额 199）。在批次中途发起的范围对账（payment_after=1、上界缺省）
// 必须等整批结束：最大付款序号与本次上界均为 2，下界仍为 1；p1 返回
// out_of_scope 并携带原记录，p2 返回 matched；两侧扣款合计都只计 p2 的
// 300，净扣款差额为零，p1 的 501 不混入本次范围；付款与退款缺失列表均空。
func TestRangedReconcileWaitsForInFlightPaymentBatchBothSettled(t *testing.T) {
	runRangedReconcileDuringPaymentInterleave(t, false)
}

// TestRangedReconcileWaitsForInFlightPaymentBatchSecondSaveFails：p1 成功、
// p2 保存失败。付款批次保留 p1、p2 报 storage_error、余额 499；并发范围对账
// 仍正常返回：最大付款序号与本次上界均为 1，付款范围 (1,1] 为空；p1 仍是
// out_of_scope，p2 为 missing_in_ledger 且不携带成功记录；账本侧没有入选
// 付款，流水侧仍计入账本外 p2 的 300，净扣款差额按流水减账本为 300。
func TestRangedReconcileWaitsForInFlightPaymentBatchSecondSaveFails(t *testing.T) {
	runRangedReconcileDuringPaymentInterleave(t, true)
}
