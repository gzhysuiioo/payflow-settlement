package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

// 本文件从公开命令行入口（payflow reconcile）回归保障“大额账本历史”：
// 单笔付款/退款都在 int64 内，但全额退款释放同一笔钱后可再次付款，合法
// 历史的累计扣款与累计退款仍会超过 int64 上界。
//
// 账本：aa-ll/usdc 初始余额 9223372036854775807（int64 上界）。连续三轮
// “付款 4000000000000000000（30 基点，手续费 12000000000000000、扣款
// 4012000000000000000）→ 全额退款（含手续费）→ 再付款”，编号分别为
// p1/r1、p2/r2、p3/r3。三轮后余额回到初始值，累计扣款与累计退款均为
// 12036000000000000000（超过 int64 上界）。
//
// 报告必须分别保留两项合计的精确十进制值：全量对账逐笔 matched、净额为
// 字符串 "0"；只选全部退款（付款范围为空）时账本侧扣款合计为 "0"、退款
// 合计仍是该精确值；该范围收到空流水时按退款成功顺序列出三笔缺失退款，
// 流水侧净扣款按零参与比较，净差额（流水减账本）为该精确值。整个过程
// 退出码 0，query 输出与账本文件逐字节不变。

const (
	cliLLInitial = "9223372036854775807"
	cliLLAmount  = "4000000000000000000"
	cliLLFee     = "12000000000000000"
	cliLLCharged = "4012000000000000000"
	cliLLTotal   = "12036000000000000000"
)

// cliLargeLedger 用命令行入口准备三轮“付款→全额退款”的大额历史，返回
// 账本路径。三轮后余额必须回到 int64 上界。
func cliLargeLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-ll","asset":"usdc","balance":`+cliLLInitial+`}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	for i := 0; i < 3; i++ {
		pid := "p" + string(rune('1'+i))
		rid := "r" + string(rune('1'+i))
		submit := `{"fee_bps":30,"intents":[
		  {"id":"` + pid + `","account":"aa-ll","paymaster":"pm","asset":"usdc","amount":` + cliLLAmount + `,"nonce":` + string(rune('1'+i)) + `}
		]}`
		if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, submit); r.code != 0 {
			t.Fatalf("submit %s: %s", pid, r.err)
		}
		refund := `{"refunds":[
		  {"id":"` + rid + `","settlement_id":"` + pid + `","reason":"cancel"}
		]}`
		if r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, refund); r.code != 0 {
			t.Fatalf("refund %s: %s", rid, r.err)
		}
	}
	// 三轮后余额回到初始值（int64 上界）。
	q := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if q.code != 0 {
		t.Fatalf("query: %s", q.err)
	}
	var bal struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
	}
	if err := json.Unmarshal([]byte(q.out), &bal); err != nil {
		t.Fatalf("decode query balance: %v", err)
	}
	if len(bal.Balances) != 1 || bal.Balances[0].Balance != 9223372036854775807 {
		t.Fatalf("balance after 3 rounds=%+v want %s", bal.Balances, cliLLInitial)
	}
	return ledgerPath
}

