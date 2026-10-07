package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// 对账报告输出失败场景共用流水：p1/p2 与账本完全一致。
// 即使全部流水完全匹配，报告未完整交付也必须退出 1。
const reconMatchedFlow = `{"entries":[
  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":0,"charged":10}
]}`

// reconMismatchFlow 同时包含字段差异与账本外流水：完整交付时仍是正常对账结果。
const reconMismatchFlow = `{"entries":[
  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":9,"charged":1003},
  {"kind":"payment","id":"pX","account":"aa-1","asset":"usdc","amount":7,"fee":0,"charged":7}
]}`

// checkReconEnvelope 校验标准错误信封：storage_error，message 必须点明
// “对账报告写到标准输出”阶段，并保留可辨认实际故障的说明。
func checkReconEnvelope(t *testing.T, stderr, cause string) {
	t.Helper()
	env := decodeErrEnvelope(t, stderr)
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	msg, _ := env["message"].(string)
	ml := strings.ToLower(msg)
	if !strings.Contains(ml, "reconcile") || !strings.Contains(ml, "standard output") {
		t.Fatalf("message must name the reconcile report / standard output stage: %q", msg)
	}
	if cause != "" && !strings.Contains(msg, cause) {
		t.Fatalf("message %q must retain identifiable cause %q", msg, cause)
	}
}

// 报告在标准输出写到一半时故障：退出码 1；即使全部流水完全匹配也算失败。
// 标准输出只保留已交付前缀（不追加错误 JSON、不重发整份报告），信封只在标准错误。
func TestCLIReconcileOutputWriteError(t *testing.T) {
	ledgerPath := setupReconLedger(t)

	// 先取一份完整报告，失败时留在 stdout 的内容必须正好是它的前缀。
	full := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, reconMatchedFlow)
	if full.code != 0 {
		t.Fatalf("measure reconcile: %s", full.err)
	}

	var stdout failAfterWriter
	stdout.limit = 10 // 报告远超 10 字节，必写在中途失败
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconMatchedFlow), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	checkReconEnvelope(t, stderr.String(), "simulated stdout failure")

	partial := stdout.buf.String()
	if partial != full.out[:stdout.limit] {
		t.Fatalf("stdout must keep exactly the delivered prefix:\n got %q\nwant %q", partial, full.out[:stdout.limit])
	}
	if strings.Contains(partial, `"error"`) || strings.Contains(partial, "storage_error") {
		t.Fatalf("error JSON leaked into stdout: %q", partial)
	}
	// 失败后不重发整份报告、不补成功提示：接收方拿到的就是已交付的那一段。
	if stdout.buf.Len() != stdout.limit {
		t.Fatalf("stdout len=%d want %d (no re-output)", stdout.buf.Len(), stdout.limit)
	}
}

// 静默短写（接收方未报错却只接收部分字节）同样退出 1；
// 没有底层错误说明时，短写本身必须给出明确原因（short write）。
func TestCLIReconcileOutputShortWrite(t *testing.T) {
	ledgerPath := setupReconLedger(t)

	var stdout silentShortWriter
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconMismatchFlow), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	checkReconEnvelope(t, stderr.String(), "short write")
	if strings.Contains(stdout.buf.String(), `"error"`) ||
		strings.Contains(stdout.buf.String(), "storage_error") {
		t.Fatalf("error JSON leaked into stdout: %q", stdout.buf.String())
	}
}

// 正文已完整写出、最后的末尾换行写入失败，也算输出失败；正文原样保留。
func TestCLIReconcileOutputNewlineFailure(t *testing.T) {
	ledgerPath := setupReconLedger(t)

	var measure bytes.Buffer
	if code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconMatchedFlow), &measure, &bytes.Buffer{}); code != 0 {
		t.Fatalf("measure reconcile failed")
	}
	full := measure.String()
	if !strings.HasSuffix(full, "\n") {
		t.Fatalf("measured report must end with newline: %q", full)
	}

	var stdout failAfterWriter
	stdout.limit = len(full) - 1 // 正文恰好写完，换行写不进去
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconMatchedFlow), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1 (newline write failure is an output failure)", code)
	}
	checkReconEnvelope(t, stderr.String(), "simulated stdout failure")
	// 正文完整留在标准输出，但不含换行，也不混入错误或成功提示。
	if got := stdout.buf.String(); got != full[:len(full)-1] {
		t.Fatalf("stdout must keep the full body without newline:\n got %q\nwant %q", got, full[:len(full)-1])
	}
}

// 失败发生在报告开头：标准输出为空，错误信封只在标准错误。
func TestCLIReconcileOutputFailureAtStart(t *testing.T) {
	ledgerPath := setupReconLedger(t)

	var stdout failAfterWriter // limit 0：第一个字节就写不进去
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconMatchedFlow), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	checkReconEnvelope(t, stderr.String(), "simulated stdout failure")
	if stdout.buf.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.buf.String())
	}
}

