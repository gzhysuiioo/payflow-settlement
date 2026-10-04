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

// bigNumberConfig 的 r-big 规则要求 plan 按字符串原文等于 "1e400"；feature-off
// 关闭，feature-plain 启用但规则不引用 plan，用于验证校验与求值互不豁免。
const bigNumberConfig = `{"flags":[
	{"key":"feature-big","enabled":true,"default":false,"rules":[
		{"id":"r-big","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"1e400"}]}]},
	{"key":"feature-off","enabled":false,"default":true,"rules":[]},
	{"key":"feature-plain","enabled":true,"default":true,"rules":[
		{"id":"r-region","value":false,"conditions":[
			{"attribute":"region","op":"in","value":["eu"]}]}]}]}`

func TestCLIEvaluateOutOfRangeNumbersRejected(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	cases := []struct {
		name    string
		key     string
		context string
		want    string // stderr 必须包含的片段
	}{
		{"exponent overflow", "feature-big", `{"plan":1e400}`, "context.plan"},
		{"negative exponent overflow", "feature-big", `{"plan":-1e400}`, "context.plan"},
		{"400-digit integer", "feature-big", `{"plan":` + hugeInt + `}`, "context.plan"},
		{"object value holding big number", "feature-big", `{"plan":{"inner":1e400}}`, "context.plan"},
		{"array value holding big number", "feature-big", `{"plan":[1e400]}`, "context.plan"},
		{"attr not referenced by any rule", "feature-plain", `{"unreferenced":1e400}`, "context.unreferenced"},
		{"disabled flag does not bypass validation", "feature-off", `{"plan":1e400}`, "context.plan"},
		{"bool before big number reports first attr", "feature-big", `{"zeta":false,"plan":1e400}`, "context.zeta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, bigNumberConfig, tc.context)
			var stdout, stderr bytes.Buffer
			code := run(h.evalArgs(tc.key), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("expected non-zero exit")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty on validation failure: %q", stdout.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "value must be a string") {
				t.Fatalf("stderr=%q, want %s string-type error", msg, tc.want)
			}
			if strings.Contains(msg, "invalid JSON") {
				t.Fatalf("a legal big number must be a type error, not a JSON syntax error: %q", msg)
			}
		})
	}
}

func TestCLIEvaluateBigNumberPrecedenceAndFix(t *testing.T) {
	// 完整输入检查优先：大数字之后的重复字段必须以重复字段名义报错，
	// 而不是较早属性的类型错误。
	h := newHarness(t, bigNumberConfig, `{"plan":1e400,"plan":"x"}`)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalArgs("feature-big"), &stdout, &stderr); code == 0 {
		t.Fatalf("duplicate field: expected non-zero exit")
	}
	if stdout.Len() != 0 {
		t.Fatalf("duplicate field: stdout must be empty: %q", stdout.String())
	}
	if msg := stderr.String(); !strings.Contains(msg, `context.plan: duplicate field "plan"`) {
		t.Fatalf("duplicate field: stderr=%q", msg)
	}

	// 无效的数字写法（1e+ 缺指数数字）仍是 JSON 语法错误，与大数字类型错误区分。
	h = newHarness(t, bigNumberConfig, `{"plan":1e+}`)
	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalArgs("feature-big"), &stdout, &stderr); code == 0 {
		t.Fatalf("malformed number: expected non-zero exit")
	}
	if stdout.Len() != 0 {
		t.Fatalf("malformed number: stdout must be empty: %q", stdout.String())
	}
	if msg := stderr.String(); !strings.Contains(msg, "invalid JSON") ||
		strings.Contains(msg, "value must be a string") {
		t.Fatalf("malformed number must be a syntax error: stderr=%q", msg)
	}

	// 修正为加引号的合法字符串后，按原文命中 eq 规则：stdout 为单个 JSON 对象，
	// 含开关键、布尔结果、命中原因与规则编号；stderr 为空。
	h = newHarness(t, bigNumberConfig, `{"plan":"1e400"}`)
	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalArgs("feature-big"), &stdout, &stderr); code != 0 {
		t.Fatalf("quoted big number: exit=%d stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("quoted big number: stderr must be empty: %q", stderr.String())
	}
	out := stdout.String()
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("stdout must contain exactly one JSON object line: %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %q (%v)", out, err)
	}
	if got["key"] != "feature-big" || got["value"] != true ||
		got["reason"] != "rule" || got["ruleId"] != "r-big" {
		t.Fatalf("unexpected payload: %v", got)
	}

	// 合法字符串但未命中：回到默认值行为，规则编号为 null。
	h = newHarness(t, bigNumberConfig, `{"plan":"1e401"}`)
	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalArgs("feature-big"), &stdout, &stderr); code != 0 {
		t.Fatalf("non-matching string: exit=%d stderr=%s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("non-matching output: %q (%v)", stdout.String(), err)
	}
	if got["value"] != false || got["reason"] != "default" || got["ruleId"] != nil {
		t.Fatalf("non-matching string must fall through to default: %v", got)
	}

	// 关闭开关配合合法字符串仍固定 disabled，不因上下文变为合法而启用。
	h = newHarness(t, bigNumberConfig, `{"plan":"1e400"}`)
	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalArgs("feature-off"), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("disabled flag: exit=%d stderr=%q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("disabled output: %q", stdout.String())
	}
	if got["value"] != false || got["reason"] != "disabled" || got["ruleId"] != nil {
		t.Fatalf("disabled flag must stay disabled with valid context: %v", got)
	}
}

