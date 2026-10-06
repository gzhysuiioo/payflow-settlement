package payflow

import (
	"bytes"
	"os"
	"reflect"
	"testing"
)

// 本文件为 Preview（submit --dry-run）返回结果与真实账本之间的独立性提供
// 回归保障。预览结果中的预计成功项（settled）与重复项（duplicate）都会向
// 调用者提供完整结算记录（*Record），本文件围绕这些返回数据被保留或修改
// 后的行为锁定：
//
//  1. 空间独立：调用者可以任意修改自己手里的预览结果（返回项的编号、状态、
//     说明，以及记录中的账户、资产、付款方、金额、手续费、扣款总额和成功
//     序号），改动只作用于这一份数据：真实账本（余额、结算/退款历史、账本
//     文件字节）与其他已取得的预览结果都不受影响。
//  2. 判定独立：修改预览中的历史结算详情不能改变真实记录——查询仍看到原
//     账户、原资产、原金额与原序号；再次预览原请求仍返回 duplicate 和真实
//     原记录；用原编号提交合法但付款字段不同的请求仍返回 conflict。
//  3. 改预览不等于改账本：仅在预览中预计成功的新编号，即使调用者把它改成
//     别的编号或把扣款额改小，随后真实提交原请求仍按首次付款处理，实际
//     扣款与手续费按请求计算，不采用被修改的预览数据。
//  4. 时效独立：预览是“当时的结果”——后续真实付款或退款可以改变新查询与
//     新预览，但已经取得且未修改的旧结果不能跟着变化；原付款已退款时预览
//     重复项仍携带原结算详情，修改它不能改变退款历史或退回金额；余额不足
//     的失败项继续不带结算记录，修改它的状态不能让账本多出付款。
//
// 付款规则、公开接口、PreviewResult/ItemResult/Record 的公开 JSON 字段与
// 命令行输出均不在本文件变更。

// mustPreview 发起一次预览并返回结果；批次级错误直接失败。
func mustPreview(t *testing.T, l *Ledger, batch FeeBatch) *PreviewResult {
	t.Helper()
	res, err := l.Preview(batch)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return res
}

