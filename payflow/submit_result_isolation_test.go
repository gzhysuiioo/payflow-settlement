package payflow

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 本文件为 Submit 返回结果与账本结算事实之间的独立性提供回归保障。
// 首次付款成功项（settled）与幂等重复项（duplicate）都会向调用者提供完整
// 结算记录（*Record），本文件围绕这两种结果锁定：
//
//  1. 空间独立：调用者可以任意修改自己手里的成功/重复结果（返回项的编号、状态、
//     说明，以及记录中的编号、账户、资产、付款方、金额、nonce、费率、手续费、
//     扣款总额和成功序号），改动只能作用于这份结果：账本中的结算记录、真实余额
//     与付款去向都不变。
//  2. 判定独立：改大返回金额不能抬高可用余额，改编号不能释放已成功的付款编号，
//     改状态/说明不能影响后续 settled/duplicate/conflict/insufficient_balance
//     判定；只改返回数据不能新增成功记录，也不能触发账本文件改写。
//  3. 改对象不等于改请求：只改返回对象不产生任何业务变更；原请求重提仍返回
//     duplicate 和最初的完整记录、不再扣款；用原编号提交字段已合法改变的请求
//     仍返回 conflict，不能因为调用者改过旧结果就接受新内容。
//  4. 结果之间彼此独立：同批次内的成功项与重复项、不同次调用取得的结果、
//     后续新付款前后保留的旧结果，各自持有可修改副本，编号、成功序号与金额
//     互不影响。
//
// 基础场景：账户原有 2000，付款 1500、费率 30 基点、指定付款方与 nonce，
// 手续费 4、扣款 1504、余额 496。
//
// 付款规则、公开接口、ItemResult/Record 的公开 JSON 字段与命令行输出均不在
// 本文件变更。

// baseScenarioPayment 构造基础场景付款：aa/usdc 付款 1500，费率 30 基点，
// 付款方 pm-1、nonce 7。起始余额 2000 时手续费 4、扣款 1504、余 496。
func baseScenarioPayment(id string) PaymentIntent {
	it := intent(id, "aa", "usdc", 1500)
	it.Paymaster = "pm-1"
	it.Nonce = 7
	return it
}

// baseScenarioRecord 是基础场景付款成功后账本应保留的真实记录；seq 由调用处
// 按成功先后给出。
func baseScenarioRecord(id string, seq int64) Record {
	return Record{
		ID: id, Account: "aa", Paymaster: "pm-1", Asset: "usdc",
		Amount: 1500, Nonce: 7, FeeBps: 30, Fee: 4, Charged: 1504, Seq: seq,
	}
}

// corruptItemResult 像调用者为自己展示所做的那样，破坏性改写一份付款结果：
// 返回项的编号、状态、说明，以及完整记录的全部字段一律换成伪造值
// （金额改小、去向改成 zz/btc、序号改成 77）。
func corruptItemResult(r *ItemResult) {
	r.ID = "HACK-ID"
	r.Status = "forged-status"
	r.Reason = "forged reason"
	if r.Record == nil {
		return
	}
	r.Record.ID = "HACK-REC"
	r.Record.Account = "zz"
	r.Record.Paymaster = "fake-pm"
	r.Record.Asset = "btc"
	r.Record.Amount = 1
	r.Record.Nonce = 99
	r.Record.FeeBps = 9999
	r.Record.Fee = 9
	r.Record.Charged = 99
	r.Record.Seq = 77
}

// assertItemResult 断言一份付款结果（成功或重复）逐项等于预期：编号、状态、
// 说明以及完整结算记录。
func assertItemResult(t *testing.T, got ItemResult, wantStatus, wantReason string, want Record) {
	t.Helper()
	if got.ID != want.ID {
		t.Fatalf("id=%q want %q: %+v", got.ID, want.ID, got)
	}
	if got.Status != wantStatus {
		t.Fatalf("status=%s want %s: %+v", got.Status, wantStatus, got)
	}
	if got.Reason != wantReason {
		t.Fatalf("reason=%q want %q: %+v", got.Reason, wantReason, got)
	}
	if got.Record == nil {
		t.Fatalf("%s result must carry a record: %+v", wantStatus, got)
	}
	if *got.Record != want {
		t.Fatalf("record:\n got %+v\nwant %+v", *got.Record, want)
	}
}

