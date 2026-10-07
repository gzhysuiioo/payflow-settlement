package payflow

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 本文件集中回归：外部流水把付款记错账户时，逐笔核对与两侧汇总必须同时
// 保持正确——逐笔按付款编号定位原记录（字段差异照常显示），汇总则各自采用
// 自己记录中的账户，不能把错记金额悄悄归回原账户、也不能显示成全部匹配。

// wrongAccountLedger 构造同资产两笔成功付款、无退款的账本：
// p1 属于 acct-a（amount 1000、fee 3、charged 1003），
// p2 属于 acct-b（amount 500、fee 1、charged 501）。
func wrongAccountLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := newTestLedger(t, []BalanceInit{
		{Account: "acct-a", Asset: "usdc", Balance: 100000},
		{Account: "acct-b", Asset: "usdc", Balance: 100000},
	})
	l := openOrFail(t, path)
	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		{ID: "p1", Account: "acct-a", Paymaster: "pm", Asset: "usdc", Amount: i64p(1000), Nonce: 1, State: "pending"},
		{ID: "p2", Account: "acct-b", Paymaster: "pm", Asset: "usdc", Amount: i64p(500), Nonce: 1, State: "pending"},
	}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := statuses(res); got[0] != StatusSettled || got[1] != StatusSettled {
		t.Fatalf("setup statuses=%v", got)
	}
	return l, path
}

func findNetDiffRow(t *testing.T, diffs []ReconNetDiff, acct, asset string) ReconNetDiff {
	t.Helper()
	for _, d := range diffs {
		if d.Account == acct && d.Asset == asset {
			return d
		}
	}
	t.Fatalf("net diff %s/%s not found in %+v", acct, asset, diffs)
	return ReconNetDiff{}
}