// TestPreviewResultEditsStayPrivate 覆盖核心隔离：同一批次既含已结算付款的
// 重复项、也含新付款的预计成功项（及同编号的批内重复项），各项携带各自
// 独立的结算详情。破坏性改写其中任意一项（编号、状态、说明、账户、资产、
// 付款方、金额、手续费、扣款总额、成功序号）后：其他项保持原值，真实账本
// 与账本文件不变；再次预览原请求仍返回 duplicate 和真实原记录；用原编号
// 提交字段不同的合法请求仍 conflict；真实提交被改过预览的新编号请求仍按
// 首次付款处理，扣款与手续费按请求计算。
func TestPreviewResultEditsStayPrivate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 10000},
		{Account: "bb", Asset: "usdc", Balance: 800},
		{Account: "aa", Asset: "eth", Balance: 100},
	})
	l := openOrFail(t, path)

	// 真实落账 p1：1500@30bps，手续费 4、扣款 1504、序号 1，aa/usdc 余 8496。
	first := mustSettle(t, l, 30, baseScenarioPayment("p1"))
	assertItemResult(t, first, StatusSettled, "", baseScenarioRecord("p1", 1))
	wantP1 := baseScenarioRecord("p1", 1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 8496 {
		t.Fatalf("balance after settle=%d want 8496", bal)
	}

	// 预览批次：p1 重复项、新编号 n1 预计成功（2000@30bps：费 6、扣 2006、
	// 预计序号 2）、n1 相同请求再次出现（批内重复，携带同一份预计详情）。
	n1 := intent("n1", "aa", "usdc", 2000)
	prev := mustPreview(t, l, FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		baseScenarioPayment("p1"),
		n1,
		n1,
	}})
	if !prev.DryRun {
		t.Fatalf("preview must carry dry_run=true")
	}
	wantStatuses := []string{StatusDuplicate, StatusSettled, StatusDuplicate}
	if got := previewStatuses(prev); !reflect.DeepEqual(got, wantStatuses) {
		t.Fatalf("statuses=%v want %v", got, wantStatuses)
	}
	wantN1 := Record{
		ID: "n1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 2000, Nonce: 1, FeeBps: 30, Fee: 6, Charged: 2006, Seq: 2,
	}
	assertItemResult(t, prev.Results[0], StatusDuplicate, reasonDuplicate, wantP1)
	assertItemResult(t, prev.Results[1], StatusSettled, "", wantN1)
	assertItemResult(t, prev.Results[2], StatusDuplicate, reasonDuplicate, wantN1)

	// 三项的记录必须是各自独立的分配（内容相同的重复项也不例外）。
	if prev.Results[0].Record == prev.Results[1].Record ||
		prev.Results[1].Record == prev.Results[2].Record ||
		prev.Results[0].Record == prev.Results[2].Record {
		t.Fatal("preview items must carry independent *Record allocations")
	}

	// 预览不写账本：记录调用者改写之前的账本文件字节。
	fileBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 破坏性改写重复项携带的历史结算详情（编号、账户、资产、付款方、金额、
	// nonce、费率、手续费、扣款总额、序号全部换成伪造值）。
	corruptItemResult(&prev.Results[0])

	// 同批次其他项保持原值。
	assertItemResult(t, prev.Results[1], StatusSettled, "", wantN1)
	assertItemResult(t, prev.Results[2], StatusDuplicate, reasonDuplicate, wantN1)

	// 1. 查询仍看到原账户、原资产、原金额与原序号；真实余额不变。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1}) {
		t.Fatalf("real settlement altered by preview edit:\n got %+v\nwant %+v",
			snap.Settlements, wantP1)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 8496 {
		t.Fatalf("balance changed after editing preview: %d want 8496", bal)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("payment leaked to forged destination: zz/btc=%d want 0", bal)
	}

	// 2. 账本文件字节保持不变：只改预览数据不能触发任何写入。
	fileAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBefore, fileAfter) {
		t.Fatalf("ledger file changed by editing preview results")
	}

	// 3. 再次预览原请求仍返回 duplicate 和真实原记录，而非被修改的内容。
	again := mustPreview(t, l, FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		baseScenarioPayment("p1"),
	}})
	assertItemResult(t, again.Results[0], StatusDuplicate, reasonDuplicate, wantP1)

	// 4. 用原编号提交合法但付款字段不同的请求仍返回 conflict，不新增记录。
	mut := baseScenarioPayment("p1")
	mut.Amount = i64p(1501)
	conf := mustSettle(t, l, 30, mut)
	if conf.Status != StatusConflict || conf.Record != nil {
		t.Fatalf("mutated fields with original id want conflict without record, got %+v", conf)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 8496 {
		t.Fatalf("conflict changed balance: %d want 8496", bal)
	}
	if len(mustQuery(t, l).Settlements) != 1 {
		t.Fatalf("conflict must not add a settlement")
	}

	// 5. 改写预计成功项：把编号改成别的、扣款额改小、去向改成 zz/btc。
	corruptItemResult(&prev.Results[1])
	// 批内重复项持有自己的副本，不受波及。
	assertItemResult(t, prev.Results[2], StatusDuplicate, reasonDuplicate, wantN1)

	// 随后真实提交原请求：仍按首次付款处理（不是 duplicate/conflict），
	// 实际扣款与手续费按请求计算（费 6、扣 2006、序号 2），不采用被修改
	// 的预览数据（伪造编号、改小的扣款额、伪造去向一律不生效）。
	real := mustSettle(t, l, 30, n1)
	assertItemResult(t, real, StatusSettled, "", wantN1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 8496-2006 {
		t.Fatalf("real submit charged by forged preview data: bal=%d want %d", bal, 8496-2006)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("charge leaked to forged destination: zz/btc=%d want 0", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 800 {
		t.Fatalf("unrelated account balance changed: bb/usdc=%d want 800", bal)
	}
	snap = mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantN1}) {
		t.Fatalf("ledger settlements after real submit:\n got %+v\nwant %+v / %+v",
			snap.Settlements, wantP1, wantN1)
	}
}

// TestPreviewResultsIndependentAcrossItemsAndCalls 锁定同一份预览内各项之间、
// 以及不同次调用取得的预览之间的独立性：修改其中一项的结算详情，其他项仍
// 保留原来的内容；调整一份预览的列表顺序或替换其中一项，另一份内容相同的
// 预览不受影响；真实账本始终不变。
func TestPreviewResultsIndependentAcrossItemsAndCalls(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openOrFail(t, path)

	// 先真实落账一笔作为历史：100@0bps，序号 1。
	seed := mustSettle(t, l, 0, intent("p0", "aa", "usdc", 100))
	wantP0 := Record{
		ID: "p0", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 100, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 100, Seq: 1,
	}
	assertItemResult(t, seed, StatusSettled, "", wantP0)

	// 预览批次：历史重复项、新编号预计成功、同编号批内重复、另一新编号预计成功。
	n1 := intent("n1", "aa", "usdc", 100)
	batch := FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p0", "aa", "usdc", 100),
		n1,
		n1,
		intent("n2", "aa", "usdc", 100),
	}}
	wantN1 := Record{
		ID: "n1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 100, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 100, Seq: 2,
	}
	wantN2 := Record{
		ID: "n2", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 100, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 100, Seq: 3,
	}
	wantStatuses := []string{StatusDuplicate, StatusSettled, StatusDuplicate, StatusSettled}
	wantRecords := []Record{wantP0, wantN1, wantN1, wantN2}
	wantReasons := []string{reasonDuplicate, "", reasonDuplicate, ""}

	assertPreview := func(label string, prev *PreviewResult) {
		t.Helper()
		if got := previewStatuses(prev); !reflect.DeepEqual(got, wantStatuses) {
			t.Fatalf("%s statuses=%v want %v", label, got, wantStatuses)
		}
		for i := range prev.Results {
			if prev.Results[i].Reason != wantReasons[i] {
				t.Fatalf("%s item %d reason=%q want %q", label, i, prev.Results[i].Reason, wantReasons[i])
			}
			assertRecord(t, label, prev.Results[i].Record, wantRecords[i])
		}
	}

	// 不同次调用取得的两份预览，内容相同。
	prevA := mustPreview(t, l, batch)
	prevB := mustPreview(t, l, batch)
	assertPreview("preview A", prevA)
	assertPreview("preview B", prevB)
	if prevA.Results[1].Record == prevB.Results[1].Record {
		t.Fatal("previews from separate calls share one *Record allocation")
	}

	// 修改 A 中预计成功项的结算详情：A 的其他项保留原来的内容，B 不受影响。
	corruptItemResult(&prevA.Results[1])
	assertRecord(t, "A item 0 after editing item 1", prevA.Results[0].Record, wantP0)
	assertRecord(t, "A item 2 after editing item 1", prevA.Results[2].Record, wantN1)
	assertRecord(t, "A item 3 after editing item 1", prevA.Results[3].Record, wantN2)
	assertPreview("preview B after editing A", prevB)

	// 调整 A 的列表顺序、替换其中一项：B 与新取得的预览都不受影响。
	prevA.Results[0], prevA.Results[3] = prevA.Results[3], prevA.Results[0]
	prevA.Results[1] = prevA.Results[2]
	assertPreview("preview B after reordering A", prevB)
	prevC := mustPreview(t, l, batch)
	assertPreview("fresh preview C", prevC)

	// 账本事实不受任何预览结果改写影响：仍只有 p0 一条记录，余额不变。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP0}) {
		t.Fatalf("ledger settlements after preview edits:\n got %+v\nwant %+v",
			snap.Settlements, wantP0)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1_000_000-100 {
		t.Fatalf("balance changed after preview edits: %d want %d", bal, 1_000_000-100)
	}
}

