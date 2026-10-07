package payflow

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// 本文件为本地账本 Ledger.Submit 锁定“同一批次内刚成功的付款编号被后续请求
// 再次使用”这一幂等判断：
//
//	首项按正常费率扣款并留下成功记录 -> 同批次后项立即复用该编号：
//	  - 其余付款内容完全一致（费率也一致）：duplicate，携带首项的完整原记录，
//	    不再次扣款、不分配新的成功序号、不占本批次额度；
//	  - 账户、付款方、资产、金额、nonce 中任一字段合法改变：conflict，
//	    不携带成功结算记录、不扣款、不留记录、不占编号与本批次额度。
//
// 关键判断顺序（见 judgeIntent）：已成功编号的 duplicate/conflict 判断先于
// 状态、溢出、本批次扣款上限与余额判断。因此即使后项所在组合余额充足、额度
// 充足，编号冲突仍必须报 conflict；反过来即使后项自己的金额在剩余余额/额度
// 下已无法支付，它得到的也必须是 conflict 而不是 limit_exceeded 或
// insufficient_balance。逐项冲突只体现在对应的那一项结果中，绝不导致整批
// 失败：批次本身合法时 Submit 返回 nil error，结果按输入顺序逐项对应。
//
// 本文件只补充回归测试，不改变任何公开调用与付款规则。

// reusedIDBaseIntent 构造同批次编号复用场景的首项：aa/usdc 付款 1000，
// 费率 30 基点，手续费 3、扣款总额 1003，付款方 pm、nonce 1。
// 选择金额 1000 是为了让“只改付款方 / 只改 nonce”的后项在金额不变时
// 手续费（3）与扣款总额（1003）与首项完全相同——若仅按金额结果判重，
// 这两个后项会被误判成 duplicate，它们必须仍被识别为另一个请求。
func reusedIDBaseIntent() PaymentIntent {
	return intent("p1", "aa", "usdc", 1000)
}

func reusedIDBaseRecord(seq int64) Record {
	return Record{
		ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 1000, Nonce: 1, FeeBps: 30, Fee: 3, Charged: 1003, Seq: seq,
	}
}

