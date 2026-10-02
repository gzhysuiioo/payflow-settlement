package payflow

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func limit(account, asset string, max int64) ChargeLimit {
	return ChargeLimit{Account: account, Asset: asset, MaxCharged: i64p(max)}
}

// 基本限额：累计恰好等于上限时允许成功，超出则 limit_exceeded；
// 超限项不扣款、不留记录，后项继续。
func TestLimitBasic(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 1000)}, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 600), // 成功，used=600
		intent("p2", "aa", "usdc", 400), // 恰好 1000，成功
		intent("p3", "aa", "usdc", 1),   // 1001 > 1000，超限
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusSettled, StatusLimit}
	if got := statuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	// 超限原因说明上限、已用额度与本项需扣金额。
	r := res.Results[2]
	if !strings.Contains(r.Reason, "max_charged 1000") ||
		!strings.Contains(r.Reason, "already charged in batch 1000") ||
		!strings.Contains(r.Reason, "this intent needs 1") {
		t.Fatalf("limit reason missing details: %q", r.Reason)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 9000 {
		t.Fatalf("balance=%d want 9000 (超限项不扣款)", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 2 || snap.Settlements[0].ID != "p1" || snap.Settlements[1].ID != "p2" {
		t.Fatalf("settlements=%+v", snap.Settlements)
	}
}

// 不同 (账户, 资产) 组合的额度分别计算、互不借用。
func TestLimitPerComboIsolation(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 10000},
		{Account: "bb", Asset: "usdc", Balance: 10000},
	})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{
		limit("aa", "usdc", 100),
		limit("bb", "usdc", 100),
	}, Intents: []PaymentIntent{
		intent("a1", "aa", "usdc", 100), // 成功，aa 额度用尽
		intent("b1", "bb", "usdc", 100), // 成功，bb 额度用尽（不借用 aa 的剩余）
		intent("a2", "aa", "usdc", 1),   // aa 超限
		intent("b2", "bb", "usdc", 1),   // bb 超限
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusSettled, StatusLimit, StatusLimit}
	if got := statuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
}

// 未列出的组合沿用现有行为（不限制扣款）。
func TestLimitUnlistedComboUnrestricted(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 10000},
		{Account: "zz", Asset: "eth", Balance: 10000},
	})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 100)}, Intents: []PaymentIntent{
		intent("z1", "zz", "eth", 5000), // zz/eth 未列出，不受限
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("unlisted combo want settled, got %+v", res.Results[0])
	}
}

// max_charged=0 禁止该组合新增扣款。
func TestLimitZeroForbids(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 0)}, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 1), // 0 < 1，超限
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusLimit {
		t.Fatalf("zero limit want limit_exceeded, got %+v", res.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 10000 {
		t.Fatalf("balance=%d want 10000", bal)
	}
}

// 非法限额项整次拒绝（ErrInvalid），任何意图都不执行；空意图列表也不例外。
func TestLimitInvalidRejectsBatch(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	cases := []struct {
		name   string
		limits []ChargeLimit
	}{
		{"empty account", []ChargeLimit{limit("", "usdc", 100)}},
		{"empty asset", []ChargeLimit{limit("aa", "", 100)}},
		{"missing max_charged", []ChargeLimit{{Account: "aa", Asset: "usdc", MaxCharged: nil}}},
		{"negative max_charged", []ChargeLimit{limit("aa", "usdc", -1)}},
		{"duplicate combo", []ChargeLimit{limit("aa", "usdc", 100), limit("aa", "usdc", 200)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 即使带了会成功的意图，也整次拒绝、不执行。
			_, err := l.Submit(FeeBatch{FeeBps: 0, Limits: tc.limits, Intents: []PaymentIntent{
				intent("p", "aa", "usdc", 1),
			}})
			if err == nil || KindOf(err) != ErrInvalid {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}

	// 空意图列表也要检查限额。
	_, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", -1)}, Intents: nil})
	if err == nil || KindOf(err) != ErrInvalid {
		t.Fatalf("empty intents with invalid limit want ErrInvalid, got %v", err)
	}
	// 非法限额不执行任何意图：余额与记录不变。
	if bal, _ := l.Balance("aa", "usdc"); bal != 10000 {
		t.Fatalf("balance changed after invalid limits: %d", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 0 {
		t.Fatalf("invalid limits left records: %+v", snap.Settlements)
	}
}

// 超限失败不占用付款编号：同一编号改为较小金额仍可在本批次内成功。
func TestLimitSmallerAmountRetryInBatch(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 500)}, Intents: []PaymentIntent{
		intent("p", "aa", "usdc", 600), // 超限，编号不占用
		intent("p", "aa", "usdc", 400), // 较小金额成功
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusLimit, StatusSettled}
	if got := statuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 9600 {
		t.Fatalf("balance=%d want 9600", bal)
	}
}

// 重复提交已成功付款仍返回 duplicate，即使本批次额度已用完也不改成超限；
// 同批次重复与历史重复都如此。
func TestLimitDuplicateStillDuplicate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	// 首批：p 扣 500，额度用尽。
	first, _ := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 500)}, Intents: []PaymentIntent{
		intent("p", "aa", "usdc", 500),
	}})
	if first.Results[0].Status != StatusSettled {
		t.Fatalf("first: %+v", first.Results[0])
	}

	// 历史重复：额度已用完，仍返回 duplicate。
	again, _ := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 500)}, Intents: []PaymentIntent{
		intent("p", "aa", "usdc", 500),
	}})
	if again.Results[0].Status != StatusDuplicate {
		t.Fatalf("historical duplicate with exhausted limit want duplicate, got %+v", again.Results[0])
	}

	// 同批次重复：首项用尽额度，次项仍为 duplicate。
	res, _ := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 500)}, Intents: []PaymentIntent{
		intent("q", "aa", "usdc", 500),
		intent("q", "aa", "usdc", 500),
	}})
	want := []string{StatusSettled, StatusDuplicate}
	if got := statuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("in-batch duplicate with exhausted limit: %v want %v", got, want)
	}
}

