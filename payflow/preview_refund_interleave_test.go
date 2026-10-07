package payflow

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 本文件为“扣款前预览”（submit --dry-run / Ledger.Preview）补充与在途退款批次
// 交错使用下的回归保障：同一进程中两个句柄指向同一本账本时，一个句柄正在处理
// 有序的全额退款批次（批次仍在逐项保存、账本互斥锁尚未释放），另一个句柄发起
// 付款预览。预览必须等该退款批次整批处理完毕，再依据“完整余额 + 完整成功历史”
// 在副本上判断整份预览：
//   - 第一笔退款已保存、第二笔还未确定成败时，预览不能提前返回；
//   - 不能把尚未保存成功的回补余额用于预计付款（不能读到批次中途的余额）；
//   - 不能把尚在保存的退款当成退款历史（不能读到批次中途的历史）；
//   - 第一笔退款成功、第二笔保存失败是允许的完整状态：预览基于“只回补了第一笔”
//     的余额判断，且预览本身正常返回，不把退款的 storage_error 复制成自己的
//     逐项状态；
//   - 预览始终带 dry_run，不扣余额、不落盘、不预留任何编号与成功序号。
//
// 场景固定为：一个账户 aa、一种资产 usdc，初始余额 200，费率 1000 基点。
// 先成功支付 p1=70（手续费 7、扣款总额 77、序号 1）与 p2=90（手续费 9、
// 扣款总额 99、序号 2），付款后余额为 200-77-99=24。随后退款批次按顺序
// 用 r1 退回 p1、用 r2 退回 p2；在 r2 的“保存点”用故障注入门把批次停住
// （此时 r1 已完整落账、锁仍被退款句柄持有），另一句柄就在此刻以同样的
// 1000 基点费率发起预览，依次放入两个新编号的付款 n1=150、n2=20。随后
// 放行 r2 的保存，分别覆盖两种完整结局：
//   - 两笔退款都保存成功：余额回补 77+99 回到 200；预览中 n1（扣款总额
//     165）与 n2（扣款总额 22）都预计成功，预计序号 3、4，n2 使用 n1
//     预计扣款后的余额 35；
//   - r1 成功、r2 保存失败：批次第二项为 storage_error，第一笔成功保留，
//     余额只回补 77（为 101），退款历史只有 r1；预览首项 n1 扣款总额 165
//     超过 101，为 insufficient_balance 且不携带成功记录，第二项 n2 仍可
//     预计成功（扣款总额 22、预计序号 3——失败项不消耗序号）。
//
// 两种结局下预览都正常返回并保留 dry_run:true，逐项对应输入顺序；预览结束后
// 账本余额、付款历史和退款历史只反映真实发生的付款与退款，原付款记录及序号
// 保持原样，两笔预计付款不进入历史、不预留编号与序号，账本文件不因预览额外
// 改写。全部行为只使用既有公开接口与测试故障注入，可在本机离线稳定复现。