// TestSubmitSameBatchSettledIDFieldChangeIsConflict 逐字段保护同批次内的冲突
// 判断：后项参数合法且只改变账户、付款方、资产、金额或 nonce 中任意一个
// 字段时，都必须返回 conflict，不携带成功结算记录；其余付款内容与首项完全
// 一致。改变账户或资产的后项使用余额充足的组合，使 conflict 结果确实来自
// “编号已被占用”，而不是余额或额度不足。所有冲突项都不改变任何组合的余额，
// 首项记录的账户、金额、费用与成功序号保持原值。
func TestSubmitSameBatchSettledIDFieldChangeIsConflict(t *testing.T) {
	// 先锁定数值前提：金额 1000 在 30 基点下手续费为 3、扣款 1003；
	// 只改付款方或 nonce 的后项金额不变，因而手续费与扣款总额与首项逐位相同。
	if fee := feeFor(1000, 30); fee != 3 {
		t.Fatalf("setup: feeFor(1000,30)=%d want 3", fee)
	}
	const firstCharged = int64(1003)

	const (
		startAAUSDC = int64(1_000_000) // 远大于后项所需：同组合后项其实也付得起
		startBBUSDC = int64(5000)
		startAAETH  = int64(5000)
	)

	cases := []struct {
		name string
		mut  func(*PaymentIntent)
		// altAccount / altAsset / altWant 指出后项改去的组合（未改账户/资产时
		// altAccount 为空）及其应保持不变的初始余额，用于专门断言“余额充足的
		// 另一组合也没有被冲突项扣款”。
		altAccount string
		altAsset   string
		altWant    int64
	}{
		// 账户改为余额充足的 bb/usdc：若编号未被占用，这笔 1003 本可成功；
		// 它得到 conflict 只能来自编号已被首项占用。
		{"account_only", func(it *PaymentIntent) { it.Account = "bb" }, "bb", "usdc", startBBUSDC},
		// 只改付款方：金额未变，手续费与扣款总额与首项完全相同，仍是不同请求。
		{"paymaster_only", func(it *PaymentIntent) { it.Paymaster = "pm-2" }, "", "", 0},
		// 资产改为余额充足的 aa/eth：同理，conflict 与目标组合余额无关。
		{"asset_only", func(it *PaymentIntent) { it.Asset = "eth" }, "aa", "eth", startAAETH},
		// 只改金额：1001 的手续费同样是 3、扣款 1004，金额字段本身不同即冲突。
		{"amount_only", func(it *PaymentIntent) {
			v := int64(1001)
			it.Amount = &v // 必须换一个指针，不能改写首项共享的 *Amount
		}, "", "", 0},
		// 只改 nonce：手续费与扣款总额与首项完全相同，仍是不同请求。
		{"nonce_only", func(it *PaymentIntent) { it.Nonce = 2 }, "", "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := newTestLedger(t, []BalanceInit{
				{Account: "aa", Asset: "usdc", Balance: startAAUSDC},
				{Account: "bb", Asset: "usdc", Balance: startBBUSDC},
				{Account: "aa", Asset: "eth", Balance: startAAETH},
			})
			l := openOrFail(t, path)

			first := reusedIDBaseIntent()
			second := reusedIDBaseIntent()
			tc.mut(&second)

			// 同一批次：首项先成功，后项紧接着复用同一编号。
			res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{first, second}})
			if err != nil {
				t.Fatalf("a batch containing a per-item conflict must still return normally: %v", err)
			}
			if len(res.Results) != 2 {
				t.Fatalf("want 2 per-item results in submit order, got %d: %+v", len(res.Results), res.Results)
			}

			// 首项按正常费率 settled：aa/usdc 扣 1003，seq=1，记录字段为原值。
			wantFirst := reusedIDBaseRecord(1)
			r0 := res.Results[0]
			if r0.ID != "p1" {
				t.Fatalf("first result id=%q want p1 (results must follow submit order)", r0.ID)
			}
			assertItemResult(t, r0, StatusSettled, "", wantFirst)

			// 后项：conflict，无成功结算记录；编号仍是它提交时的 p1。
			r1 := res.Results[1]
			if r1.ID != "p1" {
				t.Fatalf("second result id=%q want p1", r1.ID)
			}
			if r1.Status != StatusConflict {
				t.Fatalf("second item with changed %s must be conflict, got status=%s reason=%q",
					tc.name, r1.Status, r1.Reason)
			}
			if r1.Reason != reasonConflict {
				t.Fatalf("conflict reason=%q want %q", r1.Reason, reasonConflict)
			}
			if r1.Record != nil {
				t.Fatalf("conflict item must not carry a settlement record: %+v", r1.Record)
			}
			// 公开 JSON 格式回归：conflict 结果不带 record 字段。
			raw, err := json.Marshal(r1)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(`"record"`)) {
				t.Fatalf("conflict JSON must omit the settlement record: %s", raw)
			}
			if !bytes.Contains(raw, []byte(`"status":"conflict"`)) {
				t.Fatalf("conflict JSON must carry the conflict status: %s", raw)
			}

			// 余额只被首项扣过一次：aa/usdc 余 startAAUSDC-1003；其余组合保持
			// 初始值——特别地，改账户/资产的后项即使指向余额充足的组合，也没有
			// 在那个组合上发生任何扣款。
			if bal, _ := l.Balance("aa", "usdc"); bal != startAAUSDC-firstCharged {
				t.Fatalf("aa/usdc balance=%d want %d", bal, startAAUSDC-firstCharged)
			}
			if bal, _ := l.Balance("bb", "usdc"); bal != startBBUSDC {
				t.Fatalf("bb/usdc balance changed by conflict item: %d want %d", bal, startBBUSDC)
			}
			if bal, _ := l.Balance("aa", "eth"); bal != startAAETH {
				t.Fatalf("aa/eth balance changed by conflict item: %d want %d", bal, startAAETH)
			}

			// 成功历史只留下首项一条，账户、金额、费用与成功序号均为原值；
			// 冲突项不分配新的成功序号。
			snap := mustQuery(t, l)
			if !reflect.DeepEqual(snap.Settlements, []Record{wantFirst}) {
				t.Fatalf("settlements:\n got %+v\nwant %+v", snap.Settlements, []Record{wantFirst})
			}
			if len(snap.Balances) != 3 {
				t.Fatalf("conflict item must not create balance combos: %+v", snap.Balances)
			}

			// 被 conflict 拒绝的后项不占编号资格的“冲突面”：原请求原样再提仍是
			// duplicate（首项记录仍可幂等返回），改变后的请求再提仍是 conflict——
			// 同批次内的冲突项没有替任何一种内容占住 p1 之外的判定。
			dup := mustSettle(t, l, 30, reusedIDBaseIntent())
			assertItemResult(t, dup, StatusDuplicate, reasonDuplicate, wantFirst)
			again := reusedIDBaseIntent()
			tc.mut(&again)
			cf := mustSettle(t, l, 30, again)
			if cf.Status != StatusConflict || cf.Record != nil {
				t.Fatalf("changed request must keep conflicting after the batch: %+v", cf)
			}
			if bal, _ := l.Balance("aa", "usdc"); bal != startAAUSDC-firstCharged {
				t.Fatalf("follow-up conflict/duplicate must not move balances: %d", bal)
			}
			if tc.altAccount != "" {
				if bal, _ := l.Balance(tc.altAccount, tc.altAsset); bal != tc.altWant {
					t.Fatalf("alternate combo must stay untouched after follow-up conflict: %s/%s=%d want %d",
						tc.altAccount, tc.altAsset, bal, tc.altWant)
				}
			}
		})
	}
}

