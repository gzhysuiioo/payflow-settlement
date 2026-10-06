package payflow

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 本文件为“同一批次中某项退款保存账本失败后，继续使用该退款编号”的连续行为
// 提供回归保障。失败申请既不占用退款编号，也不把原付款标成已退款；后续条目
// 可以把同一编号改指向另一笔付款并填写不同原因，然后原样重复得到 duplicate，
// 最后再用新编号退回保存失败时针对的那笔付款。整个过程沿用既有退款规则
// （judgeRefund 判定顺序）与公开接口（Ledger.Refund / Query / Open），
// 存储故障仅通过测试钩子 failNth 确定性注入一次，本机离线即可稳定复现。
//
// 连续操作（单个退款批次，逐项对应输入顺序）：
//  1. r1 退回 p1（成功并落盘）；
//  2. rx 申请退回 p2，该项保存账本时失败一次（storage_error，回滚该项）；
//  3. 紧接复用编号 rx，改指向 p3 并填写不同原因（正常退款，不是 duplicate/
//     conflict，p3 也不是 already_refunded）；
//  4. 原样重复第 3 项（duplicate，携带刚刚成功的退款记录，不再增加余额）；
//  5. 用另一个新编号 r3 退回第 2 项的目标 p2（成功，证明保存失败没有占用
//     p2 的退款资格，也没有占用 rx 编号）。
//
// 批次结束后：余额只增加三笔实际成功退款的总额（重复申请不入账）；退款历史
// 只保留三条成功记录，按成功先后排列、序号 1/2/3 连续，失败与重复都不占序号；
// 复用编号 rx 只对应后来成功的目标 p3 与修改后的原因；三笔原付款的编号、金额、
// 手续费与成功顺序保持原样。关闭重开后磁盘账本与查询结果逐项一致。

