package payflow

import (
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 本文件为“扣款前预览”（submit --dry-run / Ledger.Preview）补充交错使用下的
// 回归保障：同一进程中两个句柄指向同一本账本时，一个句柄正在提交付款批次
// （批次仍在逐项保存、账本互斥锁尚未释放），另一个句柄发起预览。预览必须等
// 该批次整批处理完毕，再依据“完整余额 + 完整成功历史”在副本上判断整份预览：
//   - 不能把尚在保存的付款当成已结算（不能读到批次中途的历史）；
//   - 不能拿批次中途的余额去跑整份预览（不能读到批次中途的余额）；
//   - 前项成功、后项保存失败是允许的完整状态：后项已回滚、不留成功记录，
//     预览不得把该失败项当成已结算记录，也不得继承它的 storage_error；
//   - 预览始终带 dry_run，不扣余额、不落盘、不预留任何编号与成功序号。
//
// 场景固定为：一个账户 aa、一种资产 usdc，初始余额 100，费率 0。
// 句柄 A 顺序真实提交 p1=60、p2=30；在 p2 的“保存点”用故障注入门把批次
// 停住（此时 p1 已完整落账、锁仍被 A 持有）。句柄 B 就在此刻发起预览，
// 依次放入与原请求完全相同的 p1、p2 与新编号 p3=15。随后放行 p2 的保存，
// 分别覆盖“两笔都成功”和“p1 成功、p2 保存失败”两种完整结局。

// secondPersistGate 是仅用于测试的持久化故障注入：第 1 次持久化（p1）正常
// 通过，第 2 次持久化（p2 的保存点）先通知 reached，再阻塞到 release 关闭，
// 最后按 fail 决定本次保存成功还是失败。整个等待期间调用方仍持有账本互斥锁。
type secondPersistGate struct {
	calls   int
	reached chan struct{} // p2 保存点已进入（批次停在中途、锁仍持有）
	release chan struct{} // 放行 p2 的保存
	fail    bool          // 放行后是否让 p2 本次持久化失败
}

func (g *secondPersistGate) FailNextPersist() bool {
	g.calls++
	if g.calls != 2 {
		return false
	}
	close(g.reached)
	<-g.release
	return g.fail
}

// assertRecord 断言结果携带的结算记录与期望逐字段一致（含编号、账户、付款方、
// 资产、金额、nonce、费率、手续费、扣款总额与成功序号）。
func assertRecord(t *testing.T, label string, got *Record, want Record) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: missing record, want %+v", label, want)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("%s:\n got %+v\nwant %+v", label, *got, want)
	}
}

