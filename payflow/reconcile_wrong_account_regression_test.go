package payflow

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 本文件为“外部流水把付款记错账户”提供回归保障：付款编号正确并不代表金额
// 应计入账本中的账户。对账必须同时约束两种判断——
//   - 逐笔核对按付款编号定位原记录：账户写错是 field_mismatch，差异中并列
//     保留账户的账本值与流水值，记录详情仍指向原账户的成功记录；
//   - 两侧汇总分别采用各自记录中的账户和资产：账本侧按原账户合计，流水侧
//     按流水所写的账户合计，净差额逐账户给出（流水减账本）。
//
// 场景：同一资产 usdc 下两笔成功付款，p1 属于 aa-1（甲），amount=1000、
// fee=3、charged=1003；p2 属于 aa-2（乙），amount=500、fee=1、charged=501，
// 没有退款。外部流水的付款编号与各项金额均正确，只把 p1 的账户写成 aa-2。
// 两个账户的差额数值相互抵消（-1003 与 +1003），但报告不得因此省略任一
// 账户的差额、不得把整批流水显示成全部匹配，也不得把 aa-2 的 p2 当成 p1
// 的匹配对象。错记账户改回原值后两笔都恢复 matched。全程对账只读。

// regWrongAcctLedger 构造上述两账户同资产、无退款的账本。
func regWrongAcctLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa-1", Asset: "usdc", Balance: 100000},
		{Account: "aa-2", Asset: "usdc", Balance: 100000},
	})
	l := openOrFail(t, path)
	// 30bps：p1 fee=1000*30/10000=3；p2 fee=500*30/10000=1（向下取整）。
	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		{ID: "p1", Account: "aa-1", Paymaster: "pm", Asset: "usdc", Amount: i64p(1000), Nonce: 1, State: "pending"},
		{ID: "p2", Account: "aa-2", Paymaster: "pm", Asset: "usdc", Amount: i64p(500), Nonce: 2, State: "pending"},
	}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := statuses(res); got[0] != StatusSettled || got[1] != StatusSettled {
		t.Fatalf("setup statuses=%v", got)
	}
	return l, path
}