// TestSubmitSuccessResultEditsStayPrivate 覆盖核心隔离：一笔带手续费、付款方和
// nonce 的付款首次成功后，调用者留存返回结果并任意改写（返回项编号、状态、
// 说明，以及记录的编号、账户、资产、付款方、金额、nonce、费率、手续费、扣款
// 总额和成功序号），账本中的结算事实必须原样保留；后续全额退款只能按真实原
// 记录退回原账户、原资产。
func TestSubmitSuccessResultEditsStayPrivate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 800},
		{Account: "aa", Asset: "eth", Balance: 100},
	})
	l := openOrFail(t, path)

	// 首次成功：1500 本金 + 30bps 手续费 4，共扣 1504，aa/usdc 余 496。
	first := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	wantP1 := baseScenarioRecord("p1", 1)
	assertItemResult(t, first, StatusSettled, "", wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("balance after settle=%d want 496", bal)
	}

	// 公开 JSON 格式回归：成功结果携带完整记录，键名固定；成功项不带说明。
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"id":"p1"`, `"status":"settled"`, `"record":{`,
		`"account":"aa"`, `"paymaster":"pm-1"`, `"asset":"usdc"`,
		`"amount":1500`, `"nonce":7`, `"fee_bps":30`, `"fee":4`,
		`"charged":1504`, `"seq":1`,
	} {
		if !bytes.Contains(raw, []byte(frag)) {
			t.Fatalf("success result JSON missing %s:\n%s", frag, raw)
		}
	}
	if bytes.Contains(raw, []byte(`"reason"`)) {
		t.Fatalf("settled result must not carry a reason:\n%s", raw)
	}

	// 记录调用者改写之前的账本文件字节：单纯修改返回数据不能改写账本文件。
	fileBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 调用者留存并破坏性改写手里的结果（金额改小、去向改成 zz/btc、序号改成 77）。
	corruptItemResult(&first)

	// 1. 账本查询仍看到最初成功的那笔付款，字段逐字不变；只有一条结算记录。
	snap := mustQuery(t, l)
	if len(snap.Settlements) != 1 {
		t.Fatalf("settlement count=%d want 1: %+v", len(snap.Settlements), snap.Settlements)
	}
	if snap.Settlements[0] != wantP1 {
		t.Fatalf("real settlement altered by caller edit:\n got %+v\nwant %+v",
			snap.Settlements[0], wantP1)
	}

	// 2. 真实余额仍是 496：改小返回金额不能把钱补回来，改去向也不能动其他组合。
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("balance changed after editing result: %d want 496", bal)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("payment leaked to forged destination: zz/btc=%d want 0", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 800 {
		t.Fatalf("unrelated account balance changed: bb/usdc=%d want 800", bal)
	}

	// 3. 超过真实余额 496 的新付款仍返回 insufficient_balance，且失败不留记录。
	overspend := mustSettle(t, l, 0, intent("p-over", "aa", "usdc", 497))
	if overspend.Status != StatusFunds {
		t.Fatalf("want insufficient_balance against the real balance, got %+v", overspend)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("failed payment changed balance: %d want 496", bal)
	}
	if len(mustQuery(t, l).Settlements) != 1 {
		t.Fatalf("a failed payment must not add a settlement")
	}

	// 4. 只改返回记录不释放已成功编号：原请求重提仍 duplicate 且携带真实原记录，
	//    不再扣款。
	dup := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	assertItemResult(t, dup, StatusDuplicate, reasonDuplicate, wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("duplicate charged again: bal=%d want 496", bal)
	}

	// 查询、失败付款、重复提交与调用者改写都没有触发账本文件改写。
	fileAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBefore, fileAfter) {
		t.Fatalf("ledger file changed without a successful charge:\nbefore=%s\nafter =%s",
			fileBefore, fileAfter)
	}

	// 5. 之后正常申请全额退款：必须按账本里的原记录把 1504 退回原账户 aa、
	//    原资产 usdc，而不是采用返回对象里伪造的 zz/btc 或伪造金额。
	ref := mustRefundOne(t, l, "r1", "p1", "full refund")
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "full refund",
		Account: "aa", Asset: "usdc", Amount: 1500, Fee: 4, Charged: 1504,
		AfterSeq: 1, Seq: 1,
	}
	assertRefundResultRecord(t, ref, StatusRefundSuccess, wantR1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("refund credit=%d want 2000", bal)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("refund leaked to forged destination: zz/btc=%d want 0", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 800 {
		t.Fatalf("refund leaked to another account: bb/usdc=%d want 800", bal)
	}

	// 退款后编号仍被真实结算占用：原请求重提依旧 duplicate、携带原记录、不扣款。
	again := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	assertItemResult(t, again, StatusDuplicate, reasonDuplicate, wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("retry after refund charged again: bal=%d want 2000", bal)
	}
	if len(mustQuery(t, l).Settlements) != 1 {
		t.Fatalf("retries must not add settlement records")
	}
}

// TestSubmitDuplicateCarriesOriginalDetailNotCallerEdits 区分“修改返回对象”与
// “提交已修改的请求”：前者不产生业务变更；原请求重提仍 duplicate 且携带最初
// 成功时的完整记录，而不是调用者改写后的内容；用原编号提交付款字段已合法
// 改变的请求一律 conflict，不新增记录、不重复扣款。
func TestSubmitDuplicateCarriesOriginalDetailNotCallerEdits(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 1_000_000},
		{Account: "bb", Asset: "usdc", Balance: 5000},
		{Account: "aa", Asset: "eth", Balance: 100},
	})
	l := openOrFail(t, path)

	first := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	wantP1 := baseScenarioRecord("p1", 1)
	assertItemResult(t, first, StatusSettled, "", wantP1)
	balAfter := int64(1_000_000 - 1504)
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("balance after settle=%d want %d", bal, balAfter)
	}

	// 原请求原样再次提交：duplicate，携带最初成功的完整记录，不再扣款。
	dup := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	assertItemResult(t, dup, StatusDuplicate, reasonDuplicate, wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("duplicate charged again: %d want %d", bal, balAfter)
	}
	// 公开 JSON 格式回归：duplicate 携带固定状态、说明与完整原始记录。
	draw, err := json.Marshal(dup)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"status":"duplicate"`, `"reason":"duplicate settlement attempt"`,
		`"charged":1504`, `"seq":1`,
	} {
		if !bytes.Contains(draw, []byte(frag)) {
			t.Fatalf("duplicate result JSON missing %s:\n%s", frag, draw)
		}
	}
	// 成功结果与重复结果是各自独立的记录分配。
	if first.Record == dup.Record {
		t.Fatal("settled and duplicate results share one *Record allocation")
	}

	// 改写首次成功结果：刚取得的 duplicate 结果不受波及。
	corruptItemResult(&first)
	assertItemResult(t, dup, StatusDuplicate, reasonDuplicate, wantP1)

	// 再改写 duplicate 携带的记录：再次重提原请求，得到的仍是真实原始详情。
	corruptItemResult(&dup)
	dup2 := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	assertItemResult(t, dup2, StatusDuplicate, reasonDuplicate, wantP1)
	if dup2.Record == dup.Record {
		t.Fatal("fresh duplicate result shares allocation with a corrupted held result")
	}

	// 用原编号提交“付款字段已合法改变”的请求：每种单项改动都仍为 conflict，
	// 不能因为调用者改过旧结果就接受新内容（目标账户/资产即使余额充足也一样，
	// 因为重复/冲突判定先于余额）。
	mutations := []struct {
		name string
		mut  func(*PaymentIntent, *int)
	}{
		{"amount", func(it *PaymentIntent, _ *int) { *it.Amount = 1501 }},
		{"nonce", func(it *PaymentIntent, _ *int) { it.Nonce = 8 }},
		{"paymaster", func(it *PaymentIntent, _ *int) { it.Paymaster = "pm-2" }},
		{"account", func(it *PaymentIntent, _ *int) { it.Account = "bb" }},
		{"asset", func(it *PaymentIntent, _ *int) { it.Asset = "eth" }},
		{"fee_bps", func(_ *PaymentIntent, bps *int) { *bps = 31 }},
	}
	for _, tc := range mutations {
		it := baseScenarioPayment("p1")
		bps := 30
		tc.mut(&it, &bps)
		res := mustSettle(t, l, bps, it)
		if res.Status != StatusConflict || res.Record != nil {
			t.Fatalf("mutation %s want conflict without record, got %+v", tc.name, res)
		}
	}

	// 所有冲突都不新增记录、不改变原记录、不扣余额。
	snap := mustQuery(t, l)
	if len(snap.Settlements) != 1 || snap.Settlements[0] != wantP1 {
		t.Fatalf("ledger settlements after conflicts:\n got %+v\nwant only %+v",
			snap.Settlements, wantP1)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("conflicts changed balance: %d want %d", bal, balAfter)
	}

	// 最后原请求再次重提，仍返回 duplicate 和最初的完整记录。
	dup3 := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	assertItemResult(t, dup3, StatusDuplicate, reasonDuplicate, wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("duplicate after conflicts charged again: %d want %d", bal, balAfter)
	}
}

