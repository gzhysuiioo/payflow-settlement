package payflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// 本文件回归查询结果（*Snapshot）与真实账本之间的独立性：
//
//  1. 后续业务改变账本时，先前取得的查询结果仍保留当时的余额、记录数量与
//     记录内容，不跟随后续付款或退款变化（时间维度的不可变快照）。
//  2. 调用者修改自己拿到的查询结果（余额、付款记录、退款记录）只能影响
//     自己那份数据；再次查询必须仍得到真实业务产生的数据，且修改一份
//     结果不改变此前独立取得的另一份结果（调用者间的写隔离）。
//  3. 对返回数据的改动不改变后续业务判定：后续付款仍按真实余额结算，
//     已退款的原付款仍保持已退款判断。
//  4. 多账户、多资产并存时，未发生实际收支的组合保持原余额，新查询继续
//     按账户、资产排序，付款与退款记录各按成功先后排列。
//  5. 只查询或修改返回数据时，账本文件内容及成功记录数量保持不变。

// ---- 时间维度：先前快照不跟随账本变化 ----

// 覆盖有手续费的付款与全额退款发生前后的查询结果：
//   - 空历史快照：无余额变动/无付款/无退款；
//   - 付款成功后新查询：金额与手续费一起扣除后的余额 + 对应结算记录；
//   - 退款成功后新查询：退回原扣款总额后的余额 + 关联原付款的退款记录，
//     原结算仍然保留；
//   - 先前取得的结果继续显示先前的余额、记录数量及记录内容，不跟随变化。
func TestQuerySnapshotFreezesAcrossFeePaymentAndFullRefund(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	// s0：尚无任何付款或退款时取得的空历史。
	s0 := mustQuery(t, l)
	if len(s0.Balances) != 1 || s0.Balances[0].Balance != 2000 {
		t.Fatalf("s0 balances=%+v want single 2000 row", s0.Balances)
	}
	if len(s0.Settlements) != 0 || len(s0.Refunds) != 0 {
		t.Fatalf("s0 history must be empty: settlements=%+v refunds=%+v", s0.Settlements, s0.Refunds)
	}
	if s0.Refunds == nil || s0.Settlements == nil {
		t.Fatalf("empty history must be empty (non-nil) slices, got settlements=%v refunds=%v",
			s0.Settlements, s0.Refunds)
	}

	// 带手续费付款：1500 × 30bp = 4，扣款总额 1504，余额 2000-1504=496。
	paid := PaymentIntent{ID: "p1", Account: "aa", Paymaster: "pm-1", Asset: "usdc",
		Amount: i64p(1500), Nonce: 7, State: "pending"}
	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{paid}})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("submit: err=%v res=%+v", err, res.Results)
	}

	// s1：付款后的快照。
	s1 := mustQuery(t, l)
	if got := s1.Balances[0].Balance; got != 496 {
		t.Fatalf("s1 balance=%d want 496 (amount+fee deducted)", got)
	}
	if len(s1.Settlements) != 1 {
		t.Fatalf("s1 settlements=%+v want exactly 1 record", s1.Settlements)
	}
	if r := s1.Settlements[0]; !isExpectedPaymentRecord(r) {
		t.Fatalf("s1 settlement=%+v want id=p1 account=aa paymaster=pm-1 asset=usdc "+
			"amount=1500 nonce=7 fee_bps=30 fee=4 charged=1504 seq=1", r)
	}
	if len(s1.Refunds) != 0 {
		t.Fatalf("s1 refunds must stay empty, got %+v", s1.Refunds)
	}

	// 全额退款（含手续费）：退回原扣款总额 1504，余额回到 2000。
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r1", SettlementID: "p1", Reason: "customer cancelled"},
	}})
	if err != nil || rr.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("refund: err=%v res=%+v", err, rr.Results)
	}

	// s2：退款后的快照。
	s2 := mustQuery(t, l)
	if got := s2.Balances[0].Balance; got != 2000 {
		t.Fatalf("s2 balance=%d want 2000 (full charged amount returned)", got)
	}
	if len(s2.Settlements) != 1 {
		t.Fatalf("s2 must still retain the original settlement, got %+v", s2.Settlements)
	}
	if r := s2.Settlements[0]; !isExpectedPaymentRecord(r) {
		t.Fatalf("s2 original settlement altered: %+v", r)
	}
	if len(s2.Refunds) != 1 {
		t.Fatalf("s2 refunds=%+v want exactly 1 record", s2.Refunds)
	}
	if r := s2.Refunds[0]; !isExpectedRefundRecord(r) {
		t.Fatalf("s2 refund=%+v want id=r1 settlement_id=p1 reason=customer cancelled "+
			"account=aa asset=usdc amount=1500 fee=4 charged=1504 seq=1", r)
	}

	// ---- 先前快照必须继续显示先前的内容，不跟随付款/退款 ----
	assertBalanceValue(t, s0, "aa", "usdc", 2000)
	if len(s0.Settlements) != 0 || len(s0.Refunds) != 0 {
		t.Fatalf("s0 mutated by later business: settlements=%d refunds=%d",
			len(s0.Settlements), len(s0.Refunds))
	}
	assertBalanceValue(t, s1, "aa", "usdc", 496)
	if len(s1.Settlements) != 1 || len(s1.Refunds) != 0 {
		t.Fatalf("s1 mutated by later refund: settlements=%d refunds=%d",
			len(s1.Settlements), len(s1.Refunds))
	}
	if r := s1.Settlements[0]; !isExpectedPaymentRecord(r) {
		t.Fatalf("s1 settlement content changed after refund: %+v", r)
	}
}