// TestSubmitSameBatchIdenticalReuseIsDuplicateWithOriginalRecord 保护同批次内
// 的原样重试：首项 settled 后，付款内容完全相同的后项返回 duplicate，附带
// 首项的完整原记录，不再次扣款，也不分配新的成功序号；两项持有的是相互
// 独立的记录副本。
func TestSubmitSameBatchIdenticalReuseIsDuplicateWithOriginalRecord(t *testing.T) {
	// 2000 余额、30 基点：1500 -> 手续费 4、扣款 1504、余 496（基础场景）。
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	req := baseScenarioPayment("p1") // aa/usdc 1500，付款方 pm-1、nonce 7
	wantFirst := baseScenarioRecord("p1", 1)

	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{req, req}})
	if err != nil {
		t.Fatalf("identical in-batch reuse must return normally: %v", err)
	}
	if got := statuses(res); !reflect.DeepEqual(got, []string{StatusSettled, StatusDuplicate}) {
		t.Fatalf("statuses in submit order=%v want [settled duplicate]", got)
	}

	// duplicate 附带首项的完整原记录（全部字段逐项相等，含费率、手续费、
	// 扣款总额与成功序号），而不是一条新分配序号的记录。
	assertItemResult(t, res.Results[0], StatusSettled, "", wantFirst)
	assertItemResult(t, res.Results[1], StatusDuplicate, reasonDuplicate, wantFirst)
	if res.Results[0].Record == res.Results[1].Record {
		t.Fatal("settled and duplicate items must not share one *Record allocation")
	}

	// 不再次扣款：余额只减少一次 1504。
	if bal, _ := l.Balance("aa", "usdc"); bal != 496 {
		t.Fatalf("identical reuse charged twice: balance=%d want 496", bal)
	}
	// 不分配新的成功序号：成功历史只有首项一条，seq=1。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantFirst}) {
		t.Fatalf("history must keep only the first record:\n got %+v\nwant %+v",
			snap.Settlements, []Record{wantFirst})
	}
}

