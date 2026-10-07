package payflow

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// 本文件为“有序全额退款批次处理期间，另一句柄发起付款预览”补充交错回归保障，
// 入口固定为既有公开接口 Ledger.Preview（--dry-run 的同一实现）：
//
//	同一进程中两个句柄打开同一本账本。句柄 A 正在顺序处理一个全额退款批次
//	（第一项已完整保存、第二项停在保存点，账本互斥锁仍被 A 持有），句柄 B
//	就在此刻发起付款预览。预览必须在同一把账本锁上排队，等退款批次“全部”
//	处理结束后，再依据完整的余额与完整的成功/退款历史在副本上给出结果：
//	  - 第一笔退款已保存、第二笔成败未定时不能提前返回；
//	  - 不能把“尚未保存成功的回补余额”计入可用余额；
//	  - 退款批次第二项若是 storage_error，预览只基于“第一项成功保留”的完整
//	    状态判断，绝不把该保存错误复制成自己的逐项状态（预览不报错、
//	    不出现 storage_error）；
//	  - 结果恒带 dry_run:true、逐项与输入顺序对应；预览结束后余额、两类历史、
//	    账本文件、新编号与付款序号都只反映真实发生过的操作。
//
// 场景与数值固定：账户 aa、资产 usdc，初始余额 200，费率 1000 基点。
// 先成功付款 p1=70（手续费 7、扣款总额 77、序号 1）与 p2=90（手续费 9、
// 扣款总额 99、序号 2），余额剩 24。随后顺序全额退回这两笔付款；在第一笔
// 退款完成、第二笔仍在保存期间，另一句柄以同一费率依次预览新编号付款
// n1=150（手续费 15、扣款总额 165）与 n2=20（手续费 2、扣款总额 22）。

// secondRefundPersistGate 是仅用于测试的持久化故障注入：第 1 次持久化
// （第一笔退款）正常通过，第 2 次持久化（第二笔退款的保存点）先通知
// reached，再阻塞到 release 关闭，最后按 fail 决定本次保存成功还是失败。
// 整个等待期间调用方仍持有账本互斥锁。
type secondRefundPersistGate struct {
	calls   int
	reached chan struct{} // 第二笔退款保存点已进入（批次停在中途、锁仍持有）
	release chan struct{} // 放行第二笔退款的保存
	fail    bool          // 放行后是否让该次持久化失败
}

func (g *secondRefundPersistGate) FailNextPersist() bool {
	g.calls++
	if g.calls != 2 {
		return false
	}
	close(g.reached)
	<-g.release
	return g.fail
}

