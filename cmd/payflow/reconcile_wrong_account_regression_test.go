package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

// 本文件从公开命令行入口（payflow reconcile，全量对账）回归保障：
// 外部流水把付款记错账户时，逐笔核对仍按付款编号定位原记录，两侧汇总
// 分别采用各自记录中的账户。
//
// 账本：同一资产 usdc 下两笔成功付款，p1 属于 aa-1（amount=1000 fee=3
// charged=1003），p2 属于 aa-2（amount=500 fee=1 charged=501），没有退款。
// 外部流水的付款编号与各项金额均正确，只把 p1 的账户写成 aa-2。报告必须
// 以退出码 0 正常返回：p1 为 field_mismatch（差异并列账户的账本值 aa-1
// 与流水值 aa-2，记录详情仍指向 aa-1 的原付款），p2 为 matched；两笔都
// 不进缺失列表。账本侧扣款合计仍是 aa-1 1003、aa-2 501，流水侧只有 aa-2
// 一组 1504；净差额按流水减账本为 aa-1 -1003、aa-2 +1003，两行相互抵消
// 也都必须出现。错记账户改回原值后两笔都 matched、净差额为零。全程只读：
// query 输出与账本文件内容逐字节不变。

// cliWrongAcctLedger 用命令行入口准备两账户同资产、无退款的账本。
func cliWrongAcctLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[
		  {"account":"aa-1","asset":"usdc","balance":100000},
		  {"account":"aa-2","asset":"usdc","balance":100000}
		]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	// 30bps：p1 fee=3 charged=1003；p2 fee=1 charged=501。
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1000,"nonce":1},
	  {"id":"p2","account":"aa-2","paymaster":"pm","asset":"usdc","amount":500,"nonce":2}
	]}`); r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}
	return ledgerPath
}

// cliReconcileReport 走全量对账入口：账户错记是正常报告（退出码 0），
// 不是 invalid_parameter 错误信封。
func cliReconcileReport(t *testing.T, ledgerPath, body string) payflow.ReconReport {
	t.Helper()
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, body)
	if r.code != 0 {
		t.Fatalf("reconcile must exit 0 with valid-but-misattributed flow, got code=%d stderr=%s", r.code, r.err)
	}
	if r.err != "" {
		t.Fatalf("reconcile must not emit an error envelope: %s", r.err)
	}
	var rep payflow.ReconReport
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, r.out)
	}
	return rep
}

func cliFindTotalRow(t *testing.T, rows []payflow.ReconTotalRow, acct string) payflow.ReconTotalRow {
	t.Helper()
	for _, r := range rows {
		if r.Account == acct && r.Asset == "usdc" {
			return r
		}
	}
	t.Fatalf("total row %s/usdc missing: %+v", acct, rows)
	return payflow.ReconTotalRow{}
}

func cliFindNetDiff(t *testing.T, rep payflow.ReconReport, acct string) payflow.ReconNetDiff {
	t.Helper()
	for _, d := range rep.NetDiff {
		if d.Account == acct && d.Asset == "usdc" {
			return d
		}
	}
	t.Fatalf("net_diff row %s/usdc missing: %+v", acct, rep.NetDiff)
	return payflow.ReconNetDiff{}
}

// TestCLIReconcileWrongAccount 错记账户的完整场景：逐笔状态与差异、记录详情、
// 缺失列表、双侧汇总与净差额，以及修复后恢复 matched，全程只读。
func TestCLIReconcileWrongAccount(t *testing.T) {
	ledgerPath := cliWrongAcctLedger(t)

	queryBefore := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if queryBefore.code != 0 {
		t.Fatalf("query before: %s", queryBefore.err)
	}
	fileBefore, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger before: %v", err)
	}

	// p1 的账户错写成 aa-2；编号与金额均正确，p2 保持原样。
	wrongBody := `{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-2","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"usdc","amount":500,"fee":1,"charged":501}
	]}`
	rep := cliReconcileReport(t, ledgerPath, wrongBody)

	// 逐条结果保持输入顺序：p1 字段不符，p2 匹配。
	if len(rep.Results) != 2 {
		t.Fatalf("results=%d want 2", len(rep.Results))
	}
	r0 := rep.Results[0]
	if r0.Index != 0 || r0.ID != "p1" || r0.Status != payflow.ReconFieldMismatch {
		t.Fatalf("result 0=%+v want p1 field_mismatch", r0)
	}
	diffs := cliDiffMap(r0)
	if len(diffs) != 1 || diffs["account"] != [2]string{"aa-1", "aa-2"} {
		t.Fatalf("p1 diffs=%v want exactly account aa-1->aa-2", diffs)
	}
	// 记录详情仍指向 aa-1 的原付款 p1，不能把 aa-2 的 p2 当成匹配对象。
	if r0.Record == nil || r0.Record.ID != "p1" || r0.Record.Account != "aa-1" ||
		r0.Record.Seq != 1 || r0.Record.Charged != 1003 {
		t.Fatalf("p1 record=%+v", r0.Record)
	}
	r1 := rep.Results[1]
	if r1.Index != 1 || r1.ID != "p2" || r1.Status != payflow.ReconMatched {
		t.Fatalf("result 1=%+v want p2 matched", r1)
	}
	// 两笔都按编号找到：缺失列表为空。
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing=%+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}

	// 账本侧：aa-1 扣款 1003、aa-2 扣款 501，各一行。
	if len(rep.Totals.Ledger) != 2 {
		t.Fatalf("ledger totals=%+v want two rows", rep.Totals.Ledger)
	}
	if l1 := cliFindTotalRow(t, rep.Totals.Ledger, "aa-1"); l1.ChargedTotal != "1003" ||
		l1.RefundedTotal != "0" || l1.NetCharged != "1003" {
		t.Fatalf("ledger aa-1=%+v", l1)
	}
	if l2 := cliFindTotalRow(t, rep.Totals.Ledger, "aa-2"); l2.ChargedTotal != "501" ||
		l2.RefundedTotal != "0" || l2.NetCharged != "501" {
		t.Fatalf("ledger aa-2=%+v", l2)
	}
	// 流水侧只有 aa-2 一组，扣款合计 1504。
	if len(rep.Totals.Flow) != 1 {
		t.Fatalf("flow totals=%+v want single aa-2 row", rep.Totals.Flow)
	}
	if f := rep.Totals.Flow[0]; f.Account != "aa-2" || f.ChargedTotal != "1504" ||
		f.RefundedTotal != "0" || f.NetCharged != "1504" {
		t.Fatalf("flow row=%+v", f)
	}
	// 净差额两行（流水减账本）：aa-1 -1003、aa-2 +1003，抵消也不得省略。
	if len(rep.NetDiff) != 2 || rep.NetDiff[0].Account != "aa-1" || rep.NetDiff[1].Account != "aa-2" {
		t.Fatalf("net_diff=%+v want aa-1,aa-2", rep.NetDiff)
	}
	if d := cliFindNetDiff(t, rep, "aa-1"); d.LedgerNetCharged != "1003" ||
		d.FlowNetCharged != "0" || d.NetChargedDiff != "-1003" {
		t.Fatalf("aa-1 diff=%+v", d)
	}
	if d := cliFindNetDiff(t, rep, "aa-2"); d.LedgerNetCharged != "501" ||
		d.FlowNetCharged != "1504" || d.NetChargedDiff != "1003" {
		t.Fatalf("aa-2 diff=%+v", d)
	}

	// 错记账户改回账本原值：两笔都 matched，各账户净差额为零。
	fixedBody := `{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"usdc","amount":500,"fee":1,"charged":501}
	]}`
	rep = cliReconcileReport(t, ledgerPath, fixedBody)
	for i, r := range rep.Results {
		if r.Status != payflow.ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("fixed result %d (%s)=%+v want matched", i, r.ID, r)
		}
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("fixed missing=%+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}
	for _, d := range rep.NetDiff {
		if d.NetChargedDiff != "0" {
			t.Fatalf("fixed net_diff %s=%s want 0", d.Account, d.NetChargedDiff)
		}
	}

	// 全程只读：query 输出与账本文件逐字节一致。
	queryAfter := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if queryAfter.code != 0 {
		t.Fatalf("query after: %s", queryAfter.err)
	}
	if queryBefore.out != queryAfter.out {
		t.Fatalf("ledger changed by reconcile:\nbefore=%s\nafter =%s", queryBefore.out, queryAfter.out)
	}
	fileAfter, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger after: %v", err)
	}
	if string(fileBefore) != string(fileAfter) {
		t.Fatalf("ledger file changed by reconcile:\nbefore=%s\nafter =%s", fileBefore, fileAfter)
	}
}