// TestSubmitSameBatchReusedIDWithChargeLimitScenario 是任务给定的连续付款
// 综合场景，把同批次编号复用的判断放在带本批次扣款上限的连续付款中保护，
// 同时锁定：
//
//  1. 已成功编号的 conflict/duplicate 判断优先于剩余额度与余额判断；
//  2. conflict 与 duplicate 都不占本批次额度，后续新编号仍能用满剩余额度；
//  3. 逐项冲突不导致整批失败，四项结果严格按提交顺序返回；
//  4. 首项记录的账户、金额、费用与成功序号保持原值。
//
// 一个尚无付款历史的账本，原账户 aa/usdc 余额与本批次上限都是 1604，
// 费率 30 基点：
//
//	1500 -> 手续费 4、扣款 1504（seq=1）
//	1501 -> 手续费 4、扣款 1505（剩余余额/额度仅 100，本应付不来）
//	1500 -> 与首项完全相同的原样请求
//	 100 -> 手续费 0、扣款 100（seq=2）
//
// 四项依次 settled、conflict、duplicate、settled。
func TestSubmitSameBatchReusedIDWithChargeLimitScenario(t *testing.T) {
	// 数值前提：1500 与 1501 的手续费都是 4（扣款 1504/1505）；100 的手续费
	// 为 0。第二项在剩余余额与额度都只有 100 时若没有“编号已成功”的前置判断，
	// 会落到 limit_exceeded；它必须得到 conflict。
	if fee := feeFor(1500, 30); fee != 4 {
		t.Fatalf("setup: feeFor(1500,30)=%d want 4", fee)
	}
	if fee := feeFor(1501, 30); fee != 4 {
		t.Fatalf("setup: feeFor(1501,30)=%d want 4", fee)
	}
	if fee := feeFor(100, 30); fee != 0 {
		t.Fatalf("setup: feeFor(100,30)=%d want 0", fee)
	}

	const startUSDC = int64(1604)
	const capUSDC = int64(1604)
	// 同账本另有两个与本批次无关的组合，用于证明冲突与重复都没有波及
	// 其他账户和资产的余额。
	const startBBUSDC = int64(2003)
	const startAAETH = int64(7)

	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: startUSDC},
		{Account: "bb", Asset: "usdc", Balance: startBBUSDC},
		{Account: "aa", Asset: "eth", Balance: startAAETH},
	})
	l := openOrFail(t, path)

	amount1501 := int64(1501)
	res, err := l.Submit(FeeBatch{
		FeeBps: 30,
		Limits: []ChargeLimit{limit("aa", "usdc", capUSDC)},
		Intents: []PaymentIntent{
			intent("p1", "aa", "usdc", 1500), // 1) 首项：正常费率扣款 1504
			{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: &amount1501, Nonce: 1, State: "pending"}, // 2) 同编号改金额
			intent("p1", "aa", "usdc", 1500), // 3) 首项原样请求
			intent("p2", "aa", "usdc", 100),  // 4) 新编号，用满剩余 100
		},
	})
	// 批次本身合法：即使中间夹着 conflict，Submit 也正常返回逐项结果。
	if err != nil {
		t.Fatalf("legal batch with per-item conflict/duplicate must not error: %v", err)
	}
	wantStatus := []string{StatusSettled, StatusConflict, StatusDuplicate, StatusSettled}
	if got := statuses(res); !reflect.DeepEqual(got, wantStatus) {
		t.Fatalf("statuses in submit order=%v want %v", got, wantStatus)
	}
	wantIDs := []string{"p1", "p1", "p1", "p2"}
	for i, wantID := range wantIDs {
		if res.Results[i].ID != wantID {
			t.Fatalf("result %d id=%q want %q (results must follow submit order)",
				i, res.Results[i].ID, wantID)
		}
	}

	// 1) 首项：手续费 4、扣款 1504、seq=1。
	wantP1 := Record{
		ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 1500, Nonce: 1, FeeBps: 30, Fee: 4, Charged: 1504, Seq: 1,
	}
	assertItemResult(t, res.Results[0], StatusSettled, "", wantP1)

	// 2) 第二项是 conflict 而不是 limit_exceeded/insufficient_balance：
	//    已成功编号的冲突判断优先于剩余额度（仅剩 100）与余额判断。
	conflictItem := res.Results[1]
	if conflictItem.Reason != reasonConflict {
		t.Fatalf("over-quota reuse must report the id conflict first, reason=%q want %q",
			conflictItem.Reason, reasonConflict)
	}
	if conflictItem.Record != nil {
		t.Fatalf("conflict item must not carry a settlement record: %+v", conflictItem.Record)
	}

	// 3) 原样请求是 duplicate，附带首项的完整原记录（seq 仍为 1），
	//    不再次扣款、不分配新的成功序号。
	assertItemResult(t, res.Results[2], StatusDuplicate, reasonDuplicate, wantP1)
	if res.Results[0].Record == res.Results[2].Record {
		t.Fatal("settled and duplicate items must not share one *Record allocation")
	}

	// 4) 新编号 100、手续费为零、扣款恰为 100：conflict 与 duplicate 都没有
	//    占用本批次额度，剩余额度与余额恰好允许这项成功，seq=2。
	wantP2 := Record{
		ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 100, Nonce: 1, FeeBps: 30, Fee: 0, Charged: 100, Seq: 2,
	}
	assertItemResult(t, res.Results[3], StatusSettled, "", wantP2)

	// 原账户余额归零（1604 - 1504 - 100）；其他账户和资产的余额保持原值。
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("aa/usdc balance=%d want 0 (1604 - 1504 - 100)", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != startBBUSDC {
		t.Fatalf("bb/usdc balance changed: %d want %d", bal, startBBUSDC)
	}
	if bal, _ := l.Balance("aa", "eth"); bal != startAAETH {
		t.Fatalf("aa/eth balance changed: %d want %d", bal, startAAETH)
	}

	// 成功历史只留下两笔付款，序号为 1、2，内容与顺序固定。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("success history:\n got %+v\nwant %+v / %+v",
			snap.Settlements, wantP1, wantP2)
	}
	if len(snap.Balances) != 3 {
		t.Fatalf("conflict/duplicate items must not create combos: %+v", snap.Balances)
	}

	// 持久化同样只留下两笔成功付款；关闭重开后余额与历史一致。
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2 := openOrFail(t, path)
	snap2 := mustQuery(t, l2)
	if !reflect.DeepEqual(snap2.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("after reopen history:\n got %+v\nwant %+v / %+v",
			snap2.Settlements, wantP1, wantP2)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("after reopen aa/usdc balance=%d want 0", bal)
	}
	if bal, _ := l2.Balance("bb", "usdc"); bal != startBBUSDC {
		t.Fatalf("after reopen bb/usdc balance=%d want %d", bal, startBBUSDC)
	}
	if bal, _ := l2.Balance("aa", "eth"); bal != startAAETH {
		t.Fatalf("after reopen aa/eth balance=%d want %d", bal, startAAETH)
	}
}
