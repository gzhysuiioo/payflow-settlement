package payflow

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 本文件为公开查询（Ledger.Query）补充交错使用下的回归保障：同一进程中两个
// 句柄指向同一本账本时，一个句柄正在提交有序退款批次（批次仍在逐项保存、
// 账本互斥锁尚未释放），另一个句柄调用公开的 Ledger.Query。查询必须等该退款
// 批次整批处理完毕，再依据“完整余额 + 完整付款历史 + 完整退款历史”返回同一个
// 完整账本状态：
//   - 不能提前返回批次前的旧快照；
//   - 不能只读到“第一笔退款已保存、第二笔尚未确定”的中途快照（既不能漏读
//     尚未落定的第二笔，也不能把刚增加但尚未保存成功的余额当成可见余额）；
//   - 正常等待本身不得报 ledger_closed 或 storage_error，查询必须正常成功；
//   - 前项成功、后项保存失败是允许的完整状态：失败项已回滚，余额只回补前项
//     原付款的扣款总额（含手续费），退款历史只含前项成功记录，失败申请不留
//     退款记录、不虚增余额、不占用退款成功序号；
//   - 查询只读：不改写账本文件、不产生任何记录；查询结束后再次读取得到相同
//     的业务状态。
//
// 场景固定为：一个账户 aa、一种资产 usdc，初始余额 2000，费率 100 基点。
// 先成功支付 p1=1000（手续费 10、扣款 1010）与 p2=500（手续费 5、扣款 505），
// 两笔原付款都实际扣除了非零手续费，当前余额 485。退款批次依次用 r1 退回 p1、
// 用 r2 退回 p2（退款编号、原付款编号与原因均合法且各自不同）；在 r2 的
// “保存点”用故障注入门把批次停住（此时 r1 已完整落账、锁仍被退款句柄持有），
// 另一句柄就在此刻发起查询。随后放行 r2 的保存，分别覆盖“两笔退款都成功”和
// “r1 成功、r2 保存失败”两种完整结局。
//
// 查询返回格式（余额按账户、资产排序，付款与退款分别按成功顺序排列）与公开
// 接口均不在本文件变更。

