package payflow

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// 本文件为“退款保存失败后继续复用该退款编号”提供端到端回归保障，锁定连续
// 操作中失败项不占编号、不把原付款标成已退款的既有语义：
//
//	账本中先有同一账户、同一资产的三笔成功付款（p1/p2/p3），扣款总额彼此
//	不同（1010/1504/2010），且都实际扣除了非零手续费（10/4/10）。
//	一个退款批次按顺序提交五项：
//	  ra -> p1（原因一）                    成功；
//	  rb -> p2（原因二）                    保存账本时恰好失败一次 -> storage_error；
//	  rb -> p3（原因三，换目标、换原因）     复用刚失败的编号，必须正常退款；
//	  rb -> p3（原因三，原样重复上一项）     duplicate，携带上一项的成功记录；
//	  rc -> p2（另一个新编号）              成功，证明先前保存失败没有占用
//	                                        p2 的退款资格。
//
// 整批不报错，结果与输入逐项对应；失败项只携带状态与说明，既不携带成功
// 退款记录，也不撤销此前已完成的退款；同编号申请随后成功后，失败项的结果
// 仍保持失败。批次结束后余额只增加三笔实际成功退款的总额（重复申请不入账），
// 退款历史只保留成功记录、序号连续，复用编号只对应后来成功的目标与原因，
// 三笔原付款记录原样保留，磁盘账本与重新打开后的查询一致。全程只使用
// 既有公开接口与测试故障注入，可在本机离线稳定复现。

