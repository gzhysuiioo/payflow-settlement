package payflow

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 本文件为 Submit 返回结果与账本结算事实之间的独立性提供回归保障。
// 付款首次成功项（settled）与幂等重复项（duplicate）都会向调用者提供完整
// 结算记录（Record），本文件围绕这两种结果锁定：
//
//  1. 空间独立：调用者可以任意修改自己手里的成功/重复结果（项的编号、状态、
//     说明，以及记录中的编号、账户、付款方、资产、金额、nonce、费率、手续费、
//     扣款总额与成功序号），改动只能作用于这份结果：账本中的结算记录、真实
//     余额与扣款去向都不变。
//  2. 判定独立：改大返回金额不能抬高可用余额，改状态/说明不能影响后续
//     duplicate/conflict/insufficient_balance 判定，改编号也不能新增成功记录。
//  3. 改对象不等于改请求：只改返回对象不产生任何业务变更；原请求原样重提仍
//     duplicate 且携带最初成功的完整记录；用原编号提交付款字段已合法改变的
//     请求仍 conflict，不能因为调用者改过旧结果就接受新内容。
//  4. 结果之间彼此独立：同批次内的成功项与重复项、不同次调用取得的结果，
//     各自持有可修改副本；后续新付款追加记录不能改动此前保留且未修改的结果。
//  5. 只读取或修改返回数据不触发写盘；之后的全额退款仍按账本中的原记录把
//     原扣款总额退回原账户、原资产，不采用返回对象里伪造的去向或金额。
//
// 付款规则、公开接口、ItemResult/Record 的公开 JSON 字段与命令行输出均不在
// 本文件变更。

// corruptItemResult 像调用者为自己展示所做的那样，破坏性改写一项提交结果：
// 项的编号、状态、说明与完整结算记录的全部字段一律换成伪造值。
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
	r.Record.Nonce = 4242
	r.Record.FeeBps = 9999
	r.Record.Fee = 9
	r.Record.Charged = 999_999
	r.Record.Seq = 77
}

// assertItemRecord 断言一项提交结果（成功或重复）的编号、状态、说明与完整
// 结算记录逐字段等于预期：duplicate 必须携带固定的重复说明，settled 的说明
// 必须为空；两者都必须携带记录。
func assertItemRecord(t *testing.T, got ItemResult, wantStatus string, want Record) {
	t.Helper()
	if got.Status != wantStatus {
		t.Fatalf("status=%s want %s: %+v", got.Status, wantStatus, got)
	}
	if got.ID != want.ID {
		t.Fatalf("id=%q want %q: %+v", got.ID, want.ID, got)
	}
	switch wantStatus {
	case StatusSettled:
		if got.Reason != "" {
			t.Fatalf("settled result must carry no reason, got %q: %+v", got.Reason, got)
		}
	case StatusDuplicate:
		if got.Reason != reasonDuplicate {
			t.Fatalf("duplicate reason=%q want %q: %+v", got.Reason, reasonDuplicate, got)
		}
	}
	if got.Record == nil {
		t.Fatalf("%s result must carry a record: %+v", wantStatus, got)
	}
	if *got.Record != want {
		t.Fatalf("record:\n got %+v\nwant %+v", *got.Record, want)
	}
}