// runPreviewDuringSaveInterleave 执行交错用例。failSecond=false 时 p1、p2 都
// 真实成功；failSecond=true 时 p1 成功、p2 在保存点失败（storage_error，回滚）。
func runPreviewDuringSaveInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const (
		acct, asset = "aa", "usdc"
		p1Amount    = int64(60)
		p2Amount    = int64(30)
		p3Amount    = int64(15)
	)
	wantP1 := Record{ID: "p1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: p1Amount, Nonce: 1, FeeBps: 0, Fee: 0, Charged: p1Amount, Seq: 1}
	wantP2 := Record{ID: "p2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: p2Amount, Nonce: 1, FeeBps: 0, Fee: 0, Charged: p2Amount, Seq: 2}

	// 两种结局下真实批次自身完成后的期望。
	wantRealStatuses := []string{StatusSettled, StatusSettled}
	wantRealRecords := []Record{wantP1, wantP2}
	wantBalanceAfterBatch := int64(10) // 100-60-30
	// 预览基于完整状态的期望：两笔都成功 → p1/p2 均 duplicate，p3 余额不足。
	wantPreviewStatuses := []string{StatusDuplicate, StatusDuplicate, StatusFunds}
	if failSecond {
		// p1 成功、p2 保存失败：真实保留 p1、报告 p2 storage_error，余额 40。
		wantRealStatuses = []string{StatusSettled, StatusStorage}
		wantRealRecords = []Record{wantP1}
		wantBalanceAfterBatch = 40 // 100-60
		// 预览基于完整状态：p1 duplicate，p2 视为首次预计成功（序号 2），
		// 模拟余额 40-30=10，随后 p3=15 余额不足。
		wantPreviewStatuses = []string{StatusDuplicate, StatusSettled, StatusFunds}
	}

	path := newTestLedger(t, []BalanceInit{{Account: acct, Asset: asset, Balance: 100}})
	lSubmit := openOrFail(t, path)  // 句柄 A：真实提交
	lPreview := openOrFail(t, path) // 句柄 B：预览（同一进程、同一本账本）

	realBatch := FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", acct, asset, p1Amount),
		intent("p2", acct, asset, p2Amount),
	}}
	// 预览依次放入与原请求完全相同的 p1、p2，以及新编号 p3。
	previewBatch := FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", acct, asset, p1Amount),
		intent("p2", acct, asset, p2Amount),
		intent("p3", acct, asset, p3Amount),
	}}

	gate := &secondPersistGate{
		reached: make(chan struct{}),
		release: make(chan struct{}),
		fail:    failSecond,
	}
	SetFailHook(lSubmit, gate)

	var (
		wg sync.WaitGroup

		realRes     *BatchResult
		realErr     error
		prevStarted = make(chan struct{})
		prevDone    = make(chan struct{}) // 预览返回（无论成功失败）后关闭
		prevRes     *PreviewResult
		prevErr     error
	)

	// 句柄 A：真实顺序提交 p1、p2。p2 的保存会停在 gate，整批持锁不释放。
	wg.Add(1)
	go func() {
		defer wg.Done()
		realRes, realErr = lSubmit.Submit(realBatch)
	}()

	// 等 A 把 p1 完整落账、停在 p2 的保存点（批次仍在保存、锁仍被持有）。
	<-gate.reached

	// 句柄 B：就在该批次仍在保存时发起预览。它应当在同一把账本锁上排队，
	// 直到 A 整批处理完毕，而不是读到 p1 已扣、p2 未定的中途状态。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(prevDone)
		close(prevStarted)
		prevRes, prevErr = lPreview.Preview(previewBatch)
	}()

	<-prevStarted
	// 让出 P，令预览 goroutine 实际运行到争夺账本锁并排队；A 仍停在 p2 保存点。
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
	// 批次仍在保存、锁仍被 A 持有：预览绝不可能在此刻返回。正确实现下预览线程
	// 正阻塞在同一把账本锁上，因此该非阻塞检查不会误报；它直接锁定“预览等待
	// 在途批次整批完成”这一性质（在“两笔都成功”、中途状态恰好等于最终状态时，
	// 仅凭结果无法区分等待与否，本检查补上这重保障）。
	select {
	case <-prevDone:
		t.Fatal("preview returned while the real batch was still saving; it must wait for the whole batch")
	default:
	}
	close(gate.release) // 放行 p2 保存（成功或失败），随后整批结束并释放锁
	wg.Wait()

	SetFailHook(lSubmit, nil) // 用例结束，拆卸故障注入

	if realErr != nil {
		t.Fatalf("real submit: %v", realErr)
	}
	if prevErr != nil {
		t.Fatalf("preview: %v", prevErr)
	}

	// ---- 真实批次逐项结果：与输入顺序一一对应，各自独立处理 ----
	if got, want := statuses(realRes), wantRealStatuses; !reflect.DeepEqual(got, want) {
		t.Fatalf("real statuses=%v want %v", got, want)
	}
	if realRes.Results[0].ID != "p1" || realRes.Results[1].ID != "p2" {
		t.Fatalf("real results out of order: %+v", realRes.Results)
	}
	assertRecord(t, "real p1", realRes.Results[0].Record, wantP1)
	if failSecond {
		// p2 保存失败：报告 storage_error、无结算记录，前项 p1 保留不回滚。
		if r := realRes.Results[1]; r.Status != StatusStorage || r.Record != nil {
			t.Fatalf("failed real p2: %+v", r)
		}
	} else {
		assertRecord(t, "real p2", realRes.Results[1].Record, wantP2)
	}

	// ---- 预览逐项结果：三个输入、同序对应，整份基于同一个完整状态 ----
	if !prevRes.DryRun {
		t.Fatalf("preview must carry dry_run=true: %+v", prevRes)
	}
	if got, want := previewStatuses(prevRes), wantPreviewStatuses; !reflect.DeepEqual(got, want) {
		t.Fatalf("preview statuses=%v want %v", got, want)
	}
	wantIDs := []string{"p1", "p2", "p3"}
	for i, id := range wantIDs {
		if prevRes.Results[i].ID != id {
			t.Fatalf("preview result %d id=%q want %q (results must line up with inputs)", i, prevRes.Results[i].ID, id)
		}
	}
	// p1 两种结局下都是 duplicate，携带各自的“原结算记录”（真实 p1，序号 1）。
	p1Prev := prevRes.Results[0]
	if p1Prev.Status != StatusDuplicate || p1Prev.Reason != reasonDuplicate {
		t.Fatalf("preview p1: %+v", p1Prev)
	}
	assertRecord(t, "preview p1 duplicate record", p1Prev.Record, wantP1)

	if failSecond {
		// 失败的真实 p2 不能被当成已结算：预览里它是首次“预计成功”，
		// 预计成功序号为 2；预览不继承真实 p2 的 storage_error。
		p2Prev := prevRes.Results[1]
		if p2Prev.Status != StatusSettled {
			t.Fatalf("preview p2 must be predicted settled, got %+v", p2Prev)
		}
		assertRecord(t, "preview p2 predicted record", p2Prev.Record, wantP2)
	} else {
		// 两笔都成功：预览里 p2 也是 duplicate，携带真实 p2 的原结算记录（序号 2）。
		p2Prev := prevRes.Results[1]
		if p2Prev.Status != StatusDuplicate || p2Prev.Reason != reasonDuplicate {
			t.Fatalf("preview p2: %+v", p2Prev)
		}
		assertRecord(t, "preview p2 duplicate record", p2Prev.Record, wantP2)
	}
	// p3 两种结局下都是余额不足，且失败项不携带任何记录。
	if r := prevRes.Results[2]; r.Status != StatusFunds || r.Record != nil {
		t.Fatalf("preview p3: %+v", r)
	}
	for i, r := range prevRes.Results {
		if r.Status == StatusStorage {
			t.Fatalf("preview must never report storage_error, item %d: %+v", i, r)
		}
	}

	// ---- 预览不写账本：预览结束后的余额与历史只等于“真实批次自身完成后” ----
	snap := mustQuery(t, lPreview)
	if len(snap.Refunds) != 0 {
		t.Fatalf("unexpected refunds: %+v", snap.Refunds)
	}
	if len(snap.Settlements) != len(wantRealRecords) ||
		!reflect.DeepEqual(snap.Settlements, wantRealRecords) {
		t.Fatalf("history after preview:\n got %+v\nwant %+v (predicted p2/p3 must not be persisted, no seq reserved)",
			snap.Settlements, wantRealRecords)
	}
	if bal, _ := lPreview.Balance(acct, asset); bal != wantBalanceAfterBatch {
		t.Fatalf("balance after preview=%d want %d (preview must not charge)", bal, wantBalanceAfterBatch)
	}

	// 账本文件同样只反映真实批次：关闭全部句柄使注册表项注销，重新从磁盘打开。
	lSubmit.Close()
	lPreview.Close()
	fromDisk := openOrFail(t, path)
	diskSnap := mustQuery(t, fromDisk)
	if !reflect.DeepEqual(diskSnap.Settlements, wantRealRecords) {
		t.Fatalf("ledger file after preview:\n got %+v\nwant %+v", diskSnap.Settlements, wantRealRecords)
	}
	if bal, _ := fromDisk.Balance(acct, asset); bal != wantBalanceAfterBatch {
		t.Fatalf("ledger file balance=%d want %d", bal, wantBalanceAfterBatch)
	}

	// ---- 预览不预留编号/序号：失败的真实 p2 仍可真实提交成功，不变成 duplicate ----
	if failSecond {
		// p2 之前只是“保存失败”，预览里又“预计成功”过；二者都不能占用编号：
		// 现在真实提交 p2 必须 settled（而非 duplicate），成功序号 2，余额 40→10。
		res, err := fromDisk.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
			intent("p2", acct, asset, p2Amount),
		}})
		if err != nil {
			t.Fatalf("resubmit p2: %v", err)
		}
		if r := res.Results[0]; r.Status != StatusSettled {
			t.Fatalf("storage-failed p2 must settle for real on retry, got %+v", r)
		}
		assertRecord(t, "resubmitted p2", res.Results[0].Record, wantP2)
	} else {
		// 两笔都已成功：p2 原样重复提交仍 duplicate、携带原记录、不再扣款。
		res, err := fromDisk.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
			intent("p2", acct, asset, p2Amount),
		}})
		if err != nil {
			t.Fatalf("repeat p2: %v", err)
		}
		if r := res.Results[0]; r.Status != StatusDuplicate {
			t.Fatalf("settled p2 repeat must be duplicate, got %+v", r)
		}
		assertRecord(t, "repeat p2 original record", res.Results[0].Record, wantP2)
	}

	// 此刻两种结局收敛到同一状态：余额 10。预览里“预计余额不足”的 p3 从未预留，
	// 真实提交 p3=15 依旧因 10<15 而余额不足，且不留记录、序号仍停在 2。
	p3, err := fromDisk.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p3", acct, asset, p3Amount),
	}})
	if err != nil {
		t.Fatalf("submit p3: %v", err)
	}
	if r := p3.Results[0]; r.Status != StatusFunds || r.Record != nil {
		t.Fatalf("real p3: %+v", r)
	}
	if bal, _ := fromDisk.Balance(acct, asset); bal != 10 {
		t.Fatalf("balance after resubmits=%d want 10", bal)
	}
	final := mustQuery(t, fromDisk)
	if !reflect.DeepEqual(final.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("final history:\n got %+v\nwant [p1,p2]", final.Settlements)
	}

	// 再关句柄重开，确认磁盘文件与内存一致：p1 金额/扣款总额/序号、p2 全部原样，
	// 没有任何预测成功记录或 p3，余额 10。
	fromDisk.Close()
	reopened := openOrFail(t, path)
	diskFinal := mustQuery(t, reopened)
	if !reflect.DeepEqual(diskFinal.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("reopened ledger history:\n got %+v\nwant [p1,p2]", diskFinal.Settlements)
	}
	if bal, _ := reopened.Balance(acct, asset); bal != 10 {
		t.Fatalf("reopened balance=%d want 10", bal)
	}
}

// TestPreviewWaitsForInFlightBatchBothSettled：两笔真实付款都成功（余额剩 10）。
// 与批次并发发起的预览必须等批次完成：p1、p2 均 duplicate 并携带各自原结算
// 记录，p3=15 因余额 10 而 insufficient_balance。
func TestPreviewWaitsForInFlightBatchBothSettled(t *testing.T) {
	runPreviewDuringSaveInterleave(t, false)
}

// TestPreviewWaitsForInFlightBatchSecondSaveFails：p1 成功、p2 保存失败。
// 真实批次保留 p1、p2 报 storage_error、余额 40；并发预览基于该完整状态：
// p1 duplicate，p2 首次预计成功（序号 2），模拟余额 40→10 后 p3 余额不足；
// 失败的真实 p2 不被当成已结算，预览也不继承 storage_error。
func TestPreviewWaitsForInFlightBatchSecondSaveFails(t *testing.T) {
	runPreviewDuringSaveInterleave(t, true)
}
