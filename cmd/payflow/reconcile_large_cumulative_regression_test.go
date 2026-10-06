package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

// 本文件从公开命令行入口（payflow reconcile）回归保障大额账本历史：单笔
// 付款与退款都在 int64 范围内，但全额退款释放余额后同一笔钱可再次付款，
// 合法历史的累计扣款与累计退款仍会超过 int64 上界。命令行报告必须分别保留
// 两项合计的完整十进制整数字符串，不能只凭最终余额正常或净额为零判断金额
// 正确。
//
// 场景：aa-1/usdc 初始余额 9223372036854775807；每轮付款 4000000000000000000
// （30 基点，手续费 12000000000000000，扣款与退款总额均为 4012000000000000000），
// 再对该付款全额退款（含手续费）。每轮使用不同编号（p1/r1、p2/r2、p3/r3），
// 三轮结束后余额回到初始值，累计扣款与累计退款都为 12036000000000000000
// （超过 int64 上界 9223372036854775807）。
//
// 覆盖：
//   - 全量对账且外部流水完整一致：6 条逐笔 matched，两侧两项合计均为精确值，
//     净扣款、净差额为字符串 "0"，缺失列表为空，原始输出里不出现科学计数法；
//   - 同一历史只选全部退款（--payment-through 0 使付款范围为空）、退款流水
//     完整：账本侧扣款合计 "0"、退款合计仍为完整累计值、净扣款
//     "-12036000000000000000"，两侧净差额仍为 "0"，原付款不被顺带计入扣款；
//   - 该退款范围收到空流水：三笔缺失退款按成功顺序列出，流水侧净扣款按零
//     参与比较，净差额为 "12036000000000000000"，方向流水减账本。
//
// 全程退出码 0（差异/缺失是正常报告，不是错误信封），query 输出与账本文件
// 逐字节不变。

const (
	cliBigInitJSON    = `9223372036854775807`
	cliBigAmountJSON  = `4000000000000000000`
	cliBigFeeJSON     = `12000000000000000`
	cliBigChargedJSON = `4012000000000000000`
	cliBigTotalJSON   = `12036000000000000000`
)

// cliBigSetup 通过命令行入口完成三轮“付款 + 全额退款”，返回账本路径。
func cliBigSetup(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":`+cliBigInitJSON+`}]}`); r.code != 0 {
		t.Fatalf("init: code=%d stderr=%s", r.code, r.err)
	}
	for round := 1; round <= 3; round++ {
		pid := "p" + string(rune('0'+round))
		rid := "r" + string(rune('0'+round))

		submitBody := `{"fee_bps":30,"intents":[
		  {"id":"` + pid + `","account":"aa-1","paymaster":"pm","asset":"usdc","amount":` + cliBigAmountJSON + `,"nonce":1}
		]}`
		sr := runCLIWith(t, []string{"submit", "-l", ledgerPath}, submitBody)
		if sr.code != 0 || sr.err != "" {
			t.Fatalf("round %d submit: code=%d stderr=%s", round, sr.code, sr.err)
		}
		var submit payflow.BatchResult
		if err := json.Unmarshal([]byte(sr.out), &submit); err != nil {
			t.Fatalf("round %d decode submit: %v\n%s", round, err, sr.out)
		}
		if len(submit.Results) != 1 || submit.Results[0].Status != payflow.StatusSettled ||
			submit.Results[0].Record == nil {
			t.Fatalf("round %d submit result=%+v", round, submit.Results)
		}
		if rec := submit.Results[0].Record; rec.Amount != 4000000000000000000 ||
			rec.Fee != 12000000000000000 || rec.Charged != 4012000000000000000 {
			t.Fatalf("round %d settled record=%+v want amount=%s fee=%s charged=%s",
				round, rec, cliBigAmountJSON, cliBigFeeJSON, cliBigChargedJSON)
		}

		refundBody := `{"refunds":[
		  {"id":"` + rid + `","settlement_id":"` + pid + `","reason":"cancel"}
		]}`
		rr := runCLIWith(t, []string{"refund", "-l", ledgerPath}, refundBody)
		if rr.code != 0 || rr.err != "" {
			t.Fatalf("round %d refund: code=%d stderr=%s", round, rr.code, rr.err)
		}
		var refunds payflow.RefundBatchResult
		if err := json.Unmarshal([]byte(rr.out), &refunds); err != nil {
			t.Fatalf("round %d decode refund: %v\n%s", round, err, rr.out)
		}
		if len(refunds.Results) != 1 || refunds.Results[0].Status != payflow.StatusRefundSuccess ||
			refunds.Results[0].Charged != 4012000000000000000 {
			t.Fatalf("round %d refund result=%+v", round, refunds.Results)
		}
	}

	// 三轮结束：余额逐字节回到初始值；3 笔付款、3 笔退款。
	q := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if q.code != 0 {
		t.Fatalf("query after setup: %s", q.err)
	}
	var snap payflow.Snapshot
	if err := json.Unmarshal([]byte(q.out), &snap); err != nil {
		t.Fatalf("decode query: %v\n%s", err, q.out)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 9223372036854775807 {
		t.Fatalf("balances=%+v want balance back at int64 max", snap.Balances)
	}
	if len(snap.Settlements) != 3 || len(snap.Refunds) != 3 {
		t.Fatalf("history=%d/%d want 3/3", len(snap.Settlements), len(snap.Refunds))
	}
	return ledgerPath
}