// 空历史快照在后续新增记录后仍为空；新记录只能出现在新的查询结果中。
func TestQueryEmptyHistoryStaysEmptyWhenRecordsAppear(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)

	empty := mustQuery(t, l)
	if len(empty.Settlements) != 0 || len(empty.Refunds) != 0 || len(empty.Balances) != 1 {
		t.Fatalf("precondition: %+v", empty)
	}

	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 100),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", "why"),
	}}); err != nil {
		t.Fatal(err)
	}

	// 先前的空快照仍为空（非 nil 空列表，记录数量为零），新记录只进新快照。
	if len(empty.Settlements) != 0 || len(empty.Refunds) != 0 {
		t.Fatalf("old empty snapshot gained records: settlements=%d refunds=%d",
			len(empty.Settlements), len(empty.Refunds))
	}
	if empty.Settlements == nil || empty.Refunds == nil {
		t.Fatalf("old snapshot lost its empty-list identity: settlements=%v refunds=%v",
			empty.Settlements, empty.Refunds)
	}
	fresh := mustQuery(t, l)
	if len(fresh.Settlements) != 1 || fresh.Settlements[0].ID != "p1" {
		t.Fatalf("fresh query must show the new settlement: %+v", fresh.Settlements)
	}
	if len(fresh.Refunds) != 1 || fresh.Refunds[0].ID != "r1" {
		t.Fatalf("fresh query must show the new refund: %+v", fresh.Refunds)
	}
}

// 业务持续推进（付款→退款→再付款）时，每份旧快照逐字段冻结在取得时刻，
// 余额、记录数量与记录内容都不被后续业务改写。
func TestQuerySnapshotsFreezeThroughContinuedBusiness(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 5000}})
	l := openOrFail(t, path)

	first := mustQuery(t, l) // 0 结算 / 0 退款，余额 5000

	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 1000),
	}}); err != nil {
		t.Fatal(err)
	}
	second := mustQuery(t, l) // 1 结算，余额 4000

	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p2", "aa", "usdc", 500),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", "a reason"),
	}}); err != nil {
		t.Fatal(err)
	}
	third := mustQuery(t, l) // 2 结算 / 1 退款，余额 4000-500+1000=4500

	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p3", "aa", "usdc", 2000),
	}}); err != nil {
		t.Fatal(err)
	}

	// first：0/0、5000。
	assertBalanceValue(t, first, "aa", "usdc", 5000)
	if len(first.Settlements) != 0 || len(first.Refunds) != 0 {
		t.Fatalf("first snapshot changed: settlements=%d refunds=%d",
			len(first.Settlements), len(first.Refunds))
	}
	// second：1 结算（p1）、0 退款、4000。
	assertBalanceValue(t, second, "aa", "usdc", 4000)
	if len(second.Settlements) != 1 || second.Settlements[0].ID != "p1" ||
		second.Settlements[0].Charged != 1000 {
		t.Fatalf("second snapshot settlement no longer its own: %+v", second.Settlements)
	}
	if len(second.Refunds) != 0 {
		t.Fatalf("second snapshot gained a refund: %+v", second.Refunds)
	}
	// third：2 结算（p1,p2 按成功先后）、1 退款（r1）、4500。
	assertBalanceValue(t, third, "aa", "usdc", 4500)
	if ids := settlementIDs(third); fmtSprint(ids) != fmtSprint([]string{"p1", "p2"}) {
		t.Fatalf("third snapshot settlements=%v want [p1 p2]", ids)
	}
	if ids := refundIDs(third); fmtSprint(ids) != fmtSprint([]string{"r1"}) {
		t.Fatalf("third snapshot refunds=%v want [r1]", ids)
	}
}