// trailingCtx 与 validConfig 配套：feature-a 立即命中 r-pro，feature-b 已关闭，
// feature-c 无规则走默认值。
const trailingCtx = `{"plan":"pro","region":"cn"}`

func TestCLIEvaluateRejectsTrailingContent(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		context string
		key     string
		wantSub string // stderr 需指出出错的是 config 还是 context，并说明 JSON 无效
		forbid  string // 另一份文件的名字不得出现在错误里
	}{
		{"config second object adjacent", validConfig + `{"flags":[]}`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config second object spaced", validConfig + " \t\r\n" + `{"flags":[]}`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config trailing array", validConfig + `[1]`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config trailing string", validConfig + `"x"`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config trailing number", validConfig + `42`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config trailing bool", validConfig + `true`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config trailing null", validConfig + `null`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config trailing incomplete", validConfig + `{"flags":[`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config trailing garbage", validConfig + `###`, trailingCtx, "feature-a", "config: invalid JSON", "context"},
		{"config trailing with disabled flag", validConfig + ` {}`, trailingCtx, "feature-b", "config: invalid JSON", "context"},
		{"context second object adjacent", validConfig, trailingCtx + `{"plan":"pro"}`, "feature-a", "context: invalid JSON", "config"},
		{"context second object spaced", validConfig, trailingCtx + "  " + `{"plan":"pro"}`, "feature-a", "context: invalid JSON", "config"},
		{"context trailing array", validConfig, trailingCtx + `["x"]`, "feature-a", "context: invalid JSON", "config"},
		{"context trailing string", validConfig, trailingCtx + `"x"`, "feature-a", "context: invalid JSON", "config"},
		{"context trailing number", validConfig, trailingCtx + `42`, "feature-a", "context: invalid JSON", "config"},
		{"context trailing bool", validConfig, trailingCtx + `false`, "feature-a", "context: invalid JSON", "config"},
		{"context trailing null", validConfig, trailingCtx + `null`, "feature-a", "context: invalid JSON", "config"},
		{"context trailing incomplete", validConfig, trailingCtx + `{"plan":`, "feature-a", "context: invalid JSON", "config"},
		{"context trailing garbage", validConfig, trailingCtx + `???`, "feature-a", "context: invalid JSON", "config"},
		{"context trailing with disabled flag", validConfig, trailingCtx + ` {}`, "feature-b", "context: invalid JSON", "config"},
		{"both files trailing reports config", validConfig + ` {}`, trailingCtx + ` []`, "feature-a", "config: invalid JSON", "context"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.config, tc.context)
			var stdout, stderr bytes.Buffer
			code := run(h.evalArgs(tc.key), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("expected non-zero exit")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty on trailing content, got: %q", stdout.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, tc.wantSub) {
				t.Fatalf("stderr=%q, want substring %q", msg, tc.wantSub)
			}
			if strings.Contains(msg, tc.forbid+":") {
				t.Fatalf("stderr must blame only the file with trailing content: %q", msg)
			}
		})
	}
}