// runQueryDuringRefundInterleave 执行交错用例。failSecond=false 时 r1、r2 都
// 真实成功；failSecond=true 时 r1 成功、r2 在保存点失败（storage_error，回滚）。
func runQueryDuringRefundInterleave(t *testing.T, failSecond bool) {
	t.Helper()
	const acct, asset = "aa", "usdc"

	// 两笔原付款都含实际扣除的手续费（100 基点：1000→费 10 扣 1010，500→费 5 扣 505）。
	wantP1 := Record{
		ID: "p1", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 1000, Nonce: 1, FeeBps: 100, Fee: 10, Charged: 1010, Seq: 1,
	}
	wantP2 := Record{
		ID: "p2", Account: acct, Paymaster: "pm", Asset: asset,
		Amount: 500, Nonce: 1, FeeBps: 100, Fee: 5, Charged: 505, Seq: 2,
	}
	// 退款落账时两笔结算都已存在：after_seq=2；金额与手续费来自各自原结算。
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "customer cancelled",
		Account: acct, Asset: asset, Amount: 1000, Fee: 10, Charged: 1010,
		AfterSeq: 2, Seq: 1,
	}
	wantR2 := RefundRecord{
		ID: "r2", SettlementID: "p2", Reason: "duplicate charge",
		Account: acct, Asset: asset, Amount: 500, Fee: 5, Charged: 505,
		AfterSeq: 2, Seq: 2,
	}

	// 两种结局下退款批次自身与查询结果的期望。
	wantRefundStatuses := []string{StatusRefundSuccess, StatusRefundSuccess}
	wantBalance := int64(2000) // 2000-1515+1010+505：两笔扣款总额（含手续费）全部回补
	wantRefunds := []RefundRecord{wantR1, wantR2}
	if failSecond {
		// r1 成功、r2 保存失败：批次保留 r1、r2 报 storage_error；
		// 余额只回补第一笔原付款的扣款总额 485+1010=1495，退款历史只有 r1。
		wantRefundStatuses = []string{StatusRefundSuccess, StatusStorage}
		wantBalance = 1495
		wantRefunds = []RefundRecord{wantR1}
	}
	wantSettlements := []Record{wantP1, wantP2} // 两种结局下原付款历史内容与顺序都不变
	wantBalances := []BalanceView{{Account: acct, Asset: asset, Balance: wantBalance}}

	path := newTestLedger(t, []BalanceInit{{Account: acct, Asset: asset, Balance: 2000}})
	lRefund := openOrFail(t, path) // 句柄 A：提交退款批次
	lQuery := openOrFail(t, path)  // 句柄 B：公开查询（同一进程、同一本账本）

	// 先顺序成功支付 p1、p2（100 基点：扣 1010、505），余额 485，无退款。
	if r := mustSettle(t, lRefund, 100, intent("p1", acct, asset, 1000)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}
	if r := mustSettle(t, lRefund, 100, intent("p2", acct, asset, 500)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}
	if bal, _ := lRefund.Balance(acct, asset); bal != 485 {
		t.Fatalf("balance after payments=%d want 485", bal)
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
		queryDone    = make(chan struct{}) // 查询返回（无论成功失败）后关闭
		querySnap    *Snapshot
		queryErr     error
	)

	// 句柄 A：顺序提交退款 r1、r2。r2 的保存会停在 gate，整批持锁不释放。
	wg.Add(1)
	go func() {
		defer wg.Done()
		refundRes, refundErr = lRefund.Refund(RefundBatch{Refunds: []RefundRequest{
			refundReq("r1", "p1", "customer cancelled"),
			refundReq("r2", "p2", "duplicate charge"),
		}})
	}()

	// 等 A 把 r1 完整落账、停在 r2 的保存点（批次仍在保存、锁仍被持有）。
	<-gate.reached

	// 句柄 B：就在该退款批次仍在保存时发起公开查询。它应当在同一把账本锁上
	// 排队，直到 A 整批处理完毕，而不是读到 r1 已退、r2 未定的中途状态，也不能
	// 把 r2 刚增加但尚未保存成功的余额当成可见余额。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(queryDone)
		close(queryStarted)
		querySnap, queryErr = lQuery.Query()
	}()

	<-queryStarted
	// 让出 P，令查询 goroutine 实际运行到争夺账本锁并排队；A 仍停在 r2 保存点。
	for i := 0; i < 4; i++ {
		runtime.Gosched()
	}
	// 退款批次仍在保存、锁仍被 A 持有：查询绝不可能在此刻返回——既不能返回批次
	// 前的旧快照（余额 485、无退款），也不能返回只有 r1 的快照，更不能返回携带
	// 尚未确认的 r2 的快照。正确实现下查询线程正阻塞在同一把账本锁上，因此该
	// 非阻塞检查不会误报；它直接锁定“查询等待在途退款批次整批完成”这一性质
	//（两笔都成功时，中途状态与最终状态可能恰好可被区分，但仅凭最终结果无法
	// 证明查询确实等过，本检查补上这重保障）。
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
		r := refundRes.Results[1]
		if r.Status != StatusStorage || r.Record != nil {
			t.Fatalf("failed refund r2: %+v", r)
		}
		if r.SettlementID != "" || r.Account != "" || r.Asset != "" || r.Charged != 0 {
			t.Fatalf("storage_error must not carry success detail: %+v", r)
		}
	} else {
		assertRefundResultRecord(t, refundRes.Results[1], StatusRefundSuccess, wantR2)
	}

	// ---- 查询结果：等待本身不得报错（不能是 ledger_closed / storage_error），
	// 余额、付款历史与退款历史共同对应退款批次处理结束后的同一个完整状态 ----
	if queryErr != nil {
		t.Fatalf("Query must succeed after waiting for the refund batch, got kind=%s err=%v",
			KindOf(queryErr), queryErr)
	}
	if !reflect.DeepEqual(querySnap.Balances, wantBalances) {
		t.Fatalf("query balances:\n got %+v\nwant %+v", querySnap.Balances, wantBalances)
	}
	if !reflect.DeepEqual(querySnap.Settlements, wantSettlements) {
		t.Fatalf("query settlements:\n got %+v\nwant %+v", querySnap.Settlements, wantSettlements)
	}
	if !reflect.DeepEqual(querySnap.Refunds, wantRefunds) {
		t.Fatalf("query refunds:\n got %+v\nwant %+v", querySnap.Refunds, wantRefunds)
	}
	// 余额按账户、资产排序；付款与退款分别按成功顺序（成功序号连续）展示。
	if !sortBalancesIsSorted(querySnap.Balances) {
		t.Fatalf("balances not sorted by account/asset: %+v", querySnap.Balances)
	}
	for i, s := range querySnap.Settlements {
		if s.Seq != int64(i+1) {
			t.Fatalf("settlements not in success order: %+v", querySnap.Settlements)
		}
	}
	for i, r := range querySnap.Refunds {
		if r.Seq != int64(i+1) {
			t.Fatalf("refunds not in success order: %+v", querySnap.Refunds)
		}
	}
	if failSecond {
		// 失败申请绝不能以任何形式出现在退款历史中。
		for _, r := range querySnap.Refunds {
			if r.ID == "r2" || r.SettlementID == "p2" {
				t.Fatalf("failed refund r2 leaked into query history: %+v", querySnap.Refunds)
			}
		}
	}

	// ---- 查询只读且可重复：查询期间账本的变化只能来自在途退款；再次读取得到
	// 相同业务状态，查询本身不写盘、不产生记录 ----
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []*Ledger{lQuery, lRefund, lQuery} {
		again := mustQuery(t, h)
		if !reflect.DeepEqual(again, querySnap) {
			t.Fatalf("repeat Query disagrees with the interleave Query:\n first %+v\nagain %+v",
				querySnap, again)
		}
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("ledger file rewritten by Query:\nbefore=%s\nafter =%s", beforeBytes, afterBytes)
	}

	// ---- 关闭全部句柄后从磁盘重新打开：查询看到的完整状态就是实际持久化的状态，
	// r2 成功时两笔退款都在盘上，r2 失败时盘上也只有 r1（回滚不留痕） ----
	lRefund.Close()
	lQuery.Close()
	fromDisk := openOrFail(t, path)
	diskSnap := mustQuery(t, fromDisk)
	if !reflect.DeepEqual(diskSnap.Balances, wantBalances) {
		t.Fatalf("disk balances:\n got %+v\nwant %+v", diskSnap.Balances, wantBalances)
	}
	if !reflect.DeepEqual(diskSnap.Settlements, wantSettlements) {
		t.Fatalf("disk settlements:\n got %+v\nwant %+v", diskSnap.Settlements, wantSettlements)
	}
	if !reflect.DeepEqual(diskSnap.Refunds, wantRefunds) {
		t.Fatalf("disk refunds:\n got %+v\nwant %+v", diskSnap.Refunds, wantRefunds)
	}

	if failSecond {
		// 失败申请不占用退款成功序号：用新编号 r3 退回尚未退款的 p2，
		// 必须首次成功且占序号 2（不是 3），余额随之回到 2000；退款历史
		// 为 [r1(seq1), r3(seq2)]，仍然找不到失败的 r2。
		r3 := RefundRecord{
			ID: "r3", SettlementID: "p2", Reason: "retry p2 after save failure",
			Account: acct, Asset: asset, Amount: 500, Fee: 5, Charged: 505,
			AfterSeq: 2, Seq: 2,
		}
		got := mustRefundOne(t, fromDisk, r3.ID, r3.SettlementID, r3.Reason)
		assertRefundResultRecord(t, got, StatusRefundSuccess, r3)
		final := mustQuery(t, fromDisk)
		if !reflect.DeepEqual(final.Refunds, []RefundRecord{wantR1, r3}) {
			t.Fatalf("refunds after r3:\n got %+v\nwant [r1 r3]", final.Refunds)
		}
		if !reflect.DeepEqual(final.Settlements, wantSettlements) {
			t.Fatalf("settlements altered by r3:\n got %+v", final.Settlements)
		}
		if !reflect.DeepEqual(final.Balances, []BalanceView{{Account: acct, Asset: asset, Balance: 2000}}) {
			t.Fatalf("balance after r3:\n got %+v\nwant aa/usdc 2000", final.Balances)
		}
	} else {
		// 两笔都已成功：再用新编号申请退 p1/p2 只能得到 already_refunded，
		// 不增加余额、不新增退款记录。
		for _, req := range []RefundRequest{
			refundReq("rx1", "p1", "late refund"),
			refundReq("rx2", "p2", "late refund"),
		} {
			r := mustRefundOne(t, fromDisk, req.ID, req.SettlementID, req.Reason)
			if r.Status != StatusAlreadyRefunded || r.Record != nil {
				t.Fatalf("%s must be already_refunded with no record: %+v", req.SettlementID, r)
			}
		}
		final := mustQuery(t, fromDisk)
		if !reflect.DeepEqual(final, diskSnap) {
			t.Fatalf("state changed after already_refunded attempts:\n got %+v\nwant %+v", final, diskSnap)
		}
	}
}

