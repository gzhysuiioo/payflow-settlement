package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

// 本文件从公开命令行入口（payflow reconcile，全量对账）回归保障：
// 净扣款差额为零时，报告仍逐笔保留跨记录偏差。
//
// 账本：aa-1/usdc 下两笔等额成功付款 p1、p2（amount=1000 fee=3 charged=1003，
// 30bps），分别被 r1（退 p1）、r2（退 p2）全额退款。外部流水在两种构造下
// 总额都与账本完全对平：
//   - p1 多记 20、p2 等额少记 20（编号、账户、资产、fee 正确）；
//   - r1、r2 交换各自引用的原付款编号（其余字段全部正确）。
//
// 报告必须逐笔给出 field_mismatch 与每个差异字段的账本值/流水值，记录详情
// 指向各自原记录，命中编号的记录不进缺失列表，逐条结果保持输入顺序，同时
// totals/net_diff 如实反映总额相等（net_charged_diff 为十进制 "0"）。偏差改回
// 账本原值后全部恢复 matched。整个过程对账退出码为 0（差异是正常报告，不是
// invalid_parameter），且 query 输出与账本文件内容逐字节不变。

// cliNetZeroLedger 用命令行入口准备两笔付款与两笔退款，返回账本路径。
func cliNetZeroLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100000000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1000,"nonce":1},
	  {"id":"p2","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1000,"nonce":2}
	]}`); r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}
	if r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{"refunds":[
	  {"id":"r1","settlement_id":"p1","reason":"cancel"},
	  {"id":"r2","settlement_id":"p2","reason":"cancel"}
	]}`); r.code != 0 {
		t.Fatalf("refund: %s", r.err)
	}
	return ledgerPath
}