// TestSubmitSuccessResultEditsStayPrivate 覆盖核心隔离：以一笔带手续费、
// 付款方和 nonce 的成功付款为基础（2000 本金、1500 金额、30 基点 => 手续费 4、
// 扣款 1504、余额 496），调用者留存返回项并任意改写（编号、状态、说明与
// 记录的全部字段），账本中的结算事实必须原样保留。
func TestSubmitSuccessResultEditsStayPrivate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 500},
	})
	l := openOrFail(t, path)

	// 首次付款成功：1500*30/10000 向下取整 => fee=4，charged=1504，aa 余 496。
	first := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 1500))
	wantP1 := Record{
		ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 1500, Nonce: 1, FeeBps: 30, Fee: 4, Charged: 1504, Seq: 1,
	}
	assertItemRecord(t, first, StatusSettled, wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("balance after settle=%d want 496", bal)
	}

	// 公开 JSON 格式回归：成功结果携带完整记录，键名固定。
	raw, err := json.Marshal(BatchResult{Results: []ItemResult{first}})
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"id":"p1"`, `"status":"settled"`, `"record":{`,
		`"account":"aa"`, `"paymaster":"pm"`, `"asset":"usdc"`,
		`"amount":1500`, `"nonce":1`, `"fee_bps":30`, `"fee":4`,
		`"charged":1504`, `"seq":1`,
	} {
		if !bytes.Contains(raw, []byte(frag)) {
			t.Fatalf("success result JSON missing %s:\n%s", frag, raw)
		}
	}

	// 此后除真实退款外不应再有任何写盘：先固定账本文件字节。
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 调用者留存并破坏性改写手里的结果：项的编号、状态、说明，记录的编号、
	// 账户、付款方、资产、金额、nonce、费率、手续费、扣款总额、成功序号全部伪造。
	corruptItemResult(&first)

	// 1. 账本查询仍看到最初成功的那笔付款，字段逐字不变；余额仍是真实余额。
	snap := mustQuery(t, l)
	if len(snap.Settlements) != 1 || snap.Settlements[0] != wantP1 {
		t.Fatalf("real settlement altered by caller edit:\n got %+v\nwant %+v",
			snap.Settlements, wantP1)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("balance changed after editing result: %d want 496", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 500 {
		t.Fatalf("unrelated balance changed: bb/usdc=%d want 500", bal)
	}

	// 2. 改大返回金额/扣款总额不能抬高购买力：超过 496 的新付款仍余额不足，
	//    失败不扣款、不留记录，伪造去向 zz/btc 也不会出现真实组合。
	over := mustSettle(t, l, 0, intent("p-over", "aa", "usdc", 497))
	if over.Status != StatusFunds || over.Record != nil {
		t.Fatalf("want insufficient_balance without record, got %+v", over)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("failed overspend changed balance: %d want 496", bal)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("forged destination leaked into real balances: zz/btc=%d", bal)
	}

	// 3. 原请求原样再次提交：duplicate，携带最初成功的完整记录，不再扣款。
	dup := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 1500))
	assertItemRecord(t, dup, StatusDuplicate, wantP1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("duplicate charged again: bal=%d want 496", bal)
	}

	// 调用者再改写 duplicate 携带的记录，原请求重提仍得到真实的原始详情。
	corruptItemResult(&dup)
	dup2 := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 1500))
	assertItemRecord(t, dup2, StatusDuplicate, wantP1)

	// duplicate 结果的公开 JSON 同样锁定：状态与固定说明 + 完整原记录。
	rawDup, err := json.Marshal(BatchResult{Results: []ItemResult{dup2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"status":"duplicate"`, `"reason":"duplicate settlement attempt"`,
		`"charged":1504`, `"fee":4`, `"seq":1`,
	} {
		if !bytes.Contains(rawDup, []byte(frag)) {
			t.Fatalf("duplicate result JSON missing %s:\n%s", frag, rawDup)
		}
	}

	// 4. “改对象”不等于“改请求”：用原编号提交付款字段已合法改变（或费率不同）
	//    的请求，仍一律 conflict，不能因为旧结果被改过就接受新内容；conflict
	//    项不携带记录、不扣款、不留新记录。
	changed := []FeeBatch{
		{FeeBps: 30, Intents: []PaymentIntent{
			{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: i64p(100), Nonce: 1, State: "pending"},
		}}, // 金额改变（本身合法、按真实余额也足够）
		{FeeBps: 30, Intents: []PaymentIntent{
			{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: i64p(1500), Nonce: 2, State: "pending"},
		}}, // nonce 改变
		{FeeBps: 30, Intents: []PaymentIntent{
			{ID: "p1", Account: "aa", Paymaster: "pm-other", Asset: "usdc", Amount: i64p(1500), Nonce: 1, State: "pending"},
		}}, // 付款方改变
		{FeeBps: 30, Intents: []PaymentIntent{
			{ID: "p1", Account: "bb", Paymaster: "pm", Asset: "usdc", Amount: i64p(1500), Nonce: 1, State: "pending"},
		}}, // 账户改变
		{FeeBps: 30, Intents: []PaymentIntent{
			{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "eth", Amount: i64p(1500), Nonce: 1, State: "pending"},
		}}, // 资产改变
		{FeeBps: 31, Intents: []PaymentIntent{
			{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: i64p(1500), Nonce: 1, State: "pending"},
		}}, // 费率改变
	}
	for i, b := range changed {
		res, err := l.Submit(b)
		if err != nil {
			t.Fatalf("changed case %d: %v", i, err)
		}
		cf := res.Results[0]
		if cf.Status != StatusConflict || cf.Reason != reasonConflict || cf.Record != nil {
			t.Fatalf("changed case %d want conflict without record, got %+v", i, cf)
		}
		rawCf, err := json.Marshal(cf)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(rawCf, []byte(`"record"`)) {
			t.Fatalf("conflict result must not carry a record: %s", rawCf)
		}
		if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
			t.Fatalf("changed case %d altered balance: %d want 496", i, bal)
		}
	}

	// 以上所有改写、重提、冲突、余额不足失败都不能新增成功记录或改写账本文件。
	if got := mustQuery(t, l).Settlements; len(got) != 1 || got[0] != wantP1 {
		t.Fatalf("settlements must stay the single original:\n got %+v\nwant %+v", got, wantP1)
	}
	diskAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(diskBefore, diskAfter) {
		t.Fatalf("ledger file changed without a real successful operation:\nbefore=%s\nafter =%s",
			diskBefore, diskAfter)
	}

	// 5. 只改返回记录没有释放已成功的编号：之后正常申请全额退款，仍按账本里
	//    的原记录把 charged=1504 退回原账户 aa、原资产 usdc，而不是伪造的
	//    zz/btc 或伪造金额。
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "full refund",
		Account: "aa", Asset: "usdc", Amount: 1500, Fee: 4, Charged: 1504,
		AfterSeq: 1, Seq: 1,
	}
	rf := mustRefundOne(t, l, "r1", "p1", "full refund")
	assertRefundResultRecord(t, rf, StatusRefundSuccess, wantR1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("refund must restore original charged amount: bal=%d want 2000", bal)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("refund followed the forged destination: zz/btc=%d want 0", bal)
	}
	// 原结算记录在退款后仍保持原样。
	if got := mustQuery(t, l).Settlements; len(got) != 1 || got[0] != wantP1 {
		t.Fatalf("original settlement altered by refund: %+v", got)
	}
}

