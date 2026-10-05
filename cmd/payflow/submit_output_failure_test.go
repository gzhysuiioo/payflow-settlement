package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// failAfterWriter 在成功写出 limit 字节之后，对后续写入返回错误，
// 模拟标准输出中途故障（管道断裂、磁盘满等）。
type failAfterWriter struct {
	buf     bytes.Buffer
	limit   int
	written int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.written
	if remaining <= 0 {
		return 0, errors.New("simulated stdout failure")
	}
	if len(p) > remaining {
		w.buf.Write(p[:remaining])
		w.written += remaining
		return remaining, errors.New("simulated stdout failure")
	}
	n, _ := w.buf.Write(p)
	w.written += n
	return n, nil
}

// silentShortWriter 静默短写：不返回错误，但写出的字节少于请求字节。
type silentShortWriter struct {
	buf bytes.Buffer
}

func (w *silentShortWriter) Write(p []byte) (int, error) {
	n := len(p) / 2
	w.buf.Write(p[:n])
	return n, nil
}

func decodeErrEnvelope(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, s)
	}
	env, ok := v["error"].(map[string]any)
	if !ok {
		t.Fatalf("stderr missing error envelope: %s", s)
	}
	return env
}

// 输出失败场景共用账本：余额 100、费率 0，依次 60、30、20 ——
// 前两项成功、最后一项余额不足。
const outputFailBatch = `{"fee_bps":0,"intents":[
  {"id":"p1","account":"aa-1","asset":"usdc","amount":60},
  {"id":"p2","account":"aa-1","asset":"usdc","amount":30},
  {"id":"p3","account":"aa-1","asset":"usdc","amount":20}
]}`

func setupOutputFailLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	return ledgerPath
}

func querySnapshot(t *testing.T, ledgerPath string) (balance int64, settledIDs []string) {
	t.Helper()
	r := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query: %s", r.err)
	}
	var snap struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
		Settlements []struct {
			ID string `json:"id"`
		} `json:"settlements"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 {
		t.Fatalf("balances=%+v", snap.Balances)
	}
	for _, s := range snap.Settlements {
		settledIDs = append(settledIDs, s.ID)
	}
	return snap.Balances[0].Balance, settledIDs
}

func TestCLISubmitOutputWriteError(t *testing.T) {
	ledgerPath := setupOutputFailLedger(t)

	// 标准输出在结果写到一半时故障：退出码 1，错误信封只出现在标准错误。
	var stdout failAfterWriter
	stdout.limit = 10 // 结果 JSON 远超 10 字节，正文必写不全
	var stderr bytes.Buffer
	code := runCLI([]string{"submit", "-l", ledgerPath},
		strings.NewReader(outputFailBatch), &stdout, &stderr)
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

	// 输出失败不回滚已落账的付款：余额 10，p1/p2 原记录保留，p3 未结算。
	balance, ids := querySnapshot(t, ledgerPath)
	if balance != 10 || strings.Join(ids, ",") != "p1,p2" {
		t.Fatalf("balance=%d settlements=%v", balance, ids)
	}

	// 用原请求再提交：前两项 duplicate、不二次扣款，第三项仍余额不足，退出码 0。
	r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, outputFailBatch)
	if r.code != 0 {
		t.Fatalf("resubmit code=%d err=%s", r.code, r.err)
	}
	var res struct {
		Results []struct {
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	want := []string{"duplicate", "duplicate", "insufficient_balance"}
	if len(res.Results) != len(want) {
		t.Fatalf("resubmit results=%s", r.out)
	}
	for i, x := range res.Results {
		if x.Status != want[i] {
			t.Fatalf("resubmit item %d=%s want %s", i, x.Status, want[i])
		}
	}
	balance, ids = querySnapshot(t, ledgerPath)
	if balance != 10 || strings.Join(ids, ",") != "p1,p2" {
		t.Fatalf("after resubmit balance=%d settlements=%v", balance, ids)
	}
}

func TestCLISubmitOutputShortWrite(t *testing.T) {
	ledgerPath := setupOutputFailLedger(t)

	// 静默短写（无错误返回）同样算输出失败。
	var stdout silentShortWriter
	var stderr bytes.Buffer
	code := runCLI([]string{"submit", "-l", ledgerPath},
		strings.NewReader(outputFailBatch), &stdout, &stderr)
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
	balance, ids := querySnapshot(t, ledgerPath)
	if balance != 10 || strings.Join(ids, ",") != "p1,p2" {
		t.Fatalf("balance=%d settlements=%v", balance, ids)
	}
}

func TestCLISubmitOutputNewlineFailure(t *testing.T) {
	ledgerPath := setupOutputFailLedger(t)

	// 结果正文完整写出、末尾换行写入失败，也属于输出失败。
	// 先用一次成功提交测出正文长度（不含末尾换行）。
	var measure bytes.Buffer
	fresh := t.TempDir() + "/m.json"
	if r := runCLIWith(t, []string{"init", "-l", fresh},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`); r.code != 0 {
		t.Fatalf("init measure ledger: %s", r.err)
	}
	if code := runCLI([]string{"submit", "-l", fresh},
		strings.NewReader(outputFailBatch), &measure, &bytes.Buffer{}); code != 0 {
		t.Fatalf("measure submit failed")
	}
	var stdout failAfterWriter
	stdout.limit = measure.Len() - 1 // 正文恰好写完，换行写不进去

	var stderr bytes.Buffer
	code := runCLI([]string{"submit", "-l", ledgerPath},
		strings.NewReader(outputFailBatch), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d want 1 (newline write failure is an output failure)", code)
	}
	env := decodeErrEnvelope(t, stderr.String())
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	// 正文完整留在标准输出，但绝不补成功响应或混入错误。
	if !strings.Contains(stdout.buf.String(), `"insufficient_balance"`) ||
		strings.Contains(stdout.buf.String(), "storage_error") {
		t.Fatalf("stdout=%q", stdout.buf.String())
	}
	balance, ids := querySnapshot(t, ledgerPath)
	if balance != 10 || strings.Join(ids, ",") != "p1,p2" {
		t.Fatalf("balance=%d settlements=%v", balance, ids)
	}
}

