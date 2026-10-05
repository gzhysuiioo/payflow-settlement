package payflow

import "testing"

// 本文件为 Ledger.Refund 返回结果的隔离性提供回归保障：
//
//  1. 首次成功（refunded）与幂等重复（duplicate）返回的完整记录归调用者所有：
//     调用者改写编号、原付款编号、原因、去向、金额、状态或说明，都只作用于
//     自己手里那份；账本中的退款事实（退款记录、原结算、余额、后续业务判定）
//     不随之变化。
//  2. 修改返回对象 ≠ 提交已修改的请求：前者不产生任何业务变更；后者按既有
//     规则处理——原退款编号改原因或改目标仍 conflict，新退款编号退原付款仍
//     already_refunded，都不新增退款记录、不重复增加余额。
//  3. 同一批次内首次成功项与后续重复项各自持有独立结果；不同付款的退款结果
//     互不影响，后续新增退款不会让旧结果的编号、成功序号或金额发生变化。
//
// 退款规则、公开接口与命令行输出均不在本文件改动范围内。

// freezeRefundResult 把一项退款结果连同记录冻结为独立副本，作为之后对比的基准。
func freezeRefundResult(r RefundResult) RefundResult {
	if r.Record != nil {
		rec := *r.Record
		r.Record = &rec
	}
	return r
}

// assertRefundDetail 检查一项退款结果携带的详情（原付款编号、账户、资产、
// 退款总额、原因与完整记录）与基准逐字一致；状态由调用方单独断言。
func assertRefundDetail(t *testing.T, got, want RefundResult, label string) {
	t.Helper()
	if got.Reason != want.Reason || got.SettlementID != want.SettlementID ||
		got.Account != want.Account || got.Asset != want.Asset || got.Charged != want.Charged {
		t.Fatalf("%s detail:\n got %+v\nwant %+v", label, got, want)
	}
	if (got.Record == nil) != (want.Record == nil) {
		t.Fatalf("%s record presence:\n got %+v\nwant %+v", label, got.Record, want.Record)
	}
	if want.Record != nil && *got.Record != *want.Record {
		t.Fatalf("%s record:\n got %+v\nwant %+v", label, *got.Record, *want.Record)
	}
}

