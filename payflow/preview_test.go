package payflow

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// previewStatuses 提取预览结果状态序列。
func previewStatuses(r *PreviewResult) []string {
	out := make([]string, len(r.Results))
	for i, x := range r.Results {
		out[i] = x.Status
	}
	return out
}

// 规格示例：余额 100、费率 0，依次预览 70 与 40 —— 首项预计成功、
// 次项余额不足（后项必须看到前项预计成功后的余额），而不是两项都成功。
func TestPreviewOrderedBalance(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)

	res, err := l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
		intent("p2", "aa", "usdc", 40),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := previewStatuses(res), []string{StatusSettled, StatusFunds}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if !res.DryRun {
		t.Fatalf("preview result must carry dry_run=true")
	}
	r1 := res.Results[0]
	if r1.Record == nil || r1.Record.Seq != 1 || r1.Record.Charged != 70 {
		t.Fatalf("predicted record: %+v", r1.Record)
	}
	if res.Results[1].Record != nil {
		t.Fatalf("failed preview item must not carry a record: %+v", res.Results[1])
	}
}

// 预计成功的新付款序号从当前最大付款序号之后连续递增；
// 失败、重复、冲突不消耗序号。
func TestPreviewSeqContinuesAndFailuresDontConsume(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openOrFail(t, path)
	// 先真实落账一笔，占住序号 1。
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("seed", "aa", "usdc", 10)}}); err != nil {
		t.Fatal(err)
	}

	// 首项余额不足（不占序号），后项用同编号合法请求仍成功，序号接在 1 之后。
	res, err := l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p", "aa", "usdc", 2_000_000), // funds
		intent("q", "aa", "usdc", 10),        // settled -> seq 2
		intent("p", "aa", "usdc", 10),        // settled -> seq 3（前一失败不占编号/序号）
		intent("q", "aa", "usdc", 10),        // duplicate，不占序号
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusFunds, StatusSettled, StatusSettled, StatusDuplicate}
	if got := previewStatuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if res.Results[1].Record.Seq != 2 || res.Results[2].Record.Seq != 3 {
		t.Fatalf("predicted seqs: q=%d p=%d want 2,3", res.Results[1].Record.Seq, res.Results[2].Record.Seq)
	}
	if dup := res.Results[3]; dup.Record == nil || dup.Record.Seq != 2 {
		t.Fatalf("in-batch duplicate must carry the predicted original record: %+v", dup)
	}
}

// 预览不写账本：文件字节、余额、两类历史与预览前完全一致；
// 也绝不触发持久化（结果中不会出现 storage_error）。
type persistCounter struct{ calls int }

func (c *persistCounter) FailNextPersist() bool { c.calls++; return false }

func TestPreviewDoesNotWrite(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	hook := &persistCounter{}
	SetFailHook(l, hook)
	t.Cleanup(func() { SetFailHook(l, nil) })

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapBefore, err := l.Query()
	if err != nil {
		t.Fatal(err)
	}

	res, err := l.Preview(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70), // 预计成功
		intent("p2", "aa", "usdc", 90), // 预计余额不足
		intent("p3", "zz", "usdc", 1),  // 不存在组合
		intent("p1", "aa", "usdc", 70), // 批内重复
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := previewStatuses(res), []string{
		StatusSettled, StatusFunds, StatusFunds, StatusDuplicate,
	}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if hook.calls != 0 {
		t.Fatalf("preview must never persist, got %d persist calls", hook.calls)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file changed by preview")
	}
	snapAfter, err := l.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapAfter.Settlements) != len(snapBefore.Settlements) ||
		len(snapAfter.Refunds) != len(snapBefore.Refunds) {
		t.Fatalf("preview changed history: before=%+v after=%+v", snapBefore, snapAfter)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 100 {
		t.Fatalf("preview changed balance: %d", bal)
	}
}

// 即使注入“每次持久化都失败”，预览仍逐项给出业务结果，不出现 storage_error。
func TestPreviewIgnoresStorageFailures(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	SetFailHook(l, &alwaysFailHook{})
	t.Cleanup(func() { SetFailHook(l, nil) })

	res, err := l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
		intent("p2", "aa", "usdc", 40),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := previewStatuses(res), []string{StatusSettled, StatusFunds}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v (preview cannot report storage_error)", got, want)
	}
}

type alwaysFailHook struct{}