// TestSubmitSameBatchSettledAndDuplicateResultsAreIndependent 锁定同一批次内
// 连续提交相同合法付款时的结果隔离：首项 settled、后项 duplicate，整批只扣款
// 一次；两项携带相同的原始结算详情，但各自持有独立副本，改动其中一项不能
// 波及另一项、更早批次保留的结果或账本事实。
func TestSubmitSameBatchSettledAndDuplicateResultsAreIndependent(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	wantP1 := Record{
		ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 1500, Nonce: 1, FeeBps: 30, Fee: 4, Charged: 1504, Seq: 1,
	}

	// 同一批次连续提交相同的合法付款：首项 settled，其余 duplicate。
	rb, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 1500),
		intent("p1", "aa", "usdc", 1500),
		intent("p1", "aa", "usdc", 1500),
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := []string{StatusSettled, StatusDuplicate, StatusDuplicate}
	if got := statuses(rb); !reflect.DeepEqual(got, wantStatus) {
		t.Fatalf("statuses=%v want %v", got, wantStatus)
	}
	for i, st := range wantStatus {
		assertItemRecord(t, rb.Results[i], st, wantP1)
	}

	// 整批只扣款一次：1504，余额 496，只有一条成功记录。
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("batch charged more than once: bal=%d want 496", bal)
	}
	if got := mustQuery(t, l).Settlements; len(got) != 1 || got[0] != wantP1 {
		t.Fatalf("ledger settlements after batch:\n got %+v\nwant %+v", got, wantP1)
	}

	// 三个结果（含成功项与重复项的 Record）必须是各自独立的分配，两两不共享。
	for i := 0; i < len(rb.Results); i++ {
		for j := i + 1; j < len(rb.Results); j++ {
			if rb.Results[i].Record == rb.Results[j].Record {
				t.Fatalf("results %d and %d share the same *Record", i, j)
			}
		}
	}

	// 逐项破坏性改写：每改一项，其余尚未被改的项必须保持原值，证明同批次内
	// settled 与 duplicate 各自持有独立副本，改动不会回波到其他结果。
	corrupted := map[int]bool{}
	for i := range rb.Results {
		corruptItemResult(&rb.Results[i])
		corrupted[i] = true
		for j, st := range wantStatus {
			if corrupted[j] {
				continue
			}
			if *rb.Results[j].Record != wantP1 {
				t.Fatalf("after corrupting result %d, result %d changed:\n got %+v\nwant %+v",
					i, j, *rb.Results[j].Record, wantP1)
			}
			if rb.Results[j].Status != st {
				t.Fatalf("after corrupting result %d, result %d status=%s want %s",
					i, j, rb.Results[j].Status, st)
			}
			if st == StatusDuplicate && rb.Results[j].Reason != reasonDuplicate {
				t.Fatalf("after corrupting result %d, result %d reason altered: %+v",
					i, j, rb.Results[j])
			}
		}
	}

	// 账本事实不受任何结果改写影响；原请求重提仍 duplicate 且详情来自账本。
	if got := mustQuery(t, l).Settlements; len(got) != 1 || got[0] != wantP1 {
		t.Fatalf("ledger settlements altered via held results: %+v", got)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("balance changed after result edits: %d want 496", bal)
	}
	again := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 1500))
	assertItemRecord(t, again, StatusDuplicate, wantP1)
}