// runPreviewDuringRefundInterleave 执行交错用例。failSecond=false 时 r1、r2
// 都真实保存成功；failSecond=true 时 r1 成功、r2 在保存点失败
//（storage_error，回滚）。
func runPreviewDuringRefundInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const (
		acct, asset = "aa", "usdc"
		feeBps      = 1000
	)

	// 两笔原付款：1000 基点费率，手续费与扣款总额分别为 7/77 与 9/99。
	wantP1 := Record{
		ID: "p1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 70, Nonce: 1, FeeBps: feeBps, Fee: 7, Charged: 77, Seq: 1,
	}
	wantP2 := Record{
		ID: "p2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 90, Nonce: 1, FeeBps: feeBps, Fee: 9, Charged: 99, Seq: 2,
	}
	wantPayments := []Record{wantP1, wantP2}

	// 退款落账时两笔结算都已存在：after_seq=2。
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "cancel p1",
		Account: acct, Asset: asset, Amount: 70, Fee: 7, Charged: 77,
		AfterSeq: 2, Seq: 1,
	}
	wantR2 := RefundRecord{
		ID: "r2", SettlementID: "p2", Reason: "cancel p2",
		Account: acct, Asset: asset, Amount: 90, Fee: 9, Charged: 99,
		AfterSeq: 2, Seq: 2,
	}

	// 预览的两笔新付款：n1=150（手续费 15、扣款总额 165）、n2=20（手续费 2、
	// 扣款总额 22）。预计成功时的记录按预计序号连续递增。
	wantN1 := Record{
		ID: "n1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 150, Nonce: 1, FeeBps: feeBps, Fee: 15, Charged: 165, Seq: 3,
	}
	wantN2 := Record{
		ID: "n2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 20, Nonce: 1, FeeBps: feeBps, Fee: 2, Charged: 22, Seq: 4,
	}

	// 两种结局的完整账本状态与预览期望。
	wantRefundStatuses := []string{StatusRefundSuccess, StatusRefundSuccess}
	wantBalance := int64(200) // 200-77-99+77+99
	wantRefunds := []RefundRecord{wantR1, wantR2}
	wantPreviewStatuses := []string{StatusSettled, StatusSettled}
	if failSecond {
		// r1 成功、r2 保存失败：只回补第一笔原付款扣款 77，退款历史只有 r1。
		wantRefundStatuses = []string{StatusRefundSuccess, StatusStorage}
		wantBalance = 101 // 200-77-99+77
		wantRefunds = []RefundRecord{wantR1}
		// n1 扣款总额 165 超过 101：余额不足、不携带记录、不消耗预计序号；
		// n2 仍预计成功，预计序号为 3。
		wantPreviewStatuses = []string{StatusFunds, StatusSettled}
		wantN2.Seq = 3
	}

	path := newTestLedger(t, []BalanceInit{{Account: acct, Asset: asset, Balance: 200}})
	lRefund := openOrFail(t, path)  // 句柄 A：提交退款批次
	lPreview := openOrFail(t, path) // 句柄 B：付款预览（同一进程、同一本账本）

	// 先按 1000 基点顺序成功支付 p1=70、p2=90。
	if r := mustSettle(t, lRefund, feeBps, intent("p1", acct, asset, 70)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}
	if r := mustSettle(t, lRefund, feeBps, intent("p2", acct, asset, 90)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}
	if bal, _ := lRefund.Balance(acct, asset); bal != 24 {
		t.Fatalf("balance after payments=%d want 24", bal)
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

		refundRes   *RefundBatchResult
		refundErr   error
		prevStarted = make(chan struct{})
		prevDone    = make(chan struct{}) // 预览返回（无论成功失败）后关闭
		prevRes     *PreviewResult
		prevErr     error
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

	// 句柄 B：就在该退款批次仍在保存时，以同样的 1000 基点费率发起预览，
	// 依次预览两个新编号的付款 n1=150、n2=20。预览应当在同一把账本锁上排队，
	// 直到 A 整批处理完毕，而不是读到 r1 已退、r2 未定的中途状态。
	previewBatch := FeeBatch{FeeBps: feeBps, Intents: []PaymentIntent{
		intent("n1", acct, asset, 150),
		intent("n2", acct, asset, 20),
	}}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(prevDone)
		close(prevStarted)
		prevRes, prevErr = lPreview.Preview(previewBatch)
	}()

	<-prevStarted
	// 让出 P，令预览 goroutine 实际运行到争夺账本锁并排队；A 仍停在 r2 保存点。
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
	// 退款批次仍在保存、锁仍被 A 持有：预览绝不可能在此刻返回。正确实现下预览
	// goroutine 正阻塞在同一把账本锁上，因此该非阻塞检查不会误报。它直接锁定
	// “预览等待在途退款批次整批完成”这一性质——仅凭结果无法区分等待与否：
	// r2 最终失败时，若预览读到的是“r2 回补已生效但未保存”的中途余额 200，
	// 会错误放行 n1；本检查补上这重保障，杜绝预览提前返回。
	select {
	case <-prevDone:
		t.Fatal("preview returned while the refund batch was still saving; it must wait for the whole batch")
	default:
	}
	close(gate.release) // 放行 r2 保存（成功或失败），随后整批结束并释放锁
	wg.Wait()

	SetFailHook(lRefund, nil) // 用例结束，拆卸故障注入

	if refundErr != nil {
		t.Fatalf("refund batch: %v", refundErr)
	}
	// 预览正常等待并成功返回：不得因为等待在途批次而报账本关闭或存储错误，
	// 也不把退款的保存错误复制成预览自身的错误。
	if prevErr != nil {
		t.Fatalf("preview during in-flight refund batch must succeed after waiting, got kind=%s err=%v",
			KindOf(prevErr), prevErr)
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
		// r2 保存失败：storage_error、无退款记录、无成功详情；前项 r1 保留不回滚。
		r2 := refundRes.Results[1]
		if r2.Status != StatusStorage || r2.Record != nil {
			t.Fatalf("failed r2: %+v", r2)
		}
		if r2.SettlementID != "" || r2.Account != "" || r2.Asset != "" || r2.Charged != 0 {
			t.Fatalf("storage_error must not carry success detail: %+v", r2)
		}
	} else {
		assertRefundResultRecord(t, refundRes.Results[1], StatusRefundSuccess, wantR2)
	}

	// ---- 预览逐项结果：两个输入、同序对应，整份基于退款批次结束后的完整状态 ----
	if !prevRes.DryRun {
		t.Fatalf("preview must carry dry_run=true: %+v", prevRes)
	}
	if got := previewStatuses(prevRes); !reflect.DeepEqual(got, wantPreviewStatuses) {
		t.Fatalf("preview statuses=%v want %v", got, wantPreviewStatuses)
	}
	if prevRes.Results[0].ID != "n1" || prevRes.Results[1].ID != "n2" {
		t.Fatalf("preview results out of order: %+v", prevRes.Results)
	}
	// 预览只预测业务结果：任何一项都不得出现 storage_error。
	for i, r := range prevRes.Results {
		if r.Status == StatusStorage {
			t.Fatalf("preview must never report storage_error, item %d: %+v", i, r)
		}
	}
	if failSecond {
		// n1 余额不足：不携带成功记录、不消耗预计序号。
		if r := prevRes.Results[0]; r.Status != StatusFunds || r.Record != nil {
			t.Fatalf("preview n1: %+v", r)
		}
		// n2 仍预计成功：扣款总额 22、预计序号 3。
		assertRecord(t, "preview n2 predicted record", prevRes.Results[1].Record, wantN2)
	} else {
		// 两笔退款都成功：余额 200，n1、n2 都预计成功，序号 3、4，
		// n2 使用 n1 预计扣款后的余额（200-165=35 >= 22）。
		assertRecord(t, "preview n1 predicted record", prevRes.Results[0].Record, wantN1)
		assertRecord(t, "preview n2 predicted record", prevRes.Results[1].Record, wantN2)
	}

	// ---- 预览不写账本：预览结束后的余额与历史只等于“退款批次自身完成后” ----
	snap := mustQuery(t, lPreview)
	if bal, _ := lPreview.Balance(acct, asset); bal != wantBalance {
		t.Fatalf("balance after preview=%d want %d (preview must not move funds)", bal, wantBalance)
	}
	if !reflect.DeepEqual(snap.Settlements, wantPayments) {
		t.Fatalf("payment history after preview:\n got %+v\nwant %+v (original payments and seqs must stay intact)",
			snap.Settlements, wantPayments)
	}
	if !reflect.DeepEqual(snap.Refunds, wantRefunds) {
		t.Fatalf("refund history after preview:\n got %+v\nwant %+v", snap.Refunds, wantRefunds)
	}
	for _, rec := range snap.Settlements {
		if rec.ID == "n1" || rec.ID == "n2" {
			t.Fatalf("predicted payment must not enter history: %+v", rec)
		}
	}

	// 账本文件不因预览额外改写：退款批次落盘后的字节在再次预览前后保持一致，
	// 且相同批次在账本未变化时重复预览结果一致。
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := lPreview.Preview(previewBatch)
	if err != nil {
		t.Fatalf("repeat preview: %v", err)
	}
	if !reflect.DeepEqual(again, prevRes) {
		t.Fatalf("repeat preview differs:\n got %+v\nwant %+v", again, prevRes)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("preview rewrote the ledger file:\nbefore=%s\nafter =%s", before, after)
	}

	// 关闭全部句柄使注册表项注销，重新从磁盘打开：磁盘上的完整账本与内存一致，
	// 只反映真实发生的付款与退款。
	if err := lRefund.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lPreview.Close(); err != nil {
		t.Fatal(err)
	}
	fromDisk := openOrFail(t, path)
	diskSnap := mustQuery(t, fromDisk)
	if bal, _ := fromDisk.Balance(acct, asset); bal != wantBalance {
		t.Fatalf("ledger file balance=%d want %d", bal, wantBalance)
	}
	if !reflect.DeepEqual(diskSnap.Settlements, wantPayments) {
		t.Fatalf("ledger file payment history:\n got %+v\nwant %+v", diskSnap.Settlements, wantPayments)
	}
	if !reflect.DeepEqual(diskSnap.Refunds, wantRefunds) {
		t.Fatalf("ledger file refund history:\n got %+v\nwant %+v", diskSnap.Refunds, wantRefunds)
	}

	// ---- 预览不预留编号/序号：预览里“预计成功”过的 n1、n2 真实提交时仍按
	// 首次提交处理，成功序号接在真实历史之后，与预览时的预计序号一致 ----
	if failSecond {
		// 余额 101：预览里余额不足的 n1=150（扣款总额 165）真实提交仍余额不足，
		// 不留记录、不占序号；n2=20 真实提交成功，序号 3（预览未预留）。
		n1, err := fromDisk.Submit(FeeBatch{FeeBps: feeBps, Intents: []PaymentIntent{
			intent("n1", acct, asset, 150),
		}})
		if err != nil {
			t.Fatalf("submit n1: %v", err)
		}
		if r := n1.Results[0]; r.Status != StatusFunds || r.Record != nil {
			t.Fatalf("real n1: %+v", r)
		}
		n2, err := fromDisk.Submit(FeeBatch{FeeBps: feeBps, Intents: []PaymentIntent{
			intent("n2", acct, asset, 20),
		}})
		if err != nil {
			t.Fatalf("submit n2: %v", err)
		}
		if r := n2.Results[0]; r.Status != StatusSettled {
			t.Fatalf("previewed n2 must settle for real, got %+v", r)
		}
		assertRecord(t, "real n2", n2.Results[0].Record, wantN2)
		if bal, _ := fromDisk.Balance(acct, asset); bal != 79 {
			t.Fatalf("balance after real n2=%d want 79", bal)
		}
	} else {
		// 余额 200：n1、n2 真实提交都成功，序号 3、4 与预览预计一致。
		real, err := fromDisk.Submit(FeeBatch{FeeBps: feeBps, Intents: []PaymentIntent{
			intent("n1", acct, asset, 150),
			intent("n2", acct, asset, 20),
		}})
		if err != nil {
			t.Fatalf("submit n1+n2: %v", err)
		}
		if got := statuses(real); !reflect.DeepEqual(got, []string{StatusSettled, StatusSettled}) {
			t.Fatalf("real n1/n2 statuses=%v want [settled settled]", got)
		}
		assertRecord(t, "real n1", real.Results[0].Record, wantN1)
		assertRecord(t, "real n2", real.Results[1].Record, wantN2)
		if bal, _ := fromDisk.Balance(acct, asset); bal != 13 {
			t.Fatalf("balance after real n1+n2=%d want 13", bal)
		}
	}
}

// TestPreviewWaitsForInFlightRefundBatchBothRefunded：两笔退款都保存成功，
// 真实余额恢复为 200。与退款批次并发发起的预览必须等整批结束：n1=150、
// n2=20 都预计 settled，扣款总额依次 165、22，预计序号 3、4，n2 使用 n1
// 预计扣款后的余额 35。
func TestPreviewWaitsForInFlightRefundBatchBothRefunded(t *testing.T) {
	runPreviewDuringRefundInterleave(t, false)
}

// TestPreviewWaitsForInFlightRefundBatchSecondSaveFails：r1 成功、r2 保存失败。
// 退款批次第二项为 storage_error，第一笔成功保留，真实余额为 101，退款历史
// 只有 r1；并发预览基于该完整状态正常返回：首项 n1 为 insufficient_balance
// 且不携带成功记录，第二项 n2 仍预计成功（扣款总额 22、预计序号 3），预览
// 不把退款的保存错误复制成自己的逐项状态。
func TestPreviewWaitsForInFlightRefundBatchSecondSaveFails(t *testing.T) {
	runPreviewDuringRefundInterleave(t, true)
}
