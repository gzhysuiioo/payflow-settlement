package payflow

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// 本文件为 Refund 返回结果与账本退款事实之间的独立性提供回归保障。
// 退款成功项（refunded）与幂等重复项（duplicate）都会向调用者提供完整退款
// 详情（顶层去向字段 + 完整 RefundRecord），本文件围绕这两种结果锁定：
//
//  1. 空间独立：调用者可以任意修改自己手里的成功/重复结果（退款编号、原付款
//     编号、原因、去向账户/资产、金额与序号，乃至状态与说明），改动只能作用
//     于这份结果：账本中的退款记录、原结算记录、真实余额与去向都不变。
//  2. 判定独立：改大返回金额不能抬高可用余额，改去向不能把退款转给其他账户或
//     资产，改状态/说明不能影响后续 duplicate/conflict/already_refunded 判定。
//  3. 改对象不等于改请求：只改返回对象不产生任何业务变更；把改动后的请求重新
//     提交仍按既有规则处理（原编号换原因/换目标 => conflict，新编号退原付款
//     => already_refunded，原请求原样重提 => duplicate 且携带最初成功的详情）。
//  4. 结果之间彼此独立：同批次内的成功项与重复项、不同付款的成功结果、后续新增
//     退款前后保留的旧结果，各自持有可修改副本，编号、成功序号与金额互不影响。
//
// 退款规则、公开接口、RefundResult 的公开 JSON 字段与命令行输出均不在本文件变更。

// corruptRefundResult 像调用者为自己展示所做的那样，破坏性改写一份退款结果：
// 顶层状态、说明、去向、金额与完整记录的全部字段一律换成伪造值。
func corruptRefundResult(r *RefundResult) {
	r.ID = "HACK-ID"
	r.Status = "forged-status"
	r.Reason = "forged reason"
	r.SettlementID = "p-fake"
	r.Account = "zz"
	r.Asset = "btc"
	r.Charged = 999_999
	if r.Record == nil {
		return
	}
	r.Record.ID = "HACK-REC"
	r.Record.SettlementID = "p-fake"
	r.Record.Reason = "forged reason"
	r.Record.Account = "zz"
	r.Record.Asset = "btc"
	r.Record.Amount = 1
	r.Record.Fee = 1
	r.Record.Charged = 2
	r.Record.AfterSeq = 99
	r.Record.Seq = 99
}

// assertRefundResultRecord 断言一份退款结果（成功或重复）携带的详情逐字段等于
// 预期：顶层原付款编号、账户、资产、退款总额、原因，以及完整退款记录。
func assertRefundResultRecord(t *testing.T, got RefundResult, wantStatus string, want RefundRecord) {
	t.Helper()
	if got.Status != wantStatus {
		t.Fatalf("status=%s want %s: %+v", got.Status, wantStatus, got)
	}
	if got.ID != want.ID {
		t.Fatalf("id=%q want %q: %+v", got.ID, want.ID, got)
	}
	if got.Reason != want.Reason || got.SettlementID != want.SettlementID ||
		got.Account != want.Account || got.Asset != want.Asset || got.Charged != want.Charged {
		t.Fatalf("top-level detail:\n got %+v\nwant id=%s reason=%s sid=%s %s/%s charged=%d",
			got, want.ID, want.Reason, want.SettlementID, want.Account, want.Asset, want.Charged)
	}
	if got.Record == nil {
		t.Fatalf("%s result must carry a record: %+v", wantStatus, got)
	}
	if *got.Record != want {
		t.Fatalf("record:\n got %+v\nwant %+v", *got.Record, want)
	}
}