// runPreviewDuringRefundBatchInterleave 执行交错用例。failSecond=false 时两笔
// 退款都成功（余额恢复 200）；failSecond=true 时第一笔成功、第二笔保存失败
// （storage_error 并回滚，余额 101，退款历史只留第一笔）。
func runPreviewDuringRefundBatchInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const (
		acct, asset = "aa", "usdc"
		feeBps      = 1000
		reason      = "full refund"
	)

	wantP1 := Record{ID: "p1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 70, Nonce: 1, FeeBps: feeBps, Fee: 7, Charged: 77, Seq: 1}
	wantP2 := Record{ID: "p2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 90, Nonce: 1, FeeBps: feeBps, Fee: 9, Charged: 99, Seq: 2}
	// 两笔退款落账时两笔付款都已存在，after_seq 均为 2；退款序号按成功先后为 1、2。
	wantR1 := RefundRecord{ID: "r1", SettlementID: "p1", Reason: reason,
		Account: acct, Asset: asset, Amount: 70, Fee: 7, Charged: 77, AfterSeq: 2, Seq: 1}
	wantR2 := RefundRecord{ID: "r2", SettlementID: "p2", Reason: reason,
		Account: acct, Asset: asset, Amount: 90, Fee: 9, Charged: 99, AfterSeq: 2, Seq: 2}
	// 预览里的两笔新编号预计付款：150+15=165、20+2=22。
	wantN1 := Record{ID: "n1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 150, Nonce: 1, FeeBps: feeBps, Fee: 15, Charged: 165, Seq: 3}
	wantN2Seq4 := Record{ID: "n2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 20, Nonce: 1, FeeBps: feeBps, Fee: 2, Charged: 22, Seq: 4}
	wantN2Seq3 := wantN2Seq4
	wantN2Seq3.Seq = 3

	// 两种结局下真实状态与预览结果的期望。
	wantRefundStatuses := []string{StatusRefundSuccess, StatusRefundSuccess}
	wantRefunds := []RefundRecord{wantR1, wantR2}
	wantRealBalance := int64(200) // 24+77+99
	wantPreviewStatuses := []string{StatusSettled, StatusSettled}
	if failSecond {
		// 第一笔退款成功保留，第二笔保存失败回滚：24+77=101，退款历史只留 r1。
		wantRefundStatuses = []string{StatusRefundSuccess, StatusStorage}
		wantRefunds = []RefundRecord{wantR1}
		wantRealBalance = 101
		// 预览基于该完整状态：n1=165 > 101 余额不足（不带成功记录，也不模拟扣款）；
		// n2=22 仍可预计成功，序号 3（结算历史长度仍为 2）。
		wantPreviewStatuses = []string{StatusFunds, StatusSettled}
	}

	path := newTestLedger(t, []BalanceInit{{Account: acct, Asset: asset, Balance: 200}})
	lRefund := openOrFail(t, path)  // 句柄 A：顺序处理全额退款批次
	lPreview := openOrFail(t, path) // 句柄 B：并发预览（同一进程、同一本账本）

	// 前置：按 1000 基点费率顺序成功付款 70、90，余额 24，序号 1、2。
	setup, err := lRefund.Submit(FeeBatch{FeeBps: feeBps, Intents: []PaymentIntent{
		intent("p1", acct, asset, 70),
		intent("p2", acct, asset, 90),
	}})
	if err != nil {
		t.Fatalf("setup submit: %v", err)
	}
	if got, want := statuses(setup), []string{StatusSettled, StatusSettled}; !reflect.DeepEqual(got, want) {
		t.Fatalf("setup statuses=%v want %v", got, want)
	}
	assertRecord(t, "setup p1", setup.Results[0].Record, wantP1)
	assertRecord(t, "setup p2", setup.Results[1].Record, wantP2)
	if bal, _ := lRefund.Balance(acct, asset); bal != 24 {
		t.Fatalf("balance after setup=%d want 24", bal)
	}

	refundBatch := RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", reason),
		refundReq("r2", "p2", reason),
	}}
	// 预览两个新编号，费率与原付款相同。
	previewBatch := FeeBatch{FeeBps: feeBps, Intents: []PaymentIntent{
		intent("n1", acct, asset, 150),
		intent("n2", acct, asset, 20),
	}}

	gate := &secondRefundPersistGate{
		reached: make(chan struct{}),
		release: make(chan struct{}),
		fail:    failSecond,
	}
	SetFailHook(lRefund, gate)

	var (
		wg sync.WaitGroup

		refundRes  *RefundBatchResult
		refundErr  error
		prevStart  = make(chan struct{})
		prevDone   = make(chan struct{}) // 预览返回（无论成功失败）后关闭
		prevRes    *PreviewResult
		prevErr    error
	)

	// 句柄 A：顺序全额退回 p1、p2。第二笔退款的保存会停在 gate，整批持锁不释放。
	wg.Add(1)
	go func() {
		defer wg.Done()
		refundRes, refundErr = lRefund.Refund(refundBatch)
	}()

	// 等第一笔退款完整落账、批次停在第二笔退款的保存点（锁仍被 A 持有）。
	<-gate.reached

	// 句柄 B：就在第二笔退款成败未定时发起预览。它应当在同一把账本锁上排队，
	// 直到退款批次整批处理完毕，而不是读到“第一笔已回补、第二笔未定”的中途
	// 状态——中途余额 101 与最终余额在两种结局下都可能相同或不同，仅凭结果
	// 无法锁定等待性质，下面另有非阻塞检查。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(prevDone)
		close(prevStart)
		prevRes, prevErr = lPreview.Preview(previewBatch)
	}()

	<-prevStart
	// 让出 P，令预览 goroutine 实际运行到争夺账本锁并排队；A 仍停在保存点。
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
	// 第一笔退款已保存、第二笔尚未定成败：预览绝不可能在此刻提前返回。
	select {
	case <-prevDone:
		t.Fatal("preview returned while the refund batch was still saving; it must wait for the whole batch")
	default:
	}
	close(gate.release) // 放行第二笔退款保存（成功或失败），随后整批结束并释放锁
	wg.Wait()

	SetFailHook(lRefund, nil) // 用例结束，拆卸故障注入

	if refundErr != nil {
		t.Fatalf("refund batch: %v", refundErr)
	}
	if prevErr != nil {
		t.Fatalf("preview: %v", prevErr)
	}

	// ---- 退款批次逐项结果：与输入顺序一一对应，各自独立处理 ----
	if len(refundRes.Results) != 2 ||
		refundRes.Results[0].ID != "r1" || refundRes.Results[1].ID != "r2" {
		t.Fatalf("refund results out of order: %+v", refundRes.Results)
	}
	if got, want := refundStatuses(refundRes), wantRefundStatuses; !reflect.DeepEqual(got, want) {
		t.Fatalf("refund statuses=%v want %v", got, want)
	}
	assertRefundResultRecord(t, refundRes.Results[0], StatusRefundSuccess, wantR1)
	if failSecond {
		// 第二笔保存失败：storage_error、无退款记录与去向详情；第一笔成功不回滚。
		r := refundRes.Results[1]
		if r.Status != StatusStorage || r.Record != nil {
			t.Fatalf("failed second refund: %+v", r)
		}
		if r.SettlementID != "" || r.Account != "" || r.Asset != "" || r.Charged != 0 {
			t.Fatalf("storage_error must not carry success detail: %+v", r)
		}
		if !strings.Contains(r.Reason, "injected write failure") {
			t.Fatalf("second refund must carry the storage failure explanation, got %q", r.Reason)
		}
	} else {
		assertRefundResultRecord(t, refundRes.Results[1], StatusRefundSuccess, wantR2)
	}

	// ---- 预览逐项结果：两个输入、同序对应，整份基于同一个完整状态 ----
	if !prevRes.DryRun {
		t.Fatalf("preview must carry dry_run=true: %+v", prevRes)
	}
	if len(prevRes.Results) != 2 ||
		prevRes.Results[0].ID != "n1" || prevRes.Results[1].ID != "n2" {
		t.Fatalf("preview results must line up with inputs n1,n2: %+v", prevRes.Results)
	}
	if got, want := previewStatuses(prevRes), wantPreviewStatuses; !reflect.DeepEqual(got, want) {
		t.Fatalf("preview statuses=%v want %v", got, want)
	}
	if failSecond {
		// 首项余额不足：只带状态与原因，不携带任何成功记录；它不模拟扣款，
		// 因此第二项仍按真实余额 101 判断。
		first := prevRes.Results[0]
		if first.Status != StatusFunds || first.Record != nil || first.Reason != reasonFunds {
			t.Fatalf("preview n1: %+v", first)
		}
		// 第二项仍预计成功：扣款总额 22、预计序号 3（失败的退款不占序号，
		// 结算历史仍是最初两笔；也证明后项没有继承首项不存在的预计扣款）。
		assertRecord(t, "preview n2 predicted record", prevRes.Results[1].Record, wantN2Seq3)
	} else {
		// 两笔退款都成功、真实余额恢复 200：两项都预计成功，后项使用前项
		// 预计扣款后的余额（200-165=35 >= 22）；预计序号 3、4 连续递增。
		assertRecord(t, "preview n1 predicted record", prevRes.Results[0].Record, wantN1)
		assertRecord(t, "preview n2 predicted record", prevRes.Results[1].Record, wantN2Seq4)
	}
	// 预览本身正常返回：任何一项都不能复制退款批次的 storage_error。
	for i, r := range prevRes.Results {
		if r.Status == StatusStorage {
			t.Fatalf("preview must never report storage_error, item %d: %+v", i, r)
		}
	}

	// ---- 预览不写账本：余额、付款历史、退款历史只反映真实发生的操作 ----
	snap := mustQuery(t, lPreview)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("settlements after preview:\n got %+v\nwant [p1,p2] unchanged (predicted n1/n2 must not be persisted)",
			snap.Settlements)
	}
	if !reflect.DeepEqual(snap.Refunds, wantRefunds) {
		t.Fatalf("refund history after preview:\n got %+v\nwant %+v", snap.Refunds, wantRefunds)
	}
	if bal, _ := lPreview.Balance(acct, asset); bal != wantRealBalance {
		t.Fatalf("balance after preview=%d want %d (preview must not change balances)", bal, wantRealBalance)
	}

	// 账本文件同样只反映真实退款：记录文件字节，关闭全部句柄使注册表项注销，
	// 重新从磁盘打开核对；重开与再次预览都不得额外改写文件。
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lRefund.Close()
	lPreview.Close()
	fromDisk := openOrFail(t, path)
	diskSnap := mustQuery(t, fromDisk)
	if !reflect.DeepEqual(diskSnap.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("ledger file settlements:\n got %+v\nwant [p1,p2]", diskSnap.Settlements)
	}
	if !reflect.DeepEqual(diskSnap.Refunds, wantRefunds) {
		t.Fatalf("ledger file refunds:\n got %+v\nwant %+v", diskSnap.Refunds, wantRefunds)
	}
	if bal, _ := fromDisk.Balance(acct, asset); bal != wantRealBalance {
		t.Fatalf("ledger file balance=%d want %d", bal, wantRealBalance)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, fileBytes) {
		t.Fatalf("reopen/query rewrote the ledger file (err=%v)", err)
	}
	// 再跑一次相同预览：账本文件必须逐字节不变。
	if _, err := fromDisk.Preview(previewBatch); err != nil {
		t.Fatalf("second preview: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, fileBytes) {
		t.Fatalf("preview rewrote the ledger file (err=%v)", err)
	}

	// ---- 预览不预留新编号或序号：随后真实提交按首次付款处理 ----
	// 先真实提交预览中的第二项 n2=22：两种结局下它都必须 settled（而非
	// duplicate/conflict），序号为 3——预览里出现过的序号 3/4 都未被占用。
	realN2, err := fromDisk.Submit(FeeBatch{FeeBps: feeBps, Intents: []PaymentIntent{
		intent("n2", acct, asset, 20),
	}})
	if err != nil {
		t.Fatalf("real submit n2: %v", err)
	}
	if r := realN2.Results[0]; r.Status != StatusSettled {
		t.Fatalf("predicted n2 must settle for real as a fresh payment, got %+v", r)
	}
	assertRecord(t, "real n2", realN2.Results[0].Record, wantN2Seq3)

	// 再真实提交预览首项 n1=165：编号同样未被预览占用（不会是 duplicate/conflict）。
	realN1, err := fromDisk.Submit(FeeBatch{FeeBps: feeBps, Intents: []PaymentIntent{
		intent("n1", acct, asset, 150),
	}})
	if err != nil {
		t.Fatalf("real submit n1: %v", err)
	}
	if failSecond {
		// 真实余额 101，n2 真实扣款 22 后余 79：n1=165 余额不足、不留记录；
		// 关键是状态为 insufficient_balance 而非 duplicate——预览从未占用 n1。
		if r := realN1.Results[0]; r.Status != StatusFunds || r.Record != nil {
			t.Fatalf("real n1 against balance 79: %+v", r)
		}
		final := mustQuery(t, fromDisk)
		if !reflect.DeepEqual(final.Settlements, []Record{wantP1, wantP2, wantN2Seq3}) {
			t.Fatalf("history after real submits (second refund failed):\n got %+v", final.Settlements)
		}
		if !reflect.DeepEqual(final.Refunds, wantRefunds) {
			t.Fatalf("refund history changed after real submits:\n got %+v\nwant %+v", final.Refunds, wantRefunds)
		}
		if bal, _ := fromDisk.Balance(acct, asset); bal != 79 {
			t.Fatalf("balance after real submits=%d want 79", bal)
		}
	} else {
		// 真实余额 200，n2 真实扣款 22 后余 178：n1=165 预计成功的同一项现在
		// 真实成功，序号 4（与当时预览里 n2 占用序号 4 的预测不同——序号从未预留）。
		wantN1Real := wantN1
		wantN1Real.Seq = 4
		if r := realN1.Results[0]; r.Status != StatusSettled {
			t.Fatalf("real n1 must settle, got %+v", r)
		}
		assertRecord(t, "real n1", realN1.Results[0].Record, wantN1Real)
		final := mustQuery(t, fromDisk)
		if !reflect.DeepEqual(final.Settlements, []Record{wantP1, wantP2, wantN2Seq3, wantN1Real}) {
			t.Fatalf("history after real submits (both refunds succeeded):\n got %+v", final.Settlements)
		}
		if !reflect.DeepEqual(final.Refunds, wantRefunds) {
			t.Fatalf("refund history changed after real submits:\n got %+v\nwant %+v", final.Refunds, wantRefunds)
		}
		if bal, _ := fromDisk.Balance(acct, asset); bal != 13 {
			t.Fatalf("balance after real submits=%d want 13 (200-22-165)", bal)
		}
	}
}

// TestPreviewWaitsForInFlightRefundBatchBothRefunded：两笔退款都成功，真实余额
// 恢复 200。与退款批次并发发起的预览必须等整批退款完成：n1=165、n2=22 均
// settled（扣款总额 165、22，预计序号 3、4，后项使用前项预计扣款后的余额）。
func TestPreviewWaitsForInFlightRefundBatchBothRefunded(t *testing.T) {
	runPreviewDuringRefundBatchInterleave(t, false)
}

// TestPreviewWaitsForInFlightRefundBatchSecondSaveFails：第一笔退款成功、第二笔
// 保存失败。真实余额 101、退款历史只保留第一笔；并发预览基于该完整状态：
// n1=165 insufficient_balance 且不携带成功记录，n2=22 仍可预计成功（扣款
// 总额 22、预计序号 3）；预览正常返回，不继承第二笔退款的 storage_error。
func TestPreviewWaitsForInFlightRefundBatchSecondSaveFails(t *testing.T) {
	runPreviewDuringRefundBatchInterleave(t, true)
}