// ---- 调用者写隔离：修改返回数据不污染真实账本或其他结果 ----

// 调用者改写查询中的余额、付款记录、退款记录后，再次查询仍得到真实业务
// 数据，涵盖原编号、账户、资产、金额、手续费、成功序号以及退款原因与关联付款。
func TestQueryCallerMutationDoesNotCorruptLedger(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)
	setupFeePaymentAndRefund(t, l)

	mutated := mustQuery(t, l)

	// 调用者为自己的展示调整：改余额、改付款记录、改退款记录。
	mutated.Balances[0].Balance = 1
	mutated.Balances[0].Account = "hacked-account"
	mutated.Balances[0].Asset = "hacked-asset"
	mutated.Settlements[0].ID = "hacked-payment"
	mutated.Settlements[0].Account = "hacked"
	mutated.Settlements[0].Paymaster = "hacked-pm"
	mutated.Settlements[0].Asset = "zzz"
	mutated.Settlements[0].Amount = 999
	mutated.Settlements[0].Nonce = 999
	mutated.Settlements[0].FeeBps = 9999
	mutated.Settlements[0].Fee = 999
	mutated.Settlements[0].Charged = 99999
	mutated.Settlements[0].Seq = 42
	mutated.Refunds[0].ID = "hacked-refund"
	mutated.Refunds[0].SettlementID = "ghost-payment"
	mutated.Refunds[0].Reason = "tampered reason"
	mutated.Refunds[0].Account = "hacked"
	mutated.Refunds[0].Asset = "zzz"
	mutated.Refunds[0].Amount = 1
	mutated.Refunds[0].Fee = 1
	mutated.Refunds[0].Charged = 2
	mutated.Refunds[0].AfterSeq = 42
	mutated.Refunds[0].Seq = 42

	// 再次查询必须仍得到真实业务产生的数据。
	fresh := mustQuery(t, l)
	assertBalanceValue(t, fresh, "aa", "usdc", 2000)
	if len(fresh.Balances) != 1 {
		t.Fatalf("fresh balances count=%d want 1", len(fresh.Balances))
	}
	if r := fresh.Settlements[0]; !isExpectedPaymentRecord(r) {
		t.Fatalf("settlement corrupted by caller mutation: %+v", r)
	}
	if r := fresh.Refunds[0]; !isExpectedRefundRecord(r) {
		t.Fatalf("refund corrupted by caller mutation: %+v", r)
	}

	// Balance 便捷查询同样读取真实状态。
	if bal, err := l.Balance("aa", "usdc"); err != nil || bal != 2000 {
		t.Fatalf("Balance after caller mutation: bal=%d err=%v want 2000", bal, err)
	}
}

// 修改一个返回结果不能改变此前独立取得的另一份结果（余额、结算、退款三者
// 一起受保护，不能出现余额独立但历史被改写）。
func TestQueryMutationOfOneSnapshotLeavesOtherSnapshotIntact(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)
	setupFeePaymentAndRefund(t, l)

	kept := mustQuery(t, l)
	victim := mustQuery(t, l)

	// 对 victim 做覆盖余额与两类历史的大幅改写。
	victim.Balances[0].Balance = 7
	victim.Settlements[0].ID = "spoof"
	victim.Settlements[0].Amount = 7
	victim.Settlements[0].Fee = 7
	victim.Settlements[0].Charged = 7
	victim.Settlements[0].Seq = 7
	victim.Refunds[0].ID = "spoof-r"
	victim.Refunds[0].Reason = "spoofed"
	victim.Refunds[0].Charged = 7

	// kept 必须保持取得时的真实内容：余额与历史一起未变。
	assertBalanceValue(t, kept, "aa", "usdc", 2000)
	if len(kept.Settlements) != 1 || len(kept.Refunds) != 1 {
		t.Fatalf("kept counts changed: settlements=%d refunds=%d",
			len(kept.Settlements), len(kept.Refunds))
	}
	if r := kept.Settlements[0]; !isExpectedPaymentRecord(r) {
		t.Fatalf("kept settlement rewritten by sibling mutation: %+v", r)
	}
	if r := kept.Refunds[0]; !isExpectedRefundRecord(r) {
		t.Fatalf("kept refund rewritten by sibling mutation: %+v", r)
	}
}

