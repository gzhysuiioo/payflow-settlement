package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件回归 init 对余额字段“是否被明确提供”的处理：
// 缺省或 null 的 balance 整次拒绝（invalid_parameter，退出码 1，不创建账本），
// 显式 0 仍是合法零余额；非法条目无论排在列表开头还是合法条目之后都一样。

// assertInitRejected 断言一次 init 被整次拒绝：退出码 1、stderr 为
// invalid_parameter 错误信封、stdout 无成功输出、账本文件未被创建；
// 返回错误说明文本供进一步断言。
func assertInitRejected(t *testing.T, r cliResult, ledgerPath string) string {
	t.Helper()
	if r.code != 1 {
		t.Fatalf("exit code=%d want 1 (out=%q err=%q)", r.code, r.out, r.err)
	}
	if r.out != "" {
		t.Fatalf("no success output allowed on rejected init: %q", r.out)
	}
	env := decodeOut(t, r.err)
	e, ok := env["error"].(map[string]any)
	if !ok || e["kind"] != "invalid_parameter" {
		t.Fatalf("stderr envelope=%q want invalid_parameter", r.err)
	}
	msg, _ := e["message"].(string)
	if msg == "" {
		t.Fatalf("error envelope missing message: %q", r.err)
	}
	if _, err := os.Stat(ledgerPath); !os.IsNotExist(err) {
		t.Fatalf("rejected init must not create the ledger: stat err=%v", err)
	}
	return msg
}

// 缺省 balance 的条目（无论在列表开头还是合法条目之后）整次拒绝，
// 错误说明指出从 0 开始的条目位置，账本不被创建。
func TestCLIInitRejectsMissingBalance(t *testing.T) {
	cases := []struct {
		name  string
		input string
		index string
	}{
		{"first", `{"balances":[{"account":"aa","asset":"usdc"}]}`, "0"},
		{"after valid", `{"balances":[{"account":"aa","asset":"usdc","balance":100},{"account":"bb","asset":"eth"}]}`, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
			r := runCLIWith(t, []string{"init", "-l", ledgerPath}, tc.input)
			msg := assertInitRejected(t, r, ledgerPath)
			if !strings.Contains(msg, "initial balance "+tc.index) {
				t.Fatalf("message must point at 0-based entry %s: %q", tc.index, msg)
			}
			if !strings.Contains(msg, "non-negative int64") {
				t.Fatalf("message must explain the balance requirement: %q", msg)
			}
		})
	}
}

// 显式写 null 的 balance 与缺省同等处理：整次拒绝，不创建账本。
func TestCLIInitRejectsNullBalance(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa","asset":"usdc","balance":100},{"account":"bb","asset":"eth","balance":null}]}`)
	msg := assertInitRejected(t, r, ledgerPath)
	if !strings.Contains(msg, "initial balance 1") {
		t.Fatalf("message must point at the null entry: %q", msg)
	}
}

// 通过 -f/--file 读取 JSON 文件时适用同一规则。
func TestCLIInitRejectsMissingBalanceFromFile(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "init.json")
	if err := os.WriteFile(inputPath, []byte(`{"balances":[{"account":"aa","asset":"usdc"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(dir, "ledger.json")
	for _, flag := range []string{"-f", "--file"} {
		r := runCLIWith(t, []string{"init", "-l", ledgerPath, flag, inputPath}, "")
		assertInitRejected(t, r, ledgerPath)
	}
}

// 显式写出的 balance:0 是合法零余额：初始化成功，查询保留该组合及其零余额。
func TestCLIInitAcceptsExplicitZeroBalance(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa","asset":"usdc","balance":0},{"account":"bb","asset":"eth","balance":7}]}`)
	if r.code != 0 {
		t.Fatalf("init with explicit zero: code=%d err=%s", r.code, r.err)
	}
	if v := decodeOut(t, r.out); v["status"] != "initialized" {
		t.Fatalf("init out=%s", r.out)
	}

	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query: code=%d err=%s", r.code, r.err)
	}
	var snap struct {
		Balances []struct {
			Account string `json:"account"`
			Asset   string `json:"asset"`
			Balance int64  `json:"balance"`
		} `json:"balances"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatalf("query out not JSON: %v\n%s", err, r.out)
	}
	if len(snap.Balances) != 2 ||
		snap.Balances[0].Account != "aa" || snap.Balances[0].Balance != 0 ||
		snap.Balances[1].Account != "bb" || snap.Balances[1].Balance != 7 {
		t.Fatalf("explicit zero balance must be preserved: %+v", snap.Balances)
	}
}

// int64 上限 9223372036854775807 按原值准确接受，不经过小数转换。
func TestCLIInitAcceptsMaxInt64Balance(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa","asset":"usdc","balance":9223372036854775807}]}`)
	if r.code != 0 {
		t.Fatalf("init with max int64: code=%d err=%s", r.code, r.err)
	}
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query: code=%d err=%s", r.code, r.err)
	}
	var snap struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatalf("query out not JSON: %v\n%s", err, r.out)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 9223372036854775807 {
		t.Fatalf("max int64 balance altered: %+v", snap.Balances)
	}}

// 负数、字符串、小数、布尔值与超出 int64 范围的数值继续作为非法输入拒绝。
func TestCLIInitRejectsInvalidBalanceValues(t *testing.T) {
	cases := map[string]string{
		"negative":  `{"balances":[{"account":"aa","asset":"usdc","balance":-1}]}`,
		"string":    `{"balances":[{"account":"aa","asset":"usdc","balance":"100"}]}`,
		"fraction":  `{"balances":[{"account":"aa","asset":"usdc","balance":1.5}]}`,
		"boolean":   `{"balances":[{"account":"aa","asset":"usdc","balance":true}]}`,
		"overflow":  `{"balances":[{"account":"aa","asset":"usdc","balance":9223372036854775808}]}`,
		"underflow": `{"balances":[{"account":"aa","asset":"usdc","balance":-9223372036854775809}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
			r := runCLIWith(t, []string{"init", "-l", ledgerPath}, input)
			assertInitRejected(t, r, ledgerPath)
		})
	}
}

// 空余额列表初始化行为保持不变。
func TestCLIInitEmptyBalancesStillWorks(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
	r := runCLIWith(t, []string{"init", "-l", ledgerPath}, `{"balances":[]}`)
	if r.code != 0 {
		t.Fatalf("empty init: code=%d err=%s", r.code, r.err)
	}
	if v := decodeOut(t, r.out); v["status"] != "initialized" {
		t.Fatalf("init out=%s", r.out)
	}
}