func TestRefundStorageFailureIDReusedForDifferentPayment(t *testing.T) {
	const initial int64 = 1_000_000
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: initial}})
	l := openOrFail(t, path)

	// 三笔同账户、同资产的成功付款：扣款总额彼此不同，手续费均为非零并实际扣除。
	// p1: 1000 + 100bps*1000(=10) = 1010
	// p2: 1500 + 30bps*1500(=4)  = 1504
	// p3: 2000 + 50bps*2000(=10) = 2010
	setup := []struct {
		id      string
		amount  int64
		bps     int
		fee     int64
		charged int64
	}{
		{"p1", 1000, 100, 10, 1010},
		{"p2", 1500, 30, 4, 1504},
		{"p3", 2000, 50, 10, 2010},
	}
	var chargedTotal int64
	wantSettlements := make([]Record, 0, len(setup))
	for i, p := range setup {
		r := mustSettle(t, l, p.bps, intent(p.id, "aa", "usdc", p.amount))
		if r.Status != StatusSettled {
			t.Fatalf("setup settle %s: %+v", p.id, r)
		}
		if r.Record.Fee != p.fee || r.Record.Charged != p.charged {
			t.Fatalf("setup %s fee/charged=%d/%d want %d/%d", p.id, r.Record.Fee, r.Record.Charged, p.fee, p.charged)
		}
		chargedTotal += p.charged
		wantSettlements = append(wantSettlements, Record{
			ID:        p.id,
			Account:   "aa",
			Paymaster: "pm",
			Asset:     "usdc",
			Amount:    p.amount,
			Nonce:     1,
			FeeBps:    p.bps,
			Fee:       p.fee,
			Charged:   p.charged,
			Seq:       int64(i + 1),
		})
	}
	if chargedTotal != 4524 {
		t.Fatalf("charged total=%d want 4524", chargedTotal)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != initial-chargedTotal {
		t.Fatalf("balance after setup=%d want %d", bal, initial-chargedTotal)
	}

	// 仅让批次内第 2 次持久化（rb -> p2）失败一次，此后保存恢复正常。
	SetFailHook(l, &failNth{nth: 2})
	t.Cleanup(func() { SetFailHook(l, nil) })

	const (
		reasonOne      = "reason one"      // ra -> p1 成功
		reasonTwoFail  = "reason two-fail" // rb -> p2 保存失败
		reasonThree    = "reason three"    // rb 复用后改退 p3
		reasonTwoRetry = "reason two-ok"   // 新编号 rc -> p2 成功
	)
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("ra", "p1", reasonOne),      // 1) 成功
		refundReq("rb", "p2", reasonTwoFail),  // 2) 保存失败一次
		refundReq("rb", "p3", reasonThree),    // 3) 复用失败编号：换另一笔付款、换原因
		refundReq("rb", "p3", reasonThree),    // 4) 原样重复 3)
		refundReq("rc", "p2", reasonTwoRetry), // 5) 新编号退回第二笔付款
	}})
	if err != nil {
		t.Fatalf("refund batch must not fail as a whole: %v", err)
	}
	if len(rr.Results) != 5 {
		t.Fatalf("results len=%d want 5 (逐项对应输入)", len(rr.Results))
	}

	// 状态序列严格按输入顺序对应：成功 / 存储失败 / 成功 / 重复 / 成功。
	wantStatus := []string{
		StatusRefundSuccess, StatusStorage, StatusRefundSuccess, StatusDuplicate, StatusRefundSuccess,
	}
	if got := refundStatuses(rr); !reflect.DeepEqual(got, wantStatus) {
		t.Fatalf("statuses=%v want %v", got, wantStatus)
	}

	// 留存失败项结果：同编号申请随后成功后，它仍必须保持失败，不被成功详情覆盖。
	failed := rr.Results[1]
	if failed.Status != StatusStorage {
		t.Fatalf("item 2 status=%s want storage_error", failed.Status)
	}
	if failed.Reason == "" || !strings.Contains(failed.Reason, "injected write failure") {
		t.Fatalf("item 2 must carry the failure explanation, got %q", failed.Reason)
	}

	// 三笔成功退款的预期记录：全部 after_seq=3（三笔付款早已存在），成功序号
	// 连续为 1/2/3——失败项与重复项都不占序号；复用编号 rb 只对应后来成功的
	// 目标 p3 与原因三。
	wantRA := RefundRecord{
		ID: "ra", SettlementID: "p1", Reason: reasonOne,
		Account: "aa", Asset: "usdc", Amount: 1000, Fee: 10, Charged: 1010,
		AfterSeq: 3, Seq: 1,
	}
	wantRB := RefundRecord{
		ID: "rb", SettlementID: "p3", Reason: reasonThree,
		Account: "aa", Asset: "usdc", Amount: 2000, Fee: 10, Charged: 2010,
		AfterSeq: 3, Seq: 2,
	}
	wantRC := RefundRecord{
		ID: "rc", SettlementID: "p2", Reason: reasonTwoRetry,
		Account: "aa", Asset: "usdc", Amount: 1500, Fee: 4, Charged: 1504,
		AfterSeq: 3, Seq: 3,
	}
	// 成功项与紧随的重复项指向第三笔付款、记录修改后的原因、按原付款 charged
	// 全额（含当时手续费）退回原账户与原资产；两者逐字段一致。
	assertRefundResultRecord(t, rr.Results[0], StatusRefundSuccess, wantRA)
	assertRefundResultRecord(t, rr.Results[2], StatusRefundSuccess, wantRB)
	assertRefundResultRecord(t, rr.Results[3], StatusDuplicate, wantRB)
	assertRefundResultRecord(t, rr.Results[4], StatusRefundSuccess, wantRC)
	// 重复项必须携带刚刚成功的退款记录，且与成功项持有各自独立的副本。
	if rr.Results[2].Record == nil || rr.Results[3].Record == nil {
		t.Fatalf("success/duplicate must both carry records: %+v", rr.Results)
	}
	if rr.Results[2].Record == rr.Results[3].Record {
		t.Fatal("success and duplicate results share one *RefundRecord allocation")
	}
	if rr.Results[3].SettlementID != "p3" || rr.Results[3].Account != "aa" ||
		rr.Results[3].Asset != "usdc" || rr.Results[3].Reason != reasonThree ||
		rr.Results[3].Charged != 2010 {
		t.Fatalf("duplicate must point at p3 with the reused reason: %+v", rr.Results[3])
	}

	// 失败项在同编号申请成功后仍保持失败：只有状态与说明，不携带成功退款记录，
	// 也不带任何去向详情。
	if failed.ID != "rb" || failed.Status != StatusStorage {
		t.Fatalf("failed result overwritten by later success: %+v", failed)
	}
	if failed.Record != nil {
		t.Fatalf("storage_error must not carry a refund record: %+v", failed.Record)
	}
	if failed.SettlementID != "" || failed.Account != "" || failed.Asset != "" || failed.Charged != 0 {
		t.Fatalf("storage_error must not carry success detail: %+v", failed)
	}
	if !strings.Contains(failed.Reason, "injected write failure") {
		t.Fatalf("failed result lost its explanation: %q", failed.Reason)
	}

	// 余额只增加三笔实际成功退款的总额（1010+2010+1504=4524），重复申请不入账，
	// 保存失败项不加余额：余额恰好回到初始值。
	if bal, _ := l.Balance("aa", "usdc"); bal != initial {
		t.Fatalf("balance after batch=%d want %d (three successful refunds only)", bal, initial)
	}

	// 查询到的退款历史只保留成功记录，按成功先后排列、序号连续；复用编号 rb
	// 只对应 p3 与原因三，找不到任何 rb -> p2 的痕迹。
	wantRefunds := []RefundRecord{wantRA, wantRB, wantRC}
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Refunds, wantRefunds) {
		t.Fatalf("refund history:\n got %+v\nwant %+v", snap.Refunds, wantRefunds)
	}
	// 三笔原付款的编号、金额、手续费、序号与成功顺序保持原样。
	if !reflect.DeepEqual(snap.Settlements, wantSettlements) {
		t.Fatalf("settlements altered:\n got %+v\nwant %+v", snap.Settlements, wantSettlements)
	}
	if len(snap.Balances) != 1 || snap.Balances[0] != (BalanceView{Account: "aa", Asset: "usdc", Balance: initial}) {
		t.Fatalf("balance view: %+v", snap.Balances)
	}

	// 磁盘字节层面同样不留失败申请的痕迹：失败原因不落盘，三个成功原因都在。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), reasonTwoFail) {
		t.Fatalf("failed refund request persisted to ledger file:\n%s", raw)
	}
	for _, frag := range []string{
		`"id":"ra"`, `"settlement_id":"p1"`, `"reason":"` + reasonOne + `"`,
		`"id":"rb"`, `"settlement_id":"p3"`, `"reason":"` + reasonThree + `"`, `"seq":2`,
		`"id":"rc"`, `"settlement_id":"p2"`, `"reason":"` + reasonTwoRetry + `"`, `"seq":3`,
	} {
		if !strings.Contains(string(raw), frag) {
			t.Fatalf("ledger file missing %s:\n%s", frag, raw)
		}
	}

	// 关闭重开（新进程语义）：实际保存的账本与此前查询结果完全一致。
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2 := openOrFail(t, path)
	snap2 := mustQuery(t, l2)
	if !reflect.DeepEqual(snap2.Refunds, wantRefunds) {
		t.Fatalf("refunds after reopen:\n got %+v\nwant %+v", snap2.Refunds, wantRefunds)
	}
	if !reflect.DeepEqual(snap2.Settlements, wantSettlements) {
		t.Fatalf("settlements after reopen:\n got %+v\nwant %+v", snap2.Settlements, wantSettlements)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != initial {
		t.Fatalf("balance after reopen=%d want %d", bal, initial)
	}

	// 重开后的后续判定沿用既有规则，且均不再入账：
	// 原样重复成功申请仍 duplicate 并携带同一条成功记录（rb 只指向 p3）；
	// 再换新编号退 p2 得到 already_refunded——它恰好是被 rc 退回的，
	// 先前那次保存失败没有占用它的退款资格，但成功的 rc 已经占用。
	dup := mustRefundOne(t, l2, "rb", "p3", reasonThree)
	assertRefundResultRecord(t, dup, StatusDuplicate, wantRB)
	againP2 := mustRefundOne(t, l2, "rd", "p2", "yet another id")
	if againP2.Status != StatusAlreadyRefunded {
		t.Fatalf("p2 must be refunded exactly once (by rc): %+v", againP2)
	}
	if againP2.Record != nil {
		t.Fatalf("already_refunded must not carry a record: %+v", againP2.Record)
	}
	dupRA := mustRefundOne(t, l2, "ra", "p1", reasonOne)
	assertRefundResultRecord(t, dupRA, StatusDuplicate, wantRA)
	if bal, _ := l2.Balance("aa", "usdc"); bal != initial {
		t.Fatalf("follow-up duplicates/already_refunded changed balance: %d want %d", bal, initial)
	}
	if got := mustQuery(t, l2).Refunds; !reflect.DeepEqual(got, wantRefunds) {
		t.Fatalf("refund history changed after follow-ups:\n got %+v\nwant %+v", got, wantRefunds)
	}
}
