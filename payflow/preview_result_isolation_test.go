package payflow

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 本文件为 Preview 返回结果与真实账本之间的独立性提供回归保障。
// 预览结果中的预计成功项（settled）与重复项（duplicate）都会向调用者提供
// 结算记录（*Record），调用者可能为展示而修改这些编号、状态、说明与结算详情，
// 或调整结果列表。本文件围绕“返回数据被保留或修改后的行为”锁定：
//
//  1. 空间独立：同一批次内已结算付款的重复项、新付款的预计成功项、批内重复项
//     各自持有独立的结算详情；修改其中一项的账户、资产、付款方、金额、手续费、
//     扣款总额或成功序号，其他项仍保留原来的内容；调整列表顺序或替换其中一项，
//     也不能改变另一次调用取得的另一份预览。
//  2. 判定独立：修改预览中的历史结算详情后，查询仍看到原账户、原资产、原金额
//     与原序号，真实余额与账本文件保持不变；再次预览原请求仍返回 duplicate 和
//     真实原记录；用原编号提交合法但付款字段不同的请求仍返回 conflict。
//  3. 预计不落账：仅在预览中预计成功的新编号，即使调用者把它改成别的编号或把
//     扣款额改小，随后真实提交原请求仍按首次付款处理，实际扣款与手续费按请求
//     计算，不采用被修改的预览数据。
//  4. 时效语义：预览是“当时的结果”——后续真实付款或退款可以改变新查询与新
//     预览，但已经取得且未修改的旧结果不能跟着变化；原付款已退款时预览重复项
//     仍携带原结算详情，修改它不能改变退款历史或退回金额；余额不足的失败项
//     不带结算记录，修改它的状态不能让账本多出付款。
//
// 付款规则、预览参数、结算/退款功能与公开输出格式均不在本文件变更。

// mustPreview 执行一次预览并返回结果；批次级错误直接失败。
func mustPreview(t *testing.T, l *Ledger, batch FeeBatch) *PreviewResult {
	t.Helper()
	res, err := l.Preview(batch)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return res
}

