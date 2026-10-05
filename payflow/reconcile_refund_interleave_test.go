package payflow

import (
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 本文件为“离线流水对账”（ReconcileFlow）补充交错使用下的回归保障，重点固定
// 这一现有行为：退款批次尚未处理完时，对账必须等待整批结束，再依据完整账本
// （全部成功付款与全部成功退款落定后的余额、历史与关联）生成报告——
//   - 不能在 r1 已完整落账、r2 仍在保存（成功与否尚未确定）时提前返回
//     “第一笔退款已完成、第二笔退款尚未确定”的中途状态；
//   - r1 成功、r2 保存失败（storage_error，已回滚）是允许的完整状态：对账
//     不得把失败退款算成成功记录，r2 必须是 missing_in_ledger；
//   - 对账自身只读：不增加退款、不改变余额或账本文件，公开接口与报告格式不变。
//
// 仅“先退款完成、再顺序调用对账”不足以保障等待行为（两种结局下中途状态都可能
// 恰好等于某个最终状态）；因此本文件通过 Go 账本公开入口，在同一进程中打开同一本
// 账本的两个句柄（共享同一注册表项、同一状态与同一把互斥锁），让句柄 A 在 r2 的
// 保存点持锁停住，句柄 B 就在退款仍在保存期间发起全量对账，并用非阻塞检查锁定
// “对账此刻不得返回”，随后放行 r2 的保存。
//
// 场景固定为一个账户 aa、一种资产 usdc，初始余额 1000：
//   - 先按 30 基点成功支付 p1=500、p2=300：手续费分别为 1 和 0，
//     扣款分别为 501 和 300；付款后余额 199。
//   - 退款批次依次用 r1 退回 p1、用 r2 退回 p2，原因均为非空字符串。
//   - 对账输入按 p1、p2、r1、r2 排列，账户、资产、金额与对应原结算一致，
//     退款各自引用原付款。

// runReconcileDuringRefundSaveInterleave 执行交错用例。
// failSecond=false 时 r1、r2 都成功；failSecond=true 时 r1 成功、r2 在保存点
// 失败（storage_error，回滚：不留退款记录、余额不增加）。
func runReconcileDuringRefundSaveInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const (
		acct, asset       = "aa", "usdc"
		initial     int64 = 1000
		p1Amount    int64 = 500
		p2Amount    int64 = 300
		p1Fee       int64 = 1                // 500*30/10000 向下取整
		p2Fee       int64 = 0                // 300*30/10000 向下取整
		p1Charged         = p1Amount + p1Fee // 501
		p2Charged         = p2Amount + p2Fee // 300
	)

	path := newTestLedger(t, []BalanceInit{{Account: acct, Asset: asset, Balance: initial}})
	lRefund := openOrFail(t, path) // 句柄 A：提交退款批次
	lRecon := openOrFail(t, path)  // 句柄 B：全量对账（同一进程、同一本账本）

	// 前置：p1、p2 按 30 基点成功支付；扣款 501、300，余额 1000-801=199。
	payRes, err := lRefund.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		intent("p1", acct, asset, p1Amount),
		intent("p2", acct, asset, p2Amount),
	}})
	if err != nil {
		t.Fatalf("setup submit: %v", err)
	}
	if got := statuses(payRes); !reflect.DeepEqual(got, []string{StatusSettled, StatusSettled}) {
		t.Fatalf("setup payment statuses=%v want [settled settled]", got)
	}
	wantP1 := Record{ID: "p1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: p1Amount, Nonce: 1, FeeBps: 30, Fee: p1Fee, Charged: p1Charged, Seq: 1}
	wantP2 := Record{ID: "p2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: p2Amount, Nonce: 1, FeeBps: 30, Fee: p2Fee, Charged: p2Charged, Seq: 2}
	assertRecord(t, "setup p1", payRes.Results[0].Record, wantP1)
	assertRecord(t, "setup p2", payRes.Results[1].Record, wantP2)
	if bal, _ := lRefund.Balance(acct, asset); bal != 199 {
		t.Fatalf("balance after payments=%d want 199", bal)
	}

	// 对账输入按 p1、p2、r1、r2 排列；付款及退款的账户、资产、金额与原结算一致，
	// 退款引用各自原付款。即便 r2 保存失败，输入仍包含 r2（账本缺这笔 => missing）。
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", acct, asset, p1Amount, p1Fee, p1Charged, ""),
		flowEntry(1, "payment", "p2", acct, asset, p2Amount, p2Fee, p2Charged, ""),
		flowEntry(2, "refund", "r1", acct, asset, p1Amount, p1Fee, p1Charged, "p1"),
		flowEntry(3, "refund", "r2", acct, asset, p2Amount, p2Fee, p2Charged, "p2"),
	}

	// 退款批次：r1 退回 p1、r2 退回 p2，原因均为非空字符串。
	refundBatch := RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", "refund p1"),
		refundReq("r2", "p2", "refund p2"),
	}}

	// 第 1 次持久化（r1）正常通过；第 2 次（r2 的保存点）先通知 reached 再阻塞，
	// 直到 release 关闭，最后按 failSecond 决定 r2 本次保存成功还是失败。
	gate := &secondPersistGate{
		reached: make(chan struct{}),
		release: make(chan struct{}),
		fail:    failSecond,
	}
	SetFailHook(lRefund, gate)

	var (
		wg sync.WaitGroup

		refundRes  *RefundBatchResult
		refundErr  error
		reconStart = make(chan struct{})
		reconDone  = make(chan struct{}) // 对账返回（无论成功失败）后关闭
		report     *ReconReport
		reconErr   error
	)

	// 句柄 A：真实提交退款批次。r2 的保存会停在 gate，整批持锁不释放。
	wg.Add(1)
	go func() {
		defer wg.Done()
		refundRes, refundErr = lRefund.Refund(refundBatch)
	}()

	// 等 A 把 r1 完整落账、停在 r2 的保存点（批次仍在保存、锁仍被 A 持有）。
	<-gate.reached

	// 句柄 B：就在退款仍在保存期间发起全量对账。它应当在同一把账本锁上排队，
	// 直到退款批次整批结束，而不是读到 r1 已退、r2 未定的中途状态。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(reconDone)
		close(reconStart)
		report, reconErr = lRecon.ReconcileFlow(entries)
	}()

	<-reconStart
	// 让出 P，令对账 goroutine 实际运行到争夺账本锁并排队；A 仍停在 r2 保存点。
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
	// 退款批次仍在保存、锁仍被 A 持有：对账绝不可能在此刻返回。正确实现下对账线程
	// 正阻塞在同一把账本锁上，因此该非阻塞检查不会误报；它直接锁定“对账等待在途
	// 退款批次整批完成”这一性质——尤其在两笔退款都会成功、中途状态的合计
	// （801/501）与最终状态不同但仍可能被误读的情况下，只有等待才允许返回。
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

	// ---- 退款批次逐项结果：与输入顺序一一对应，r2 失败时报告 storage_error ----
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "refund p1",
		Account: acct, Asset: asset, Amount: p1Amount, Fee: p1Fee, Charged: p1Charged,
		AfterSeq: 2, Seq: 1,
	}
	if !failSecond {
		if got := refundStatuses(refundRes); !reflect.DeepEqual(got,
			[]string{StatusRefundSuccess, StatusRefundSuccess}) {
			t.Fatalf("refund statuses=%v want [refunded refunded]", got)
		}
		wantR2 := RefundRecord{
			ID: "r2", SettlementID: "p2", Reason: "refund p2",
			Account: acct, Asset: asset, Amount: p2Amount, Fee: p2Fee, Charged: p2Charged,
			AfterSeq: 2, Seq: 2,
		}
		assertRefundResultRecord(t, refundRes.Results[0], StatusRefundSuccess, wantR1)
		assertRefundResultRecord(t, refundRes.Results[1], StatusRefundSuccess, wantR2)
	} else {
		if got := refundStatuses(refundRes); !reflect.DeepEqual(got,
			[]string{StatusRefundSuccess, StatusStorage}) {
			t.Fatalf("refund statuses=%v want [refunded storage_error]", got)
		}
		assertRefundResultRecord(t, refundRes.Results[0], StatusRefundSuccess, wantR1)
		// r2 保存失败：storage_error、无退款记录，前项 r1 保留不回滚。
		if r := refundRes.Results[1]; r.ID != "r2" || r.Status != StatusStorage || r.Record != nil {
			t.Fatalf("failed r2 result: %+v", r)
		}
	}

	// ---- 对账逐条状态（保持 p1、p2、r1、r2 输入顺序）与关联 ----
	wantStatuses := []string{
		ReconMatched, ReconMatched, ReconMatched, ReconMatched,
	}
	if failSecond {
		// r2 保存失败：账本无成功退款 r2，前三条仍匹配，r2 记为账本缺失；
		// 失败退款绝不能被算成成功记录（不能是 matched）。
		wantStatuses[3] = ReconMissingInLedger
	}
	if len(report.Results) != 4 {
		t.Fatalf("results len=%d want 4", len(report.Results))
	}
	for i, want := range wantStatuses {
		if report.Results[i].Status != want {
			t.Fatalf("result %d (%s) status=%s want %s: %+v",
				i, entries[i].ID, report.Results[i].Status, want, report.Results[i])
		}
		if report.Results[i].Index != i || report.Results[i].ID != entries[i].ID ||
			report.Results[i].Kind != entries[i].Kind {
			t.Fatalf("result %d identity out of line with input: %+v", i, report.Results[i])
		}
	}

	// p1 关联 r1（两种结局都成立）；r1 关联原付款 p1。
	p1Ref := report.Results[0].Record
	if p1Ref == nil {
		t.Fatalf("matched p1 must carry its ledger record")
	}
	wantP1Ref := ReconRecordRef{
		Kind: "payment", ID: "p1", Account: acct, Asset: asset,
		Amount: p1Amount, Fee: p1Fee, Charged: p1Charged, Seq: 1,
		RefundID: "r1", RefundSeq: 1,
	}
	assertRefUntouched(t, *p1Ref, wantP1Ref, "p1 matched record/refund link")

	r1Ref := report.Results[2].Record
	if r1Ref == nil {
		t.Fatalf("matched r1 must carry its ledger record")
	}
	wantR1Ref := ReconRecordRef{
		Kind: "refund", ID: "r1", Account: acct, Asset: asset,
		Amount: p1Amount, Fee: p1Fee, Charged: p1Charged, Seq: 1, SettlementID: "p1",
	}
	assertRefUntouched(t, *r1Ref, wantR1Ref, "r1 matched record/settlement link")

	if !failSecond {
		// p2 关联 r2；r2 关联原付款 p2。
		p2Ref := report.Results[1].Record
		wantP2Ref := ReconRecordRef{
			Kind: "payment", ID: "p2", Account: acct, Asset: asset,
			Amount: p2Amount, Fee: p2Fee, Charged: p2Charged, Seq: 2,
			RefundID: "r2", RefundSeq: 2,
		}
		assertRefUntouched(t, *p2Ref, wantP2Ref, "p2 matched record/refund link")
		r2Ref := report.Results[3].Record
		wantR2Ref := ReconRecordRef{
			Kind: "refund", ID: "r2", Account: acct, Asset: asset,
			Amount: p2Amount, Fee: p2Fee, Charged: p2Charged, Seq: 2, SettlementID: "p2",
		}
		assertRefUntouched(t, *r2Ref, wantR2Ref, "r2 matched record/settlement link")
	} else {
		// p2 没有退款关联（r2 未成功），但付款本身仍匹配；r2 缺失项不携带账本记录。
		p2Ref := report.Results[1].Record
		wantP2Ref := ReconRecordRef{
			Kind: "payment", ID: "p2", Account: acct, Asset: asset,
			Amount: p2Amount, Fee: p2Fee, Charged: p2Charged, Seq: 2,
		}
		assertRefUntouched(t, *p2Ref, wantP2Ref, "p2 must match but have no refund link")
		if report.Results[3].Record != nil {
			t.Fatalf("missing r2 must carry no ledger record: %+v", report.Results[3].Record)
		}
	}

	// ---- 最大成功序号 ----
	if report.MaxPaymentSeq != 2 {
		t.Fatalf("max payment seq=%d want 2", report.MaxPaymentSeq)
	}
	if wantMaxRef := int64(2); !failSecond && report.MaxRefundSeq != wantMaxRef {
		t.Fatalf("max refund seq=%d want 2", report.MaxRefundSeq)
	}
	if failSecond && report.MaxRefundSeq != 1 {
		t.Fatalf("max refund seq=%d want 1 (failed r2 must not count)", report.MaxRefundSeq)
	}
	// 全量对账：范围即完整历史。
	if report.PaymentAfter != 0 || report.PaymentThrough != 2 ||
		report.RefundAfter != 0 || report.RefundThrough != report.MaxRefundSeq {
		t.Fatalf("full-history bounds wrong: %+v", report)
	}

	// ---- 账本侧 / 流水侧汇总（十进制字符串）与净差额（流水减账本） ----
	if len(report.Totals.Ledger) != 1 || len(report.Totals.Flow) != 1 {
		t.Fatalf("totals rows must be the single account/asset: %+v", report.Totals)
	}
	ledgerRow := report.Totals.Ledger[0]
	flowRow := report.Totals.Flow[0]
	if ledgerRow.Account != acct || ledgerRow.Asset != asset ||
		flowRow.Account != acct || flowRow.Asset != asset {
		t.Fatalf("totals rows target wrong account/asset: %+v / %+v", ledgerRow, flowRow)
	}
	// 两笔付款合计 801，两种结局都不变。
	if ledgerRow.ChargedTotal != "801" || flowRow.ChargedTotal != "801" {
		t.Fatalf("charged totals ledger=%q flow=%q want 801/801",
			ledgerRow.ChargedTotal, flowRow.ChargedTotal)
	}
	// 流水侧始终包含输入的两笔退款（501+300=801）。
	if flowRow.RefundedTotal != "801" {
		t.Fatalf("flow refunded total=%q want 801", flowRow.RefundedTotal)
	}
	if len(report.NetDiff) != 1 {
		t.Fatalf("net diff rows=%+v want one row", report.NetDiff)
	}
	diff := report.NetDiff[0]
	if diff.Account != acct || diff.Asset != asset {
		t.Fatalf("net diff target wrong: %+v", diff)
	}

	if !failSecond {
		// 两笔退款都成功：账本退款 501+300=801，两侧净扣款均为 0，净差额 0。
		if ledgerRow.RefundedTotal != "801" {
			t.Fatalf("ledger refunded total=%q want 801", ledgerRow.RefundedTotal)
		}
		if ledgerRow.NetCharged != "0" || flowRow.NetCharged != "0" {
			t.Fatalf("net charged ledger=%q flow=%q want 0/0",
				ledgerRow.NetCharged, flowRow.NetCharged)
		}
		if diff.LedgerNetCharged != "0" || diff.FlowNetCharged != "0" ||
			diff.NetChargedDiff != "0" {
			t.Fatalf("net diff=%+v want all \"0\"", diff)
		}
	} else {
		// r1 成功、r2 失败：账本退款只有 501，净扣款 801-501=300；
		// 流水侧仍含两笔退款，净扣款 0；流水减账本 = 0-300 = -300。
		if ledgerRow.RefundedTotal != "501" {
			t.Fatalf("ledger refunded total=%q want 501", ledgerRow.RefundedTotal)
		}
		if ledgerRow.NetCharged != "300" {
			t.Fatalf("ledger net charged=%q want 300", ledgerRow.NetCharged)
		}
		if flowRow.NetCharged != "0" {
			t.Fatalf("flow net charged=%q want 0", flowRow.NetCharged)
		}
		if diff.LedgerNetCharged != "300" || diff.FlowNetCharged != "0" ||
			diff.NetChargedDiff != "-300" {
			t.Fatalf("net diff=%+v want ledger 300 / flow 0 / diff -300", diff)
		}
	}

	// ---- 账本缺失列表：两种结局都为空 ----
	// 全部成功：四条流水都命中，账本没有无流水的成功记录。
	// r2 失败：成功记录是 p1、p2、r1，恰好都被流水覆盖；失败的 r2 不是成功记录，
	// 不能出现在账本缺失列表里。
	if len(report.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty", report.MissingPayments)
	}
	if len(report.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v want empty (failed r2 is not a successful ledger record)",
			report.MissingRefunds)
	}

	// ---- 余额：对账只读，余额只由真实退款决定 ----
	// 全部成功：两笔扣款 801、两笔退款 801，余额回到 1000。
	wantBalance := initial // 1000
	if failSecond {
		// 只退回 r1=501：1000-801+501=700。
		wantBalance = 700
	}
	if bal, _ := lRecon.Balance(acct, asset); bal != wantBalance {
		t.Fatalf("balance after reconcile=%d want %d (reconcile must not change balances)",
			bal, wantBalance)
	}

	// ---- 对账不增加退款、不改写账本文件：关句柄后从磁盘重开核对 ----
	snap := mustQuery(t, lRecon)
	wantRefunds := []RefundRecord{wantR1}
	if !failSecond {
		wantRefunds = append(wantRefunds, RefundRecord{
			ID: "r2", SettlementID: "p2", Reason: "refund p2",
			Account: acct, Asset: asset, Amount: p2Amount, Fee: p2Fee, Charged: p2Charged,
			AfterSeq: 2, Seq: 2,
		})
	}
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("settlements changed by reconcile: %+v", snap.Settlements)
	}
	if !reflect.DeepEqual(snap.Refunds, wantRefunds) {
		t.Fatalf("refunds:\n got %+v\nwant %+v (reconcile must not add refunds)",
			snap.Refunds, wantRefunds)
	}

	lRefund.Close()
	lRecon.Close()
	fromDisk := openOrFail(t, path)
	diskSnap := mustQuery(t, fromDisk)
	if !reflect.DeepEqual(diskSnap.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("ledger file settlements: %+v", diskSnap.Settlements)
	}
	if !reflect.DeepEqual(diskSnap.Refunds, wantRefunds) {
		t.Fatalf("ledger file refunds:\n got %+v\nwant %+v", diskSnap.Refunds, wantRefunds)
	}
	if bal, _ := fromDisk.Balance(acct, asset); bal != wantBalance {
		t.Fatalf("ledger file balance=%d want %d", bal, wantBalance)
	}

	// 失败结局下，r2 从未占用退款编号/序号：重新提交 r2 必须真正成功（不是 duplicate），
	// 成功序号为 2，余额随之恢复到 1000；且这不改变“对账只读”的结论。
	if failSecond {
		retry, err := fromDisk.Refund(RefundBatch{Refunds: []RefundRequest{
			refundReq("r2", "p2", "refund p2"),
		}})
		if err != nil {
			t.Fatalf("retry r2: %v", err)
		}
		wantR2Retry := RefundRecord{
			ID: "r2", SettlementID: "p2", Reason: "refund p2",
			Account: acct, Asset: asset, Amount: p2Amount, Fee: p2Fee, Charged: p2Charged,
			AfterSeq: 2, Seq: 2,
		}
		assertRefundResultRecord(t, retry.Results[0], StatusRefundSuccess, wantR2Retry)
		if bal, _ := fromDisk.Balance(acct, asset); bal != 1000 {
			t.Fatalf("balance after r2 retry=%d want 1000", bal)
		}
	}
}

