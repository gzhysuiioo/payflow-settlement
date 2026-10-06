package main

import (
	"os"
	"strings"
	"testing"
)

// 回归：init 的每个余额条目都必须明确给出 balance。
// 缺省或 null 曾被视为 0 而静默接受，把遗漏的数据当成已确认的初始余额。
func TestCLIInitRequiresExplicitBalance(t *testing.T) {
	errKind := func(r cliResult) string {
		return decodeOut(t, r.err)["error"].(map[string]any)["kind"].(string)
	}

	// 缺省 balance（位于列表开头）：整次拒绝，不创建账本。
	missingPath := t.TempDir() + "/missing.json"
	r := runCLIWith(t, []string{"init", "-l", missingPath},
		`{"balances":[{"account":"aa-1","asset":"usdc"}]}`)
	if r.code != 1 || errKind(r) != "invalid_parameter" {
		t.Fatalf("missing balance: code=%d err=%s", r.code, r.err)
	}
	if !strings.Contains(r.err, "initial balance 0") {
		t.Fatalf("missing balance message should cite index 0: %s", r.err)
	}
	if strings.Contains(r.out, "initialized") {
		t.Fatalf("missing balance must not report success: %s", r.out)
	}
	if _, err := os.Stat(missingPath); !os.IsNotExist(err) {
		t.Fatalf("missing balance must not create ledger: %v", err)
	}

	// 显式 null（排在合法条目之后）：同样整次拒绝，前面的合法账户不得单独落账。
	nullPath := t.TempDir() + "/null.json"
	r = runCLIWith(t, []string{"init", "-l", nullPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100},{"account":"bb-2","asset":"eth","balance":null}]}`)
	if r.code != 1 || errKind(r) != "invalid_parameter" {
		t.Fatalf("null balance: code=%d err=%s", r.code, r.err)
	}
	if !strings.Contains(r.err, "initial balance 1") {
		t.Fatalf("null balance message should cite index 1: %s", r.err)
	}
	if _, err := os.Stat(nullPath); !os.IsNotExist(err) {
		t.Fatalf("null balance must not create ledger: %v", err)
	}

	// 通过 -f/--file 读取时适用同一规则。
	filePath := t.TempDir() + "/file.json"
	inputFile := t.TempDir() + "/input.json"
	if err := os.WriteFile(inputFile, []byte(`{"balances":[{"account":"aa-1","asset":"usdc"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runCLIWith(t, []string{"init", "-l", filePath, "-f", inputFile}, "")
	if r.code != 1 || errKind(r) != "invalid_parameter" {
		t.Fatalf("file input missing balance: code=%d err=%s", r.code, r.err)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatalf("file input missing balance must not create ledger: %v", err)
	}

	// 显式写出的 0 仍是合法余额，int64 上界按原值精确保留。
	zeroPath := t.TempDir() + "/zero.json"
	r = runCLIWith(t, []string{"init", "-l", zeroPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":0},{"account":"bb-2","asset":"eth","balance":9223372036854775807}]}`)
	if r.code != 0 {
		t.Fatalf("explicit zero/max init: code=%d err=%s", r.code, r.err)
	}
	r = runCLIWith(t, []string{"query", "-l", zeroPath}, "")
	if r.code != 0 {
		t.Fatalf("query: code=%d err=%s", r.code, r.err)
	}
	bals := decodeOut(t, r.out)["balances"].([]any)
	if len(bals) != 2 {
		t.Fatalf("query balances=%s", r.out)
	}
	first := bals[0].(map[string]any)
	if first["account"] != "aa-1" || first["balance"] != float64(0) {
		t.Fatalf("explicit zero balance not preserved: %s", r.out)
	}
	second := bals[1].(map[string]any)
	if second["account"] != "bb-2" {
		t.Fatalf("max int64 entry missing: %s", r.out)
	}
	// 数值必须逐字保留，不能经过小数转换（float64 无法精确表示该值）。
	if !strings.Contains(r.out, "9223372036854775807") {
		t.Fatalf("max int64 balance not preserved exactly: %s", r.out)
	}

	// 既有非法金额继续拒绝：负数、字符串、小数、布尔、超出 int64 范围。
	for _, body := range []string{
		`{"balances":[{"account":"a","asset":"usdc","balance":-1}]}`,
		`{"balances":[{"account":"a","asset":"usdc","balance":"5"}]}`,
		`{"balances":[{"account":"a","asset":"usdc","balance":1.5}]}`,
		`{"balances":[{"account":"a","asset":"usdc","balance":true}]}`,
		`{"balances":[{"account":"a","asset":"usdc","balance":9223372036854775808}]}`,
	} {
		p := t.TempDir() + "/bad.json"
		r = runCLIWith(t, []string{"init", "-l", p}, body)
		if r.code != 1 || errKind(r) != "invalid_parameter" {
			t.Fatalf("invalid balance %s: code=%d err=%s", body, r.code, r.err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("invalid balance %s must not create ledger: %v", body, err)
		}
	}
}