// 流水把 p1 的账户错写成 acct-b（其余字段与编号均正确），p2 保持原样：
// 逐笔按编号定位，p1 为 field_mismatch 且所带记录仍属于 acct-a；
// 两侧汇总各用各的账户，差额在两个账户间相互抵消但都不得省略。
func TestReconcileFlowWrongAccountMismatchAndTotals(t *testing.T) {
	l, path := wrongAccountLedger(t)
	before, err := l.Query()
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "acct-b", "usdc", 1000, 3, 1003, ""), // 账户错记为 acct-b
		flowEntry(1, "payment", "p2", "acct-b", "usdc", 500, 1, 501, ""),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 逐笔：p1 字段不符（仅 account 一项差异），p2 匹配。
	if len(rep.Results) != 2 {
		t.Fatalf("results=%+v", rep.Results)
	}
	r0 := rep.Results[0]
	if r0.Status != ReconFieldMismatch {
		t.Fatalf("p1 status=%s want field_mismatch", r0.Status)
	}
	if len(r0.Diffs) != 1 || r0.Diffs[0].Field != "account" ||
		r0.Diffs[0].Ledger != "acct-a" || r0.Diffs[0].Flow != "acct-b" {
		t.Fatalf("p1 diffs must keep ledger/flow accounts side by side: %+v", r0.Diffs)
	}
	// 所带成功记录仍是 acct-a 的 p1 本人（原编号、原序号），不是 acct-b 的 p2。
	if r0.Record == nil || r0.Record.ID != "p1" || r0.Record.Account != "acct-a" || r0.Record.Seq != 1 {
		t.Fatalf("p1 must carry its own ledger record: %+v", r0.Record)
	}
	r1 := rep.Results[1]
	if r1.Status != ReconMatched {
		t.Fatalf("p2 status=%s want matched", r1.Status)
	}
	if r1.Record == nil || r1.Record.ID != "p2" || r1.Record.Account != "acct-b" || r1.Record.Seq != 2 {
		t.Fatalf("p2 record=%+v", r1.Record)
	}

	// 两笔都已按编号找到：不进入缺失列表。
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("no missing expected, got payments=%+v refunds=%+v", rep.MissingPayments, rep.MissingRefunds)
	}

	// 账本侧按账本记录的账户汇总：acct-a 1003、acct-b 501，各自成行。
	if len(rep.Totals.Ledger) != 2 {
		t.Fatalf("ledger rows=%+v", rep.Totals.Ledger)
	}
	la := findTotalRow(t, rep.Totals.Ledger, "acct-a", "usdc")
	if la.ChargedTotal != "1003" || la.RefundedTotal != "0" || la.NetCharged != "1003" {
		t.Fatalf("ledger acct-a=%+v", la)
	}
	lb := findTotalRow(t, rep.Totals.Ledger, "acct-b", "usdc")
	if lb.ChargedTotal != "501" || lb.RefundedTotal != "0" || lb.NetCharged != "501" {
		t.Fatalf("ledger acct-b=%+v", lb)
	}
	// 流水侧按流水记录的账户汇总：只有 acct-b 一组，扣款合计 1504。
	if len(rep.Totals.Flow) != 1 {
		t.Fatalf("flow rows=%+v", rep.Totals.Flow)
	}
	fb := findTotalRow(t, rep.Totals.Flow, "acct-b", "usdc")
	if fb.ChargedTotal != "1504" || fb.RefundedTotal != "0" || fb.NetCharged != "1504" {
		t.Fatalf("flow acct-b=%+v", fb)
	}

	// 净差额 = 流水 - 账本：acct-a 为 -1003、acct-b 为 1003。
	// 数值相互抵消也不能省略任一账户，行保持账户排序。
	if len(rep.NetDiff) != 2 || rep.NetDiff[0].Account != "acct-a" || rep.NetDiff[1].Account != "acct-b" {
		t.Fatalf("net_diff rows=%+v", rep.NetDiff)
	}
	da := rep.NetDiff[0]
	if da.LedgerNetCharged != "1003" || da.FlowNetCharged != "0" || da.NetChargedDiff != "-1003" {
		t.Fatalf("acct-a diff=%+v", da)
	}
	db := rep.NetDiff[1]
	if db.LedgerNetCharged != "501" || db.FlowNetCharged != "1504" || db.NetChargedDiff != "1003" {
		t.Fatalf("acct-b diff=%+v", db)
	}

	// 对账只读：余额、成功付款历史与账本文件保持原样。
	after, err := l.Query()
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(after)
	if string(bj) != string(aj) {
		t.Fatalf("ledger changed by reconcile:\nbefore=%s\nafter =%s", bj, aj)
	}
	diskAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(diskBefore) != string(diskAfter) {
		t.Fatalf("ledger file changed by reconcile")
	}
}

