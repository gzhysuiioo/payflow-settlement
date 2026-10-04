package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// harness captures stdout/stderr while running the command dispatcher.
type harness struct {
	configPath  string
	contextPath string
	stdout      bytes.Buffer
	stderr      bytes.Buffer
}

func newHarness(t *testing.T, config, context string) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{}
	if config != "" {
		h.configPath = filepath.Join(dir, "config.json")
		if err := os.WriteFile(h.configPath, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if context != "" {
		h.contextPath = filepath.Join(dir, "context.json")
		if err := os.WriteFile(h.contextPath, []byte(context), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) evalArgs(key string) []string {
	return []string{"evaluate", h.configPath, key, h.contextPath}
}

const validConfig = `{"flags":[
	{"key":"feature-a","enabled":true,"default":false,"rules":[
		{"id":"r-pro","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"region","op":"in","value":["cn","us"]}]}]},
	{"key":"feature-b","enabled":false,"default":true,"rules":[]},
	{"key":"feature-c","enabled":true,"default":true,"rules":[]}]}`

func TestCLIEvaluateRuleHit(t *testing.T) {
	h := newHarness(t, validConfig, `{"plan":"pro","region":"cn"}`)
	code := run(h.evalArgs("feature-a"), &h.stdout, &h.stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, h.stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %q (%v)", h.stdout.String(), err)
	}
	if got["key"] != "feature-a" || got["value"] != true || got["reason"] != "rule" || got["ruleId"] != "r-pro" {
		t.Fatalf("unexpected payload: %v", got)
	}
	if strings.Count(strings.TrimSpace(h.stdout.String()), "\n") != 0 {
		t.Fatalf("stdout must contain exactly one JSON object line: %q", h.stdout.String())
	}
}

func TestCLIEvaluateDisabled(t *testing.T) {
	h := newHarness(t, validConfig, `{"plan":"pro","region":"cn"}`)
	code := run(h.evalArgs("feature-b"), &h.stdout, &h.stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, h.stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["value"] != false || got["reason"] != "disabled" || got["ruleId"] != nil {
		t.Fatalf("unexpected: %v", got)
	}
}

func TestCLIEvaluateDefault(t *testing.T) {
	h := newHarness(t, validConfig, `{}`)
	code := run(h.evalArgs("feature-c"), &h.stdout, &h.stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, h.stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["value"] != true || got["reason"] != "default" || got["ruleId"] != nil {
		t.Fatalf("unexpected: %v", got)
	}
}

func TestCLIEvaluateRuleMissFallsThrough(t *testing.T) {
	h := newHarness(t, validConfig, `{"plan":"pro","region":"eu"}`)
	code := run(h.evalArgs("feature-a"), &h.stdout, &h.stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["reason"] != "default" || got["value"] != false {
		t.Fatalf("unexpected: %v", got)
	}
}

func TestCLIEvaluateRepeatedIsStable(t *testing.T) {
	h := newHarness(t, validConfig, `{"plan":"pro","region":"us"}`)
	var first string
	for i := 0; i < 5; i++ {
		var out bytes.Buffer
		if code := run(h.evalArgs("feature-a"), &out, &bytes.Buffer{}); code != 0 {
			t.Fatalf("iter %d exit=%d", i, code)
		}
		if i == 0 {
			first = out.String()
		} else if out.String() != first {
			t.Fatalf("iter %d: %q != %q", i, out.String(), first)
		}
	}
}

func TestCLIErrorCases(t *testing.T) {
	goodCfg := validConfig
	goodCtx := `{}`

	cases := []struct {
		name    string
		prepare func(t *testing.T) (args []string)
		wantSub string
	}{
		{
			"no arguments",
			func(t *testing.T) []string { return []string{"evaluate"} },
			"expected 3 arguments",
		},
		{
			"two arguments",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, goodCtx)
				return []string{"evaluate", h.configPath, "feature-a"}
			},
			"expected 3 arguments",
		},
		{
			"config unreadable",
			func(t *testing.T) []string {
				h := newHarness(t, "", goodCtx)
				return []string{"evaluate", filepath.Join(t.TempDir(), "missing.json"), "feature-a", h.contextPath}
			},
			"cannot read config file",
		},
		{
			"context unreadable",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, "")
				return []string{"evaluate", h.configPath, "feature-a", filepath.Join(t.TempDir(), "missing.json")}
			},
			"cannot read context file",
		},
		{
			"config invalid json",
			func(t *testing.T) []string {
				h := newHarness(t, `{"flags":[`, goodCtx)
				return h.evalArgs("feature-a")
			},
			"invalid JSON",
		},
		{
			"config missing bool field",
			func(t *testing.T) []string {
				h := newHarness(t, `{"flags":[{"key":"f","default":false,"rules":[]}]}`, goodCtx)
				return h.evalArgs("f")
			},
			"flags[0].enabled",
		},
		{
			"duplicate flag key",
			func(t *testing.T) []string {
				h := newHarness(t, `{"flags":[
					{"key":"f","enabled":true,"default":false,"rules":[]},
					{"key":"f","enabled":true,"default":false,"rules":[]}]}`, goodCtx)
				return h.evalArgs("f")
			},
			"duplicate flag key",
		},
		{
			"duplicate field in config",
			func(t *testing.T) []string {
				h := newHarness(t, `{"flags":[
					{"key":"f","enabled":true,"default":false,"rules":[]},
					{"key":"g","enabled":false,"enabled":true,"default":false,"rules":[]}]}`, goodCtx)
				return h.evalArgs("f")
			},
			`config.flags[1].enabled: duplicate field "enabled"`,
		},
		{
			"duplicate field in context",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, `{"plan":"free","plan":"pro"}`)
				return h.evalArgs("feature-a")
			},
			`context.plan: duplicate field "plan"`,
		},
		{
			"empty conditions on disabled flag",
			func(t *testing.T) []string {
				// 已关闭的开关也要完整校验。
				h := newHarness(t, `{"flags":[{"key":"f","enabled":false,"default":false,"rules":[
					{"id":"r","value":true,"conditions":[]}]}]}`, goodCtx)
				return h.evalArgs("f")
			},
			"conditions",
		},
		{
			"unknown op in unselected flag",
			func(t *testing.T) []string {
				h := newHarness(t, `{"flags":[
					{"key":"good","enabled":true,"default":false,"rules":[]},
					{"key":"bad","enabled":true,"default":false,"rules":[
						{"id":"r","value":true,"conditions":[{"attribute":"a","op":"nope","value":"x"}]}]}]}`, goodCtx)
				return h.evalArgs("good")
			},
			`conditions[0].op`,
		},
		{
			"context invalid json",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, `{bad`)
				return h.evalArgs("feature-a")
			},
			"invalid JSON",
		},
		{
			"context non-string value",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, `{"plan":5}`)
				return h.evalArgs("feature-a")
			},
			"context.plan",
		},
		{
			"flag not found",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, goodCtx)
				return h.evalArgs("nope")
			},
			"not found",
		},
		{
			"config lone surrogate escape",
			func(t *testing.T) []string {
				h := newHarness(t, `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
					{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`, goodCtx)
				return h.evalArgs("f")
			},
			`config.flags[0].rules[0].conditions[0].value`,
		},
		{
			"context lone surrogate escape",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, `{"plan":"\uD800"}`)
				return h.evalArgs("feature-a")
			},
			`context.plan`,
		},
		{
			"context lone surrogate in field name",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, `{"pl\uD800an":"pro"}`)
				return h.evalArgs("feature-a")
			},
			`\uD800`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.prepare(t), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("expected non-zero exit")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty on error, got: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.wantSub) {
				t.Fatalf("stderr=%q, want substring %q", stderr.String(), tc.wantSub)
			}
		})
	}
}