// cliBigRecon 走 reconcile 命令行入口（可附带范围参数），差异必须是退出码 0
// 的正常报告；返回解码报告与原始输出（用于逐字检查十进制金额）。
func cliBigRecon(t *testing.T, ledgerPath string, extraArgs []string, body string) (payflow.ReconReport, string) {
	t.Helper()
	args := append([]string{"reconcile", "-l", ledgerPath}, extraArgs...)
	r := runCLIWith(t, args, body)
	if r.code != 0 {
		t.Fatalf("reconcile %v must exit 0, got code=%d stderr=%s", extraArgs, r.code, r.err)
	}
	if r.err != "" {
		t.Fatalf("reconcile %v must not emit an error envelope: %s", extraArgs, r.err)
	}
	var rep payflow.ReconReport
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, r.out)
	}
	return rep, r.out
}

// cliBigFullBody 是 6 条与账本逐字段一致的完整流水（先付款后退款）。
func cliBigFullBody() string {
	return `{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `},
	  {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `},
	  {"kind":"payment","id":"p3","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `},
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `,"settlement_id":"p1"},
	  {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `,"settlement_id":"p2"},
	  {"kind":"refund","id":"r3","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `,"settlement_id":"p3"}
	]}`
}

// cliBigRefundBody 只含三笔退款的流水。
func cliBigRefundBody() string {
	return `{"entries":[
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `,"settlement_id":"p1"},
	  {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `,"settlement_id":"p2"},
	  {"kind":"refund","id":"r3","account":"aa-1","asset":"usdc","amount":` + cliBigAmountJSON + `,"fee":` + cliBigFeeJSON + `,"charged":` + cliBigChargedJSON + `,"settlement_id":"p3"}
	]}`
}

// TestCLIReconcileLargeCumulativeTotals 命令行端到端锁定大额历史的精确合计
// 与范围语义，且对账全程只读。
func TestCLIReconcileLargeCumulativeTotals(t *testing.T) {
	ledgerPath := cliBigSetup(t)

	// 对账前的 query 输出与账本文件基线。
	queryBefore := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if queryBefore.code != 0 {
		t.Fatalf("query before: %s", queryBefore.err)
	}
	fileBefore, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger before: %v", err)
	}

	// ---- 1. 全量对账：6 条逐笔 matched，两项合计精确，净额/净差额为 "0"。----
	rep, raw := cliBigRecon(t, ledgerPath, nil, cliBigFullBody())

	wantIDs := []string{"p1", "p2", "p3", "r1", "r2", "r3"}
	wantKinds := []string{"payment", "payment", "payment", "refund", "refund", "refund"}
	if len(rep.Results) != 6 {
		t.Fatalf("results=%d want 6", len(rep.Results))
	}
	for i := 0; i < 6; i++ {
		r := rep.Results[i]
		if r.Index != i || r.ID != wantIDs[i] || r.Kind != wantKinds[i] {
			t.Fatalf("result %d=%+v want %s/%s", i, r, wantKinds[i], wantIDs[i])
		}
		if r.Status != payflow.ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("result %d (%s) status=%s diffs=%+v want matched", i, r.ID, r.Status, r.Diffs)
		}
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing payments=%+v refunds=%+v want empty", rep.MissingPayments, rep.MissingRefunds)
	}
	lg := cliTotalRow(t, rep.Totals.Ledger)
	fl := cliTotalRow(t, rep.Totals.Flow)
	if lg.ChargedTotal != cliBigTotalJSON || lg.RefundedTotal != cliBigTotalJSON || lg.NetCharged != "0" {
		t.Fatalf("ledger totals=%+v", lg)
	}
	if fl.ChargedTotal != cliBigTotalJSON || fl.RefundedTotal != cliBigTotalJSON || fl.NetCharged != "0" {
		t.Fatalf("flow totals=%+v", fl)
	}
	if len(rep.NetDiff) != 1 {
		t.Fatalf("net_diff=%+v want one row", rep.NetDiff)
	}
	if d := rep.NetDiff[0]; d.LedgerNetCharged != "0" || d.FlowNetCharged != "0" || d.NetChargedDiff != "0" {
		t.Fatalf("net_diff=%+v want all zero", d)
	}
	// 原始输出必须逐字包含完整 20 位十进制整数，不能截断或科学计数法。
	if !strings.Contains(raw, `"charged_total": "`+cliBigTotalJSON+`"`) ||
		!strings.Contains(raw, `"refunded_total": "`+cliBigTotalJSON+`"`) {
		t.Fatalf("raw report missing exact totals: %s", raw)
	}
	if strings.Contains(raw, "e+") || strings.Contains(raw, "E+") {
		t.Fatalf("raw report must not use scientific notation: %s", raw)
	}

	// ---- 2. 只选全部退款（付款范围为空）、退款流水完整：原付款不计入扣款。----
	rep, raw = cliBigRecon(t, ledgerPath, []string{"--payment-through", "0"}, cliBigRefundBody())
	if len(rep.Results) != 3 {
		t.Fatalf("refund-only results=%d want 3", len(rep.Results))
	}
	for i := 1; i <= 3; i++ {
		r := rep.Results[i-1]
		wantID := "r" + string(rune('0'+i))
		wantP := "p" + string(rune('0'+i))
		if r.ID != wantID || r.Status != payflow.ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("result=%+v want %s matched", r, wantID)
		}
		if r.Record == nil || r.Record.SettlementID != wantP || r.Record.Seq != int64(i) {
			t.Fatalf("%s record=%+v must carry true origin %s", wantID, r.Record, wantP)
		}
	}
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("refund-only missing=%+v/%+v want empty", rep.MissingPayments, rep.MissingRefunds)
	}
	lg = cliTotalRow(t, rep.Totals.Ledger)
	fl = cliTotalRow(t, rep.Totals.Flow)
	// 账本侧：扣款 0（退款引用的 p1/p2/p3 不被顺带计入），退款完整累计。
	if lg.ChargedTotal != "0" || lg.RefundedTotal != cliBigTotalJSON || lg.NetCharged != "-"+cliBigTotalJSON {
		t.Fatalf("refund-only ledger totals=%+v", lg)
	}
	// 流水侧只给了退款：同样扣款 0、退款完整、净扣款为负的完整累计。
	if fl.ChargedTotal != "0" || fl.RefundedTotal != cliBigTotalJSON || fl.NetCharged != "-"+cliBigTotalJSON {
		t.Fatalf("refund-only flow totals=%+v", fl)
	}
	if d := rep.NetDiff[0]; d.LedgerNetCharged != "-"+cliBigTotalJSON ||
		d.FlowNetCharged != "-"+cliBigTotalJSON || d.NetChargedDiff != "0" {
		t.Fatalf("refund-only net_diff=%+v", d)
	}
	if !strings.Contains(raw, `"charged_total": "0"`) {
		t.Fatalf("refund-only raw report must show zero charged totals: %s", raw)
	}

	// ---- 3. 同一退款范围收到空流水：三笔缺失退款按成功顺序列出。----
	rep, raw = cliBigRecon(t, ledgerPath, []string{"--payment-through", "0"}, `{"entries":[]}`)
	if len(rep.Results) != 0 {
		t.Fatalf("empty-flow results=%+v want none", rep.Results)
	}
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("empty-flow missing payments=%+v want empty", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 3 {
		t.Fatalf("empty-flow missing refunds=%+v want 3", rep.MissingRefunds)
	}
	for i := 1; i <= 3; i++ {
		mr := rep.MissingRefunds[i-1]
		wantID := "r" + string(rune('0'+i))
		wantP := "p" + string(rune('0'+i))
		if mr.ID != wantID || mr.Seq != int64(i) || mr.SettlementID != wantP ||
			mr.Charged != 4012000000000000000 {
			t.Fatalf("missing refund %d=%+v", i, mr)
		}
	}
	// 账本侧仍是 0 扣款 / 完整退款 / 负净扣款；流水侧没有任何行。
	lg = cliTotalRow(t, rep.Totals.Ledger)
	if lg.ChargedTotal != "0" || lg.RefundedTotal != cliBigTotalJSON || lg.NetCharged != "-"+cliBigTotalJSON {
		t.Fatalf("empty-flow ledger totals=%+v", lg)
	}
	if len(rep.Totals.Flow) != 0 {
		t.Fatalf("empty-flow flow totals=%+v want no rows", rep.Totals.Flow)
	}
	if d := rep.NetDiff[0]; d.FlowNetCharged != "0" ||
		d.LedgerNetCharged != "-"+cliBigTotalJSON || d.NetChargedDiff != cliBigTotalJSON {
		t.Fatalf("empty-flow net_diff=%+v want flow 0 ledger -%s diff %s (flow minus ledger)",
			d, cliBigTotalJSON, cliBigTotalJSON)
	}
	if !strings.Contains(raw, `"net_charged_diff": "`+cliBigTotalJSON+`"`) {
		t.Fatalf("empty-flow raw report missing exact positive diff: %s", raw)
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