// 通过切片操作向旧快照追加/删除元素，不改变新查询的记录数量与内容。
func TestQuerySnapshotSliceReshapingDoesNotAffectLedger(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 100),
	}}); err != nil {
		t.Fatal(err)
	}

	snap := mustQuery(t, l)
	// 调用者截断、追加自己的切片。
	snap.Settlements = snap.Settlements[:0]
	snap.Refunds = append(snap.Refunds, RefundRecord{ID: "fake"})
	snap.Balances = append(snap.Balances, BalanceView{Account: "x", Asset: "y", Balance: 1})

	fresh := mustQuery(t, l)
	if len(fresh.Settlements) != 1 || fresh.Settlements[0].ID != "p1" {
		t.Fatalf("reshaping caller slice altered ledger settlements: %+v", fresh.Settlements)
	}
	if len(fresh.Refunds) != 0 {
		t.Fatalf("appended fake refund leaked into ledger: %+v", fresh.Refunds)
	}
	if len(fresh.Balances) != 1 || fresh.Balances[0].Account != "aa" {
		t.Fatalf("appended balance leaked into ledger: %+v", fresh.Balances)
	}
}

// ---- 对返回数据的改动不能改变业务判定 ----

// 改坏查询结果后：
//   - 后续付款仍按真实余额结算（不因快照里的假余额而成功/失败）；
//   - 已退款的付款按原请求再次提交仍返回 duplicate 和原结算记录，不再次扣款；
//   - 用新退款编号退回同一付款仍返回 already_refunded，不增加余额。
func TestQueryMutationDoesNotChangeBusinessDecisions(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)
	paid := PaymentIntent{ID: "p1", Account: "aa", Paymaster: "pm-1", Asset: "usdc",
		Amount: i64p(1500), Nonce: 7, State: "pending"}
	res, _ := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{paid}})
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("setup: %+v", res.Results[0])
	}
	rr, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r1", SettlementID: "p1", Reason: "customer cancelled"},
	}})
	if rr.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("setup refund: %+v", rr.Results[0])
	}
	// 真实余额已回到 2000。调用者把快照余额改成 0 并伪造历史。
	snap := mustQuery(t, l)
	snap.Balances[0].Balance = 0
	snap.Settlements[0].Amount = 1
	snap.Settlements[0].Charged = 1
	snap.Refunds[0] = RefundRecord{}

	// 新付款按真实余额 2000 结算：2000 费率 0 恰好成功。
	pay, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p2", "aa", "usdc", 2000),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if pay.Results[0].Status != StatusSettled {
		t.Fatalf("payment must settle against the REAL balance 2000, got %+v", pay.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("balance after p2=%d want 0", bal)
	}

	// 已退款的 p1 按原请求（含费率）再提交：duplicate + 原结算记录，不再次扣款。
	dup, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{paid}})
	if err != nil {
		t.Fatal(err)
	}
	d := dup.Results[0]
	if d.Status != StatusDuplicate || d.Record == nil || !isExpectedPaymentRecord(*d.Record) {
		t.Fatalf("refunded payment retry want duplicate with original record, got %+v", d)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("duplicate re-charged after snapshot tampering: %d", bal)
	}

	// 新退款编号退回同一付款：already_refunded，不增加余额。
	ar, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r2", SettlementID: "p1", Reason: "second attempt"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ar.Results[0].Status != StatusAlreadyRefunded {
		t.Fatalf("want already_refunded, got %+v", ar.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("already_refunded changed balance: %d", bal)
	}
}

// 快照里的“已退款”状态是只读视图：即使调用者把快照退款记录删光或伪造成
// “未退款”，真实付款仍被判定为已退款，且真实历史不会少一条退款。
func TestQueryMutationCannotUnmarkRefundedPayment(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)
	setupFeePaymentAndRefund(t, l)

	snap := mustQuery(t, l)
	// 调用者让自己的视图“回到未退款”。
	snap.Refunds = snap.Refunds[:0]
	snap.Balances[0].Balance = 496

	ar, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r-other", SettlementID: "p1", Reason: "try again"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ar.Results[0].Status != StatusAlreadyRefunded {
		t.Fatalf("real ledger must still consider p1 refunded, got %+v", ar.Results[0])
	}
	fresh := mustQuery(t, l)
	if len(fresh.Refunds) != 1 || fresh.Refunds[0].ID != "r1" {
		t.Fatalf("real refund history must keep r1: %+v", fresh.Refunds)
	}
	assertBalanceValue(t, fresh, "aa", "usdc", 2000)
}