func (alwaysFailHook) FailNextPersist() bool { return true }

// 连续预览相同批次（账本未变）结果一致；随后真实提交按首次提交处理。
func TestPreviewDeterministicThenRealSubmit(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	batch := FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
		intent("p2", "aa", "usdc", 40),
	}}

	r1, err := l.Preview(batch)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.Preview(batch)
	if err != nil {
		t.Fatal(err)
	}
	j1, _ := json.Marshal(r1)
	j2, _ := json.Marshal(r2)
	if !bytes.Equal(j1, j2) {
		t.Fatalf("repeated previews differ:\n%s\n%s", j1, j2)
	}

	// 真实提交仍是首次提交：p1 成功（不是 duplicate），序号从 1 开始。
	sub, err := l.Submit(batch)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Results[0].Status != StatusSettled || sub.Results[0].Record.Seq != 1 {
		t.Fatalf("real submit after preview must be first-time: %+v", sub.Results[0])
	}
	if sub.Results[1].Status != StatusFunds {
		t.Fatalf("real submit second item: %+v", sub.Results[1])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("balance after real submit=%d want 30", bal)
	}
}

// 预览中首次预计成功的编号对本批次后续项生效：相同请求重复（携带预计记录），
// 字段不同冲突；两者都不再模拟扣款。
func TestPreviewInBatchDuplicateAndConflict(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openOrFail(t, path)
	it := intent("p", "aa", "usdc", 100)

	res, _ := l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{it, it}})
	if res.Results[0].Status != StatusSettled || res.Results[1].Status != StatusDuplicate {
		t.Fatalf("identical repeat: %+v", res.Results)
	}
	if d := res.Results[1]; d.Record == nil || d.Record.Seq != 1 || d.Record.Charged != 100 {
		t.Fatalf("duplicate must carry predicted record: %+v", d)
	}

	diff := it
	diff.Amount = i64p(101) // 独立指针，避免与 it 的 Amount 别名
	res, _ = l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{it, diff}})
	if res.Results[0].Status != StatusSettled || res.Results[1].Status != StatusConflict {
		t.Fatalf("different fields: %+v", res.Results)
	}
	if res.Results[1].Record != nil {
		t.Fatalf("conflict must not carry a record: %+v", res.Results[1])
	}
}

// 失败项（参数/状态/余额/超限）不占编号：后项用同一编号改成合法可支付请求，
// 在同一次预览中仍可预计成功。
func TestPreviewFailedIDsAreReusableInBatch(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)

	badState := intent("a", "aa", "usdc", 1)
	badState.State = "failed"
	zeroAmt := intent("b", "aa", "usdc", 0) // amount 不合法
	res, err := l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		badState,
		intent("a", "aa", "usdc", 10), // 同编号改为 pending：成功
		zeroAmt,
		intent("b", "aa", "usdc", 10),  // 同编号给出正金额：成功
		intent("c", "aa", "usdc", 200), // 余额不足
		intent("c", "aa", "usdc", 20),  // 同编号改小：成功
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		StatusState, StatusSettled,
		StatusInvalid, StatusSettled,
		StatusFunds, StatusSettled,
	}
	if got := previewStatuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	// 三个成功项序号连续 1、2、3（失败项不占序号）。
	for i, idx := range []int{1, 3, 5} {
		if res.Results[idx].Record.Seq != int64(i+1) {
			t.Fatalf("item %d seq=%d want %d", idx, res.Results[idx].Record.Seq, i+1)
		}
	}
}