// regWrongAcctFlow 是账户写错的外部流水：p1 的账户写成 aa-2，p2 保持原样；
// 编号与各项金额全部正确。
func regWrongAcctFlow() []FlowEntry {
	return []FlowEntry{
		{Index: 0, Kind: "payment", ID: "p1", Account: "aa-2", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
		{Index: 1, Kind: "payment", ID: "p2", Account: "aa-2", Asset: "usdc", Amount: 500, Fee: 1, Charged: 501},
	}
}

// regCorrectAcctFlow 把 p1 的账户改回账本原值 aa-1。
func regCorrectAcctFlow() []FlowEntry {
	return []FlowEntry{
		{Index: 0, Kind: "payment", ID: "p1", Account: "aa-1", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
		{Index: 1, Kind: "payment", ID: "p2", Account: "aa-2", Asset: "usdc", Amount: 500, Fee: 1, Charged: 501},
	}
}

// regAssertWrongAcctTotals 校验错记账户场景的双侧汇总与净差额：
// 账本侧仍是 aa-1 扣款 1003、aa-2 扣款 501；流水侧只有 aa-2 一组，
// 扣款合计 1504。没有退款时各组净扣款等于扣款合计，净差额按流水减账本：
// aa-1 为 -1003、aa-2 为 +1003。两行差额相互抵消也必须都出现。
func regAssertWrongAcctTotals(t *testing.T, rep *ReconReport) {
	t.Helper()

	// 账本侧两行，按账户排序。
	if len(rep.Totals.Ledger) != 2 ||
		rep.Totals.Ledger[0].Account != "aa-1" || rep.Totals.Ledger[1].Account != "aa-2" {
		t.Fatalf("ledger totals rows=%+v want aa-1,aa-2", rep.Totals.Ledger)
	}
	l1 := rep.Totals.Ledger[0]
	if l1.Asset != "usdc" || l1.ChargedTotal != "1003" || l1.RefundedTotal != "0" || l1.NetCharged != "1003" {
		t.Fatalf("ledger aa-1 row=%+v", l1)
	}
	l2 := rep.Totals.Ledger[1]
	if l2.Asset != "usdc" || l2.ChargedTotal != "501" || l2.RefundedTotal != "0" || l2.NetCharged != "501" {
		t.Fatalf("ledger aa-2 row=%+v", l2)
	}

	// 流水侧只有 aa-2 一组：错记的 p1（1003）与正确的 p2（501）都计入它。
	if len(rep.Totals.Flow) != 1 {
		t.Fatalf("flow totals rows=%+v want single aa-2 row", rep.Totals.Flow)
	}
	f := rep.Totals.Flow[0]
	if f.Account != "aa-2" || f.Asset != "usdc" ||
		f.ChargedTotal != "1504" || f.RefundedTotal != "0" || f.NetCharged != "1504" {
		t.Fatalf("flow row=%+v want aa-2 charged/net 1504", f)
	}

	// 净差额两行（流水减账本），数值抵消也不得省略任一行。
	if len(rep.NetDiff) != 2 ||
		rep.NetDiff[0].Account != "aa-1" || rep.NetDiff[1].Account != "aa-2" {
		t.Fatalf("net_diff rows=%+v want aa-1,aa-2", rep.NetDiff)
	}
	d1 := rep.NetDiff[0]
	if d1.Asset != "usdc" || d1.LedgerNetCharged != "1003" ||
		d1.FlowNetCharged != "0" || d1.NetChargedDiff != "-1003" {
		t.Fatalf("net_diff aa-1=%+v want 0-1003=-1003", d1)
	}
	d2 := rep.NetDiff[1]
	if d2.Asset != "usdc" || d2.LedgerNetCharged != "501" ||
		d2.FlowNetCharged != "1504" || d2.NetChargedDiff != "1003" {
		t.Fatalf("net_diff aa-2=%+v want 1504-501=1003", d2)
	}
}

// TestReconcileWrongAccountMismatchAndTotals 外部流水把 p1 的账户写成对方
// 账户 aa-2：p1 必须是 field_mismatch（差异只列 account，账本值 aa-1、流水
// 值 aa-2），所带成功记录仍属于 aa-1 并保持原编号与序号；p2 为 matched。
// 两笔都按编号找到，不进缺失列表；汇总按各自记录的账户分列两侧。
func TestReconcileWrongAccountMismatchAndTotals(t *testing.T) {
	l, _ := regWrongAcctLedger(t)

	rep, err := l.ReconcileFlow(regWrongAcctFlow())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 逐条结果保持输入顺序：p1 字段不符，p2 匹配。
	if len(rep.Results) != 2 {
		t.Fatalf("results=%d want 2", len(rep.Results))
	}
	r0 := rep.Results[0]
	if r0.Index != 0 || r0.ID != "p1" || r0.Status != ReconFieldMismatch {
		t.Fatalf("result 0=%+v want p1 field_mismatch", r0)
	}
	diffs := regDiffMap(r0)
	if len(diffs) != 1 || diffs["account"] != [2]string{"aa-1", "aa-2"} {
		t.Fatalf("p1 diffs=%v want exactly account aa-1->aa-2", diffs)
	}
	// 记录详情仍指向 aa-1 的原付款 p1（原编号、原序号、原金额），
	// 不能把 aa-2 的 p2 当成 p1 的匹配对象。
	rec := r0.Record
	if rec == nil || rec.Kind != "payment" || rec.ID != "p1" || rec.Account != "aa-1" ||
		rec.Asset != "usdc" || rec.Seq != 1 || rec.Amount != 1000 || rec.Fee != 3 || rec.Charged != 1003 {
		t.Fatalf("p1 record must be the original aa-1 payment: %+v", rec)
	}

	r1 := rep.Results[1]
	if r1.Index != 1 || r1.ID != "p2" || r1.Status != ReconMatched || len(r1.Diffs) != 0 {
		t.Fatalf("result 1=%+v want p2 matched", r1)
	}
	if r1.Record == nil || r1.Record.ID != "p2" || r1.Record.Account != "aa-2" || r1.Record.Seq != 2 {
		t.Fatalf("p2 record=%+v", r1.Record)
	}

	// 两笔都已按编号找到：不进入缺失付款列表，也没有退款可缺。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("matched-by-id payments must not be missing: %+v", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("no refunds exist: %+v", rep.MissingRefunds)
	}

	regAssertWrongAcctTotals(t, rep)
}

// TestReconcileWrongAccountUnknownAccount 错写成账本中从未出现过的非空账户：
// 仍是字段不符的正常对账（不是参数错误），该账户只承载流水侧金额及对应差额，
// 不会变成账本缺少付款，也不会在账本里新建余额。
func TestReconcileWrongAccountUnknownAccount(t *testing.T) {
	l, _ := regWrongAcctLedger(t)

	entries := []FlowEntry{
		{Index: 0, Kind: "payment", ID: "p1", Account: "ghost", Asset: "usdc", Amount: 1000, Fee: 3, Charged: 1003},
		{Index: 1, Kind: "payment", ID: "p2", Account: "aa-2", Asset: "usdc", Amount: 500, Fee: 1, Charged: 501},
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil || rep == nil {
		t.Fatalf("unknown account is a normal report, not invalid_parameter: rep=%+v err=%v", rep, err)
	}

	r0 := rep.Results[0]
	if r0.Status != ReconFieldMismatch {
		t.Fatalf("p1=%s want field_mismatch", r0.Status)
	}
	if diffs := regDiffMap(r0); len(diffs) != 1 || diffs["account"] != [2]string{"aa-1", "ghost"} {
		t.Fatalf("p1 diffs=%v want account aa-1->ghost", diffs)
	}
	if r0.Record == nil || r0.Record.ID != "p1" || r0.Record.Account != "aa-1" || r0.Record.Seq != 1 {
		t.Fatalf("p1 record=%+v", r0.Record)
	}
	if rep.Results[1].Status != ReconMatched {
		t.Fatalf("p2=%s want matched", rep.Results[1].Status)
	}
	// 编号都命中：不错记成缺失付款。
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing=%+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}

	// 账本侧不变（aa-1 1003、aa-2 501）；流水侧按所写账户分为 ghost 与 aa-2。
	if len(rep.Totals.Ledger) != 2 {
		t.Fatalf("ledger rows=%+v", rep.Totals.Ledger)
	}
	g := findTotalRow(t, rep.Totals.Flow, "ghost", "usdc")
	if g.ChargedTotal != "1003" || g.RefundedTotal != "0" || g.NetCharged != "1003" {
		t.Fatalf("flow ghost row=%+v", g)
	}
	f2 := findTotalRow(t, rep.Totals.Flow, "aa-2", "usdc")
	if f2.ChargedTotal != "501" || f2.NetCharged != "501" {
		t.Fatalf("flow aa-2 row=%+v", f2)
	}
	if len(rep.Totals.Flow) != 2 {
		t.Fatalf("flow rows=%+v want ghost,aa-2", rep.Totals.Flow)
	}

	// 差额：ghost 只承载流水侧金额（流水减账本：1003-0=+1003），
	// aa-1 为 -1003，aa-2 两侧相抵为 0。
	if d := regDiff(t, rep, "ghost", "usdc"); d.LedgerNetCharged != "0" ||
		d.FlowNetCharged != "1003" || d.NetChargedDiff != "1003" {
		t.Fatalf("ghost diff=%+v", d)
	}
	if d := regDiff(t, rep, "aa-1", "usdc"); d.NetChargedDiff != "-1003" {
		t.Fatalf("aa-1 diff=%+v", d)
	}
	if d := regDiff(t, rep, "aa-2", "usdc"); d.NetChargedDiff != "0" {
		t.Fatalf("aa-2 diff=%+v", d)
	}

	// 账本里不会因此出现 ghost 的余额。
	q, err := l.Query()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, b := range q.Balances {
		if b.Account == "ghost" {
			t.Fatalf("reconcile must not create a balance for ghost: %+v", q.Balances)
		}
	}
}

// TestReconcileWrongAccountCorrectedAllMatched 把错记账户改回账本原值后，
// 同样的两笔流水应都匹配，各账户净差额为零。
func TestReconcileWrongAccountCorrectedAllMatched(t *testing.T) {
	l, _ := regWrongAcctLedger(t)

	rep, err := l.ReconcileFlow(regCorrectAcctFlow())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for i, r := range rep.Results {
		if r.Status != ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("corrected result %d (%s)=%+v want matched", i, r.ID, r)
		}
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("corrected missing=%+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}
	// 两侧各自按正确账户合计：aa-1 1003、aa-2 501，净差额全为零。
	if len(rep.Totals.Ledger) != 2 || len(rep.Totals.Flow) != 2 {
		t.Fatalf("totals rows ledger=%+v flow=%+v", rep.Totals.Ledger, rep.Totals.Flow)
	}
	for _, row := range rep.Totals.Flow {
		want := map[string]string{"aa-1": "1003", "aa-2": "501"}[row.Account]
		if row.ChargedTotal != want || row.NetCharged != want || row.RefundedTotal != "0" {
			t.Fatalf("flow row=%+v want charged/net %s", row, want)
		}
	}
	if len(rep.NetDiff) != 2 {
		t.Fatalf("net_diff=%+v want two rows", rep.NetDiff)
	}
	for _, d := range rep.NetDiff {
		if d.NetChargedDiff != "0" {
			t.Fatalf("corrected net_diff %s/%s=%s want 0", d.Account, d.Asset, d.NetChargedDiff)
		}
	}
}

// TestReconcileWrongAccountIsReadOnly 错记账户的对账前后，余额、成功付款
// 历史与账本文件内容保持原样；重新打开读到的状态与对账前完全一致。
func TestReconcileWrongAccountIsReadOnly(t *testing.T) {
	l, path := regWrongAcctLedger(t)

	before, err := l.Query()
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger before: %v", err)
	}

	rep, err := l.ReconcileFlow(regWrongAcctFlow())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconFieldMismatch || rep.Results[1].Status != ReconMatched {
		t.Fatalf("setup statuses=%s,%s", rep.Results[0].Status, rep.Results[1].Status)
	}

	after, err := l.Query()
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		bj, _ := json.Marshal(before)
		aj, _ := json.Marshal(after)
		t.Fatalf("ledger state changed by reconcile:\nbefore=%s\nafter =%s", bj, aj)
	}
	diskAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger after: %v", err)
	}
	if string(diskBefore) != string(diskAfter) {
		t.Fatalf("ledger file changed by reconcile:\nbefore=%s\nafter =%s", diskBefore, diskAfter)
	}

	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	reopened, err := l2.Query()
	if err != nil {
		t.Fatalf("query reopened: %v", err)
	}
	if !reflect.DeepEqual(before, reopened) {
		bj, _ := json.Marshal(before)
		rj, _ := json.Marshal(reopened)
		t.Fatalf("reopened ledger differs:\nbefore=%s\ndisk  =%s", bj, rj)
	}
}