// TestReconcileWaitsForInFlightRefundBatchBothRefunded：r1、r2 都成功。
// 在 r2 保存期间发起的全量对账必须等整批结束：p1、p2、r1、r2 全部 matched，
// p1/p2 分别关联 r1/r2；付款、退款最大成功序号均为 2；两侧扣款与退款合计均为
// "801"，净扣款与净差额均为 "0"，余额 1000，账本缺失列表为空。
func TestReconcileWaitsForInFlightRefundBatchBothRefunded(t *testing.T) {
	runReconcileDuringRefundSaveInterleave(t, false)
}

// TestReconcileWaitsForInFlightRefundBatchSecondSaveFails：r1 成功、r2 保存失败。
// 在 r2 保存期间发起的对账必须等整批（含失败回滚）结束：前三条 matched、r2 为
// missing_in_ledger；p1 关联 r1、p2 无退款关联；退款最大成功序号为 1；
// 账本侧扣款 "801"、退款 "501"、净扣款 "300"，流水侧两笔退款都在、净扣款 "0"，
// 流水减账本净差额 "-300"，余额 700，账本缺失列表为空，失败退款不算成功记录。
func TestReconcileWaitsForInFlightRefundBatchSecondSaveFails(t *testing.T) {
	runReconcileDuringRefundSaveInterleave(t, true)
}