// ---- 多账户、多资产 ----

// 多个账户或资产同时存在时，未发生实际收支的组合保持原余额；新查询继续
// 按账户、资产排序，付款与退款记录各按成功先后排列。
func TestQueryMultiAccountAssetOrderAndIsolation(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "bb", Asset: "usdc", Balance: 300},
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "aa", Asset: "eth", Balance: 5},
	})
	l := openOrFail(t, path)

	// aa/usdc 付 1500+4(费)=1504；bb/usdc 付 100；aa/eth 不发生收支。
	if _, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		{ID: "p1", Account: "aa", Paymaster: "pm-1", Asset: "usdc", Amount: i64p(1500), Nonce: 1, State: "pending"},
		{ID: "p2", Account: "bb", Paymaster: "pm-2", Asset: "usdc", Amount: i64p(100), Nonce: 2, State: "pending"},
	}}); err != nil {
		t.Fatal(err)
	}
	// 只退 aa/usdc 的 p1。
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r1", SettlementID: "p1", Reason: "customer cancelled"},
	}}); err != nil {
		t.Fatal(err)
	}

	snap := mustQuery(t, l)

	// 余额按账户、资产排序：aa/eth, aa/usdc, bb/usdc。
	wantBals := []BalanceView{
		{Account: "aa", Asset: "eth", Balance: 5},     // 无收支，保持
		{Account: "aa", Asset: "usdc", Balance: 2000}, // 付款后退款，回到初始
		{Account: "bb", Asset: "usdc", Balance: 200},  // 300-100
	}
	if !balancesEqual(snap.Balances, wantBals) {
		t.Fatalf("balances=%s\nwant %s", marshal(t, snap.Balances), marshal(t, wantBals))
	}

	// 结算按成功先后：p1、p2。
	if len(snap.Settlements) != 2 {
		t.Fatalf("settlements=%+v want 2 records", snap.Settlements)
	}
	if snap.Settlements[0].ID != "p1" || snap.Settlements[1].ID != "p2" {
		t.Fatalf("settlements not in success order: %+v", snap.Settlements)
	}
	if r0, r1 := snap.Settlements[0], snap.Settlements[1]; r0.Seq != 1 || r1.Seq != 2 ||
		r0.Account != "aa" || r1.Account != "bb" || r1.Charged != 100 {
		t.Fatalf("settlement fields/order: %+v %+v", r0, r1)
	}

	// 退款按成功先后：r1，关联 p1，退回 aa/usdc 原扣款总额。
	if len(snap.Refunds) != 1 {
		t.Fatalf("refunds=%+v want 1 record", snap.Refunds)
	}
	if r := snap.Refunds[0]; r.ID != "r1" || r.SettlementID != "p1" ||
		r.Account != "aa" || r.Asset != "usdc" || r.Charged != 1504 || r.Seq != 1 {
		t.Fatalf("refund fields: %+v", r)
	}

	// 多笔付款与退款交错后的成功先后顺序。
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p3", "bb", "usdc", 50),
		intent("p4", "aa", "usdc", 20),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r2", "p2", "bb refund"),
		refundReq("r3", "p3", "bb refund too"),
	}}); err != nil {
		t.Fatal(err)
	}
	fresh := mustQuery(t, l)
	if ids := settlementIDs(fresh); fmtSprint(ids) != fmtSprint([]string{"p1", "p2", "p3", "p4"}) {
		t.Fatalf("settlements success order=%v", ids)
	}
	if ids := refundIDs(fresh); fmtSprint(ids) != fmtSprint([]string{"r1", "r2", "r3"}) {
		t.Fatalf("refunds success order=%v", ids)
	}
	// bb/usdc: 300-100(p2)-50(p3)+100(r2 退 p2)+50(r3 退 p3)=300；
	// aa/usdc: 2000-20(p4)=1980；aa/eth 仍为 5。
	wantBals2 := []BalanceView{
		{Account: "aa", Asset: "eth", Balance: 5},
		{Account: "aa", Asset: "usdc", Balance: 1980},
		{Account: "bb", Asset: "usdc", Balance: 300},
	}
	if !balancesEqual(fresh.Balances, wantBals2) {
		t.Fatalf("balances after interleaving=%s\nwant %s", marshal(t, fresh.Balances), marshal(t, wantBals2))
	}
}