// 含手续费的付款首次全额退款后，调用者任意改写手里的成功结果（编号、原付款
// 编号、原因、去向、金额、状态、说明），账本中的退款事实不受影响：查询仍看到
// 最初那笔退款与原结算，余额只增加一次原扣款总额，退款不会被转去其他账户或
// 资产，改大的返回值也不能抬高可用余额。
func TestRefundSuccessResultEditsDoNotTouchLedger(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 500},
	})
	l := openOrFail(t, path)

	// 带手续费付款：fee=10, charged=1010，aa 余 990。
	if r := mustSettle(t, l, 100, intent("p1", "aa", "usdc", 1000)); r.Status != StatusSettled {
		t.Fatalf("p1: %+v", r)
	}

	// 首次全额退款：refunded，退回原账户、原资产，总额为原付款的 charged。
	first := mustRefundOne(t, l, "r1", "p1", "cancel order")
	if first.Status != StatusRefundSuccess {
		t.Fatalf("r1: %+v", first)
	}
	if first.SettlementID != "p1" || first.Account != "aa" || first.Asset != "usdc" || first.Charged != 1010 {
		t.Fatalf("refund detail: %+v", first)
	}
	if first.Record == nil || first.Record.ID != "r1" || first.Record.SettlementID != "p1" ||
		first.Record.Reason != "cancel order" || first.Record.Amount != 1000 ||
		first.Record.Fee != 10 || first.Record.Charged != 1010 ||
		first.Record.AfterSeq != 1 || first.Record.Seq != 1 {
		t.Fatalf("refund record: %+v", first.Record)
	}
	frozen := freezeRefundResult(first)

	// 调用者像为自己展示那样任意改写手里的结果：状态、说明、退款编号、
	// 原付款编号、原因、去向（账户/资产）与金额全部改坏。
	first.Status = StatusConflict
	first.Reason = "forged"
	first.SettlementID = "ghost"
	first.Account = "bb"
	first.Asset = "eth"
	first.Charged = 999_999
	first.Record.ID = "HACK"
	first.Record.SettlementID = "ghost"
	first.Record.Reason = "forged"
	first.Record.Account = "bb"
	first.Record.Asset = "eth"
	first.Record.Amount = 1
	first.Record.Fee = 1
	first.Record.Charged = 999_999
	first.Record.AfterSeq = 99
	first.Record.Seq = 99

	// 查询仍看到最初成功的那笔退款及原结算，逐字段完整。
	snap := mustQuery(t, l)
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" ||
		snap.Settlements[0].Account != "aa" || snap.Settlements[0].Asset != "usdc" ||
		snap.Settlements[0].Amount != 1000 || snap.Settlements[0].Fee != 10 ||
		snap.Settlements[0].Charged != 1010 || snap.Settlements[0].Seq != 1 {
		t.Fatalf("original settlement altered by caller edit: %+v", snap.Settlements)
	}
	if len(snap.Refunds) != 1 {
		t.Fatalf("caller edit changed refund count: %+v", snap.Refunds)
	}
	rf := snap.Refunds[0]
	if rf.ID != "r1" || rf.SettlementID != "p1" || rf.Reason != "cancel order" ||
		rf.Account != "aa" || rf.Asset != "usdc" || rf.Amount != 1000 ||
		rf.Fee != 10 || rf.Charged != 1010 || rf.AfterSeq != 1 || rf.Seq != 1 {
		t.Fatalf("original refund altered by caller edit: %+v", rf)
	}

	// 实际余额只增加一次原扣款总额；退款没有被转去其他账户或资产。
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("aa/usdc=%d want 2000 (refunded exactly once)", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 500 {
		t.Fatalf("refund leaked into bb/usdc: %d", bal)
	}
	if bal, _ := l.Balance("bb", "eth"); bal != 0 {
		t.Fatalf("refund leaked into bb/eth: %d", bal)
	}

	// 返回值被改大不能增加可用余额：超出真实余额 2000 的付款仍失败。
	if r := mustSettle(t, l, 0, intent("p2", "aa", "usdc", 2001)); r.Status != StatusFunds {
		t.Fatalf("inflated result must not raise spendable balance: %+v", r)
	}

	// 返回结果的状态与说明被调整，不影响后续业务判断：原退款请求再提交
	// 仍是 duplicate，且携带最初成功时的详情而非调用者改写后的内容。
	dup := mustRefundOne(t, l, "r1", "p1", "cancel order")
	if dup.Status != StatusDuplicate {
		t.Fatalf("want duplicate, got %+v", dup)
	}
	assertRefundDetail(t, dup, frozen, "duplicate after caller edits")
}

