package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 退款输出失败场景共用账本：余额 2000、费率 30bps，p1 扣 1500+手续费 4=1504，
// p2 余额不足未结算；退款批次退 p1（成功）与 p2（not_found）。
func setupRefundOutputFailLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	batch := `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1500,"nonce":1},
	  {"id":"p2","account":"aa-1","asset":"usdc","amount":900}
	]}`
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, batch); r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}
	return ledgerPath
}

// 退款批次：r1 退 p1 成功（charged 1504），r2 退未结算的 p2 为 not_found。
const outputFailRefundBatch = `{"refunds":[
  {"id":"r1","settlement_id":"p1","reason":"cancel order"},
  {"id":"r2","settlement_id":"p2","reason":"cancel order"}
]}`

func queryRefundSnapshot(t *testing.T, ledgerPath string) (balance int64, refunds []struct {
	ID           string `json:"id"`
	SettlementID string `json:"settlement_id"`
	Reason       string `json:"reason"`
	Charged      int64  `json:"charged"`
	Seq          int64  `json:"seq"`
}) {
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
			Seq          int64  `json:"seq"`
		} `json:"refunds"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 {
		t.Fatalf("balances=%+v", snap.Balances)
	}
	return snap.Balances[0].Balance, snap.Refunds
}

// assertRefundLanded 断言输出失败后退款事实完整保留：余额恢复为 2000，
// r1 按原编号、原因、去向与总额 1504 落账，r2 不产生退款记录。
func assertRefundLanded(t *testing.T, ledgerPath string) {
	t.Helper()
	balance, refunds := queryRefundSnapshot(t, ledgerPath)
	if balance != 2000 {
		t.Fatalf("balance=%d want 2000 (refund must not be rolled back)", balance)
	}
	if len(refunds) != 1 {
		t.Fatalf("refunds=%+v want exactly r1", refunds)
	}
	r1 := refunds[0]
	if r1.ID != "r1" || r1.SettlementID != "p1" || r1.Reason != "cancel order" ||
		r1.Charged != 1504 || r1.Seq != 1 {
		t.Fatalf("refund record=%+v", r1)
	}
}

// assertRefundRetryDuplicate 断言用原退款编号、原付款编号与原原因重试：
// 返回 duplicate 及原退款记录，退出码 0，余额不会再次增加。
func assertRefundRetryDuplicate(t *testing.T, ledgerPath string) {
	t.Helper()
	r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{"refunds":[
	  {"id":"r1","settlement_id":"p1","reason":"cancel order"}
	]}`)
	if r.code != 0 {
		t.Fatalf("retry code=%d err=%s", r.code, r.err)
	}
	var res struct {
		Results []struct {
			Status       string `json:"status"`
			SettlementID string `json:"settlement_id"`
			Account      string `json:"account"`
			Asset        string `json:"asset"`
			Charged      int64  `json:"charged"`
			Record       *struct {
				ID  string `json:"id"`
				Seq int64  `json:"seq"`
			} `json:"record"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || res.Results[0].Status != "duplicate" {
		t.Fatalf("retry must be duplicate: %s", r.out)
	}
	x := res.Results[0]
	if x.SettlementID != "p1" || x.Account != "aa-1" || x.Asset != "usdc" || x.Charged != 1504 ||
		x.Record == nil || x.Record.ID != "r1" || x.Record.Seq != 1 {
		t.Fatalf("duplicate must carry the original record: %+v", x)
	}
	assertRefundLanded(t, ledgerPath)
}

func TestCLIRefundOutputWriteError(t *testing.T) {
	ledgerPath := setupRefundOutputFailLedger(t)

	// 标准输出在结果写到一半时故障：退出码 1，错误信封只出现在标准错误。
	var stdout failAfterWriter
	stdout.limit = 10 // 结果 JSON 远超 10 字节，正文必写不全
	var stderr bytes.Buffer
	code := runCLI([]string{"refund", "-l", ledgerPath},
		strings.NewReader(outputFailRefundBatch), &stdout, &stderr)
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
	if !strings.HasPrefix(strings.TrimSpace(partial), "{") {
		t.Fatalf("unexpected stdout content: %q", partial)
	}

	// 输出失败不回滚已落账的退款：1504 保留在原账户、原资产中。
	assertRefundLanded(t, ledgerPath)
	// 原样重试：duplicate 且携带原退款记录，余额不会再次增加。
	assertRefundRetryDuplicate(t, ledgerPath)
}

func TestCLIRefundOutputShortWrite(t *testing.T) {
	ledgerPath := setupRefundOutputFailLedger(t)

	// 静默短写（无错误返回）同样算输出失败。
	var stdout silentShortWriter
	var stderr bytes.Buffer
	code := runCLI([]string{"refund", "-l", ledgerPath},
		strings.NewReader(outputFailRefundBatch), &stdout, &stderr)
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
	assertRefundLanded(t, ledgerPath)
}

func TestCLIRefundOutputNewlineFailure(t *testing.T) {
	ledgerPath := setupRefundOutputFailLedger(t)

	// 结果正文完整写出、末尾换行写入失败，也属于输出失败。
	// 先用一次成功退款测出正文长度（不含末尾换行）。
	var measure bytes.Buffer
	fresh := setupRefundOutputFailLedger(t)
	if code := runCLI([]string{"refund", "-l", fresh},
		strings.NewReader(outputFailRefundBatch), &measure, &bytes.Buffer{}); code != 0 {
		t.Fatalf("measure refund failed")
	}
	var stdout failAfterWriter
	stdout.limit = measure.Len() - 1 // 正文恰好写完，换行写不进去

	var stderr bytes.Buffer
	code := runCLI([]string{"refund", "-l", ledgerPath},
		strings.NewReader(outputFailRefundBatch), &stdout, &stderr)
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
	assertRefundLanded(t, ledgerPath)
}

func TestCLIRefundOutputSuccessUnchanged(t *testing.T) {
	// 输出完整成功时保持原有退出语义：逐项业务失败仍退出 0，末尾换行保留，
	// 标准错误不增加内容。
	ledgerPath := setupRefundOutputFailLedger(t)
	r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, outputFailRefundBatch)
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
	// 合法空批次仍返回原有空列表，退出码 0。
	r = runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{"refunds":[]}`)
	if r.code != 0 || strings.TrimSpace(r.out) != "{\n  \"results\": []\n}" {
		t.Fatalf("empty batch: code=%d out=%q", r.code, r.out)
	}
	// 全部条目业务失败的批次完整输出仍退出 0。
	r = runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{"refunds":[
	  {"id":"r9","settlement_id":"ghost","reason":"x"}
	]}`)
	if r.code != 0 || !strings.Contains(r.out, `"not_found"`) {
		t.Fatalf("all-failed batch: code=%d out=%q", r.code, r.out)
	}
}

func TestCLIRefundEmptyBatchOutputFailure(t *testing.T) {
	// 空退款批次同样遵循输出规则：结果写不出去就退出 1。
	ledgerPath := setupRefundOutputFailLedger(t)
	var stdout failAfterWriter
	stdout.limit = 0 // 一开始就无法写入
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
		t.Fatalf("stdout=%q", stdout.buf.String())
	}
}