// 合法空流水与空历史账本同样以“报告完整交付”为成功条件：写不出去就退出 1。
func TestCLIReconcileEmptyOutputFailures(t *testing.T) {
	ledgerPath := setupReconLedger(t)

	// 空流水（全部账本记录缺失）：报告开头就故障。
	var stdout failAfterWriter // limit 0
	var stderr bytes.Buffer
	if code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(`{"entries":[]}`), &stdout, &stderr); code != 1 {
		t.Fatalf("empty flow code=%d want 1", code)
	}
	checkReconEnvelope(t, stderr.String(), "simulated stdout failure")
	if stdout.buf.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.buf.String())
	}

	// 空历史 + 核对范围内没有记录：短写同样退出 1。
	emptyPath := t.TempDir() + "/empty.json"
	if r := runCLIWith(t, []string{"init", "-l", emptyPath}, `{"balances":[]}`); r.code != 0 {
		t.Fatalf("init empty: %s", r.err)
	}
	var short silentShortWriter
	stderr.Reset()
	if code := runCLI([]string{"reconcile", "-l", emptyPath},
		strings.NewReader(`{"entries":[]}`), &short, &stderr); code != 1 {
		t.Fatalf("empty history code=%d want 1", code)
	}
	checkReconEnvelope(t, stderr.String(), "short write")
}

// 文件输入（-f）的报告交付遵循同样规则。
func TestCLIReconcileOutputFailureFromFile(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	inputPath := t.TempDir() + "/flow.json"
	if err := os.WriteFile(inputPath, []byte(reconMatchedFlow), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout failAfterWriter
	stdout.limit = 10
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath, "-f", inputPath},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	checkReconEnvelope(t, stderr.String(), "simulated stdout failure")
	if !strings.HasPrefix(strings.TrimSpace(stdout.buf.String()), "{") ||
		strings.Contains(stdout.buf.String(), "storage_error") {
		t.Fatalf("unexpected stdout: %q", stdout.buf.String())
	}
}

// 按序号范围核对的报告交付遵循同样规则。
func TestCLIReconcileOutputFailureWithRange(t *testing.T) {
	ledgerPath := setupRangeLedger(t)
	body := `{"entries":[
	  {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":2000,"fee":6,"charged":2006}
	]}`

	var stdout failAfterWriter
	stdout.limit = 5
	var stderr bytes.Buffer
	code := runCLI([]string{"reconcile", "-l", ledgerPath,
		"--payment-after", "1", "--payment-through", "2"},
		strings.NewReader(body), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	checkReconEnvelope(t, stderr.String(), "simulated stdout failure")
	if stdout.buf.Len() != 5 || strings.Contains(stdout.buf.String(), "storage_error") {
		t.Fatalf("stdout=%q", stdout.buf.String())
	}
}

// 对账只读：报告输出失败时余额、历史与账本文件均保持不变。
func TestCLIReconcileOutputFailureKeepsLedger(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}

	var stdout failAfterWriter
	stdout.limit = 10
	var stderr bytes.Buffer
	if code := runCLI([]string{"reconcile", "-l", ledgerPath},
		strings.NewReader(reconMismatchFlow), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	checkReconEnvelope(t, stderr.String(), "simulated stdout failure")

	after, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file changed by failed reconcile output")
	}
	// 余额与付款历史不变。
	r := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query: %s", r.err)
	}
	if !strings.Contains(r.out, `"id": "p1"`) || !strings.Contains(r.out, `"id": "p2"`) {
		t.Fatalf("settlement history changed: %s", r.out)
	}
}

// 完整交付时保持原有语义：字段差异、缺失记录等业务结论退出 0；
// 全匹配、空流水、空历史同样退出 0，报告带末尾换行，标准错误保持静默。
func TestCLIReconcileOutputSuccessUnchanged(t *testing.T) {
	ledgerPath := setupReconLedger(t)

	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, reconMismatchFlow)
	if r.code != 0 {
		t.Fatalf("business differences must exit 0 on full delivery: %s", r.err)
	}
	if !strings.Contains(r.out, `"field_mismatch"`) ||
		!strings.Contains(r.out, `"missing_in_ledger"`) ||
		!strings.HasSuffix(r.out, "\n") {
		t.Fatalf("out=%q", r.out)
	}
	if r.err != "" {
		t.Fatalf("stderr must stay silent on success: %q", r.err)
	}

	r = runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, reconMatchedFlow)
	if r.code != 0 || !strings.Contains(r.out, `"matched"`) || !strings.HasSuffix(r.out, "\n") {
		t.Fatalf("matched flow: code=%d out=%q err=%s", r.code, r.out, r.err)
	}

	r = runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, `{"entries":[]}`)
	if r.code != 0 || !strings.HasSuffix(r.out, "\n") {
		t.Fatalf("empty flow: code=%d out=%q err=%s", r.code, r.out, r.err)
	}

	emptyPath := t.TempDir() + "/empty.json"
	if r := runCLIWith(t, []string{"init", "-l", emptyPath}, `{"balances":[]}`); r.code != 0 {
		t.Fatalf("init empty: %s", r.err)
	}
	r = runCLIWith(t, []string{"reconcile", "-l", emptyPath}, `{"entries":[]}`)
	if r.code != 0 || !strings.Contains(r.out, `"max_payment_seq": 0`) ||
		!strings.HasSuffix(r.out, "\n") {
		t.Fatalf("empty history: code=%d out=%q err=%s", r.code, r.out, r.err)
	}
}