// 错写成账本中从未出现过的非空账户：仍是字段不符的正常对账——不是参数错误、
// 不产生缺失付款、不新建余额；该账户只承载流水侧金额及对应差额。
func TestReconcileFlowUnknownAccountIsFieldMismatch(t *testing.T) {
	l, _ := wrongAccountLedger(t)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "ghost", "usdc", 1000, 3, 1003, ""),
		flowEntry(1, "payment", "p2", "acct-b", "usdc", 500, 1, 501, ""),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("unknown account is not a parameter error: %v", err)
	}
	if rep.Results[0].Status != ReconFieldMismatch {
		t.Fatalf("p1 status=%s want field_mismatch", rep.Results[0].Status)
	}
	if len(rep.Results[0].Diffs) != 1 || rep.Results[0].Diffs[0].Field != "account" ||
		rep.Results[0].Diffs[0].Ledger != "acct-a" || rep.Results[0].Diffs[0].Flow != "ghost" {
		t.Fatalf("p1 diffs=%+v", rep.Results[0].Diffs)
	}
	if rep.Results[1].Status != ReconMatched {
		t.Fatalf("p2 status=%s want matched", rep.Results[1].Status)
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("no missing expected, got %+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}

	// ghost 只出现在流水侧与差额中；账本侧没有这一行。
	if len(rep.Totals.Ledger) != 2 {
		t.Fatalf("ledger rows=%+v", rep.Totals.Ledger)
	}
	gf := findTotalRow(t, rep.Totals.Flow, "ghost", "usdc")
	if gf.ChargedTotal != "1003" || gf.NetCharged != "1003" {
		t.Fatalf("flow ghost=%+v", gf)
	}
	if len(rep.NetDiff) != 3 {
		t.Fatalf("net_diff rows=%+v", rep.NetDiff)
	}
	if d := findNetDiffRow(t, rep.NetDiff, "acct-a", "usdc"); d.NetChargedDiff != "-1003" {
		t.Fatalf("acct-a diff=%+v", d)
	}
	if d := findNetDiffRow(t, rep.NetDiff, "acct-b", "usdc"); d.NetChargedDiff != "0" {
		t.Fatalf("acct-b diff=%+v", d)
	}
	if d := findNetDiffRow(t, rep.NetDiff, "ghost", "usdc"); d.LedgerNetCharged != "0" || d.NetChargedDiff != "1003" {
		t.Fatalf("ghost diff=%+v", d)
	}

	// 不新建余额：余额仍只有原来的两个组合。
	snap, err := l.Query()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(snap.Balances) != 2 {
		t.Fatalf("reconcile must not create balances: %+v", snap.Balances)
	}
	for _, b := range snap.Balances {
		if b.Account == "ghost" {
			t.Fatalf("ghost balance created: %+v", snap.Balances)
		}
	}
}

// 把错记账户改回原值后，同样的两笔流水应都匹配，各账户净差额为零。
func TestReconcileFlowCorrectedAccountAllMatched(t *testing.T) {
	l, _ := wrongAccountLedger(t)
	entries := []FlowEntry{
		flowEntry(0, "payment", "p1", "acct-a", "usdc", 1000, 3, 1003, ""),
		flowEntry(1, "payment", "p2", "acct-b", "usdc", 500, 1, 501, ""),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for i, r := range rep.Results {
		if r.Status != ReconMatched {
			t.Fatalf("result %d status=%s want matched (diffs=%+v)", i, r.Status, r.Diffs)
		}
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("no missing expected, got %+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}
	if len(rep.NetDiff) != 2 {
		t.Fatalf("net_diff rows=%+v", rep.NetDiff)
	}
	for _, d := range rep.NetDiff {
		if d.NetChargedDiff != "0" {
			t.Fatalf("diff for %s must be zero: %+v", d.Account, d)
		}
	}
	if d := findNetDiffRow(t, rep.NetDiff, "acct-a", "usdc"); d.LedgerNetCharged != "1003" || d.FlowNetCharged != "1003" {
		t.Fatalf("acct-a diff=%+v", d)
	}
	if d := findNetDiffRow(t, rep.NetDiff, "acct-b", "usdc"); d.LedgerNetCharged != "501" || d.FlowNetCharged != "501" {
		t.Fatalf("acct-b diff=%+v", d)
	}
}

// 错记账户的流水经 JSON 入口（公开对账入口）得到同样的结论：
// 正常返回报告，p1 字段不符、汇总按各自账户分列。
func TestReconcileFlowWrongAccountViaJSONEntry(t *testing.T) {
	l, _ := wrongAccountLedger(t)
	raw := `{"entries":[
	  {"kind":"payment","id":"p1","account":"acct-b","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"p2","account":"acct-b","asset":"usdc","amount":500,"fee":1,"charged":501}
	]}`
	entries, err := ParseReconcileRequest([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Results[0].Status != ReconFieldMismatch || rep.Results[1].Status != ReconMatched {
		t.Fatalf("statuses=%s,%s", rep.Results[0].Status, rep.Results[1].Status)
	}
	var flowIDs []string
	for _, row := range rep.Totals.Flow {
		flowIDs = append(flowIDs, row.Account+":"+row.ChargedTotal)
	}
	if strings.Join(flowIDs, ",") != "acct-b:1504" {
		t.Fatalf("flow totals=%v", flowIDs)
	}
}