// 对于尚未成功且参数、状态合法的意图，先判断是否超限，再检查余额；
// 同时超限和余额不足时报告 limit_exceeded。
func TestLimitCheckBeforeBalance(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 50}})
	l := openOrFail(t, path)

	// 余额 50，限额 100；需扣 200：既超限又余额不足，应报 limit_exceeded。
	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 100)}, Intents: []PaymentIntent{
		intent("p", "aa", "usdc", 200),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusLimit {
		t.Fatalf("want limit_exceeded (before balance), got %+v", res.Results[0])
	}
}

// 金额接近 int64 上界时不能因累计溢出而放行。
func TestLimitOverflowSafety(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: math.MaxInt64}})
	l := openOrFail(t, path)

	// 上限 MaxInt64，首笔扣 MaxInt64-100（成功，used=MaxInt64-100）。
	// 必须在同一批次内测试累计溢出：used 每批从零计算。
	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", math.MaxInt64)}, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", math.MaxInt64-100), // 成功，used=MaxInt64-100
		intent("p2", "aa", "usdc", 200),                // 超限：used+200 溢出 int64，但 remaining=100 < 200
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusLimit}
	if got := statuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	// 余额未被超限项改动。
	if bal, _ := l.Balance("aa", "usdc"); bal != 100 {
		t.Fatalf("balance=%d want 100", bal)
	}
}

// 存储失败回滚该项的余额、成功记录与已用额度；已成功的前项保留。
func TestLimitStorageFailureRollsBackUsed(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)
	SetFailHook(l, &failNth{nth: 2}) // ok1 成功，boom 保存失败，ok2 成功
	t.Cleanup(func() { SetFailHook(l, nil) })

	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 500)}, Intents: []PaymentIntent{
		intent("ok1", "aa", "usdc", 100),  // 成功，used=100
		intent("boom", "aa", "usdc", 100), // 保存失败，回滚 used 至 100
		intent("ok2", "aa", "usdc", 100),  // 成功，used=200
		intent("p3", "aa", "usdc", 300),  // 成功，used=500（若 boom 未回滚则 used=600 超限）
		intent("p4", "aa", "usdc", 1),    // 超限
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusStorage, StatusSettled, StatusSettled, StatusLimit}
	if got := statuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	// 余额：1000 - 100 - 100 - 300 = 500（boom 与 p4 不扣款）。
	if bal, _ := l.Balance("aa", "usdc"); bal != 500 {
		t.Fatalf("balance=%d want 500", bal)
	}
}

// 上限仅对本次提交有效：下次提交从零计算。
func TestLimitResetsPerSubmit(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	// 首批用尽 500。
	res, _ := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 500)}, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 500),
	}})
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("first: %+v", res.Results[0])
	}
	// 次批从零计算，仍可扣 500。
	res2, _ := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 500)}, Intents: []PaymentIntent{
		intent("p2", "aa", "usdc", 500),
	}})
	if res2.Results[0].Status != StatusSettled {
		t.Fatalf("limit should reset per submit: %+v", res2.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("balance=%d want 1000", bal)
	}
}

// 历史扣款与退款不占本次额度。
func TestLimitHistoryAndRefundsDontCount(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	// 无额度限制扣 600。
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 600)}}); err != nil {
		t.Fatal(err)
	}
	// 全额退款。
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "x")}}); err != nil {
		t.Fatal(err)
	}
	// 新批次限额 100：历史扣款与退款都不占额度，可扣 100。
	res, _ := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 100)}, Intents: []PaymentIntent{
		intent("p2", "aa", "usdc", 100),
	}})
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("history/refunds must not count toward batch limit: %+v", res.Results[0])
	}
}

// 手续费计入上限。
func TestLimitFeeCountsTowardLimit(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	// 30bps：amount=1000 手续费 3，charged=1003 > 1002，超限。
	res, err := l.Submit(FeeBatch{FeeBps: 30, Limits: []ChargeLimit{limit("aa", "usdc", 1002)}, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 1000),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusLimit {
		t.Fatalf("fee must count toward limit, got %+v", res.Results[0])
	}
	// amount=997 手续费 2（997*30/10000=2），charged=999 <= 1002，成功。
	res2, _ := l.Submit(FeeBatch{FeeBps: 30, Limits: []ChargeLimit{limit("aa", "usdc", 1002)}, Intents: []PaymentIntent{
		intent("p2", "aa", "usdc", 997),
	}})
	if res2.Results[0].Status != StatusSettled {
		t.Fatalf("charged 999 within limit want settled, got %+v", res2.Results[0])
	}
}

// limits 缺省或为空数组时不限制扣款（空意图列表正常返回空结果）。
func TestLimitMissingOrEmpty(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	for i, ls := range [][]ChargeLimit{nil, {}} {
		res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: ls, Intents: []PaymentIntent{
			intent(fmt.Sprintf("p-%d", i), "aa", "usdc", 5000),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Results[0].Status != StatusSettled {
			t.Fatalf("limits=%v want settled, got %+v", ls, res.Results[0])
		}
	}
	// 空意图列表 + 合法限额：正常返回空结果。
	res, err := l.Submit(FeeBatch{FeeBps: 0, Limits: []ChargeLimit{limit("aa", "usdc", 100)}, Intents: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 0 {
		t.Fatalf("empty intents want empty results, got %+v", res.Results)
	}
}