func TestCLIEvaluateTrailingContentRemovalRestoresEvaluation(t *testing.T) {
	h := newHarness(t, validConfig+" {}", trailingCtx+" []")
	for _, key := range []string{"feature-a", "feature-b", "feature-c"} {
		var stdout, stderr bytes.Buffer
		if code := run(h.evalArgs(key), &stdout, &stderr); code == 0 {
			t.Fatalf("%s: expected non-zero exit with trailing content", key)
		} else if stdout.Len() != 0 {
			t.Fatalf("%s: stdout must be empty with trailing content: %q", key, stdout.String())
		}
	}

	// 移除两份文件的尾随内容后，同一份业务输入恢复正常求值。
	if err := os.WriteFile(h.configPath, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.contextPath, []byte(trailingCtx), 0o600); err != nil {
		t.Fatal(err)
	}
	runAndExpect := func(key, want string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(h.evalArgs(key), &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit=%d stderr=%s", key, code, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != want {
			t.Fatalf("%s: stdout=%q want %q", key, got, want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("%s: stderr must be empty: %q", key, stderr.String())
		}
	}
	// 首条完整命中规则决定结果；关闭开关固定 false；无命中采用默认值。
	runAndExpect("feature-a", `{"key":"feature-a","value":true,"reason":"rule","ruleId":"r-pro"}`)
	runAndExpect("feature-b", `{"key":"feature-b","value":false,"reason":"disabled","ruleId":null}`)
	runAndExpect("feature-c", `{"key":"feature-c","value":true,"reason":"default","ruleId":null}`)
}

func TestCLIEvaluateWhitespacePaddingKeepsResult(t *testing.T) {
	// 文件首尾的空格、制表符、回车与换行不改变求值结果。
	pad := " \t\r\n"
	plain := newHarness(t, validConfig, trailingCtx)
	var plainOut, plainErr bytes.Buffer
	if code := run(plain.evalArgs("feature-a"), &plainOut, &plainErr); code != 0 {
		t.Fatalf("plain: exit=%d stderr=%s", code, plainErr.String())
	}
	padded := newHarness(t, pad+validConfig+pad, pad+trailingCtx+pad)
	var paddedOut, paddedErr bytes.Buffer
	if code := run(padded.evalArgs("feature-a"), &paddedOut, &paddedErr); code != 0 {
		t.Fatalf("padded: exit=%d stderr=%s", code, paddedErr.String())
	}
	if paddedOut.String() != plainOut.String() {
		t.Fatalf("padded %q != plain %q", paddedOut.String(), plainOut.String())
	}
	want := `{"key":"feature-a","value":true,"reason":"rule","ruleId":"r-pro"}`
	if got := strings.TrimSpace(plainOut.String()); got != want {
		t.Fatalf("stdout=%q want %q", got, want)
	}
}

func TestCLIEvaluateStringBracesAndEscapesStayContent(t *testing.T) {
	// 字符串里的大括号、方括号、转义引号与换行转义不会被误判成第二份文档。
	tricky := `a{b}[c]\"d\"\ne} {`
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"` + tricky + `"}]}]}]}`
	h := newHarness(t, cfg, `{"plan":"`+tricky+`"}`)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalArgs("f"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	want := `{"key":"f","value":true,"reason":"rule","ruleId":"r1"}`
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("stdout=%q want %q", got, want)
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
