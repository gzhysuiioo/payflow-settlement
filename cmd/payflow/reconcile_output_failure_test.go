package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 对账输出失败场景共用流水：p1 与账本完全匹配，pX 账本中不存在。
// 业务结论（matched / missing_in_ledger）与输出故障必须区分。
const reconOutputFailFlow = `{"entries":[
  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
  {"kind":"payment","id":"pX","account":"aa-1","asset":"usdc","amount":9,"fee":0,"charged":9}
]}`

// checkReconLedgerUnchanged 确认对账输出失败后账本文件逐字节不变（对账只读）。
func checkReconLedgerUnchanged(t *testing.T, ledgerPath string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file changed by failed reconcile output")
	}
}

func readLedgerFile(t *testing.T, ledgerPath string) []byte {
	t.Helper()
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	return before
}

func TestCLIReconcileOutputWriteError(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	before := readLedgerFile(t, ledgerPath)

	// 标准输出在报告写到一半时故障：退出码 1，错误信封只出现在标准错误。
	var stdout failAfterWriter
	stdout.limit = 10 // 报告 JSON 远超 10 字节，正文必写不全
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconOutputFailFlow), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "reconcile") || !strings.Contains(msg, "output") {
		t.Fatalf("message must name the reconcile report output stage: %q", msg)
	}
	if !strings.Contains(msg, "simulated stdout failure") {
		t.Fatalf("message must keep the underlying cause: %q", msg)
	}
	// 标准输出里只有已交付的部分报告，不混入错误 JSON，也不补成功响应。
	partial := stdout.buf.String()
	if strings.Contains(partial, `"error"`) || strings.Contains(partial, "storage_error") {
		t.Fatalf("error JSON leaked into stdout: %q", partial)
	}
	if !strings.HasPrefix(strings.TrimSpace(partial), "{") {
		t.Fatalf("unexpected stdout content: %q", partial)
	}
	checkReconLedgerUnchanged(t, ledgerPath, before)
}

func TestCLIReconcileOutputShortWrite(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	before := readLedgerFile(t, ledgerPath)

	// 静默短写（无错误返回）同样算输出失败，且要给出明确原因。
	var stdout silentShortWriter
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconOutputFailFlow), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "short write") {
		t.Fatalf("short write must have an explicit cause: %q", msg)
	}
	if strings.Contains(stdout.buf.String(), "storage_error") {
		t.Fatalf("error JSON leaked into stdout: %q", stdout.buf.String())
	}
	checkReconLedgerUnchanged(t, ledgerPath, before)
}

func TestCLIReconcileOutputNewlineFailure(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	before := readLedgerFile(t, ledgerPath)

	// 报告正文完整写出、末尾换行写入失败，也属于输出失败。
	// 先用一次成功对账测出报告总长度（含末尾换行）。
	var measure bytes.Buffer
	fresh := setupReconLedger(t)
	if code := runCLI([]string{"reconcile", "-l", fresh},
		strings.NewReader(reconOutputFailFlow), &measure, &bytes.Buffer{}); code != 0 {
		t.Fatalf("measure reconcile failed")
	}
	var stdout failAfterWriter
	stdout.limit = measure.Len() - 1 // 正文恰好写完，换行写不进去

	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconOutputFailFlow), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1 (newline write failure is an output failure)", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	// 正文完整留在标准输出，但绝不补成功响应或混入错误。
	if !strings.Contains(stdout.buf.String(), `"matched"`) ||
		strings.Contains(stdout.buf.String(), "storage_error") {
		t.Fatalf("stdout=%q", stdout.buf.String())
	}
	checkReconLedgerUnchanged(t, ledgerPath, before)
}

func TestCLIReconcileEmptyFlowOutputFailure(t *testing.T) {
	ledgerPath := setupReconLedger(t)

	// 合法空流水同样遵循交付规则：报告写不出去就退出 1。
	var stdout failAfterWriter // limit 0：一开始就无法写入
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(`{"entries":[]}`), &stdout, &stderr)
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

	// 空历史账本 + 空流水：报告同样必须完整交付才算成功。
	emptyPath := t.TempDir() + "/empty.json"
	if r := runCLIWith(t, []string{"init", "-l", emptyPath}, `{"balances":[]}`); r.code != 0 {
		t.Fatalf("init empty: %s", r.err)
	}
	var stdout2 failAfterWriter
	stdout2.limit = 5
	var stderr2 bytes.Buffer
	if code := runCLI([]string{"reconcile", "-l", emptyPath},
		strings.NewReader(`{"entries":[]}`), &stdout2, &stderr2); code != 1 {
		t.Fatalf("empty history: code=%d want 1", code)
	}
}

func TestCLIReconcileRangeOutputFailure(t *testing.T) {
	ledgerPath := setupRangeLedger(t)
	before := readLedgerFile(t, ledgerPath)

	// 按序号范围核对的报告交付同样受输出失败约束。
	var stdout failAfterWriter
	stdout.limit = 20
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath,
		"--payment-after", "1", "--payment-through", "2"},
		strings.NewReader(`{"entries":[]}`), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	checkReconLedgerUnchanged(t, ledgerPath, before)
}

func TestCLIReconcileAllMatchedTruncatedOutput(t *testing.T) {
	ledgerPath := setupReconLedger(t)

	// 即使所有流水完全匹配，输出被截断也必须退出 1。
	body := `{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":0,"charged":10}
	]}`
	var stdout failAfterWriter
	stdout.limit = 30
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(body), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1 (truncated delivery fails even when all matched)", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
}

func TestCLIReconcileOutputSuccessUnchanged(t *testing.T) {
	// 输出完整成功时保持原有语义：业务差异仍退出 0，报告字段原样，末尾换行保留。
	ledgerPath := setupReconLedger(t)
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, reconOutputFailFlow)
	if r.code != 0 {
		t.Fatalf("code=%d err=%s", r.code, r.err)
	}
	if !strings.Contains(r.out, `"matched"`) || !strings.Contains(r.out, `"missing_in_ledger"`) ||
		!strings.HasSuffix(r.out, "\n") {
		t.Fatalf("out=%q", r.out)
	}
	if r.err != "" {
		t.Fatalf("stderr must stay silent on success: %q", r.err)
	}
	var rep struct {
		Results []struct {
			Index  int    `json:"index"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, r.out)
	}
	if len(rep.Results) != 2 || rep.Results[0].Status != "matched" ||
		rep.Results[1].Status != "missing_in_ledger" {
		t.Fatalf("report results changed: %s", r.out)
	}
}