// TestRefundSuccessResultEditsStayPrivate 覆盖核心隔离：一笔含手续费的付款首次
// 全额退款成功后，调用者留存返回结果并任意改写（编号、原付款编号、原因、去向、
// 金额、状态、说明），账本中的退款事实与原结算必须原样保留。
func TestRefundSuccessResultEditsStayPrivate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 800},
	})
	l := openOrFail(t, path)

	// p1 成功结算：1000 本金 + 100bps 手续费 10，共扣 1010，aa 余 990。
	if r := mustSettle(t, l, 100, intent("p1", "aa", "usdc", 1000)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 990 {
		t.Fatalf("balance after settle=%d want 990", bal)
	}

	// 首次全额退款：refunded，退回原付款的 charged=1010，原账户 aa、原资产 usdc。
	first := mustRefundOne(t, l, "r1", "p1", "cancel order")
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "cancel order",
		Account: "aa", Asset: "usdc", Amount: 1000, Fee: 10, Charged: 1010,
		AfterSeq: 1, Seq: 1,
	}
	assertRefundResultRecord(t, first, StatusRefundSuccess, wantR1)
	// 实际余额只增加一次原扣款总额：990+1010=2000。
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("balance after refund=%d want 2000", bal)
	}

	// 公开 JSON 格式回归：成功结果携带顶层去向字段与完整记录，键名固定。
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"id":"r1"`, `"status":"refunded"`, `"reason":"cancel order"`,
		`"settlement_id":"p1"`, `"account":"aa"`, `"asset":"usdc"`, `"charged":1010`,
		`"record":{`, `"after_seq":1`, `"seq":1`,
	} {
		if !bytes.Contains(raw, []byte(frag)) {
			t.Fatalf("success result JSON missing %s:\n%s", frag, raw)
		}
	}

	// 调用者留存并破坏性改写手里的结果：编号、原付款编号、原因、去向、金额、
	// 状态、说明全部换掉（金额改大到 999999、去向改成 zz/btc）。
	corruptRefundResult(&first)

	// 1. 账本查询仍看到最初成功的那笔退款，字段逐字不变。
	snap := mustQuery(t, l)
	if len(snap.Refunds) != 1 {
		t.Fatalf("refund count=%d want 1: %+v", len(snap.Refunds), snap.Refunds)
	}
	if snap.Refunds[0] != wantR1 {
		t.Fatalf("real refund record altered by caller edit:\n got %+v\nwant %+v", snap.Refunds[0], wantR1)
	}
	// 原结算同样保留，charged 仍是 1010，没有被退款或调用者改写。
	if len(snap.Settlements) != 1 {
		t.Fatalf("settlement count=%d want 1", len(snap.Settlements))
	}
	if sp1 := snap.Settlements[0]; sp1.ID != "p1" || sp1.Account != "aa" ||
		sp1.Asset != "usdc" || sp1.Amount != 1000 || sp1.Fee != 10 || sp1.Charged != 1010 || sp1.Seq != 1 {
		t.Fatalf("original settlement altered: %+v", sp1)
	}

	// 2. 实际余额只增加过一次原扣款总额，改大返回金额不能抬高可用余额。
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("balance changed after editing result: %d want 2000", bal)
	}
	overspend := mustSettle(t, l, 0, intent("p-over", "aa", "usdc", 2001))
	if overspend.Status != StatusFunds {
		t.Fatalf("inflated refund result must not raise spending power, want insufficient_balance: %+v", overspend)
	}
	// 退款不能被转给其他账户或其他资产：bb/usdc 与未发生过收支的 zz/btc 都不变。
	if bal, _ := l.Balance("bb", "usdc"); bal != 800 {
		t.Fatalf("refund leaked to another account: bb/usdc=%d want 800", bal)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("refund leaked to forged destination: zz/btc=%d want 0", bal)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("failed overspend changed balance: %d", bal)
	}

	// 3. 返回结果的状态与说明被调整，不影响后续业务判断：
	//    新退款编号申请退回原付款仍为 already_refunded，不加余额。
	again := mustRefundOne(t, l, "r-other", "p1", "trying again")
	if again.Status != StatusAlreadyRefunded {
		t.Fatalf("want already_refunded despite forged status on held result: %+v", again)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("already_refunded changed balance: %d want 2000", bal)
	}
	if len(mustQuery(t, l).Refunds) != 1 {
		t.Fatalf("no new refund record may be added")
	}
}

// TestRefundDuplicateCarriesOriginalDetailNotCallerEdits 区分“修改返回对象”与
// “提交已修改的请求”：前者不产生业务变更；原请求重提仍 duplicate 且携带最初
// 成功时的详情，而不是调用者改写后的内容；改了字段的请求按 conflict /
// already_refunded 既有规则处理，不新增退款、不重复加余额。
func TestRefundDuplicateCarriesOriginalDetailNotCallerEdits(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openOrFail(t, path)
	// 两笔存在的付款，均无手续费、各扣 100。
	for _, id := range []string{"p1", "p2"} {
		if r := mustSettle(t, l, 0, intent(id, "aa", "usdc", 100)); r.Status != StatusSettled {
			t.Fatalf("settle %s: %+v", id, r)
		}
	}

	first := mustRefundOne(t, l, "r1", "p1", "reason A")
	// 退款落账时已有两笔结算：after_seq=2、seq=1。
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "reason A",
		Account: "aa", Asset: "usdc", Amount: 100, Fee: 0, Charged: 100,
		AfterSeq: 2, Seq: 1,
	}
	assertRefundResultRecord(t, first, StatusRefundSuccess, wantR1)
	balAfter, _ := l.Balance("aa", "usdc") // 1_000_000-200+100 = 999_900

	// 原退款请求原样再次提交：duplicate，携带最初成功时的详情。
	dup := mustRefundOne(t, l, "r1", "p1", "reason A")
	assertRefundResultRecord(t, dup, StatusDuplicate, wantR1)
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("duplicate changed balance: %d vs %d", bal, balAfter)
	}

	// 调用者改写自己留存的成功结果（编号、原因、目标、去向、金额全部伪造）。
	corruptRefundResult(&first)
	// 改写也波及不到刚取得的 duplicate 结果。
	assertRefundResultRecord(t, dup, StatusDuplicate, wantR1)

	// 原请求再次提交，返回的仍是最初成功的详情，而不是调用者改写后的内容。
	dup2 := mustRefundOne(t, l, "r1", "p1", "reason A")
	assertRefundResultRecord(t, dup2, StatusDuplicate, wantR1)

	// 提交“已修改的请求”继续按既有规则处理：
	// 用原退款编号换成另一个合法原因 => conflict。
	if r := mustRefundOne(t, l, "r1", "p1", "reason B"); r.Status != StatusConflict {
		t.Fatalf("same id with changed reason want conflict: %+v", r)
	}
	// 用原退款编号指向另一笔存在的付款 => conflict（不退化成 not_found）。
	if r := mustRefundOne(t, l, "r1", "p2", "reason A"); r.Status != StatusConflict {
		t.Fatalf("same id with changed existing target want conflict: %+v", r)
	}
	// 用新的退款编号申请退回原付款 => already_refunded。
	if r := mustRefundOne(t, l, "r2", "p1", "reason A"); r.Status != StatusAlreadyRefunded {
		t.Fatalf("new id for refunded payment want already_refunded: %+v", r)
	}

	// 以上均不新增退款记录、不重复增加余额。
	snap := mustQuery(t, l)
	if len(snap.Refunds) != 1 || snap.Refunds[0] != wantR1 {
		t.Fatalf("ledger refunds:\n got %+v\nwant only %+v", snap.Refunds, wantR1)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("balance after modified requests: %d want %d", bal, balAfter)
	}
}

// TestRefundSameBatchSuccessAndDuplicateResultsAreIndependent 锁定同一批次内
// 连续出现相同退款请求时的结果隔离：首次成功项与后续重复项各自持有可修改的
// 记录副本，改动其中一项不波及其他项，也不波及此前批次保留的结果或账本事实。
func TestRefundSameBatchSuccessAndDuplicateResultsAreIndependent(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 500},
	})
	l := openOrFail(t, path)

	// 三笔付款：p0、p1 在 aa（100、1000@100bps=>charged1010），p2 在 bb（200）。
	if r := mustSettle(t, l, 0, intent("p0", "aa", "usdc", 100)); r.Status != StatusSettled {
		t.Fatalf("settle p0: %+v", r)
	}
	if r := mustSettle(t, l, 100, intent("p1", "aa", "usdc", 1000)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}
	if r := mustSettle(t, l, 0, intent("p2", "bb", "usdc", 200)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}

	// 此前批次先成功一笔并留存结果（三笔付款都已结算，r0 退回后 aa 余 990）。
	saved0 := mustRefundOne(t, l, "r0", "p0", "earlier batch")
	wantR0 := RefundRecord{
		ID: "r0", SettlementID: "p0", Reason: "earlier batch",
		Account: "aa", Asset: "usdc", Amount: 100, Fee: 0, Charged: 100,
		AfterSeq: 3, Seq: 1,
	}
	assertRefundResultRecord(t, saved0, StatusRefundSuccess, wantR0)

	// 同一批次内连续提交相同退款请求：每项首次成功、紧随的重复申请。
	rb, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", "same batch"), // 成功：aa 990+1010=2000
		refundReq("r1", "p1", "same batch"), // 同批次重复
		refundReq("r2", "p2", "same batch"), // 成功：bb 300+200=500
		refundReq("r2", "p2", "same batch"), // 同批次重复
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := []string{
		StatusRefundSuccess, StatusDuplicate, StatusRefundSuccess, StatusDuplicate,
	}
	if got := refundStatuses(rb); !reflect.DeepEqual(got, wantStatus) {
		t.Fatalf("statuses=%v want %v", got, wantStatus)
	}
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "same batch",
		Account: "aa", Asset: "usdc", Amount: 1000, Fee: 10, Charged: 1010,
		AfterSeq: 3, Seq: 2,
	}
	wantR2 := RefundRecord{
		ID: "r2", SettlementID: "p2", Reason: "same batch",
		Account: "bb", Asset: "usdc", Amount: 200, Fee: 0, Charged: 200,
		AfterSeq: 3, Seq: 3,
	}
	wants := []RefundRecord{wantR1, wantR1, wantR2, wantR2}
	for i, want := range wants {
		assertRefundResultRecord(t, rb.Results[i], wantStatus[i], want)
	}

	// 四个结果（含成功项与重复项的 Record）必须是各自独立的分配，两两不共享。
	for i := 0; i < len(rb.Results); i++ {
		for j := i + 1; j < len(rb.Results); j++ {
			if rb.Results[i].Record == rb.Results[j].Record {
				t.Fatalf("results %d and %d share the same *RefundRecord", i, j)
			}
		}
	}

	// 逐项直接破坏性改写手里的结果：每改一项，其余尚未被改的项与此前批次保留
	// 的结果都必须保持原值，证明成功项与重复项各自持有独立副本，改动一项
	// （含其 Record 指向的完整记录）不会回波到同批次其他结果或更早保留的结果。
	corrupted := map[int]bool{}
	for i := range rb.Results {
		corruptRefundResult(&rb.Results[i])
		corrupted[i] = true
		for j, want := range wants {
			if corrupted[j] {
				continue // 已被自己那次改写，不要求保持原值
			}
			if *rb.Results[j].Record != want {
				t.Fatalf("after corrupting result %d, result %d changed:\n got %+v\nwant %+v",
					i, j, *rb.Results[j].Record, want)
			}
			if rb.Results[j].Status != wantStatus[j] {
				t.Fatalf("after corrupting result %d, result %d status=%s want %s",
					i, j, rb.Results[j].Status, wantStatus[j])
			}
		}
		if *saved0.Record != wantR0 {
			t.Fatalf("after corrupting result %d, earlier held result changed: %+v",
				i, *saved0.Record)
		}
	}

	// 账本事实不受任何结果改写影响：三条退款记录原值，余额只按真实退款变动。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Refunds, []RefundRecord{wantR0, wantR1, wantR2}) {
		t.Fatalf("ledger refunds after result edits: %+v", snap.Refunds)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("aa/usdc=%d want 2000", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 500 {
		t.Fatalf("bb/usdc=%d want 500", bal)
	}
	// 原请求重提仍 duplicate 且详情来自账本，而非任何被改写的结果。
	dup := mustRefundOne(t, l, "r1", "p1", "same batch")
	assertRefundResultRecord(t, dup, StatusDuplicate, wantR1)
}

// TestRefundResultsAcrossPaymentsAndLaterRefunds 锁定不同付款的成功退款结果
// 互不影响，且后续新增退款不能改变旧结果的编号、成功序号与金额。
func TestRefundResultsAcrossPaymentsAndLaterRefunds(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 500},
	})
	l := openOrFail(t, path)

	// p1 含手续费（charged 1010，aa），p2 无手续费（charged 200，bb）。
	if r := mustSettle(t, l, 100, intent("p1", "aa", "usdc", 1000)); r.Status != StatusSettled {
		t.Fatalf("settle p1: %+v", r)
	}
	if r := mustSettle(t, l, 0, intent("p2", "bb", "usdc", 200)); r.Status != StatusSettled {
		t.Fatalf("settle p2: %+v", r)
	}
	old1 := mustRefundOne(t, l, "r1", "p1", "cancel p1")
	old2 := mustRefundOne(t, l, "r2", "p2", "cancel p2")
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "cancel p1",
		Account: "aa", Asset: "usdc", Amount: 1000, Fee: 10, Charged: 1010,
		AfterSeq: 2, Seq: 1,
	}
	wantR2 := RefundRecord{
		ID: "r2", SettlementID: "p2", Reason: "cancel p2",
		Account: "bb", Asset: "usdc", Amount: 200, Fee: 0, Charged: 200,
		AfterSeq: 2, Seq: 2,
	}
	assertRefundResultRecord(t, old1, StatusRefundSuccess, wantR1)
	assertRefundResultRecord(t, old2, StatusRefundSuccess, wantR2)
	if old1.Record == old2.Record {
		t.Fatal("success results of different payments share one record allocation")
	}

	// 改写第二笔付款的结果：第一笔结果（此前保留）与账本中的 r2 都不受影响。
	corruptRefundResult(&old2)
	if *old1.Record != wantR1 || old1.Status != StatusRefundSuccess || old1.Charged != 1010 {
		t.Fatalf("older result changed after editing a different payment's result: %+v", old1)
	}
	if rf := mustQuery(t, l).Refunds; len(rf) != 2 || rf[1] != wantR2 {
		t.Fatalf("real r2 altered via another held result: %+v", rf)
	}

	// 后续新增一笔付款并成功退款：新退款序号续增，旧结果编号、序号、金额不变。
	if r := mustSettle(t, l, 0, intent("p3", "aa", "usdc", 50)); r.Status != StatusSettled {
		t.Fatalf("settle p3: %+v", r) // aa 2000-50=1950
	}
	new3 := mustRefundOne(t, l, "r3", "p3", "cancel p3") // aa 1950+50=2000
	wantR3 := RefundRecord{
		ID: "r3", SettlementID: "p3", Reason: "cancel p3",
		Account: "aa", Asset: "usdc", Amount: 50, Fee: 0, Charged: 50,
		AfterSeq: 3, Seq: 3,
	}
	assertRefundResultRecord(t, new3, StatusRefundSuccess, wantR3)

	// 旧结果（含已被改写的 old2 之外的 old1）编号、成功序号、金额保持首次成功时的值。
	if old1.ID != "r1" || old1.Record.ID != "r1" || old1.Record.Seq != 1 ||
		old1.Record.Charged != 1010 || old1.Record.Amount != 1000 || old1.Record.Fee != 10 {
		t.Fatalf("old r1 result changed after a later refund: %+v", old1)
	}
	if old1.SettlementID != "p1" || old1.Account != "aa" || old1.Asset != "usdc" {
		t.Fatalf("old r1 destination changed: %+v", old1)
	}

	// 改写新结果同样不能回波到旧结果或账本。
	corruptRefundResult(&new3)
	if *old1.Record != wantR1 {
		t.Fatalf("old r1 changed after editing the newest result: %+v", *old1.Record)
	}

	// 账本视角：三笔退款按成功先后排列，序号与金额稳定；旧请求重提仍 duplicate
	// 并携带最初详情（seq=1），不因新退款或任何结果改写而变化。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Refunds, []RefundRecord{wantR1, wantR2, wantR3}) {
		t.Fatalf("ledger refunds:\n got %+v\nwant %+v / %+v / %+v", snap.Refunds, wantR1, wantR2, wantR3)
	}
	if len(snap.Settlements) != 3 {
		t.Fatalf("settlements=%d want 3", len(snap.Settlements))
	}
	dup1 := mustRefundOne(t, l, "r1", "p1", "cancel p1")
	assertRefundResultRecord(t, dup1, StatusDuplicate, wantR1)
	dup2 := mustRefundOne(t, l, "r2", "p2", "cancel p2")
	assertRefundResultRecord(t, dup2, StatusDuplicate, wantR2)

	// 余额：aa 只经历 p1/r1（相抵）与 p3/r3（相抵）=>2000；bb p2/r2 相抵 =>500。
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("aa/usdc=%d want 2000", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 500 {
		t.Fatalf("bb/usdc=%d want 500", bal)
	}
}
