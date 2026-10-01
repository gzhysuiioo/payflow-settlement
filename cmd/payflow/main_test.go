package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type cliResult struct {
	code int
	out  string
	err  string
}

func runCLIWith(t *testing.T, args []string, stdin string) cliResult {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runCLI(args, strings.NewReader(stdin), &out, &errBuf)
	return cliResult{code: code, out: out.String(), err: errBuf.String()}
}

func decodeOut(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	return v
}

func TestCLIVersionAndDemo(t *testing.T) {
	if r := runCLIWith(t, []string{"version"}, ""); r.code != 0 || !strings.Contains(r.out, "payflow 0.2.0") {
		t.Fatalf("version: %+v", r)
	}
	// 无参数保持历史默认行为：运行 demo。
	if r := runCLIWith(t, nil, ""); r.code != 0 || !strings.Contains(r.out, "reconciliation gaps") {
		t.Fatalf("default demo: code=%d out=%q", r.code, r.out)
	}
	if r := runCLIWith(t, []string{"demo"}, ""); r.code != 0 || !strings.Contains(r.out, "intent=pay-1") {
		t.Fatalf("demo: %+v", r)
	}
	if r := runCLIWith(t, []string{"bogus"}, ""); r.code != 2 {
		t.Fatalf("unknown command code=%d", r.code)
	}
}

func TestCLILedgerLifecycle(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"

	// init
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`)
	if r.code != 0 {
		t.Fatalf("init code=%d err=%s", r.code, r.err)
	}
	if v := decodeOut(t, r.out); v["status"] != "initialized" {
		t.Fatalf("init out=%s", r.out)
	}

	// 重复 init：退出码 1 + ledger_exists，且文件未被覆盖。
	r = runCLIWith(t, []string{"init", "-l", ledgerPath}, `{"balances":[]}`)
	if r.code != 1 {
		t.Fatalf("re-init code=%d", r.code)
	}
	if env := decodeOut(t, r.err); env["error"].(map[string]any)["kind"] != "ledger_exists" {
		t.Fatalf("re-init envelope=%s", r.err)
	}

	// 非法 init：分类为 invalid_parameter。
	badPath := t.TempDir() + "/bad.json"
	r = runCLIWith(t, []string{"init", "-l", badPath},
		`{"balances":[{"account":"","asset":"usdc","balance":1}]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("bad init: code=%d err=%s", r.code, r.err)
	}

	// submit：混合结果逐项返回，退出码 0。
	batch := `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1500,"nonce":1},
	  {"id":"p2","account":"aa-1","asset":"usdc","amount":900},
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1500,"nonce":1},
	  {"id":"p3","account":"aa-1","asset":"usdc","amount":1,"state":"failed"}
	]}`
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, batch)
	if r.code != 0 {
		t.Fatalf("submit code=%d err=%s", r.code, r.err)
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
	got := make([]string, len(res.Results))
	for i, x := range res.Results {
		got[i] = x.Status
	}
	want := []string{"settled", "insufficient_balance", "duplicate", "state_error"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}

	// 空批次返回空结果。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":0,"intents":[]}`)
	if r.code != 0 || strings.TrimSpace(r.out) != `{
  "results": []
}` {
		t.Fatalf("empty batch: code=%d out=%q", r.code, r.out)
	}

	// 非法费率走错误信封（批次级错误，不是逐项）。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":-2,"intents":[]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("bad fee: code=%d err=%s", r.code, r.err)
	}

	// query（模拟新进程重新打开）。
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query code=%d err=%s", r.code, r.err)
	}
	var snap struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
		Settlements []struct {
			ID      string `json:"id"`
			FeeBps  int    `json:"fee_bps"`
			Charged int64  `json:"charged"`
		} `json:"settlements"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 496 {
		t.Fatalf("query balances=%+v", snap.Balances)
	}
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" || snap.Settlements[0].FeeBps != 30 || snap.Settlements[0].Charged != 1504 {
		t.Fatalf("query settlements=%+v", snap.Settlements)
	}

	// 未初始化路径。
	missing := t.TempDir() + "/nope.json"
	if r = runCLIWith(t, []string{"query", "-l", missing}, ""); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "ledger_not_initialized" {
		t.Fatalf("missing: %+v", r)
	}
}

func TestCLICorruptLedger(t *testing.T) {
	dir := t.TempDir()

	// 直接写出一个损坏账本（不经过本进程 init，绕过同进程注册表缓存，
	// 模拟用户拿一个被截断/损坏的文件启动程序）。
	path := dir + "/corrupt.json"
	if err := os.WriteFile(path, []byte(`{"version":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"query", "submit"} {
		r := runCLIWith(t, []string{cmd, "-l", path}, `{"fee_bps":0,"intents":[]}`)
		if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "corrupt_ledger" {
			t.Fatalf("%s on corrupt: code=%d out=%s err=%s", cmd, r.code, r.out, r.err)
		}
	}

	// 非法 JSON 输入分类为 invalid_parameter（与磁盘损坏区分）。
	good := dir + "/good.json"
	if r := runCLIWith(t, []string{"init", "-l", good}, `{"balances":[]}`); r.code != 0 {
		t.Fatalf("init good: %s", r.err)
	}
	if r := runCLIWith(t, []string{"submit", "-l", good}, `{not json`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("garbage input: code=%d err=%s", r.code, r.err)
	}
}

