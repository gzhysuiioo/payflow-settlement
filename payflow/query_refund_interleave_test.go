package payflow

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 本文件为公开查询（Ledger.Query）补充两个账本句柄交错使用下的回归保障：
// 同一进程中两个句柄指向同一本账本时，一个句柄正在提交有序退款批次（批次
// 仍在逐项保存、账本互斥锁尚未释放），另一个句柄调用公开的 Ledger.Query。
// 查询必须等该退款批次整批处理完毕，再返回“余额 + 付款历史 + 退款历史”
// 共同对应批次结束后完整状态的同一份快照：
//   - 不能提前返回批次前的旧快照；
//   - 不能只读到前半段已经成功的退款（第一笔已落账、第二笔尚未确定的中途状态）；
//   - 不能把刚增加但尚未保存成功的余额当成可见余额；
//   - 正常等待不得报账本关闭（ledger_closed）或存储错误（storage_error）。
//
// 场景固定为：一个账户 aa、两种资产（usdc 发生业务、eth 全程不动），
// usdc 初始余额 10000。先成功支付两笔引用编号不同的付款，两笔都实际扣除了
// 非零手续费：
//
//	p1=1000，100bps：手续费 10，扣款总额 1010；
//	p2=2000， 50bps：手续费 10，扣款总额 2010。
//
// 付款后 usdc 余额为 10000-3020=6980。退款批次按顺序用 r1 退回 p1、
// 用 r2 退回 p2，退款编号与原因均合法；在 r2 的“保存点”用故障注入门把
// 批次停住（此时 r1 已完整落账、锁仍被退款句柄持有），另一句柄就在此刻
// 发起 Query。随后放行 r2 的保存，分别覆盖两种完整结局：
//   - 两笔退款都保存成功：余额回补两笔原付款扣款总额 3020（手续费一并退回），
//     usdc 回到 10000；退款历史为按成功顺序排列的 r1、r2 两条成功记录，
//     每条保留退款编号、原因与原付款编号，金额与手续费来自各自原结算；
//   - r1 成功、r2 保存失败：第一笔成功保留，批次第二项为 storage_error；
//     查询本身正常成功，余额只回补第一笔原付款扣款 1010（usdc=7990），
//     退款历史只有 r1 一条成功记录——失败申请不留退款记录、不虚增余额、
//     不占用退款成功序号（r1 仍为 seq 1）。
//
// 两种结局中两笔原付款历史的内容与顺序都保持不变；查询是只读操作：
// 查询期间账本的变化只能来自正在执行的退款，查询结束后再次读取得到相同的
// 业务状态，查询本身不额外写盘、不产生任何记录。全部行为只使用既有公开
// 接口与测试故障注入，可在本机离线稳定复现。