// 区分“修改返回对象”与“提交已修改的请求”：前者不产生任何业务变更；后者
// 继续按现有规则处理——原退款编号搭配另一个合法原因或另一笔存在的付款仍
// conflict，新退款编号申请退回原付款仍 already_refunded，均不新增退款记录、
// 不重复增加余额。
func TestRefundEditedResultVsModifiedRequest(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10_000}})
	l := openOrFail(t, path)

	if r := mustSettle(t, l, 100, intent("p1", "aa", "usdc", 1000)); r.Status != StatusSettled {
		t.Fatalf("p1: %+v", r) // charged 1010
	}
	if r := mustSettle(t, l, 0, intent("p2", "aa", "usdc", 500)); r.Status != StatusSettled {
		t.Fatalf("p2: %+v", r)
	}
	first := mustRefundOne(t, l, "r1", "p1", "original reason")
	if first.Status != StatusRefundSuccess {
		t.Fatalf("r1: %+v", first)
	}
	frozen := freezeRefundResult(first)
	balAfter, _ := l.Balance("aa", "usdc") // 10000-1010-500+1010

	// 调用者把手里的返回结果改写成“另一个原因、另一笔付款”的样子。
	first.Reason = "another reason"
	first.SettlementID = "p2"
	first.Charged = 500
	first.Record.Reason = "another reason"
	first.Record.SettlementID = "p2"
	first.Record.Charged = 500

	// 这只是改了返回对象：原请求再提交仍是 duplicate，携带最初成功时的详情。
	dup := mustRefundOne(t, l, "r1", "p1", "original reason")
	if dup.Status != StatusDuplicate {
		t.Fatalf("editing the result object must not change judgement: %+v", dup)
	}
	assertRefundDetail(t, dup, frozen, "duplicate carries original detail")

	// 把改写后的内容作为请求提交，才进入既有冲突与已退款规则：
	// 原退款编号 + 另一个合法原因 → conflict。
	if r := mustRefundOne(t, l, "r1", "p1", "another reason"); r.Status != StatusConflict {
		t.Fatalf("edited reason as request: want conflict, got %+v", r)
	}
	// 原退款编号 + 另一笔存在的付款 → conflict（不退化成对 p2 的正常退款）。
	if r := mustRefundOne(t, l, "r1", "p2", "original reason"); r.Status != StatusConflict {
		t.Fatalf("edited target as request: want conflict, got %+v", r)
	}
	// 新退款编号申请退回原付款 → already_refunded。
	if r := mustRefundOne(t, l, "r2", "p1", "new id same payment"); r.Status != StatusAlreadyRefunded {
		t.Fatalf("want already_refunded, got %+v", r)
	}

	// 以上都不新增退款记录，也不重复增加余额。
	snap := mustQuery(t, l)
	if len(snap.Refunds) != 1 || snap.Refunds[0].ID != "r1" ||
		snap.Refunds[0].SettlementID != "p1" || snap.Refunds[0].Reason != "original reason" ||
		snap.Refunds[0].Charged != 1010 || snap.Refunds[0].Seq != 1 {
		t.Fatalf("conflict/already_refunded must not add or rewrite refunds: %+v", snap.Refunds)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("balance changed by rejected requests: %d vs %d", bal, balAfter)
	}
}