// TestSubmitSameBatchSettledAndDuplicateResultsAreIndependent 锁定同一批次内
// 连续提交相同合法付款时的结果隔离：首项 settled、后项 duplicate，整批只扣款
// 一次；两项各自持有可修改的记录副本，改动其中一项不能改变另一项，也不能
// 波及此前批次保留的结果、另一次调用取得的结果或账本事实。
func TestSubmitSameBatchSettledAndDuplicateResultsAreIndependent(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 1_000_000},
		{Account: "bb", Asset: "usdc", Balance: 500},
	})
	l := openOrFail(t, path)

	// 此前批次先成功一笔并留存结果：100@0bps => charged 100，seq=1。
	saved0 := mustSettle(t, l, 0, intent("p0", "aa", "usdc", 100))
	wantP0 := Record{
		ID: "p0", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 100, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 100, Seq: 1,
	}
	assertItemResult(t, saved0, StatusSettled, "", wantP0)

	// 同一批次连续提交相同的合法付款：首项 settled、后项 duplicate。
	req := baseScenarioPayment("p1")
	rb, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{req, req}})
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(rb); !reflect.DeepEqual(got, []string{StatusSettled, StatusDuplicate}) {
		t.Fatalf("statuses=%v want [settled duplicate]", got)
	}
	wantP1 := baseScenarioRecord("p1", 2)
	assertItemResult(t, rb.Results[0], StatusSettled, "", wantP1)
	assertItemResult(t, rb.Results[1], StatusDuplicate, reasonDuplicate, wantP1)

	// 整批只扣款一次：1_000_000 - 100(p0) - 1504(p1)。
	if bal, _ := l.Balance("aa", "usdc"); bal != 1_000_000-100-1504 {
		t.Fatalf("batch charged more than once: bal=%d want %d",
			bal, 1_000_000-100-1504)
	}
	if len(mustQuery(t, l).Settlements) != 2 {
		t.Fatalf("settlement count=%d want 2", len(mustQuery(t, l).Settlements))
	}
	// 同批次两项必须是各自独立的记录分配。
	if rb.Results[0].Record == rb.Results[1].Record {
		t.Fatal("settled and duplicate items share one *Record allocation")
	}

	// 不同次调用取得的结果同样独立：单独再取一次 duplicate，与批内两项都不共享。
	dupSep := mustSettle(t, l, 30, req)
	assertItemResult(t, dupSep, StatusDuplicate, reasonDuplicate, wantP1)
	if dupSep.Record == rb.Results[0].Record || dupSep.Record == rb.Results[1].Record {
		t.Fatal("duplicate from a separate call shares allocation with batch results")
	}

	// 逐项破坏性改写手里的结果：每改一项，其余尚未被改的项、此前批次保留的
	// 结果、另一次调用取得的结果都必须保持原值。
	wants := []Record{wantP1, wantP1}
	wantStatus := []string{StatusSettled, StatusDuplicate}
	corrupted := map[int]bool{}
	for i := range rb.Results {
		corruptItemResult(&rb.Results[i])
		corrupted[i] = true
		for j := range rb.Results {
			if corrupted[j] {
				continue // 已被自己那次改写，不要求保持原值
			}
			if *rb.Results[j].Record != wants[j] {
				t.Fatalf("after corrupting item %d, item %d changed:\n got %+v\nwant %+v",
					i, j, *rb.Results[j].Record, wants[j])
			}
			if rb.Results[j].Status != wantStatus[j] {
				t.Fatalf("after corrupting item %d, item %d status=%s want %s",
					i, j, rb.Results[j].Status, wantStatus[j])
			}
			wantReason := ""
			if j == 1 {
				wantReason = reasonDuplicate
			}
			if rb.Results[j].Reason != wantReason {
				t.Fatalf("after corrupting item %d, item %d reason=%q want %q",
					i, j, rb.Results[j].Reason, wantReason)
			}
		}
		if *saved0.Record != wantP0 {
			t.Fatalf("after corrupting item %d, earlier held result changed: %+v",
				i, *saved0.Record)
		}
		assertItemResult(t, dupSep, StatusDuplicate, reasonDuplicate, wantP1)
	}

	// 改写另一次调用的结果也不能回波到此前批次保留的结果或账本。
	corruptItemResult(&dupSep)
	if *saved0.Record != wantP0 {
		t.Fatalf("earlier held result changed after corrupting separate-call result: %+v",
			*saved0.Record)
	}

	// 账本事实不受任何结果改写影响：两条结算记录原值，整批只扣过一次。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP0, wantP1}) {
		t.Fatalf("ledger settlements after result edits:\n got %+v\nwant %+v / %+v",
			snap.Settlements, wantP0, wantP1)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1_000_000-100-1504 {
		t.Fatalf("balance changed after result edits: %d", bal)
	}

	// 原请求重提仍 duplicate 且详情来自账本，而非任何被改写的结果，且不再扣款。
	again := mustSettle(t, l, 30, req)
	assertItemResult(t, again, StatusDuplicate, reasonDuplicate, wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 1_000_000-100-1504 {
		t.Fatalf("retry charged again: bal=%d", bal)
	}
}