// TestCLIEvaluateOutOfRangeNumbersAreTypeErrors 端到端锁定：上下文里未加
// 引号的 1e400、-1e400 与 1 后接 400 个 0 的整数是合法 JSON 中的错误属性
// 类型——非零退出、stdout 为空、stderr 指出属性并说明必须是字符串，而不是
// JSON 语法错误。开关关闭或规则不引用该属性都不能放行。
func TestCLIEvaluateOutOfRangeNumbersAreTypeErrors(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	cases := []struct {
		name string
		key  string
		ctx  string
		want string
	}{
		{"1e400", "feature-a", `{"plan":1e400}`, "context.plan"},
		{"-1e400", "feature-a", `{"plan":-1e400}`, "context.plan"},
		{"400-digit integer", "feature-a", `{"plan":` + hugeInt + `}`, "context.plan"},
		{"disabled flag does not exempt", "feature-b", `{"plan":1e400}`, "context.plan"},
		{"unreferenced attribute", "feature-a", `{"plan":"pro","region":"cn","extra":-1e400}`, "context.extra"},
		{"inside object points to top key", "feature-a", `{"plan":{"x":1e400}}`, "context.plan"},
		{"inside array points to top key", "feature-a", `{"plan":[1e400]}`, "context.plan"},
		{"bool error before big number wins", "feature-a", `{"zeta":false,"plan":1e400}`, "context.zeta"},
		{"big number first wins", "feature-a", `{"plan":1e400,"zeta":false}`, "context.plan"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, validConfig, tc.ctx)
			var stdout, stderr bytes.Buffer
			code := run(h.evalArgs(tc.key), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("expected non-zero exit")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty on error, got: %q", stdout.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "value must be a string") {
				t.Fatalf("stderr=%q, want %s string-type error", msg, tc.want)
			}
			if strings.Contains(msg, "invalid JSON") || strings.Contains(msg, "invalid character") {
				t.Fatalf("legal out-of-range number must not be a JSON syntax error: %q", msg)
			}
		})
	}
}