// cliRecon 走全量对账入口，差异必须是退出码 0 的正常报告而非错误信封。
func cliRecon(t *testing.T, ledgerPath, body string) payflow.ReconReport {
	t.Helper()
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, body)
	if r.code != 0 {
		t.Fatalf("reconcile must exit 0 with valid-but-diverging flow, got code=%d stderr=%s", r.code, r.err)
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

func cliReconResult(rep payflow.ReconReport, id string) payflow.ReconResult {
	for _, r := range rep.Results {
		if r.ID == id {
			return r
		}
	}
	return payflow.ReconResult{}
}

func cliTotalRow(t *testing.T, rows []payflow.ReconTotalRow) payflow.ReconTotalRow {
	t.Helper()
	for _, r := range rows {
		if r.Account == "aa-1" && r.Asset == "usdc" {
			return r
		}
	}
	t.Fatalf("aa-1/usdc total row missing: %+v", rows)
	return payflow.ReconTotalRow{}
}

// cliAssertNetZero 汇总两侧都是 charged/refunded 给定值、净扣款 0、净差额 "0"。
func cliAssertNetZero(t *testing.T, rep payflow.ReconReport, chargedTotal, refundedTotal string) {
	t.Helper()
	lg := cliTotalRow(t, rep.Totals.Ledger)
	fl := cliTotalRow(t, rep.Totals.Flow)
	if lg.ChargedTotal != chargedTotal || lg.RefundedTotal != refundedTotal || lg.NetCharged != "0" {
		t.Fatalf("ledger totals=%+v", lg)
	}
	if fl.ChargedTotal != chargedTotal || fl.RefundedTotal != refundedTotal || fl.NetCharged != "0" {
		t.Fatalf("flow totals=%+v", fl)
	}
	if len(rep.NetDiff) != 1 {
		t.Fatalf("net_diff=%+v want single row", rep.NetDiff)
	}
	d := rep.NetDiff[0]
	if d.Account != "aa-1" || d.Asset != "usdc" ||
		d.LedgerNetCharged != "0" || d.FlowNetCharged != "0" || d.NetChargedDiff != "0" {
		t.Fatalf("net_diff=%+v want decimal zero", d)
	}
}

func cliDiffMap(r payflow.ReconResult) map[string][2]string {
	out := make(map[string][2]string, len(r.Diffs))
	for _, d := range r.Diffs {
		out[d.Field] = [2]string{d.Ledger, d.Flow}
	}
	return out
}

// TestCLIReconcileNetZeroKeepsPerRecordDiffs 净差额为零时逐笔差异仍完整保留：
// 付款一多一少对平、退款原付款编号互换、修复后恢复 matched，且全程只读。
func TestCLIReconcileNetZeroKeepsPerRecordDiffs(t *testing.T) {
	ledgerPath := cliNetZeroLedger(t)

	// 对账前的公开查询结果与账本文件内容基线。
	queryBefore := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if queryBefore.code != 0 {
		t.Fatalf("query before: %s", queryBefore.err)
	}
	fileBefore, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger before: %v", err)
	}

	// ---- 场景一：p1 多记 20、p2 少记 20；两笔退款正确。交错输入。----
	offsetBody := `{"entries":[
	  {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":980,"fee":3,"charged":983},
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"},
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1020,"fee":3,"charged":1023},
	  {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p2"}
	]}`
	rep := cliRecon(t, ledgerPath, offsetBody)

	// 逐条结果严格保持外部流水输入顺序。
	want := []struct {
		id     string
		status string
	}{
		{"p2", payflow.ReconFieldMismatch},
		{"r1", payflow.ReconMatched},
		{"p1", payflow.ReconFieldMismatch},
		{"r2", payflow.ReconMatched},
	}
	if len(rep.Results) != len(want) {
		t.Fatalf("results=%d want %d", len(rep.Results), len(want))
	}
	for i, w := range want {
		r := rep.Results[i]
		if r.Index != i || r.ID != w.id || r.Status != w.status {
			t.Fatalf("result %d=%+v want id=%s status=%s", i, r, w.id, w.status)
		}
	}
	// 两笔付款分别带出实际不同字段的账本值与流水值（不多不少）。
	d2 := cliDiffMap(cliReconResult(rep, "p2"))
	if len(d2) != 2 || d2["amount"] != [2]string{"1000", "980"} || d2["charged"] != [2]string{"1003", "983"} {
		t.Fatalf("p2 diffs=%v", d2)
	}
	d1 := cliDiffMap(cliReconResult(rep, "p1"))
	if len(d1) != 2 || d1["amount"] != [2]string{"1000", "1020"} || d1["charged"] != [2]string{"1003", "1023"} {
		t.Fatalf("p1 diffs=%v", d1)
	}
	// 记录详情仍指向各自原付款（含各自退款关联）。
	p2 := cliReconResult(rep, "p2").Record
	if p2 == nil || p2.ID != "p2" || p2.Seq != 2 || p2.Amount != 1000 || p2.Charged != 1003 || p2.RefundID != "r2" {
		t.Fatalf("p2 record=%+v", p2)
	}
	p1 := cliReconResult(rep, "p1").Record
	if p1 == nil || p1.ID != "p1" || p1.Seq != 1 || p1.RefundID != "r1" {
		t.Fatalf("p1 record=%+v", p1)
	}
	// 已通过编号找到的四笔不能出现在缺失列表。
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing payments=%+v refunds=%+v", rep.MissingPayments, rep.MissingRefunds)
	}
	// 扣款 983+1023=2006、退款 2006：净差额为十进制零，但逐笔差异保留。
	cliAssertNetZero(t, rep, "2006", "2006")

	// 偏差改回账本原值：四条全部 matched，汇总仍正确。
	fixedBody := `{"entries":[
	  {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"},
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p2"}
	]}`
	rep = cliRecon(t, ledgerPath, fixedBody)
	for i, r := range rep.Results {
		if r.Status != payflow.ReconMatched {
			t.Fatalf("fixed result %d (%s)=%s want matched", i, r.ID, r.Status)
		}
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("fixed missing=%+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}
	cliAssertNetZero(t, rep, "2006", "2006")

	// ---- 场景二：两笔退款交换原付款编号；两笔付款正确。交错输入。----
	swapBody := `{"entries":[
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p2"},
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"},
	  {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003}
	]}`
	rep = cliRecon(t, ledgerPath, swapBody)
	wantOrder := []struct {
		id     string
		status string
	}{
		{"r1", payflow.ReconFieldMismatch},
		{"p1", payflow.ReconMatched},
		{"r2", payflow.ReconFieldMismatch},
		{"p2", payflow.ReconMatched},
	}
	for i, w := range wantOrder {
		r := rep.Results[i]
		if r.Index != i || r.ID != w.id || r.Status != w.status {
			t.Fatalf("swap result %d=%+v want id=%s status=%s", i, r, w.id, w.status)
		}
	}
	// 差异明确落在 settlement_id，并携带账本中真正的原付款关联。
	sd1 := cliDiffMap(cliReconResult(rep, "r1"))
	if len(sd1) != 1 || sd1["settlement_id"] != [2]string{"p1", "p2"} {
		t.Fatalf("r1 diffs=%v", sd1)
	}
	sd2 := cliDiffMap(cliReconResult(rep, "r2"))
	if len(sd2) != 1 || sd2["settlement_id"] != [2]string{"p2", "p1"} {
		t.Fatalf("r2 diffs=%v", sd2)
	}
	r1rec := cliReconResult(rep, "r1").Record
	if r1rec == nil || r1rec.SettlementID != "p1" || r1rec.Seq != 1 {
		t.Fatalf("r1 record=%+v must carry true origin p1", r1rec)
	}
	r2rec := cliReconResult(rep, "r2").Record
	if r2rec == nil || r2rec.SettlementID != "p2" || r2rec.Seq != 2 {
		t.Fatalf("r2 record=%+v must carry true origin p2", r2rec)
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("swap missing=%+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}
	cliAssertNetZero(t, rep, "2006", "2006")

	// 编号换回来后全部 matched。
	rep = cliRecon(t, ledgerPath, fixedBody)
	for _, r := range rep.Results {
		if r.Status != payflow.ReconMatched {
			t.Fatalf("restored %s=%s want matched", r.ID, r.Status)
		}
	}

	// ---- 金额/手续费分项不同但 charged 相同：分项差异仍保留。----
	breakdownBody := `{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":990,"fee":13,"charged":1003}
	]}`
	rep = cliRecon(t, ledgerPath, breakdownBody)
	bd := cliDiffMap(rep.Results[0])
	if rep.Results[0].Status != payflow.ReconFieldMismatch ||
		len(bd) != 2 || bd["amount"] != [2]string{"1000", "990"} || bd["fee"] != [2]string{"3", "13"} {
		t.Fatalf("breakdown result=%+v diffs=%v", rep.Results[0], bd)
	}

	// ---- 全程只读：query 输出与账本文件逐字节一致。----
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