// ---- 只读保证：查询与改写返回数据不触盘 ----

// 只查询或只修改返回数据时，账本文件内容及成功记录数量保持不变。
func TestQueryAndMutationDoNotTouchFileOrCounts(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)
	setupFeePaymentAndRefund(t, l)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeSnap := mustQuery(t, l)
	nPay, nRef := len(beforeSnap.Settlements), len(beforeSnap.Refunds)

	// 连续多次只读查询。
	for i := 0; i < 3; i++ {
		s := mustQuery(t, l)
		if len(s.Settlements) != nPay || len(s.Refunds) != nRef {
			t.Fatalf("query %d changed counts", i)
		}
	}

	// 对返回数据做破坏性修改，不触发任何落盘。
	s := mustQuery(t, l)
	s.Balances[0].Balance = 0
	s.Settlements = s.Settlements[:0]
	s.Refunds[0].ID = "nope"
	s.Refunds = append(s.Refunds, RefundRecord{ID: "extra"})

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file changed by query / return-data mutation")
	}
	fresh := mustQuery(t, l)
	if len(fresh.Settlements) != nPay || len(fresh.Refunds) != nRef {
		t.Fatalf("success record counts changed: settlements=%d refunds=%d, want %d/%d",
			len(fresh.Settlements), len(fresh.Refunds), nPay, nRef)
	}
}

// ===========================================================================
// 辅助函数
// ===========================================================================

func mustQuery(t *testing.T, l *Ledger) *Snapshot {
	t.Helper()
	s, err := l.Query()
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return s
}

// setupFeePaymentAndRefund 在账本上完成“1500 付款（30bp，手续费 4）→ 全额
// 退款”，结束时 aa/usdc 余额回到 2000，含 1 条结算与 1 条退款。
func setupFeePaymentAndRefund(t *testing.T, l *Ledger) {
	t.Helper()
	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		{ID: "p1", Account: "aa", Paymaster: "pm-1", Asset: "usdc",
			Amount: i64p(1500), Nonce: 7, State: "pending"},
	}})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("setup submit: err=%v res=%+v", err, res.Results)
	}
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r1", SettlementID: "p1", Reason: "customer cancelled"},
	}})
	if err != nil || rr.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("setup refund: err=%v res=%+v", err, rr.Results)
	}
}

func isExpectedPaymentRecord(r Record) bool {
	return r.ID == "p1" && r.Account == "aa" && r.Paymaster == "pm-1" && r.Asset == "usdc" &&
		r.Amount == 1500 && r.Nonce == 7 && r.FeeBps == 30 && r.Fee == 4 &&
		r.Charged == 1504 && r.Seq == 1
}

func isExpectedRefundRecord(r RefundRecord) bool {
	return r.ID == "r1" && r.SettlementID == "p1" && r.Reason == "customer cancelled" &&
		r.Account == "aa" && r.Asset == "usdc" && r.Amount == 1500 && r.Fee == 4 &&
		r.Charged == 1504 && r.AfterSeq == 1 && r.Seq == 1
}

func assertBalanceValue(t *testing.T, s *Snapshot, account, asset string, want int64) {
	t.Helper()
	for _, b := range s.Balances {
		if b.Account == account && b.Asset == asset {
			if b.Balance != want {
				t.Fatalf("balance %s/%s=%d want %d (snapshot must freeze)", account, asset, b.Balance, want)
			}
			return
		}
	}
	t.Fatalf("balance %s/%s missing from snapshot %+v", account, asset, s.Balances)
}

func settlementIDs(s *Snapshot) []string {
	out := make([]string, len(s.Settlements))
	for i, r := range s.Settlements {
		out[i] = r.ID
	}
	return out
}

func refundIDs(s *Snapshot) []string {
	out := make([]string, len(s.Refunds))
	for i, r := range s.Refunds {
		out[i] = r.ID
	}
	return out
}

// fmtSprint 用 fmt.Sprint 做切片的确定性比较（避免引入 reflect）。
func fmtSprint(v any) string { return fmt.Sprint(v) }

func balancesEqual(got, want []BalanceView) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