func TestCLISubmitDryRunOutputFailure(t *testing.T) {
	ledgerPath := setupOutputFailLedger(t)
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}

	// 预览输出失败：退出码 1，报告输出阶段错误。
	var stdout failAfterWriter
	stdout.limit = 5
	var stderr bytes.Buffer
	code := runCLI([]string{"submit", "-l", ledgerPath, "--dry-run"},
		strings.NewReader(outputFailBatch), &stdout, &stderr)
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

	// 账本保持预览前状态：文件内容不变，余额、历史不变。
	after, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file changed by failed dry-run output")
	}
	balance, ids := querySnapshot(t, ledgerPath)
	if balance != 100 || len(ids) != 0 {
		t.Fatalf("balance=%d settlements=%v", balance, ids)
	}

	// 预计成功的编号与序号未被占用：真实提交 p1 仍按首次结算，seq 从 1 开始。
	r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, outputFailBatch)
	if r.code != 0 {
		t.Fatalf("real submit: %s", r.err)
	}
	var res struct {
		Results []struct {
			Status string `json:"status"`
			Record *struct {
				Seq int64 `json:"seq"`
			} `json:"record"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	want := []string{"settled", "settled", "insufficient_balance"}
	for i, x := range res.Results {
		if x.Status != want[i] {
			t.Fatalf("item %d=%s want %s", i, x.Status, want[i])
		}
	}
	if res.Results[0].Record == nil || res.Results[0].Record.Seq != 1 ||
		res.Results[1].Record == nil || res.Results[1].Record.Seq != 2 {
		t.Fatalf("predicted seqs were consumed by failed dry-run: %s", r.out)
	}
}

func TestCLISubmitOutputSuccessUnchanged(t *testing.T) {
	// 输出完整成功时保持原有退出语义：逐项业务失败仍退出 0。
	ledgerPath := setupOutputFailLedger(t)
	r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, outputFailBatch)
	if r.code != 0 {
		t.Fatalf("code=%d err=%s", r.code, r.err)
	}
	if !strings.Contains(r.out, `"insufficient_balance"`) || !strings.HasSuffix(r.out, "\n") {
		t.Fatalf("out=%q", r.out)
	}
	// 合法空批次仍返回原有空列表。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":0,"intents":[]}`)
	if r.code != 0 || strings.TrimSpace(r.out) != "{\n  \"results\": []\n}" {
		t.Fatalf("empty batch: code=%d out=%q", r.code, r.out)
	}
}