// cliReconRanged 走对账入口并可附带序号边界 flag，返回解析后的报告；
// 差异/缺失是退出码 0 的正常报告而非错误信封。
func cliReconRanged(t *testing.T, ledgerPath string, flags []string, body string) payflow.ReconReport {
	t.Helper()
	args := append([]string{"reconcile", "-l", ledgerPath}, flags...)
	r := runCLIWith(t, args, body)
	if r.code != 0 {
		t.Fatalf("reconcile must exit 0, got code=%d stderr=%s", r.code, r.err)
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

// cliLLFullBody 是六笔外部流水（三付款三退款，交错、逐字与账本一致）。
const cliLLFullBody = `{"entries":[
  {"kind":"payment","id":"p1","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `},
  {"kind":"refund","id":"r1","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `,"settlement_id":"p1"},
  {"kind":"payment","id":"p2","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `},
  {"kind":"refund","id":"r2","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `,"settlement_id":"p2"},
  {"kind":"payment","id":"p3","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `},
  {"kind":"refund","id":"r3","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `,"settlement_id":"p3"}
]}`

// cliLLRefundsBody 是只含三笔退款（按退款成功顺序）的外部流水。
const cliLLRefundsBody = `{"entries":[
  {"kind":"refund","id":"r1","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `,"settlement_id":"p1"},
  {"kind":"refund","id":"r2","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `,"settlement_id":"p2"},
  {"kind":"refund","id":"r3","account":"aa-ll","asset":"usdc","amount":` + cliLLAmount + `,"fee":` + cliLLFee + `,"charged":` + cliLLCharged + `,"settlement_id":"p3"}
]}`

// cliLLRow 定位 aa-ll/usdc 的单侧汇总行。
func cliLLRow(t *testing.T, rows []payflow.ReconTotalRow) payflow.ReconTotalRow {
	t.Helper()
	for _, r := range rows {
		if r.Account == "aa-ll" && r.Asset == "usdc" {
			return r
		}
	}
	t.Fatalf("aa-ll/usdc row missing: %+v", rows)
	return payflow.ReconTotalRow{}
}

// cliLLAssertTotals 校验两侧合计的扣款/退款/净扣款十进制字符串。
func cliLLAssertTotals(t *testing.T, rep payflow.ReconReport, charged, refunded, net string) {
	t.Helper()
	lg := cliLLRow(t, rep.Totals.Ledger)
	fl := cliLLRow(t, rep.Totals.Flow)
	if lg.ChargedTotal != charged || lg.RefundedTotal != refunded || lg.NetCharged != net {
		t.Fatalf("ledger row=%+v want charged=%s refunded=%s net=%s", lg, charged, refunded, net)
	}
	if fl.ChargedTotal != charged || fl.RefundedTotal != refunded || fl.NetCharged != net {
		t.Fatalf("flow row=%+v want charged=%s refunded=%s net=%s", fl, charged, refunded, net)
	}
}

// TestCLIReconcileLargeLedgerHistory 大额历史的全量与退款范围对账：
// 超过 int64 的两项合计在命令行报告中仍为精确十进制整数字符串。
func TestCLIReconcileLargeLedgerHistory(t *testing.T) {
	ledgerPath := cliLargeLedger(t)

	// 对账前的公开查询结果与账本文件基线（最终只读校验用）。
	queryBefore := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if queryBefore.code != 0 {
		t.Fatalf("query before: %s", queryBefore.err)
	}
	fileBefore, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger before: %v", err)
	}

	// ---- 1) 全量对账：六笔流水完整且逐字一致，全部 matched。----
	full := cliReconRanged(t, ledgerPath, nil, cliLLFullBody)
	wantIDs := []string{"p1", "r1", "p2", "r2", "p3", "r3"}
	if len(full.Results) != len(wantIDs) {
		t.Fatalf("results=%d want %d", len(full.Results), len(wantIDs))
	}
	for i, id := range wantIDs {
		r := full.Results[i]
		wantKind := "payment"
		if strings.HasPrefix(id, "r") {
			wantKind = "refund"
		}
		if r.Index != i || r.ID != id || r.Kind != wantKind || r.Status != payflow.ReconMatched {
			t.Fatalf("full result %d=%+v want %s/%s matched", i, r, wantKind, id)
		}
	}
	// 关联完整：付款带各自退款编号，退款带各自原付款编号。
	if full.Results[0].Record == nil || full.Results[0].Record.RefundID != "r1" {
		t.Fatalf("p1 must link r1: %+v", full.Results[0].Record)
	}
	if full.Results[1].Record == nil || full.Results[1].Record.SettlementID != "p1" {
		t.Fatalf("r1 must link p1: %+v", full.Results[1].Record)
	}
	if full.Results[4].Record == nil || full.Results[4].Record.RefundID != "r3" {
		t.Fatalf("p3 must link r3: %+v", full.Results[4].Record)
	}
	if full.Results[5].Record == nil || full.Results[5].Record.SettlementID != "p3" {
		t.Fatalf("r3 must link p3: %+v", full.Results[5].Record)
	}
	if len(full.MissingPayments) != 0 || len(full.MissingRefunds) != 0 {
		t.Fatalf("missing payments=%+v refunds=%+v", full.MissingPayments, full.MissingRefunds)
	}
	// 关键回归点：两侧扣款合计与退款合计都保留 >int64 的精确值，净额为 "0"。
	cliLLAssertTotals(t, full, cliLLTotal, cliLLTotal, "0")
	if len(full.NetDiff) != 1 {
		t.Fatalf("net_diff=%+v want single row", full.NetDiff)
	}
	d := full.NetDiff[0]
	if d.LedgerNetCharged != "0" || d.FlowNetCharged != "0" || d.NetChargedDiff != "0" {
		t.Fatalf("net_diff=%+v want all decimal zero", d)
	}
	if full.MaxPaymentSeq != 3 || full.MaxRefundSeq != 3 {
		t.Fatalf("max seqs=%d/%d want 3/3", full.MaxPaymentSeq, full.MaxRefundSeq)
	}
	// 命令行原始输出必须含完整十进制字面量，且不得出现科学计数法。
	rawFull := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, cliLLFullBody)
	for _, frag := range []string{
		`"charged_total": "` + cliLLTotal + `"`,
		`"refunded_total": "` + cliLLTotal + `"`,
		`"net_charged": "0"`,
		`"net_charged_diff": "0"`,
	} {
		if !strings.Contains(rawFull.out, frag) {
			t.Fatalf("report missing exact fragment %s:\n%s", frag, rawFull.out)
		}
	}
	if strings.Contains(rawFull.out, "e19") || strings.Contains(rawFull.out, "E19") {
		t.Fatalf("report must not use scientific notation:\n%s", rawFull.out)
	}

	// ---- 2) 只选全部退款（付款范围为空），退款流水完整：不把原付款计入扣款。----
	refundOnly := []string{"--payment-through", "0"}
	ro := cliReconRanged(t, ledgerPath, refundOnly, cliLLRefundsBody)
	if len(ro.Results) != 3 {
		t.Fatalf("refund-only results=%d want 3", len(ro.Results))
	}
	for i, id := range []string{"r1", "r2", "r3"} {
		r := ro.Results[i]
		if r.Index != i || r.ID != id || r.Kind != "refund" || r.Status != payflow.ReconMatched {
			t.Fatalf("refund-only result %d=%+v want refund/%s matched", i, r, id)
		}
	}
	if ro.Results[0].Record == nil || ro.Results[0].Record.SettlementID != "p1" ||
		ro.Results[2].Record == nil || ro.Results[2].Record.SettlementID != "p3" {
		t.Fatalf("refunds must carry true origin payments: %+v / %+v",
			ro.Results[0].Record, ro.Results[2].Record)
	}
	if len(ro.MissingPayments) != 0 || len(ro.MissingRefunds) != 0 {
		t.Fatalf("refund-only missing payments=%+v refunds=%+v", ro.MissingPayments, ro.MissingRefunds)
	}
	// 扣款合计两侧都是 "0"（不把退款引用的原付款顺带计入），退款合计为精确值，
	// 净扣款为负的精确十进制；净差额（流水减账本）为 "0"。
	cliLLAssertTotals(t, ro, "0", cliLLTotal, "-"+cliLLTotal)
	if len(ro.NetDiff) != 1 {
		t.Fatalf("refund-only net_diff=%+v", ro.NetDiff)
	}
	if ro.NetDiff[0].LedgerNetCharged != "-"+cliLLTotal ||
		ro.NetDiff[0].FlowNetCharged != "-"+cliLLTotal ||
		ro.NetDiff[0].NetChargedDiff != "0" {
		t.Fatalf("refund-only net_diff=%+v", ro.NetDiff[0])
	}
	if ro.PaymentAfter != 0 || ro.PaymentThrough != 0 ||
		ro.RefundAfter != 0 || ro.RefundThrough != 3 {
		t.Fatalf("refund-only bounds=%d,%d,%d,%d want 0,0,0,3",
			ro.PaymentAfter, ro.PaymentThrough, ro.RefundAfter, ro.RefundThrough)
	}

	// ---- 3) 同一退款范围收到空流水：按退款成功顺序列出三笔缺失退款；流水侧
	//         净扣款按零参与比较，净差额（流水减账本）为精确值 T。----
	gap := cliReconRanged(t, ledgerPath, refundOnly, `{"entries":[]}`)
	if len(gap.Results) != 0 {
		t.Fatalf("empty-flow results=%+v want none", gap.Results)
	}
	if len(gap.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty", gap.MissingPayments)
	}
	if len(gap.MissingRefunds) != 3 {
		t.Fatalf("missing refunds=%+v want r1,r2,r3", gap.MissingRefunds)
	}
	for i, want := range []struct {
		id, settle string
		seq        int64
	}{
		{"r1", "p1", 1},
		{"r2", "p2", 2},
		{"r3", "p3", 3},
	} {
		mr := gap.MissingRefunds[i]
		if mr.ID != want.id || mr.SettlementID != want.settle || mr.Seq != want.seq ||
			mr.Amount != 4000000000000000000 || mr.Fee != 12000000000000000 ||
			mr.Charged != 4012000000000000000 {
			t.Fatalf("missing refund %d=%+v want id=%s settle=%s seq=%d",
				i, mr, want.id, want.settle, want.seq)
		}
	}
	// 账本侧：扣款 "0"、退款 T、净 -T；流水侧空，无汇总行。
	lg := cliLLRow(t, gap.Totals.Ledger)
	if lg.ChargedTotal != "0" || lg.RefundedTotal != cliLLTotal || lg.NetCharged != "-"+cliLLTotal {
		t.Fatalf("empty-flow ledger row=%+v", lg)
	}
	if len(gap.Totals.Flow) != 0 {
		t.Fatalf("empty-flow flow totals=%+v want no rows", gap.Totals.Flow)
	}
	if len(gap.NetDiff) != 1 {
		t.Fatalf("empty-flow net_diff=%+v", gap.NetDiff)
	}
	if gap.NetDiff[0].LedgerNetCharged != "-"+cliLLTotal ||
		gap.NetDiff[0].FlowNetCharged != "0" ||
		gap.NetDiff[0].NetChargedDiff != cliLLTotal {
		t.Fatalf("empty-flow net_diff=%+v want ledger=-%s flow=0 diff=%s",
			gap.NetDiff[0], cliLLTotal, cliLLTotal)
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