// TestSubmitHeldResultsStayIndependentAcrossCallsAndLaterPayments 锁定不同次
// 调用取得的结果彼此独立，且后续新付款追加记录不能改动此前保留且未修改的
// 原始结果（编号、成功序号、金额、手续费、扣款总额都保持首次成功时的值）。
func TestSubmitHeldResultsStayIndependentAcrossCallsAndLaterPayments(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 500},
	})
	l := openOrFail(t, path)

	// p1：基础场景付款，charged 1504，aa/usdc 余 496，seq=1。
	old1 := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	wantP1 := baseScenarioRecord("p1", 1)
	assertItemResult(t, old1, StatusSettled, "", wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("balance after p1=%d want 496", bal)
	}

	// 另一次调用取得 duplicate：详情相同，但是独立分配，两者互不共享。
	dup1 := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	assertItemResult(t, dup1, StatusDuplicate, reasonDuplicate, wantP1)
	if old1.Record == dup1.Record {
		t.Fatal("results from separate calls share one *Record allocation")
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("duplicate charged again: bal=%d want 496", bal)
	}

	// 后续新付款 p2 落在另一账户，追加 seq=2（200@0bps => charged 200）。
	old2 := mustSettle(t, l, 0, intent("p2", "bb", "usdc", 200))
	wantP2 := Record{
		ID: "p2", Account: "bb", Paymaster: "pm", Asset: "usdc",
		Amount: 200, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 200, Seq: 2,
	}
	assertItemResult(t, old2, StatusSettled, "", wantP2)

	// 新付款追加记录不能改动此前保留的原始结果。
	assertItemResult(t, old1, StatusSettled, "", wantP1)
	assertItemResult(t, dup1, StatusDuplicate, reasonDuplicate, wantP1)

	// 改写新付款的结果：旧结果（含另一次调用取得的 duplicate）保持原值。
	corruptItemResult(&old2)
	assertItemResult(t, old1, StatusSettled, "", wantP1)
	assertItemResult(t, dup1, StatusDuplicate, reasonDuplicate, wantP1)

	// 再改写 duplicate 结果：首次成功结果与账本都不受影响。
	corruptItemResult(&dup1)
	assertItemResult(t, old1, StatusSettled, "", wantP1)

	// 账本视角：两条记录按成功先后排列，金额与序号稳定；余额按真实扣款变动。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("ledger settlements:\n got %+v\nwant %+v / %+v",
			snap.Settlements, wantP1, wantP2)
	}
	if !reflect.DeepEqual(snap.Balances, []BalanceView{
		{Account: "aa", Asset: "usdc", Balance: 496},
		{Account: "bb", Asset: "usdc", Balance: 300},
	}) {
		t.Fatalf("ledger balances: %+v", snap.Balances)
	}

	// 原请求重提仍 duplicate 且携带最初详情（seq=1、charged=1504），不再扣款。
	again := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	assertItemResult(t, again, StatusDuplicate, reasonDuplicate, wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("retry charged again: bal=%d want 496", bal)
	}
	if len(mustQuery(t, l).Settlements) != 2 {
		t.Fatalf("retries must not add settlement records")
	}
}