// 已有成功编号仍按原付款字段与费率判断重复/冲突，重复项携带原记录、
// 不占余额与本批次额度；原付款已退款也遵守同样规则。
func TestPreviewExistingRecordsAndRefunded(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openOrFail(t, path)
	base := intent("p1", "aa", "usdc", 1500)
	base.Paymaster = "pm-1"
	base.Nonce = 7
	if _, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{base}}); err != nil {
		t.Fatal(err)
	}
	charged := int64(1504)

	// 相同请求（含费率）：duplicate，携带原记录（原序号 1，不重新编号）。
	res, _ := l.Preview(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{base}})
	d := res.Results[0]
	if d.Status != StatusDuplicate || d.Record == nil || d.Record.Seq != 1 || d.Record.Charged != charged {
		t.Fatalf("existing duplicate: %+v", d)
	}
	// 字段/费率变化：conflict。
	for i, mut := range []func(*PaymentIntent, *int){
		func(it *PaymentIntent, bps *int) { *it.Amount = 1501 },
		func(it *PaymentIntent, bps *int) { it.Nonce = 8 },
		func(it *PaymentIntent, bps *int) { *bps = 31 },
	} {
		it := base
		it.Amount = i64p(*base.Amount) // 独立指针，避免突变回写 base
		bps := 30
		mut(&it, &bps)
		r, _ := l.Preview(FeeBatch{FeeBps: bps, Intents: []PaymentIntent{it}})
		if r.Results[0].Status != StatusConflict {
			t.Fatalf("mutation %d want conflict, got %+v", i, r.Results[0])
		}
	}

	// 原付款已退款：相同请求仍 duplicate 携带原记录，不再次扣款；
	// 退回的余额虽可用于新付款，但重复项本身不占余额。
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "x")}}); err != nil {
		t.Fatal(err)
	}
	balAfterRefund, _ := l.Balance("aa", "usdc")
	res, _ = l.Preview(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{base}})
	if res.Results[0].Status != StatusDuplicate || res.Results[0].Record == nil ||
		res.Results[0].Record.Seq != 1 {
		t.Fatalf("refunded original duplicate: %+v", res.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfterRefund {
		t.Fatalf("duplicate after refund changed balance: %d vs %d", bal, balAfterRefund)
	}
	// 退款后余额可用于一笔新编号的预计成功，序号接在原记录之后。
	res, _ = l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("new", "aa", "usdc", 100)}})
	if res.Results[0].Status != StatusSettled || res.Results[0].Record.Seq != 2 {
		t.Fatalf("new payment after refund: %+v", res.Results[0])
	}
}