// TestPreviewBatchItemsHoldIndependentRecords 锁定同一预览批次内各项结果的
// 独立性：批次同时含有已结算付款的重复项、新付款的预计成功项与该新编号的
// 批内重复项，三者各自携带独立的结算详情。逐项破坏性改写时，其余项保持原值；
// 调整列表顺序或替换其中一项，也不影响另一次调用取得的同内容预览。
func TestPreviewBatchItemsHoldIndependentRecords(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 1_000_000},
		{Account: "bb", Asset: "usdc", Balance: 500},
	})
	l := openOrFail(t, path)

	// 已真实结算的付款 p0：charged 1504，seq 1。
	settled := mustSettle(t, l, 30, baseScenarioPayment("p0"))
	wantP0 := baseScenarioRecord("p0", 1)
	assertItemResult(t, settled, StatusSettled, "", wantP0)

	// 预览批次：已结算付款的重复项 + 新付款的预计成功项 + 新编号的批内重复项。
	batch := FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		baseScenarioPayment("p0"), // duplicate，携带真实原记录 seq 1
		baseScenarioPayment("n1"), // settled，预计记录 seq 2
		baseScenarioPayment("n1"), // duplicate，携带同一份预计记录的内容
	}}
	wantN1 := baseScenarioRecord("n1", 2)

	res := mustPreview(t, l, batch)
	if !res.DryRun {
		t.Fatalf("preview result must carry dry_run=true")
	}
	assertItemResult(t, res.Results[0], StatusDuplicate, reasonDuplicate, wantP0)
	assertItemResult(t, res.Results[1], StatusSettled, "", wantN1)
	assertItemResult(t, res.Results[2], StatusDuplicate, reasonDuplicate, wantN1)

	// 公开 JSON 格式回归：预览携带 dry_run 标记与完整结算详情，键名固定。
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"dry_run":true`, `"id":"p0"`, `"status":"duplicate"`,
		`"id":"n1"`, `"status":"settled"`,
		`"account":"aa"`, `"paymaster":"pm-1"`, `"asset":"usdc"`,
		`"amount":1500`, `"nonce":7`, `"fee_bps":30`, `"fee":4`,
		`"charged":1504`, `"seq":1`, `"seq":2`,
	} {
		if !bytes.Contains(raw, []byte(frag)) {
			t.Fatalf("preview result JSON missing %s:\n%s", frag, raw)
		}
	}

	// 三项必须是各自独立的记录分配。
	if res.Results[0].Record == res.Results[1].Record ||
		res.Results[0].Record == res.Results[2].Record ||
		res.Results[1].Record == res.Results[2].Record {
		t.Fatal("preview items in one batch share *Record allocations")
	}

	// 不同次调用取得的结果内容相同，但也必须是独立分配、互不影响。
	other := mustPreview(t, l, batch)
	assertItemResult(t, other.Results[0], StatusDuplicate, reasonDuplicate, wantP0)
	assertItemResult(t, other.Results[1], StatusSettled, "", wantN1)
	assertItemResult(t, other.Results[2], StatusDuplicate, reasonDuplicate, wantN1)
	for i := range res.Results {
		if res.Results[i].Record == other.Results[i].Record {
			t.Fatalf("item %d: results from separate preview calls share one *Record allocation", i)
		}
	}

	// 逐项破坏性改写第一份预览：每改一项，其余尚未被改的项与另一份预览的
	// 所有项都必须保持原值。
	wants := []Record{wantP0, wantN1, wantN1}
	wantStatus := []string{StatusDuplicate, StatusSettled, StatusDuplicate}
	corrupted := map[int]bool{}
	for i := range res.Results {
		corruptItemResult(&res.Results[i])
		corrupted[i] = true
		for j := range res.Results {
			if corrupted[j] {
				continue // 已被自己那次改写，不要求保持原值
			}
			if *res.Results[j].Record != wants[j] {
				t.Fatalf("after corrupting item %d, item %d changed:\n got %+v\nwant %+v",
					i, j, *res.Results[j].Record, wants[j])
			}
			if res.Results[j].Status != wantStatus[j] {
				t.Fatalf("after corrupting item %d, item %d status=%s want %s",
					i, j, res.Results[j].Status, wantStatus[j])
			}
		}
		assertItemResult(t, other.Results[0], StatusDuplicate, reasonDuplicate, wantP0)
		assertItemResult(t, other.Results[1], StatusSettled, "", wantN1)
		assertItemResult(t, other.Results[2], StatusDuplicate, reasonDuplicate, wantN1)
	}

	// 调整第一份预览的列表顺序、替换其中一项：另一份预览不受影响。
	res.Results[0], res.Results[1] = res.Results[1], res.Results[0]
	res.Results[2] = ItemResult{ID: "forged", Status: StatusSettled, Record: &Record{ID: "forged", Seq: 99}}
	assertItemResult(t, other.Results[0], StatusDuplicate, reasonDuplicate, wantP0)
	assertItemResult(t, other.Results[1], StatusSettled, "", wantN1)
	assertItemResult(t, other.Results[2], StatusDuplicate, reasonDuplicate, wantN1)

	// 账本事实不受任何结果改写影响：仍只有 p0 一条记录，余额按真实扣款。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP0}) {
		t.Fatalf("ledger settlements after preview result edits:\n got %+v\nwant only %+v",
			snap.Settlements, wantP0)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1_000_000-1504 {
		t.Fatalf("balance changed after preview result edits: %d want %d", bal, 1_000_000-1504)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 500 {
		t.Fatalf("unrelated account balance changed: bb/usdc=%d want 500", bal)
	}
}

// TestPreviewDuplicateRecordEditsStayPrivate 锁定“修改预览中的历史结算详情”
// 不产生任何业务变更：查询仍看到原账户、原资产、原金额与原序号，真实余额与
// 账本文件内容保持不变；再次预览原请求仍返回 duplicate 和真实原记录；用原
// 编号提交合法但付款字段不同的请求仍返回 conflict。
func TestPreviewDuplicateRecordEditsStayPrivate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 800},
	})
	l := openOrFail(t, path)

	// 真实付款 p1：1500 本金 + 30bps 手续费 4，共扣 1504，aa/usdc 余 496。
	mustSettle(t, l, 30, baseScenarioPayment("p1"))
	wantP1 := baseScenarioRecord("p1", 1)

	// 预览原请求：duplicate，携带账本中的历史结算详情。
	res := mustPreview(t, l, FeeBatch{FeeBps: 30, Intents: []PaymentIntent{baseScenarioPayment("p1")}})
	assertItemResult(t, res.Results[0], StatusDuplicate, reasonDuplicate, wantP1)

	// 记录调用者改写之前的账本文件字节：单纯修改预览结果不能改写账本文件。
	fileBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 调用者破坏性改写预览里的历史结算详情（金额改小、去向改成 zz/btc、
	// 序号改成 77）。
	corruptItemResult(&res.Results[0])

	// 1. 查询仍看到原账户、原资产、原金额与原序号；只有一条结算记录。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1}) {
		t.Fatalf("real settlement altered by preview edit:\n got %+v\nwant %+v",
			snap.Settlements, wantP1)
	}

	// 2. 真实余额仍是 496：改小预览里的金额不能把钱补回来。
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("balance changed after editing preview result: %d want 496", bal)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("payment leaked to forged destination: zz/btc=%d want 0", bal)
	}

	// 3. 账本文件字节不变。
	fileAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBefore, fileAfter) {
		t.Fatalf("ledger file changed by editing a preview result")
	}

	// 4. 再次预览原请求：仍 duplicate，携带真实原记录（不是被改写的内容），
	//    且是独立分配。
	res2 := mustPreview(t, l, FeeBatch{FeeBps: 30, Intents: []PaymentIntent{baseScenarioPayment("p1")}})
	assertItemResult(t, res2.Results[0], StatusDuplicate, reasonDuplicate, wantP1)
	if res2.Results[0].Record == res.Results[0].Record {
		t.Fatal("fresh preview shares allocation with a corrupted held result")
	}

	// 5. 用原编号提交合法但付款字段不同的请求：仍 conflict，不新增记录、不扣款。
	mutations := []struct {
		name string
		mut  func(*PaymentIntent, *int)
	}{
		{"amount", func(it *PaymentIntent, _ *int) { *it.Amount = 1501 }},
		{"nonce", func(it *PaymentIntent, _ *int) { it.Nonce = 8 }},
		{"paymaster", func(it *PaymentIntent, _ *int) { it.Paymaster = "pm-2" }},
		{"account", func(it *PaymentIntent, _ *int) { it.Account = "bb" }},
		{"fee_bps", func(_ *PaymentIntent, bps *int) { *bps = 31 }},
	}
	for _, tc := range mutations {
		it := baseScenarioPayment("p1")
		bps := 30
		tc.mut(&it, &bps)
		got := mustSettle(t, l, bps, it)
		if got.Status != StatusConflict || got.Record != nil {
			t.Fatalf("mutation %s want conflict without record, got %+v", tc.name, got)
		}
	}
	if len(mustQuery(t, l).Settlements) != 1 {
		t.Fatalf("conflicts must not add settlement records")
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("conflicts changed balance: %d want 496", bal)
	}
}

// TestPreviewPredictedSuccessEditsDoNotLeaIntoRealSubmit 锁定“预计成功只存在于
// 预览里”：调用者把预计成功项改成别的编号、把扣款额改小之后，真实提交原请求
// 仍按首次付款处理，实际扣款与手续费按请求计算，不采用被修改的预览数据；
// 被伪造的编号也不会因此被占用。
func TestPreviewPredictedSuccessEditsDoNotLeaIntoRealSubmit(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	// 新编号 n1 仅在预览中预计成功：charged 1504，预计序号 1。
	res := mustPreview(t, l, FeeBatch{FeeBps: 30, Intents: []PaymentIntent{baseScenarioPayment("n1")}})
	wantN1 := baseScenarioRecord("n1", 1)
	assertItemResult(t, res.Results[0], StatusSettled, "", wantN1)

	// 调用者把预计成功项改成别的编号（HACK-ID/HACK-REC）、把扣款额改小（99）。
	corruptItemResult(&res.Results[0])

	// 真实提交原请求：仍按首次付款处理（不是 duplicate/conflict），扣款与
	// 手续费按请求计算（1504，含手续费 4），序号从 1 开始，不采用被修改的
	// 预览数据。
	got := mustSettle(t, l, 30, baseScenarioPayment("n1"))
	assertItemResult(t, got, StatusSettled, "", wantN1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("real submit must charge by the request: bal=%d want 496", bal)
	}

	// 被伪造进预览结果的编号没有因此被占用：以该编号真实付款仍按首次处理。
	forged := mustSettle(t, l, 0, intent("HACK-REC", "aa", "usdc", 10))
	if forged.Status != StatusSettled || forged.Record == nil || forged.Record.Seq != 2 {
		t.Fatalf("forged id must not be occupied by an edited preview result: %+v", forged)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 486 {
		t.Fatalf("balance after forged-id payment=%d want 486", bal)
	}

	// 账本视角：两条真实记录按成功先后排列，没有任何预测数据混入。
	snap := mustQuery(t, l)
	wantForged := Record{
		ID: "HACK-REC", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 10, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 10, Seq: 2,
	}
	if !reflect.DeepEqual(snap.Settlements, []Record{wantN1, wantForged}) {
		t.Fatalf("ledger settlements:\n got %+v\nwant %+v / %+v",
			snap.Settlements, wantN1, wantForged)
	}
}

// TestPreviewHeldResultKeepsPointInTimeMeaning 锁定预览的时效语义：预览是
// “当时的结果”。后续真实付款或退款可以改变新查询与新预览，但已经取得且
// 未修改的旧预览结果不能跟着变化。
func TestPreviewHeldResultKeepsPointInTimeMeaning(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 5000}})
	l := openOrFail(t, path)

	// 真实付款 p1：charged 1504，seq 1，余 3496。
	mustSettle(t, l, 30, baseScenarioPayment("p1"))
	wantP1 := baseScenarioRecord("p1", 1)

	// 当时预览：p1 duplicate（携带原记录），新编号 n1 预计成功（seq 2）。
	batch := FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		baseScenarioPayment("p1"),
		baseScenarioPayment("n1"),
	}}
	held := mustPreview(t, l, batch)
	wantN1 := baseScenarioRecord("n1", 2)
	assertItemResult(t, held.Results[0], StatusDuplicate, reasonDuplicate, wantP1)
	assertItemResult(t, held.Results[1], StatusSettled, "", wantN1)

	// 后续真实付款与退款改变账本：n1 真实落账（seq 2），p1 全额退款。
	realN1 := mustSettle(t, l, 30, baseScenarioPayment("n1"))
	assertItemResult(t, realN1, StatusSettled, "", wantN1)
	ref := mustRefundOne(t, l, "r1", "p1", "full refund")
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "full refund",
		Account: "aa", Asset: "usdc", Amount: 1500, Fee: 4, Charged: 1504,
		AfterSeq: 2, Seq: 1,
	}
	assertRefundResultRecord(t, ref, StatusRefundSuccess, wantR1)

	// 新查询反映新账本：两条结算、一条退款，余额 5000-1504-1504+1504=3496。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantN1}) {
		t.Fatalf("settlements after real changes:\n got %+v", snap.Settlements)
	}
	if !reflect.DeepEqual(snap.Refunds, []RefundRecord{wantR1}) {
		t.Fatalf("refunds after real changes:\n got %+v", snap.Refunds)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 3496 {
		t.Fatalf("balance after real changes=%d want 3496", bal)
	}

	// 新预览反映新账本：p1 已退款仍 duplicate 携带原记录；n1 现在已是
	// 真实结算，相同请求变成 duplicate 而不再是预计成功。
	fresh := mustPreview(t, l, batch)
	assertItemResult(t, fresh.Results[0], StatusDuplicate, reasonDuplicate, wantP1)
	assertItemResult(t, fresh.Results[1], StatusDuplicate, reasonDuplicate, wantN1)

	// 但已经取得且未修改的旧预览不能跟着变化：n1 一项仍是当时的“预计成功”，
	// p1 一项仍携带当时的原记录。
	if !held.DryRun {
		t.Fatalf("held preview lost its dry_run marker")
	}
	assertItemResult(t, held.Results[0], StatusDuplicate, reasonDuplicate, wantP1)
	assertItemResult(t, held.Results[1], StatusSettled, "", wantN1)
}

// TestPreviewRefundedOriginalDuplicateEditsStayPrivate 锁定“原付款已退款”
// 场景：预览重复项仍携带原结算详情；修改它不能改变退款历史或退回金额，
// 再次预览仍返回真实原记录。
func TestPreviewRefundedOriginalDuplicateEditsStayPrivate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	mustSettle(t, l, 30, baseScenarioPayment("p1"))
	wantP1 := baseScenarioRecord("p1", 1)
	ref := mustRefundOne(t, l, "r1", "p1", "full refund")
	wantR1 := RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "full refund",
		Account: "aa", Asset: "usdc", Amount: 1500, Fee: 4, Charged: 1504,
		AfterSeq: 1, Seq: 1,
	}
	assertRefundResultRecord(t, ref, StatusRefundSuccess, wantR1)
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("balance after refund=%d want 2000", bal)
	}

	// 原付款已退款：预览重复项仍携带原结算详情（原序号 1、原扣款 1504）。
	res := mustPreview(t, l, FeeBatch{FeeBps: 30, Intents: []PaymentIntent{baseScenarioPayment("p1")}})
	assertItemResult(t, res.Results[0], StatusDuplicate, reasonDuplicate, wantP1)

	fileBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 修改预览中的历史结算详情（金额改小、去向改成 zz/btc、序号改成 77）。
	corruptItemResult(&res.Results[0])

	// 退款历史与退回金额不变，原结算记录不变，余额不变，账本文件不变。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Refunds, []RefundRecord{wantR1}) {
		t.Fatalf("refund history altered by preview edit:\n got %+v\nwant %+v",
			snap.Refunds, wantR1)
	}
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1}) {
		t.Fatalf("settlement altered by preview edit:\n got %+v\nwant %+v",
			snap.Settlements, wantP1)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 2000 {
		t.Fatalf("refunded amount changed after preview edit: bal=%d want 2000", bal)
	}
	if bal, _ := l.Balance("zz", "btc"); bal != 0 {
		t.Fatalf("refund leaked to forged destination: zz/btc=%d want 0", bal)
	}
	fileAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBefore, fileAfter) {
		t.Fatalf("ledger file changed by editing a preview result")
	}

	// 再次预览原请求：仍 duplicate，携带真实原记录，且是独立分配。
	res2 := mustPreview(t, l, FeeBatch{FeeBps: 30, Intents: []PaymentIntent{baseScenarioPayment("p1")}})
	assertItemResult(t, res2.Results[0], StatusDuplicate, reasonDuplicate, wantP1)
	if res2.Results[0].Record == res.Results[0].Record {
		t.Fatal("fresh preview shares allocation with a corrupted held result")
	}
}

// TestPreviewFailedItemEditsAddNoPayment 锁定失败项的隔离：余额不足的预览
// 失败项不携带结算记录；调用者即使把它的状态改成“成功”并伪造一条结算
// 记录，账本也不能因此多出付款，真实提交原请求仍按真实余额判定。
func TestPreviewFailedItemEditsAddNoPayment(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)

	// 余额不足的失败项：insufficient_balance，不带结算记录。
	res := mustPreview(t, l, FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("big", "aa", "usdc", 500)}})
	if r := res.Results[0]; r.Status != StatusFunds || r.Record != nil {
		t.Fatalf("failed preview item must not carry a record: %+v", r)
	}

	fileBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 调用者把失败项伪造成“成功”：改状态、清空说明、附上一段伪造的结算记录。
	res.Results[0].Status = StatusSettled
	res.Results[0].Reason = ""
	res.Results[0].Record = &Record{
		ID: "big", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 500, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 500, Seq: 1,
	}

	// 账本不能因此多出付款：无结算记录、余额不变、账本文件不变。
	snap := mustQuery(t, l)
	if len(snap.Settlements) != 0 {
		t.Fatalf("forged preview item added a settlement: %+v", snap.Settlements)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 100 {
		t.Fatalf("balance changed by forging a preview item: %d want 100", bal)
	}
	fileAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBefore, fileAfter) {
		t.Fatalf("ledger file changed by editing a preview result")
	}

	// 真实提交原请求仍按真实余额判定：余额不足，且失败不留记录。
	got := mustSettle(t, l, 0, intent("big", "aa", "usdc", 500))
	if got.Status != StatusFunds || got.Record != nil {
		t.Fatalf("real submit must follow the real balance, got %+v", got)
	}
	if len(mustQuery(t, l).Settlements) != 0 {
		t.Fatalf("a failed payment must not add a settlement")
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 100 {
		t.Fatalf("failed payment changed balance: %d want 100", bal)
	}
}
