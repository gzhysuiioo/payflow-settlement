package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// failWriter 在第 failAfter 次 Write 时返回错误（之前的调用正常写到 buf）。
type failWriter struct {
	buf       bytes.Buffer
	calls     int
	failAfter int
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.failAfter {
		return 0, errors.New("injected stdout failure")
	}
	return w.buf.Write(p)
}

// shortWriter 不报错但总是少写一个字节。
type shortWriter struct{ buf bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return w.buf.Write(p[:len(p)-1])
}

func TestCLISubmitStdoutFailureKeepsLedger(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}

	// 余额 100、费率 0，依次 60、30、20：前两项成功，最后一项余额不足。
	batch := `{"fee_bps":0,"intents":[
	  {"id":"p1","account":"aa-1","asset":"usdc","amount":60},
	  {"id":"p2","account":"aa-1","asset":"usdc","amount":30},
	  {"id":"p3","account":"aa-1","asset":"usdc","amount":20}
	]}`

	// 标准输出写入失败：退出码 1，错误信封走标准错误，kind 为 storage_error，
	// message 指明失败发生在结果输出阶段。
	var stderr bytes.Buffer
	code := runCLI([]string{"submit", "-l", ledgerPath}, strings.NewReader(batch),
		&failWriter{failAfter: 0}, &stderr)
	if code != 1 {
		t.Fatalf("stdout failure: code=%d want 1", code)
	}
	env := decodeOut(t, stderr.String())["error"].(map[string]any)
	if env["kind"] != "storage_error" {
		t.Fatalf("kind=%v want storage_error", env["kind"])
	}
	if msg, _ := env["message"].(string); !strings.Contains(msg, "output") {
		t.Fatalf("message must name the output phase: %q", msg)
	}

	// 输出失败不撤销已落账的付款：余额 10，前两笔记录在案，第三笔未结算。
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
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 10 {
		t.Fatalf("balance=%+v want 10", snap.Balances)
	}
	if len(snap.Settlements) != 2 || snap.Settlements[0].ID != "p1" || snap.Settlements[1].ID != "p2" {
		t.Fatalf("settlements=%+v want p1,p2", snap.Settlements)
	}

	// 用原请求再提交：前两项 duplicate（不二次扣款），第三项再次尝试仍余额不足。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, batch)
	if r.code != 0 {
		t.Fatalf("resubmit: %s", r.err)
	}
	var res struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	want := []string{"duplicate", "duplicate", "insufficient_balance"}
	for i, x := range res.Results {
		if x.Status != want[i] {
			t.Fatalf("item %d status=%s want %s", i, x.Status, want[i])
		}
	}
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Balances[0].Balance != 10 || len(snap.Settlements) != 2 {
		t.Fatalf("resubmit must not double-charge: %+v", snap)
	}
}

func TestCLISubmitStdoutShortWrite(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	batch := `{"fee_bps":0,"intents":[{"id":"p1","account":"aa-1","asset":"usdc","amount":60}]}`

	// 短写（无错误返回）同样算输出失败。
	var stderr bytes.Buffer
	code := runCLI([]string{"submit", "-l", ledgerPath}, strings.NewReader(batch),
		&shortWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("short write: code=%d want 1", code)
	}
	if env := decodeOut(t, stderr.String())["error"].(map[string]any); env["kind"] != "storage_error" {
		t.Fatalf("short write envelope=%s", stderr.String())
	}

	// 正文写出、末尾换行失败也算输出失败。
	stderr.Reset()
	code = runCLI([]string{"submit", "-l", ledgerPath}, strings.NewReader(
		`{"fee_bps":0,"intents":[{"id":"p2","account":"aa-1","asset":"usdc","amount":30}]}`),
		&failWriter{failAfter: 1}, &stderr)
	if code != 1 {
		t.Fatalf("newline failure: code=%d want 1", code)
	}
	if env := decodeOut(t, stderr.String())["error"].(map[string]any); env["kind"] != "storage_error" {
		t.Fatalf("newline failure envelope=%s", stderr.String())
	}
}

func TestCLISubmitDryRunStdoutFailureKeepsLedger(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	batch := `{"fee_bps":0,"intents":[
	  {"id":"p1","account":"aa-1","asset":"usdc","amount":60},
	  {"id":"p2","account":"aa-1","asset":"usdc","amount":30}
	]}`

	// 预览输出失败：退出码 1 + storage_error，账本文件与状态保持预览前。
	var stderr bytes.Buffer
	code := runCLI([]string{"submit", "-l", ledgerPath, "--dry-run"}, strings.NewReader(batch),
		&failWriter{failAfter: 0}, &stderr)
	if code != 1 {
		t.Fatalf("dry-run stdout failure: code=%d want 1", code)
	}
	if env := decodeOut(t, stderr.String())["error"].(map[string]any); env["kind"] != "storage_error" {
		t.Fatalf("dry-run envelope=%s", stderr.String())
	}
	after, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("dry-run output failure must not touch the ledger file")
	}

	// 预计成功的编号未被占用：随后的真实提交仍按首次处理。
	r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, batch)
	if r.code != 0 {
		t.Fatalf("real submit: %s", r.err)
	}
	var res struct {
		Results []struct {
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 || res.Results[0].Status != "settled" || res.Results[1].Status != "settled" {
		t.Fatalf("ids must not be consumed by failed dry-run output: %s", r.out)
	}
}