func TestCLIRequiresLedgerFlag(t *testing.T) {
	if r := runCLIWith(t, []string{"query"}, ""); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("query without -l: %+v", r)
	}
}

func TestCLIRefundLifecycle(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"

	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	// p1 成功（charged 1504），p2 余额不足失败。
	batch := `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1500,"nonce":1},
	  {"id":"p2","account":"aa-1","asset":"usdc","amount":900}
	]}`
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, batch); r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}

	// 退款批次：成功 / 重复 / 冲突 / 已退款 / 目标不存在 / 参数非法，逐项返回且退出码 0。
	refunds := `{"refunds":[
	  {"id":"r1","settlement_id":"p1","reason":"cancel"},
	  {"id":"r1","settlement_id":"p1","reason":"cancel"},
	  {"id":"r1","settlement_id":"p1","reason":"changed"},
	  {"id":"r2","settlement_id":"p1","reason":"again"},
	  {"id":"r3","settlement_id":"ghost","reason":"x"},
	  {"id":"r4","settlement_id":"p2","reason":"x"},
	  {"id":"","settlement_id":"p1","reason":"x"}
	]}`
	r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, refunds)
	if r.code != 0 {
		t.Fatalf("refund batch must exit 0 even with per-item failures: %s", r.err)
	}
	var res struct {
		Results []struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			SettlementID string `json:"settlement_id"`
			Account      string `json:"account"`
			Asset        string `json:"asset"`
			Charged      int64  `json:"charged"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	want := []string{"refunded", "duplicate", "conflict", "already_refunded", "not_found", "not_found", "invalid_parameter"}
	if len(res.Results) != len(want) {
		t.Fatalf("got %d results, want %d: %s", len(res.Results), len(want), r.out)
	}
	for i, x := range res.Results {
		if x.Status != want[i] {
			t.Fatalf("item %d status=%s want %s", i, x.Status, want[i])
		}
	}
	// 成功与重复必须回传原付款编号、账户、资产、总额。
	for _, i := range []int{0, 1} {
		x := res.Results[i]
		if x.SettlementID != "p1" || x.Account != "aa-1" || x.Asset != "usdc" || x.Charged != 1504 {
			t.Fatalf("item %d missing trace fields: %+v", i, x)
		}
	}

	// 退款后余额恢复为 2000（全额含手续费）。
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
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
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 2000 {
		t.Fatalf("balance after refund=%+v", snap.Balances)
	}
	if len(snap.Refunds) != 1 || snap.Refunds[0].SettlementID != "p1" || snap.Refunds[0].Charged != 1504 {
		t.Fatalf("query refunds=%+v", snap.Refunds)
	}

	// 空列表返回空结果。
	if r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{"refunds":[]}`); r.code != 0 ||
		strings.TrimSpace(r.out) != "{\n  \"results\": []\n}" {
		t.Fatalf("empty refund batch: code=%d out=%q err=%s", r.code, r.out, r.err)
	}

	// 非法 JSON 走批次级错误信封。
	if r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{bad`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("bad refund json: code=%d err=%s", r.code, r.err)
	}

	// 缺少 -l。
	if r := runCLIWith(t, []string{"refund"}, `{"refunds":[]}`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("refund without -l: %+v", r)
	}

	// 未初始化账本。
	missing := t.TempDir() + "/nope.json"
	if r := runCLIWith(t, []string{"refund", "-l", missing}, `{"refunds":[]}`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "ledger_not_initialized" {
		t.Fatalf("refund on missing ledger: %+v", r)
	}
}

func TestCLIRefundOnCorruptLedger(t *testing.T) {
	path := t.TempDir() + "/corrupt.json"
	if err := os.WriteFile(path, []byte(`{"version":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLIWith(t, []string{"refund", "-l", path}, `{"refunds":[]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "corrupt_ledger" {
		t.Fatalf("refund on corrupt: code=%d err=%s", r.code, r.err)
	}
}

func TestCLIHelpListsRefund(t *testing.T) {
	r := runCLIWith(t, []string{"help"}, "")
	if !strings.Contains(r.out, "refund") {
		t.Fatalf("help does not mention refund:\n%s", r.out)
	}
}