// 限额在预览中同样按批次顺序累计预计成功的扣款（含手续费）；
// 同时超限与余额不足先报告超限；失败项不占额度。
func TestPreviewLimits(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	res, err := l.Preview(FeeBatch{
		FeeBps:  0,
		Limits:  []ChargeLimit{limit("aa", "usdc", 1000)},
		Intents: []PaymentIntent{intent("p1", "aa", "usdc", 600), intent("p2", "aa", "usdc", 400), intent("p3", "aa", "usdc", 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusSettled, StatusLimitExceeded}
	if got := previewStatuses(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if r := res.Results[2]; !strings.Contains(r.Reason, "max_charged 1000") ||
		!strings.Contains(r.Reason, "used 1000") || !strings.Contains(r.Reason, "charges 1") {
		t.Fatalf("limit reason: %q", r.Reason)
	}

	// 手续费计入限额。
	res, _ = l.Preview(FeeBatch{
		FeeBps:  100,
		Limits:  []ChargeLimit{limit("aa", "usdc", 101)},
		Intents: []PaymentIntent{intent("q1", "aa", "usdc", 100), intent("q2", "aa", "usdc", 1)},
	})
	if got := previewStatuses(res); strings.Join(got, ",") != strings.Join([]string{StatusSettled, StatusLimitExceeded}, ",") {
		t.Fatalf("limit incl fee statuses=%v", got)
	}
	if res.Results[0].Record.Charged != 101 {
		t.Fatalf("first charged=%d want 101", res.Results[0].Record.Charged)
	}

	// 超限优先于余额不足；限额内但余额不足仍报余额。
	small := newTestLedger(t, []BalanceInit{{Account: "bb", Asset: "usdc", Balance: 5}})
	lb := openOrFail(t, small)
	res, _ = lb.Preview(FeeBatch{
		FeeBps:  0,
		Limits:  []ChargeLimit{limit("bb", "usdc", 6)},
		Intents: []PaymentIntent{intent("x1", "bb", "usdc", 8), intent("x2", "bb", "usdc", 6)},
	})
	if got := previewStatuses(res); strings.Join(got, ",") != strings.Join([]string{StatusLimitExceeded, StatusFunds}, ",") {
		t.Fatalf("precedence statuses=%v", got)
	}

	// 预计失败不占额度（与真实提交一致）。
	mid := newTestLedger(t, []BalanceInit{{Account: "cc", Asset: "usdc", Balance: 150}})
	lc := openOrFail(t, mid)
	res, _ = lc.Preview(FeeBatch{
		FeeBps: 0,
		Limits: []ChargeLimit{limit("cc", "usdc", 200)},
		Intents: []PaymentIntent{
			intent("y1", "cc", "usdc", 100), // 成功，已用 100
			intent("y2", "cc", "usdc", 100), // 余额不足，不占额度
			intent("y3", "cc", "usdc", 50),  // 成功：用满 150（额度 200 仍够）
		},
	})
	if got := previewStatuses(res); strings.Join(got, ",") != strings.Join([]string{StatusSettled, StatusFunds, StatusSettled}, ",") {
		t.Fatalf("quota statuses=%v", got)
	}
}

// 批次级错误：非法费率、非法限额（空意图也检查）整批拒绝，不产生部分结果；
// 合法空批次返回带预览标记的空结果列表，JSON 形如 {"dry_run":true,"results":[]}。
func TestPreviewBatchValidationAndEmpty(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10}})
	l := openOrFail(t, path)

	for _, bps := range []int{-1, 10001} {
		if _, err := l.Preview(FeeBatch{FeeBps: bps, Intents: []PaymentIntent{intent("x", "aa", "usdc", 1)}}); KindOf(err) != ErrInvalid {
			t.Fatalf("bps=%d want ErrInvalid, got %v", bps, err)
		}
	}
	badLimits := [][]ChargeLimit{
		{limit("", "usdc", 1)},
		{{Account: "aa", Asset: "usdc"}}, // 缺 max_charged
		{limit("aa", "usdc", -1)},
		{limit("aa", "usdc", 1), limit("aa", "usdc", 2)},
	}
	for i, lim := range badLimits {
		// 空意图列表同样检查限额。
		if _, err := l.Preview(FeeBatch{FeeBps: 0, Limits: lim}); KindOf(err) != ErrInvalid {
			t.Fatalf("case %d empty intents want ErrInvalid, got %v", i, err)
		}
		if _, err := l.Preview(FeeBatch{FeeBps: 0, Limits: lim, Intents: []PaymentIntent{intent("p", "aa", "usdc", 1)}}); KindOf(err) != ErrInvalid {
			t.Fatalf("case %d want ErrInvalid, got %v", i, err)
		}
	}

	// 合法空批次：dry_run 标记 + 空列表。
	res, err := l.Preview(FeeBatch{FeeBps: 0, Intents: nil})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Results) != 0 {
		t.Fatalf("empty preview: %+v", res)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"dry_run":true,"results":[]}` {
		t.Fatalf("empty preview json=%s", data)
	}
}

// 手续费、溢出、状态规则与真实提交一致。
func TestPreviewFeeOverflowAndState(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: math.MaxInt64}})
	l := openOrFail(t, path)

	// 1500×30bp = 4，记录携带费率/手续费/扣款。
	res, _ := l.Preview(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{intent("p", "aa", "usdc", 1500)}})
	r := res.Results[0]
	if r.Status != StatusSettled || r.Record.Fee != 4 || r.Record.Charged != 1504 || r.Record.FeeBps != 30 {
		t.Fatalf("fee preview: %+v", r)
	}
	// amount+fee 溢出 int64：invalid_parameter，不占序号。
	res, _ = l.Preview(FeeBatch{FeeBps: 1, Intents: []PaymentIntent{intent("big", "aa", "usdc", math.MaxInt64)}})
	if res.Results[0].Status != StatusInvalid || !strings.Contains(res.Results[0].Reason, "overflows") {
		t.Fatalf("overflow preview: %+v", res.Results[0])
	}
	// 非 pending：state_error。
	bad := intent("s", "aa", "usdc", 1)
	bad.State = "failed"
	res, _ = l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{bad}})
	if res.Results[0].Status != StatusState {
		t.Fatalf("state preview: %+v", res.Results[0])
	}
	// 预览不留任何记录。
	snap, _ := l.Query()
	if len(snap.Settlements) != 0 {
		t.Fatalf("preview left records: %+v", snap.Settlements)
	}
}

// 账本不存在或损坏时沿用现有错误分类，预览不创建或修复账本。
func TestPreviewLedgerErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	if _, err := Open(missing); KindOf(err) != ErrNotInit {
		t.Fatalf("missing ledger: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("open missing ledger must not create a file")
	}

	corrupt := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"version":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(corrupt)
	if KindOf(err) != ErrCorrupt {
		t.Fatalf("corrupt ledger: %v", err)
	}
	if l != nil {
		t.Fatalf("corrupt open returned a handle")
	}
}