func TestRefundStorageFailureIDReusedForAnotherPayment(t *testing.T) {
	const (
		account = "aa-1"
		asset   = "usdc"
		initial int64 = 10000
	)
	path := newTestLedger(t, []BalanceInit{{Account: account, Asset: asset, Balance: initial}})
	l := openOrFail(t, path)

	// 同一账户、同一资产的三笔成功付款：30bps 下手续费分别为 3/6/9（均非零），
	// charged 分别为 1003/2006/3009，彼此不同。
	sb, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		{ID: "p1", Account: account, Paymaster: "pm", Asset: asset, Amount: i64p(1000), Nonce: 1, State: "pending"},
		{ID: "p2", Account: account, Paymaster: "pm", Asset: asset, Amount: i64p(2000), Nonce: 2, State: "pending"},
		{ID: "p3", Account: account, Paymaster: "pm", Asset: asset, Amount: i64p(3000), Nonce: 3, State: "pending"},
	}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := statuses(sb); !reflect.DeepEqual(got, []string{StatusSettled, StatusSettled, StatusSettled}) {
		t.Fatalf("setup payments statuses=%v want all settled", got)
	}
	wantSettlements := []Record{
		{ID: "p1", Account: account, Paymaster: "pm", Asset: asset, Amount: 1000, Nonce: 1, FeeBps: 30, Fee: 3, Charged: 1003, Seq: 1},
		{ID: "p2", Account: account, Paymaster: "pm", Asset: asset, Amount: 2000, Nonce: 2, FeeBps: 30, Fee: 6, Charged: 2006, Seq: 2},
		{ID: "p3", Account: account, Paymaster: "pm", Asset: asset, Amount: 3000, Nonce: 3, FeeBps: 30, Fee: 9, Charged: 3009, Seq: 3},
	}
	if got := mustQuery(t, l).Settlements; !reflect.DeepEqual(got, wantSettlements) {
		t.Fatalf("setup settlements:\n got %+v\nwant %+v", got, wantSettlements)
	}
	// 10000 - (1003+2006+3009) = 3982。
	if bal, _ := l.Balance(account, asset); bal != 3982 {
		t.Fatalf("balance after setup payments=%d want 3982", bal)
	}

	// 三次付款全部落盘后再安装故障：退款批次中第 2 次持久化（第 2 项 rx→p2）
	// 恰好失败一次，此后保存恢复正常；duplicate 项根本不触发持久化。
	hook := &failNth{nth: 2}
	SetFailHook(l, hook)
	t.Cleanup(func() { SetFailHook(l, nil) })

	rb, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", "customer cancelled"),    // 1. 成功退回第一笔
		refundReq("rx", "p2", "billing error"),         // 2. 保存失败一次
		refundReq("rx", "p3", "goodwill adjustment"),   // 3. 复用编号、改目标、改原因
		refundReq("rx", "p3", "goodwill adjustment"),   // 4. 原样重复第 3 项
		refundReq("r3", "p2", "billing error"),         // 5. 新编号退回第二笔
	}})
	if err != nil {
		t.Fatalf("refund batch must not fail as a whole: %v", err)
	}

	// 整批结果按输入顺序逐项对应：只有第 2 项 storage_error，批次不整体报错。
	wantStatuses := []string{
		StatusRefundSuccess, // r1 → p1
		StatusStorage,       // rx → p2 保存失败
		StatusRefundSuccess, // rx 改退 p3
		StatusDuplicate,     // 原样重复
		StatusRefundSuccess, // r3 退 p2
	}
	if got := refundStatuses(rb); !reflect.DeepEqual(got, wantStatuses) {
		t.Fatalf("refund statuses=%v want %v", got, wantStatuses)
	}
	// 实际持久化恰好 4 次：第 1、2、3、5 项各一次；duplicate 不写盘。
	if hook.calls != 4 {
		t.Fatalf("persist calls=%d want 4 (duplicate must not persist)", hook.calls)
	}

	// 三条成功退款记录的期望值（AfterSeq 均为 3：退款发生在三笔付款全部结算之后；
	// 成功序号 1/2/3 连续，失败项与重复项都不占序号）。
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "customer cancelled",
		Account: account, Asset: asset, Amount: 1000, Fee: 3, Charged: 1003,
		AfterSeq: 3, Seq: 1,
	}
	wantRx := RefundRecord{
		ID: "rx", SettlementID: "p3", Reason: "goodwill adjustment",
		Account: account, Asset: asset, Amount: 3000, Fee: 9, Charged: 3009,
		AfterSeq: 3, Seq: 2,
	}
	wantR3 := RefundRecord{
		ID: "r3", SettlementID: "p2", Reason: "billing error",
		Account: account, Asset: asset, Amount: 2000, Fee: 6, Charged: 2006,
		AfterSeq: 3, Seq: 3,
	}

	// 第 1 项：r1 成功，按 p1 的 charged 全额（含手续费）退回原账户、原资产。
	assertRefundResultRecord(t, rb.Results[0], StatusRefundSuccess, wantR1)
	// 第 3 项与第 4 项（duplicate）都指向 p3，记录修改后的原因，按 p3 的
	// charged 全额退款；两者详情逐字一致，且 duplicate 携带“刚刚成功”的记录。
	assertRefundResultRecord(t, rb.Results[2], StatusRefundSuccess, wantRx)
	assertRefundResultRecord(t, rb.Results[3], StatusDuplicate, wantRx)
	if rb.Results[2].Record == rb.Results[3].Record {
		t.Fatal("success and duplicate results share one *RefundRecord allocation")
	}
	// 第 5 项：新编号 r3 成功退回 p2，证明先前保存失败既没占用 r3 之外的
	// 任何资格，也没把 p2 标成已退款。
	assertRefundResultRecord(t, rb.Results[4], StatusRefundSuccess, wantR3)

	// 第 2 项：只返回 storage_error 与失败说明，不携带任何成功退款记录。
	failed := rb.Results[1]
	if failed.ID != "rx" {
		t.Fatalf("failed result id=%q want rx", failed.ID)
	}
	if !strings.Contains(failed.Reason, "injected write failure") {
		t.Fatalf("storage_error reason must explain the save failure: %q", failed.Reason)
	}
	if failed.Record != nil || failed.SettlementID != "" || failed.Account != "" ||
		failed.Asset != "" || failed.Charged != 0 {
		t.Fatalf("storage_error result must carry no success refund detail: %+v", failed)
	}
	// 公开 JSON 格式回归：失败项省略去向字段与 record，不泄漏任何退款详情。
	rawFailed, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"id":"rx"`, `"status":"storage_error"`} {
		if !bytes.Contains(rawFailed, []byte(frag)) {
			t.Fatalf("failed result JSON missing %s:\n%s", frag, rawFailed)
		}
	}
	for _, leaked := range []string{`"record"`, `"settlement_id"`, `"account"`, `"asset"`, `"charged"`, "p2", "p3", "billing error", "goodwill adjustment"} {
		if bytes.Contains(rawFailed, []byte(leaked)) {
			t.Fatalf("storage_error JSON leaked %s:\n%s", leaked, rawFailed)
		}
	}
	// 成功项（第 3 项）的公开 JSON 锁定新目标、修改后的原因与全额退款金额。
	rawRx, err := json.Marshal(rb.Results[2])
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"id":"rx"`, `"status":"refunded"`, `"settlement_id":"p3"`,
		`"reason":"goodwill adjustment"`, `"account":"aa-1"`, `"asset":"usdc"`,
		`"amount":3000`, `"fee":9`, `"charged":3009`, `"after_seq":3`, `"seq":2`,
	} {
		if !bytes.Contains(rawRx, []byte(frag)) {
			t.Fatalf("success result JSON missing %s:\n%s", frag, rawRx)
		}
	}

	// 同编号申请在其后成功后，失败结果仍保持失败，不被后来生成的成功详情覆盖。
	// （失败项与成功项是逐项独立的结果值，详情互不相干。）
	if got := rb.Results[1]; got.Status != StatusStorage || got.Record != nil ||
		!strings.Contains(got.Reason, "injected write failure") {
		t.Fatalf("earlier failed result overwritten by later success with same id: %+v", got)
	}

	// 批次结束后的余额：只增加三笔实际成功退款的总额（1003+3009+2006=6018），
	// 重复申请不额外入账，3982+6018 恰好回到初始余额 10000。
	if bal, _ := l.Balance(account, asset); bal != initial {
		t.Fatalf("balance after refund batch=%d want %d", bal, initial)
	}

	// 查询视角：退款历史只保留成功记录，按成功先后排列、序号连续；
	// 复用编号 rx 只对应后来成功的目标 p3 与修改后的原因，查不到失败痕迹；
	// 三笔原付款的编号、金额、手续费与成功顺序保持原样。
	snap := mustQuery(t, l)
	wantRefunds := []RefundRecord{wantR1, wantRx, wantR3}
	if !reflect.DeepEqual(snap.Refunds, wantRefunds) {
		t.Fatalf("ledger refunds:\n got %+v\nwant %+v", snap.Refunds, wantRefunds)
	}
	var rxHistory []RefundRecord
	for _, r := range snap.Refunds {
		if r.ID == "rx" {
			rxHistory = append(rxHistory, r)
		}
	}
	if len(rxHistory) != 1 || rxHistory[0] != wantRx {
		t.Fatalf("reused id rx must resolve only to the later success: %+v", rxHistory)
	}
	if !reflect.DeepEqual(snap.Settlements, wantSettlements) {
		t.Fatalf("original settlements altered by refunds:\n got %+v\nwant %+v", snap.Settlements, wantSettlements)
	}
	if len(snap.Balances) != 1 || snap.Balances[0] != (BalanceView{Account: account, Asset: asset, Balance: initial}) {
		t.Fatalf("balances=%+v want single %s/%s=%d", snap.Balances, account, asset, initial)
	}

	// 关闭重开：实际保存到磁盘的账本与查询结果一致（故障注入在原子写之前
	// 返回，旧文件完整；后续成功写入带校验和落盘）。
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	l2 := openOrFail(t, path)
	snap2 := mustQuery(t, l2)
	if !reflect.DeepEqual(snap2.Refunds, snap.Refunds) || !reflect.DeepEqual(snap2.Settlements, snap.Settlements) {
		t.Fatalf("reopened ledger differs from last query:\n settlements got %+v\n refunds got %+v",
			snap2.Settlements, snap2.Refunds)
	}
	if bal, _ := l2.Balance(account, asset); bal != initial {
		t.Fatalf("reopened balance=%d want %d", bal, initial)
	}

	// 重开后幂等与“每笔付款最多退一次”的资格仍然正确：
	// 原样重复第 3 项仍是 duplicate 且携带同一条成功记录，不再次入账；
	// 三笔原付款此时都已被退回，任何新编号再申请均为 already_refunded。
	assertRefundResultRecord(t, mustRefundOne(t, l2, "rx", "p3", "goodwill adjustment"), StatusDuplicate, wantRx)
	for _, sid := range []string{"p1", "p2", "p3"} {
		if r := mustRefundOne(t, l2, "r-again-"+sid, sid, "late refund"); r.Status != StatusAlreadyRefunded {
			t.Fatalf("payment %s must stay refunded after reopen, got %+v", sid, r)
		}
	}
	// 复用编号 rx 若改原因/改目标仍按 conflict 处理，不退化成新的成功。
	if r := mustRefundOne(t, l2, "rx", "p3", "changed reason"); r.Status != StatusConflict {
		t.Fatalf("reused id with changed reason want conflict after reopen, got %+v", r)
	}
	if bal, _ := l2.Balance(account, asset); bal != initial {
		t.Fatalf("duplicate/conflict/already_refunded attempts changed balance: %d want %d", bal, initial)
	}
	snap3 := mustQuery(t, l2)
	if !reflect.DeepEqual(snap3.Refunds, wantRefunds) {
		t.Fatalf("post-reopen retries must not add refund records: %+v", snap3.Refunds)
	}
}