// 同一批次内连续相同的退款请求：首次成功项与后续重复项各自持有可修改的
// 独立结果，改动其中一项，其他项及此前保留的结果都不随之变化；不同付款的
// 成功退款结果互不影响，后续新增退款不会让旧结果的编号、成功序号或金额
// 发生变化。
func TestRefundResultsIndependentAcrossItemsAndRefunds(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)

	for _, it := range []PaymentIntent{
		intent("p1", "aa", "usdc", 100),
		intent("p2", "aa", "usdc", 200),
		intent("p3", "aa", "usdc", 300),
	} {
		if r := mustSettle(t, l, 0, it); r.Status != StatusSettled {
			t.Fatalf("%s: %+v", it.ID, r)
		}
	}

	// 先退 p2 并保留这次成功结果，用于验证后续退款不影响旧结果。
	kept := mustRefundOne(t, l, "r0", "p2", "keep me")
	if kept.Status != StatusRefundSuccess || kept.Record == nil ||
		kept.Record.Seq != 1 || kept.Record.Charged != 200 {
		t.Fatalf("kept r0: %+v", kept)
	}
	frozenKept := freezeRefundResult(kept)

	// 同一批次内连续两个相同退款请求：首次 refunded，后续 duplicate。
	rb, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", "dup me"),
		refundReq("r1", "p1", "dup me"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	first, dup := rb.Results[0], rb.Results[1]
	if first.Status != StatusRefundSuccess || dup.Status != StatusDuplicate {
		t.Fatalf("batch statuses: %+v", rb.Results)
	}
	if first.Record == nil || dup.Record == nil {
		t.Fatalf("both items must carry the record: %+v", rb.Results)
	}
	if first.Record == dup.Record {
		t.Fatalf("batch items must each hold their own record, not share one pointer")
	}
	if first.Record.Seq != 2 || dup.Record.Seq != 2 {
		t.Fatalf("both items refer to the same successful refund seq=2: %+v", rb.Results)
	}
	frozenFirst := freezeRefundResult(first)
	frozenDup := freezeRefundResult(dup)

	// 改动首次成功项的记录：同批次的重复项与此前保留的结果都不随之变化。
	first.Record.ID = "HACK"
	first.Record.SettlementID = "p3"
	first.Record.Charged = 777
	first.Record.Seq = 99
	first.Charged = 777
	assertRefundDetail(t, dup, frozenDup, "duplicate item after editing first item")
	assertRefundDetail(t, kept, frozenKept, "earlier kept result after editing first item")

	// 反向改动重复项：账本里的真实退款记录仍是最初内容。
	dup.Record.ID = "HACK-2"
	dup.Record.Reason = "forged"
	dup.Record.Charged = 1
	mutatedDup := freezeRefundResult(dup)
	snap := mustQuery(t, l)
	if len(snap.Refunds) != 2 {
		t.Fatalf("caller edits changed refund count: %+v", snap.Refunds)
	}
	if r := snap.Refunds[1]; r.ID != "r1" || r.SettlementID != "p1" || r.Reason != "dup me" ||
		r.Charged != 100 || r.Seq != 2 {
		t.Fatalf("real r1 record altered by item edits: %+v", r)
	}

	// 后续新增退款（退 p3）：旧结果的编号、成功序号与金额保持不变，
	// 新查询中各笔退款仍按成功先后排列且内容完整。
	last := mustRefundOne(t, l, "r2", "p3", "last one")
	if last.Status != StatusRefundSuccess || last.Record == nil || last.Record.Seq != 3 {
		t.Fatalf("r2: %+v", last)
	}
	assertRefundDetail(t, kept, frozenKept, "kept result after new refund")
	// 重复项保持着调用者改写后的内容：新增退款不会回写或重置旧结果。
	assertRefundDetail(t, dup, mutatedDup, "duplicate item after new refund")
	if frozenFirst.Record.ID != "r1" || frozenFirst.Record.Seq != 2 || frozenFirst.Record.Charged != 100 {
		t.Fatalf("first item baseline moved: %+v", frozenFirst.Record)
	}

	snap = mustQuery(t, l)
	if len(snap.Refunds) != 3 {
		t.Fatalf("want 3 refunds, got %+v", snap.Refunds)
	}
	wantSeq := []struct {
		id      string
		sid     string
		charged int64
	}{
		{"r0", "p2", 200},
		{"r1", "p1", 100},
		{"r2", "p3", 300},
	}
	for i, w := range wantSeq {
		r := snap.Refunds[i]
		if r.ID != w.id || r.SettlementID != w.sid || r.Charged != w.charged || r.Seq != int64(i+1) {
			t.Fatalf("refund %d: got %+v want id=%s sid=%s charged=%d seq=%d",
				i, r, w.id, w.sid, w.charged, i+1)
		}
	}

	// 不同付款的退款结果互不影响：把保留的 r0 结果改写到指向别的付款，
	// 账本中 r0 与 r1 的记录都不变。
	kept.Record.ID = "HACK-KEPT"
	kept.Record.SettlementID = "p1"
	kept.Record.Charged = 100
	snap = mustQuery(t, l)
	if r := snap.Refunds[0]; r.ID != "r0" || r.SettlementID != "p2" || r.Charged != 200 || r.Seq != 1 {
		t.Fatalf("real r0 record altered by editing kept result: %+v", r)
	}
	if r := snap.Refunds[1]; r.ID != "r1" || r.SettlementID != "p1" || r.Charged != 100 || r.Seq != 2 {
		t.Fatalf("real r1 record altered by editing kept result: %+v", r)
	}
}