// sortBalancesIsSorted 报告余额视图是否按账户、资产升序排列。
func sortBalancesIsSorted(bals []BalanceView) bool {
	for i := 1; i < len(bals); i++ {
		if bals[i-1].Account > bals[i].Account {
			return false
		}
		if bals[i-1].Account == bals[i].Account && bals[i-1].Asset > bals[i].Asset {
			return false
		}
	}
	return true
}

// TestQueryWaitsForInFlightRefundBatchBothRefunded：两笔退款都成功。与退款批次
// 并发发起的公开查询必须等整批结束：余额 2000（两笔原付款的扣款总额连同手续费
// 全部回补），退款历史为按成功顺序排列的 r1、r2 两条成功记录，每条保留退款
// 编号、原因与原付款编号，金额与手续费来自各自原结算；两笔原付款历史不变。
func TestQueryWaitsForInFlightRefundBatchBothRefunded(t *testing.T) {
	runQueryDuringRefundInterleave(t, false)
}

// TestQueryWaitsForInFlightRefundBatchSecondSaveFails：r1 成功、r2 保存失败。
// 退款批次第二项为 storage_error 而并发查询本身正常成功；查询基于该完整状态：
// 余额只增加第一笔原付款的扣款总额（485+1010=1495），退款历史只含 r1，两笔
// 原付款历史不变；失败的 r2 不留退款记录、不虚增余额、不占用退款成功序号。
func TestQueryWaitsForInFlightRefundBatchSecondSaveFails(t *testing.T) {
	runQueryDuringRefundInterleave(t, true)
}