// TestPreviewHeldResultsSurviveLaterLedgerChanges 锁定预览作为“当时的结果”：
// 后续真实付款或退款改变新查询与新预览，但已取得且未修改的旧预览保持原样；
// 原付款已退款时预览重复项仍携带原结算详情，修改它不能改变退款历史或退回
// 金额；余额不足的失败项继续不带结算记录，修改它的状态不能让账本多出付款。
func TestPreviewHeldResultsSurviveLaterLedgerChanges(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)

	// p1 真实落账：300@0bps，序号 1，余额 700。
	mustSettle(t, l, 0, intent("p1", "aa", "usdc", 300))
	wantP1 := Record{
		ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 300, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 300, Seq: 1,
	}

	// 取得并留存一份旧预览：p1 重复项 + n1 预计成功（400，预计序号 2）。
	oldPrev := mustPreview(t, l, FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 300),
		intent("n1", "aa", "usdc", 400),
	}})
	wantN1 := Record{
		ID: "n1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 400, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 400, Seq: 2,
	}
	assertItemResult(t, oldPrev.Results[0], StatusDuplicate, reasonDuplicate, wantP1)
	assertItemResult(t, oldPrev.Results[1], StatusSettled, "", wantN1)

	// 后续真实付款与退款改变账本：n1 落账（序号 2，余额 300），p1 全额退款
	// （余额回到 600）。
	realN1 := mustSettle(t, l, 0, intent("n1", "aa", "usdc", 400))
	assertItemResult(t, realN1, StatusSettled, "", wantN1)
	ref := mustRefundOne(t, l, "r1", "p1", "cancel")
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "cancel",
		Account: "aa", Asset: "usdc", Amount: 300, Fee: 0, Charged: 300,
		AfterSeq: 2, Seq: 1,
	}
	assertRefundResultRecord(t, ref, StatusRefundSuccess, wantR1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 600 {
		t.Fatalf("balance after payment and refund=%d want 600", bal)
	}

	// 已取得且未修改的旧预览不能跟着账本变化：仍是当时的内容。
	assertItemResult(t, oldPrev.Results[0], StatusDuplicate, reasonDuplicate, wantP1)
	assertItemResult(t, oldPrev.Results[1], StatusSettled, "", wantN1)

	// 原付款已退款：新预览的重复项仍携带原结算详情（序号 1、扣款 300）。
	newPrev := mustPreview(t, l, FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 300),
	}})
	assertItemResult(t, newPrev.Results[0], StatusDuplicate, reasonDuplicate, wantP1)

	// 修改旧预览中 p1 的历史结算详情：退款历史与退回金额不变。
	corruptItemResult(&oldPrev.Results[0])
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantN1}) {
		t.Fatalf("settlements altered by preview edit:\n got %+v", snap.Settlements)
	}
	if !reflect.DeepEqual(snap.Refunds, []RefundRecord{wantR1}) {
		t.Fatalf("refund history altered by preview edit:\n got %+v\nwant %+v",
			snap.Refunds, wantR1)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 600 {
		t.Fatalf("refunded amount changed by preview edit: bal=%d want 600", bal)
	}
	// 再次预览原请求仍返回 duplicate 和真实原记录。
	again := mustPreview(t, l, FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 300),
	}})
	assertItemResult(t, again.Results[0], StatusDuplicate, reasonDuplicate, wantP1)

	// 余额不足的失败项不带结算记录；把它的状态伪造成成功，账本也不会多出付款。
	fundsPrev := mustPreview(t, l, FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("big", "aa", "usdc", 10000),
	}})
	if r := fundsPrev.Results[0]; r.Status != StatusFunds || r.Record != nil {
		t.Fatalf("insufficient preview item must not carry a record: %+v", r)
	}
	fundsPrev.Results[0].Status = StatusSettled
	fundsPrev.Results[0].Reason = "forged success"
	if len(mustQuery(t, l).Settlements) != 2 {
		t.Fatalf("forged preview status added a settlement to the ledger")
	}
	// 真实提交该请求仍按真实余额判定为余额不足，不留记录。
	big := mustSettle(t, l, 0, intent("big", "aa", "usdc", 10000))
	if big.Status != StatusFunds || big.Record != nil {
		t.Fatalf("real submit of forged-success item: %+v", big)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 600 {
		t.Fatalf("forged preview status changed balance: %d want 600", bal)
	}
	if len(mustQuery(t, l).Settlements) != 2 {
		t.Fatalf("failed payment must not add a settlement")
	}
}