// runQueryDuringRefundInterleave 执行交错用例。failSecond=false 时 r1、r2
// 都真实保存成功；failSecond=true 时 r1 成功、r2 在保存点失败
//（storage_error，回滚）。
func runQueryDuringRefundInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const (
		acct, asset = "aa", "usdc"
		other       = "eth" // 全程不发生收支的资产，用来锁定余额排序与无关余额不变
		initialUSDC = int64(10000)
		initialETH  = int64(5)
	)

	// 两笔原付款：编号不同，手续费均为非零并实际扣除。
	// p1: 1000 + 100bps*1000(=10) = 1010
	// p2: 2000 +  50bps*2000(=10) = 2010
	wantP1 := Record{
		ID: "p1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 1000, Nonce: 1, FeeBps: 100, Fee: 10, Charged: 1010, Seq: 1,
	}
	wantP2 := Record{
		ID: "p2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 2000, Nonce: 1, FeeBps: 50, Fee: 10, Charged: 2010, Seq: 2,
	}
	wantPayments := []Record{wantP1, wantP2}

	// 退款落账时两笔结算都已存在：after_seq=2。
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "cancel p1",
		Account: acct, Asset: asset, Amount: 1000, Fee: 10, Charged: 1010,
		AfterSeq: 2, Seq: 1,
	}
	wantR2 := RefundRecord{
		ID: "r2", SettlementID: "p2", Reason: "cancel p2",
		Account: acct, Asset: asset, Amount: 2000, Fee: 10, Charged: 2010,
		AfterSeq: 2, Seq: 2,
	}

	// 两种结局的完整账本状态。
	wantRefundStatuses := []string{StatusRefundSuccess, StatusRefundSuccess}
	wantBalanceUSDC := initialUSDC // 10000-3020+1010+2010
	wantRefunds := []RefundRecord{wantR1, wantR2}
	if failSecond {
		// r1 成功、r2 保存失败：只回补第一笔原付款扣款 1010。
		wantRefundStatuses = []string{StatusRefundSuccess, StatusStorage}
		wantBalanceUSDC = 7990 // 10000-3020+1010
		wantRefunds = []RefundRecord{wantR1}
	}
	// 余额始终按账户、资产排序：aa/eth 在 aa/usdc 之前；无关资产 eth 不动。
	wantBalances := []BalanceView{
		{Account: acct, Asset: other, Balance: initialETH},
		{Account: acct, Asset: asset, Balance: wantBalanceUSDC},
	}

	path := newTestLedger(t, []BalanceInit{
		{Account: acct, Asset: asset, Balance: initialUSDC},
		{Account: acct, Asset: other, Balance: initialETH},
	})
	lRefund := openOrFail(t, path) // 句柄 A：提交退款批次
	lQuery := openOrFail(t, path)  // 句柄 B：公开查询（同一进程、同一本账本）

	// 先顺序成功支付 p1、p2。
	if r := mustSettle(t, lRefund, 100, intent("p1", acct, asset, 1000)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}
	if r := mustSettle(t, lRefund, 50, intent("p2", acct, asset, 2000)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}
	if r1 := mustSettle(t, lRefund, 100, intent("p1", acct, asset, 1000)); r1.Status != StatusDuplicate {
		t.Fatalf("setup sanity: p1 retry want duplicate, got %+v", r1)
	}
	if bal, _ := lRefund.Balance(acct, asset); bal != 6980 {
		t.Fatalf("balance after payments=%d want 6980", bal)
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
		queryStarted = make(chan struct{})
		queryDone    = make(chan struct{}) // Query 返回（无论成功失败）后关闭
		snap         *Snapshot
		queryErr     error
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

	// 句柄 B：就在该退款批次仍在保存时调用公开 Query。它应当在同一把账本锁上
	// 排队，直到 A 整批处理完毕，而不是读到 r1 已退、r2 未定的中途状态。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(queryDone)
		close(queryStarted)
		snap, queryErr = lQuery.Query()
	}()

	<-queryStarted
	// 让出 P，令查询 goroutine 实际运行到争夺账本锁并排队；A 仍停在 r2 保存点。
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
	// 退款批次仍在保存、锁仍被 A 持有：查询绝不可能在此刻返回。正确实现下查询
	// goroutine 正阻塞在同一把账本锁上，因此该非阻塞检查不会误报。它直接锁定
	// “查询等待在途退款批次整批完成”这一性质——仅凭结果无法区分等待与否：
	// r2 最终失败时，批次结束状态与“r1 已落账、r2 未定”的中途状态余额恰好
	// 相同，本检查补上这重保障，杜绝查询在失败结局下提前返回中途快照。
	select {
	case <-queryDone:
		t.Fatal("Query returned while the refund batch was still saving; it must wait for the whole batch")
	default:
	}
	close(gate.release) // 放行 r2 保存（成功或失败），随后整批结束并释放锁
	wg.Wait()

	SetFailHook(lRefund, nil) // 用例结束，拆卸故障注入

	if refundErr != nil {
		t.Fatalf("refund batch: %v", refundErr)
	}
	// 查询正常等待并成功：不得因为等待在途批次而报账本关闭或存储错误。
	if queryErr != nil {
		t.Fatalf("Query during in-flight refund batch must succeed after waiting, got kind=%s err=%v",
			KindOf(queryErr), queryErr)
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
		if r2.Status != StatusStorage {
			t.Fatalf("r2 status=%s want storage_error", r2.Status)
		}
		if r2.Record != nil {
			t.Fatalf("failed r2 must not carry a refund record: %+v", r2.Record)
		}
		if r2.SettlementID != "" || r2.Account != "" || r2.Asset != "" || r2.Charged != 0 {
			t.Fatalf("storage_error must not carry success detail: %+v", r2)
		}
		if r2.Reason == "" {
			t.Fatalf("storage_error must carry the failure explanation")
		}
	} else {
		assertRefundResultRecord(t, refundRes.Results[1], StatusRefundSuccess, wantR2)
	}

	// ---- 交错查询返回的快照：余额、付款历史、退款历史共同对应批次结束后的
	// 同一个完整状态，绝不可能是旧快照、只有 r1 的快照或未确认的 r2 ----
	if !reflect.DeepEqual(snap.Balances, wantBalances) {
		t.Fatalf("interleaved query balances:\n got %#v\nwant %#v", snap.Balances, wantBalances)
	}
	if !reflect.DeepEqual(snap.Settlements, wantPayments) {
		t.Fatalf("interleaved query payment history:\n got %#v\nwant %#v", snap.Settlements, wantPayments)
	}
	if !reflect.DeepEqual(snap.Refunds, wantRefunds) {
		t.Fatalf("interleaved query refund history:\n got %#v\nwant %#v", snap.Refunds, wantRefunds)
	}
	// 显式锁定失败结局的关键不变量：失败申请不留记录、不占成功序号、不虚增余额。
	if failSecond {
		if len(snap.Refunds) != 1 || snap.Refunds[0].ID != "r1" || snap.Refunds[0].Seq != 1 {
			t.Fatalf("failed r2 must leave exactly one successful refund at seq 1: %+v", snap.Refunds)
		}
		for _, rf := range snap.Refunds {
			if rf.ID == "r2" || rf.SettlementID == "p2" {
				t.Fatalf("failed refund must not appear in history: %+v", rf)
			}
		}
		if got := snap.Balances[1].Balance; got != 7990 {
			t.Fatalf("balance must only include the first refund (1010): got %d want 7990", got)
		}
	} else {
		// 成功结局：两条成功记录按成功顺序排列，金额与手续费来自各自原结算。
		if snap.Refunds[0].Seq != 1 || snap.Refunds[1].Seq != 2 ||
			snap.Refunds[0].ID != "r1" || snap.Refunds[1].ID != "r2" ||
			snap.Refunds[0].SettlementID != "p1" || snap.Refunds[1].SettlementID != "p2" ||
			snap.Refunds[0].Reason != "cancel p1" || snap.Refunds[1].Reason != "cancel p2" ||
			snap.Refunds[0].Amount != 1000 || snap.Refunds[0].Fee != 10 || snap.Refunds[0].Charged != 1010 ||
			snap.Refunds[1].Amount != 2000 || snap.Refunds[1].Fee != 10 || snap.Refunds[1].Charged != 2010 {
			t.Fatalf("successful refund history must keep id/reason/original payment, amounts and fees: %+v", snap.Refunds)
		}
	}

	// ---- 查询只读且可重复：查询结束后用两个句柄分别再次读取，业务状态相同；
	// 查询本身不改写账本文件、不产生任何记录 ----
	againFromQuery := mustQuery(t, lQuery)
	if !reflect.DeepEqual(againFromQuery, snap) {
		t.Fatalf("re-query via query handle differs from the interleaved snapshot:\n got %+v\nwant %+v",
			againFromQuery, snap)
	}
	againFromRefund := mustQuery(t, lRefund)
	if !reflect.DeepEqual(againFromRefund, snap) {
		t.Fatalf("query via refund handle differs:\n got %+v\nwant %+v", againFromRefund, snap)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	onceMore := mustQuery(t, lQuery)
	if !reflect.DeepEqual(onceMore, snap) {
		t.Fatalf("query must be repeatable with identical business state: %+v", onceMore)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("Query rewrote the ledger file:\nbefore=%s\nafter =%s", before, after)
	}

	// 失败申请的原因不落盘；成功记录（含各自原因）必须在磁盘上。
	if failSecond && bytes.Contains(after, []byte("cancel p2")) {
		t.Fatalf("failed refund request persisted to ledger file:\n%s", after)
	}
	if !bytes.Contains(after, []byte(`"id":"r1"`)) ||
		!bytes.Contains(after, []byte(`"settlement_id":"p1"`)) ||
		!bytes.Contains(after, []byte(`"reason":"cancel p1"`)) {
		t.Fatalf("successful r1 missing from ledger file:\n%s", after)
	}

	// 关闭后重开（新进程语义）：磁盘上的完整账本与交错查询结果逐字段一致，
	// 两种结局都可在本机离线复现。
	if err := lRefund.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lQuery.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openOrFail(t, path)
	snapDisk := mustQuery(t, reopened)
	if !reflect.DeepEqual(snapDisk.Balances, wantBalances) {
		t.Fatalf("reopened balances:\n got %#v\nwant %#v", snapDisk.Balances, wantBalances)
	}
	if !reflect.DeepEqual(snapDisk.Settlements, wantPayments) {
		t.Fatalf("reopened payment history:\n got %#v\nwant %#v", snapDisk.Settlements, wantPayments)
	}
	if !reflect.DeepEqual(snapDisk.Refunds, wantRefunds) {
		t.Fatalf("reopened refund history:\n got %#v\nwant %#v", snapDisk.Refunds, wantRefunds)
	}
}

// TestQueryWaitsForInFlightRefundBatchBothRefunded：两笔退款都保存成功。
// 与退款批次并发发起的公开 Query 必须等整批结束：余额包含两笔原付款扣款
// 总额 3020 的回补（手续费一并退回），usdc 回到 10000、无关资产 eth 不变；
// 退款历史为按成功顺序排列的 r1、r2 两条成功记录；两笔原付款历史保持原样。
func TestQueryWaitsForInFlightRefundBatchBothRefunded(t *testing.T) {
	runQueryDuringRefundInterleave(t, false)
}

// TestQueryWaitsForInFlightRefundBatchSecondSaveFails：r1 成功、r2 保存失败。
// 批次第二项为 storage_error 而并发 Query 正常成功；余额只回补第一笔原付款
// 扣款 1010（usdc=7990），退款历史只有 r1（seq 1）——失败的 r2 不留记录、
// 不虚增余额、不占用成功序号；两笔原付款历史的内容与顺序保持不变。
func TestQueryWaitsForInFlightRefundBatchSecondSaveFails(t *testing.T) {
	runQueryDuringRefundInterleave(t, true)
}