// TestSubmitResultsIndependentAcrossCallsAndLaterPayments 锁定不同次调用取得的
// 结果彼此独立；后续新付款追加记录时，此前保留且未修改的原始结果（编号、
// 账户、付款方、资产、金额、nonce、费率、手续费、扣款总额、成功序号）不变。
func TestSubmitResultsIndependentAcrossCallsAndLaterPayments(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	wantP1 := Record{
		ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 1500, Nonce: 1, FeeBps: 30, Fee: 4, Charged: 1504, Seq: 1,
	}

	// 第一次调用取得成功结果并保留。
	first := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 1500))
	assertItemRecord(t, first, StatusSettled, wantP1)

	// 另一次调用取得 duplicate 结果：与首次结果内容相同但是独立分配。
	dup := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 1500))
	assertItemRecord(t, dup, StatusDuplicate, wantP1)
	if first.Record == dup.Record {
		t.Fatal("success and duplicate results across calls share one record allocation")
	}

	// 改写其中一份，另一份不动；账本事实也不动。
	corruptItemResult(&dup)
	if *first.Record != wantP1 || first.Status != StatusSettled {
		t.Fatalf("first-call result changed after editing a later duplicate result: %+v", first)
	}
	corruptItemResult(&first)
	if got := mustQuery(t, l).Settlements; len(got) != 1 || got[0] != wantP1 {
		t.Fatalf("real ledger altered by editing held results: %+v", got)
	}

	// 再保留一份未修改的 duplicate 结果，供后续追加付款后比对。
	saved := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 1500))
	assertItemRecord(t, saved, StatusDuplicate, wantP1)

	// 后续一笔新付款成功追加（496 余额内：400@0bps => charged 400，余 96，seq=2）。
	p2 := mustSettle(t, l, 0, intent("p2", "aa", "usdc", 400))
	wantP2 := Record{
		ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 400, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 400, Seq: 2,
	}
	assertItemRecord(t, p2, StatusSettled, wantP2)
	if bal, _ := l.Balance("aa", "usdc"); bal != 96 {
		t.Fatalf("balance after p2=%d want 96", bal)
	}

	// 此前保留且未修改的原始结果逐字段不变：成功序号仍是 1，金额仍是 1504。
	assertItemRecord(t, saved, StatusDuplicate, wantP1)

	// 账本视角两条记录按成功先后排列，原记录不被新追加改写。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("ledger settlements:\n got %+v\nwant %+v / %+v",
			snap.Settlements, wantP1, wantP2)
	}

	// 原请求再次重提仍返回 duplicate 与最初详情，与新付款无关。
	again := mustSettle(t, l, 30, intent("p1", "aa", "usdc", 1500))
	assertItemRecord(t, again, StatusDuplicate, wantP1)
}