// TestCLIEvaluateBigNumberPrecedenceAndSyntax 端到端锁定：完整输入检查优先
// 于属性类型检查（大数字之后的重复字段先报），而 1e+ 这类无效数字写法仍是
// JSON 语法错误。
func TestCLIEvaluateBigNumberPrecedenceAndSyntax(t *testing.T) {
	cases := []struct {
		name string
		ctx  string
		want string
	}{
		{"duplicate after big number", `{"a":1e400,"b":1,"b":2}`, `context.b: duplicate field "b"`},
		{"duplicate after negative big number", `{"a":-1e400,"b":1,"b":2}`, `context.b: duplicate field "b"`},
		{"duplicate of the big-number attr", `{"a":1e400,"a":2}`, `context.a: duplicate field "a"`},
		{"malformed exponent 1e+", `{"a":1e+}`, "invalid JSON"},
		{"malformed exponent beats later duplicate", `{"a":1e+,"b":1,"b":2}`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, validConfig, tc.ctx)
			var stdout, stderr bytes.Buffer
			code := run(h.evalArgs("feature-a"), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("expected non-zero exit")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty on error, got: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr=%q, want substring %q", stderr.String(), tc.want)
			}
		})
	}
}

// TestCLIEvaluateQuotedBigNumberMatchesRule 端到端锁定：大数字文本加上引号
// 后就是合法字符串，按原文参与 eq 匹配：成功时 stdout 仍是包含开关键、布尔
// 结果、命中原因和规则编号的单个 JSON 对象，stderr 为空，文本不被当成数值
// 改写。关闭开关场景也保持既有 disabled 行为。
func TestCLIEvaluateQuotedBigNumberMatchesRule(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	bigConfig := `{"flags":[
		{"key":"big","enabled":true,"default":false,"rules":[
			{"id":"r-big","value":true,"conditions":[
				{"attribute":"v","op":"eq","value":"1e400"},
				{"attribute":"w","op":"eq","value":"-1e400"},
				{"attribute":"n","op":"eq","value":"` + hugeInt + `"}]}]},
		{"key":"off","enabled":false,"default":true,"rules":[]}]}`
	h := newHarness(t, bigConfig, `{"v":"1e400","w":"-1e400","n":"`+hugeInt+`"}`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"evaluate", h.configPath, "big", h.contextPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr must be empty on success, got: %q", stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %q (%v)", stdout.String(), err)
	}
	if got["key"] != "big" || got["value"] != true || got["reason"] != "rule" || got["ruleId"] != "r-big" {
		t.Fatalf("unexpected payload: %v", got)
	}
	if strings.Count(strings.TrimSpace(stdout.String()), "\n") != 0 {
		t.Fatalf("stdout must contain exactly one JSON object line: %q", stdout.String())
	}

	// 文本差一个字符就不命中（不能在数值意义上被归一化）。
	miss := newHarness(t, bigConfig, `{"v":"1e400","w":"-1e400","n":"`+hugeInt+`x"}`)
	var out2, err2 bytes.Buffer
	if code := run([]string{"evaluate", miss.configPath, "big", miss.contextPath}, &out2, &err2); code != 0 {
		t.Fatalf("miss run exit=%d stderr=%s", code, err2.String())
	}
	var missResult map[string]any
	if err := json.Unmarshal(out2.Bytes(), &missResult); err != nil {
		t.Fatalf("miss output: %v", err)
	}
	if missResult["reason"] != "default" || missResult["value"] != false {
		t.Fatalf("quoted text must compare verbatim: %v", missResult)
	}

	// 关闭的开关上，带大数字字符串的上下文仍走既有 disabled 路径。
	off := newHarness(t, bigConfig, `{"anything":"1e400"}`)
	var out3, err3 bytes.Buffer
	if code := run([]string{"evaluate", off.configPath, "off", off.contextPath}, &out3, &err3); code != 0 {
		t.Fatalf("disabled run exit=%d stderr=%s", code, err3.String())
	}
	var offResult map[string]any
	if err := json.Unmarshal(out3.Bytes(), &offResult); err != nil {
		t.Fatalf("disabled output: %v", err)
	}
	if offResult["value"] != false || offResult["reason"] != "disabled" || offResult["ruleId"] != nil {
		t.Fatalf("disabled behavior changed: %v", offResult)
	}
	if err3.Len() != 0 {
		t.Fatalf("disabled success must leave stderr empty: %q", err3.String())
	}
}

func TestCLILegacyCommandsPreserved(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "payflow 0.1.0" {
		t.Fatalf("version: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"demo"}, &stdout, &stderr); code != 0 {
		t.Fatalf("demo: code=%d err=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "intent=pay-1") || !strings.Contains(out, "duplicate") ||
		!strings.Contains(out, "reconciliation gaps: []") {
		t.Fatalf("demo output changed unexpectedly:\n%s", out)
	}

	// 无参数仍运行 demo。
	stdout.Reset()
	if code := run(nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "intent=pay-1") {
		t.Fatalf("no-arg demo: code=%d out=%q", code, stdout.String())
	}

	// help 提到 evaluate 与文件格式。
	stdout.Reset()
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help: %d", code)
	}
	h := stdout.String()
	for _, frag := range []string{"evaluate", "config.json", "context.json", "disabled|rule|default", "eq", "in"} {
		if !strings.Contains(h, frag) {
			t.Errorf("help missing %q", frag)
		}
	}

	// 未知命令仍是退出码 2。
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown command exit=%d want 2", code)
	}
}
