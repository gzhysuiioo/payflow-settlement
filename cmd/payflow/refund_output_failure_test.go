package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 退款输出失败场景共用账本：余额 2000、费率 30bps，p1 成功扣款 1504。
// 退款批次 r1 成功（全额退回 1504），r2 目标不存在。
const refundOutputFailBatch = `{"refunds":[
  {"id":"r1","settlement_id":"p1","reason":"cancel"},
  {"id":"r2","settlement_id":"ghost","reason":"x"}
]}`

func setupRefundOutputFailLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1500,"nonce":1}
	]}`); r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}
	return ledgerPath
}

// queryRefundSnapshot 返回当前余额与退款记录（编号、原付款、原因、退回总额）。
func queryRefundSnapshot(t *testing.T, ledgerPath string) (balance int64, refunds [][4]any) {
	t.Helper()
	r := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query: %s", r.err)
	}
	var snap struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
		Refunds []struct {
			ID           string `json:"id"`
			SettlementID string `json:"settlement_id"`
			Reason       string `json:"reason"`
			Charged      int64  `json:"charged"`
		} `json:"refunds"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 {
		t.Fatalf("balances=%+v", snap.Balances)
	}
	for _, x := range snap.Refunds {
		refunds = append(refunds, [4]any{x.ID, x.SettlementID, x.Reason, x.Charged})
	}
	return snap.Balances[0].Balance, refunds
}

// 已成功落账的退款（r1，1504）在输出失败后必须原样保留。
func checkRefundedState(t *testing.T, ledgerPath string) {
	t.Helper()
	balance, refunds := queryRefundSnapshot(t, ledgerPath)
	if balance != 2000 {
		t.Fatalf("balance=%d want 2000 (refund of 1504 must stand)", balance)
	}
	want := [4]any{"r1", "p1", "cancel", int64(1504)}
	if len(refunds) != 1 || refunds[0] != want {
		t.Fatalf("refunds=%v want [%v]", refunds, want)
	}
}

func TestCLIRefundOutputWriteError(t *testing.T) {
	ledgerPath := setupRefundOutputFailLedger(t)

	// 标准输出在结果写到一半时故障：退出码 1，错误信封只出现在标准错误。
	var stdout failAfterWriter
	stdout.limit = 10 // 结果 JSON 远超 10 字节，正文必写不全
	var stderr bytes.Buffer
	code := runCLI([]string{"refund", "-l", ledgerPath},
		strings.NewReader(refundOutputFailBatch), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "output") {
		t.Fatalf("message must name the result output stage: %q", msg)
	}
	// 标准输出里只有部分结果，不混入错误 JSON，也不补成功响应。
	partial := stdout.buf.String()
	if strings.Contains(partial, `"error"`) || strings.Contains(partial, "storage_error") {
		t.Fatalf("error JSON leaked into stdout: %q", partial)
	}
	if !strings.HasPrefix(strings.TrimSpace(partial), "{") || strings.Contains(partial, `"results": []`) {
		t.Fatalf("unexpected stdout content: %q", partial)
	}

	// 输出失败不回滚已落账的退款：1504 留在原账户，r1 记录保留，r2 仍是 not_found。
	checkRefundedState(t, ledgerPath)

	// 用相同退款编号、原付款编号和原因重试：返回 duplicate 与原记录，余额不再增加。
	r := runCLIWith(t, []string{"refund", "-l", ledgerPath},
		`{"refunds":[{"id":"r1","settlement_id":"p1","reason":"cancel"}]}`)
	if r.code != 0 {
		t.Fatalf("retry code=%d err=%s", r.code, r.err)
	}
	var res struct {
		Results []struct {
			Status       string `json:"status"`
			SettlementID string `json:"settlement_id"`
			Charged      int64  `json:"charged"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || res.Results[0].Status != "duplicate" ||
		res.Results[0].SettlementID != "p1" || res.Results[0].Charged != 1504 {
		t.Fatalf("retry must return duplicate with original record: %s", r.out)
	}
	checkRefundedState(t, ledgerPath)
}

func TestCLIRefundOutputShortWrite(t *testing.T) {
	ledgerPath := setupRefundOutputFailLedger(t)

	// 静默短写（无错误返回）同样算输出失败。
	var stdout silentShortWriter
	var stderr bytes.Buffer
	code := runCLI([]string{"refund", "-l", ledgerPath},
		strings.NewReader(refundOutputFailBatch), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	if strings.Contains(stdout.buf.String(), "storage_error") {
		t.Fatalf("error JSON leaked into stdout: %q", stdout.buf.String())
	}
	// 账本同样不受影响。
	checkRefundedState(t, ledgerPath)
}

func TestCLIRefundOutputNewlineFailure(t *testing.T) {
	ledgerPath := setupRefundOutputFailLedger(t)

	// 结果正文完整写出、末尾换行写入失败，也属于输出失败。
	// 先用一次成功退款测出正文长度（不含末尾换行）。
	var measure bytes.Buffer
	fresh := setupRefundOutputFailLedger(t)
	if code := runCLI([]string{"refund", "-l", fresh},
		strings.NewReader(refundOutputFailBatch), &measure, &bytes.Buffer{}); code != 0 {
		t.Fatalf("measure refund failed")
	}
	var stdout failAfterWriter
	stdout.limit = measure.Len() - 1 // 正文恰好写完，换行写不进去

	var stderr bytes.Buffer
	code := runCLI([]string{"refund", "-l", ledgerPath},
		strings.NewReader(refundOutputFailBatch), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1 (newline write failure is an output failure)", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	// 正文完整留在标准输出，但绝不补成功响应或混入错误。
	if !strings.Contains(stdout.buf.String(), `"refunded"`) ||
		strings.Contains(stdout.buf.String(), "storage_error") {
		t.Fatalf("stdout=%q", stdout.buf.String())
	}
	checkRefundedState(t, ledgerPath)
}

func TestCLIRefundEmptyBatchOutputFailure(t *testing.T) {
	ledgerPath := setupRefundOutputFailLedger(t)

	// 空退款批次同样遵循输出规则：结果写不出去就退出 1。
	var stdout failAfterWriter // limit 0：一开始就无法写入
	var stderr bytes.Buffer
	code := runCLI([]string{"refund", "-l", ledgerPath},
		strings.NewReader(`{"refunds":[]}`), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	if stdout.buf.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.buf.String())
	}
}

func TestCLIRefundOutputSuccessUnchanged(t *testing.T) {
	// 输出完整成功时保持原有退出语义：逐项业务失败仍退出 0，末尾换行保留。
	ledgerPath := setupRefundOutputFailLedger(t)
	r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, refundOutputFailBatch)
	if r.code != 0 {
		t.Fatalf("code=%d err=%s", r.code, r.err)
	}
	if !strings.Contains(r.out, `"refunded"`) || !strings.Contains(r.out, `"not_found"`) ||
		!strings.HasSuffix(r.out, "\n") {
		t.Fatalf("out=%q", r.out)
	}
	if r.err != "" {
		t.Fatalf("stderr must stay silent on success: %q", r.err)
	}
}
