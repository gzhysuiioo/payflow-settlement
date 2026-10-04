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

// TestCLIEvaluateRejectsTrailingContent 验证输入文件必须是一个完整 JSON 文档：
// 完整对象之后出现第二个顶层值（即使本身合法）、未写完的内容或不能构成 JSON
// 的字符时，命令非零退出、stdout 为空、stderr 指出是配置还是上下文且说明 JSON
// 无效。作为第一份文档的 validConfig 与合法上下文自身满足全部字段规则，失败
// 只能来自文件边界。尾随内容不含重复字段或非法转义，避免触发其他检查。
func TestCLIEvaluateRejectsTrailingContent(t *testing.T) {
	goodCtx := `{"plan":"pro","region":"cn"}`
	trailings := []struct {
		name   string
		suffix string
	}{
		{"second object adjacent", `{"x":1}`},
		{"second object after whitespace", " \t\r\n" + `{"x":1}`},
		{"array", `["x"]`},
		{"string", `"extra"`},
		{"number", `42`},
		{"true", `true`},
		{"false", `false`},
		{"null", `null`},
		{"incomplete object", `{"x":`},
		{"unterminated string", `"abc`},
		{"lone closing brace", `}`},
		{"non-JSON character", `@`},
	}
	for _, tc := range trailings {
		t.Run("config/"+tc.name, func(t *testing.T) {
			h := newHarness(t, validConfig+tc.suffix, goodCtx)
			var stdout, stderr bytes.Buffer
			if code := run(h.evalArgs("feature-a"), &stdout, &stderr); code == 0 {
				t.Fatalf("expected non-zero exit, stdout=%q", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty, got %q", stdout.String())
			}
			if msg := stderr.String(); !strings.Contains(msg, "config: invalid JSON") {
				t.Fatalf("stderr=%q, want config invalid JSON", msg)
			}
		})
		t.Run("context/"+tc.name, func(t *testing.T) {
			h := newHarness(t, validConfig, goodCtx+tc.suffix)
			var stdout, stderr bytes.Buffer
			if code := run(h.evalArgs("feature-a"), &stdout, &stderr); code == 0 {
				t.Fatalf("expected non-zero exit, stdout=%q", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty, got %q", stdout.String())
			}
			if msg := stderr.String(); !strings.Contains(msg, "context: invalid JSON") {
				t.Fatalf("stderr=%q, want context invalid JSON", msg)
			}
		})
	}
}

func TestCLIEvaluateTrailingContentNotSkippedByOutcome(t *testing.T) {
	// 请求的开关已关闭、或原本会立即命中规则，都不能跳过对剩余文件内容的检查。
	goodCtx := `{"plan":"pro","region":"cn"}`
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"disabled flag", "feature-b"},
		{"immediate rule hit", "feature-a"},
		{"default fallback", "feature-c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, validConfig, goodCtx+` {}`)
			var stdout, stderr bytes.Buffer
			if code := run(h.evalArgs(tc.key), &stdout, &stderr); code == 0 {
				t.Fatalf("trailing content must not be skipped, stdout=%q", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty, got %q", stdout.String())
			}
			if msg := stderr.String(); !strings.Contains(msg, "context: invalid JSON") {
				t.Fatalf("stderr=%q, want context invalid JSON", msg)
			}
		})
	}
}

func TestCLIEvaluateWhitespaceAroundDocument(t *testing.T) {
	// 文件开头与结尾的空格、制表符、回车、换行不改变求值结果。
	padded := newHarness(t, " \t\r\n"+validConfig+"\t \r\n", "\n\t "+`{"plan":"pro","region":"cn"}`+" \r\n")
	plain := newHarness(t, validConfig, `{"plan":"pro","region":"cn"}`)
	var paddedOut, plainOut bytes.Buffer
	if code := run(padded.evalArgs("feature-a"), &paddedOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("padded input must evaluate: exit=%d", code)
	}
	if code := run(plain.evalArgs("feature-a"), &plainOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("plain input must evaluate: exit=%d", code)
	}
	if paddedOut.String() != plainOut.String() {
		t.Fatalf("whitespace changed result: %q != %q", paddedOut.String(), plainOut.String())
	}
	var got map[string]any
	if err := json.Unmarshal(paddedOut.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %q (%v)", paddedOut.String(), err)
	}
	if got["key"] != "feature-a" || got["value"] != true || got["reason"] != "rule" || got["ruleId"] != "r-pro" {
		t.Fatalf("unexpected payload: %v", got)
	}
}

func TestCLIEvaluateTrailingContentRemovalRestoresEvaluation(t *testing.T) {
	// 同一份业务输入：上下文带尾随内容时被拒绝；移除后三种求值路径恢复正常——
	// 首条完整命中规则决定结果、关闭开关固定 false、无命中采用默认值。
	goodCtx := `{"plan":"pro","region":"cn"}`
	h := newHarness(t, validConfig, goodCtx+` {"x":1}`)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalArgs("feature-a"), &stdout, &stderr); code == 0 {
		t.Fatal("trailing content must be rejected")
	}

	h = newHarness(t, validConfig, goodCtx)
	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalArgs("feature-a"), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("rule hit: exit=%d stderr=%q", code, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %q (%v)", stdout.String(), err)
	}
	if got["key"] != "feature-a" || got["value"] != true || got["reason"] != "rule" || got["ruleId"] != "r-pro" {
		t.Fatalf("first matching rule must decide: %v", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalArgs("feature-b"), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("disabled: exit=%d stderr=%q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("disabled output: %q", stdout.String())
	}
	if got["value"] != false || got["reason"] != "disabled" || got["ruleId"] != nil {
		t.Fatalf("disabled flag must stay false: %v", got)
	}

	h = newHarness(t, validConfig, `{}`)
	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalArgs("feature-c"), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("default: exit=%d stderr=%q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("default output: %q", stdout.String())
	}
	if got["value"] != true || got["reason"] != "default" || got["ruleId"] != nil {
		t.Fatalf("no match must fall back to default: %v", got)
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

// extraCLIConfig 与 validConfig 的开关定义相同，另在顶层、开关、规则、条件上
// 附带对象、数组、布尔、null 与语法合法但超出浮点范围的大数字（1e400、
// 1 后接 400 个 0 的整数）。这些附加信息不参与求值。
var extraCLIConfig = `{"meta":{"owner":"pay","big":1e400,"huge":` + "1" + strings.Repeat("0", 400) + `,"tags":["x",1e400],"ok":true,"nil":null},"flags":[
	{"key":"feature-a","enabled":true,"default":false,"note":"A","labels":["a"],"reviewed":true,"archived":null,"rules":[
		{"id":"r-pro","value":true,"comment":{"n":1e400},"conditions":[
			{"attribute":"plan","op":"eq","value":"pro","hint":"plan"},
			{"attribute":"region","op":"in","value":["cn","us"],"weight":1e400}]}]},
	{"key":"feature-b","enabled":false,"default":true,"rules":[],"meta":{"big":1e400}},
	{"key":"feature-c","enabled":true,"default":true,"rules":[]}]}`

func TestCLIEvaluateExtraFieldsDoNotChangeResult(t *testing.T) {
	// 附加信息不改变求值：带附加字段的配置与 validConfig 对同一开关键、同一
	// 上下文的输出逐字节一致；stdout 仍是单个结果对象，stderr 为空。
	cases := []struct {
		name string
		key  string
		ctx  string
	}{
		{"first matching rule", "feature-a", `{"plan":"pro","region":"cn"}`},
		{"default fallback", "feature-a", `{}`},
		{"disabled flag", "feature-b", `{"plan":"pro","region":"cn"}`},
		{"no rules default", "feature-c", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain := newHarness(t, validConfig, tc.ctx)
			extras := newHarness(t, extraCLIConfig, tc.ctx)
			var plainOut, extraOut, extraErr bytes.Buffer
			if code := run(plain.evalArgs(tc.key), &plainOut, &bytes.Buffer{}); code != 0 {
				t.Fatalf("plain config: exit=%d", code)
			}
			if code := run(extras.evalArgs(tc.key), &extraOut, &extraErr); code != 0 {
				t.Fatalf("config with extra fields: exit=%d stderr=%s", code, extraErr.String())
			}
			if extraErr.Len() != 0 {
				t.Fatalf("stderr must be empty on success: %q", extraErr.String())
			}
			if extraOut.String() != plainOut.String() {
				t.Fatalf("extra fields changed output: %q != %q", extraOut.String(), plainOut.String())
			}
			var got map[string]any
			if err := json.Unmarshal(extraOut.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not a single JSON object: %q (%v)", extraOut.String(), err)
			}
			if got["key"] != tc.key {
				t.Fatalf("unexpected payload: %v", got)
			}
		})
	}
}

func TestCLIEvaluateExtraFieldFailures(t *testing.T) {
	// 附加信息虽不参与求值，仍属于被完整检查的配置：其中的重复字段与无效
	// JSON 都要非零退出、stdout 为空，stderr 指出问题来自配置，且原因能区分
	// 重复字段与无效 JSON。
	hugeInt := "1" + strings.Repeat("0", 400)
	cases := []struct {
		name    string
		config  string
		key     string
		wantSub []string // stderr 必须包含的片段
		notSub  []string // stderr 不得包含的片段
	}{
		{
			"duplicate after exponent overflow in extra object",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
				"meta":{"n":1e400,"n":2}}]}`,
			"f",
			[]string{"config", "duplicate field", `config.flags[0].meta.n`},
			[]string{"invalid JSON"},
		},
		{
			"duplicate after 400-digit integer in extra object",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
				"meta":{"n":` + hugeInt + `,"n":2}}]}`,
			"f",
			[]string{"config", "duplicate field", `config.flags[0].meta.n`},
			[]string{"invalid JSON"},
		},
		{
			"duplicate in unselected flag extra field",
			`{"flags":[
				{"key":"good","enabled":true,"default":false,"rules":[]},
				{"key":"bad","enabled":true,"default":false,"rules":[],"meta":{"x":1,"x":2}}]}`,
			"good",
			[]string{"config", "duplicate field", `config.flags[1].meta.x`},
			nil,
		},
		{
			"duplicate in disabled flag extra field",
			`{"flags":[{"key":"off","enabled":false,"default":false,"rules":[],"meta":{"x":1,"x":2}}]}`,
			"off",
			[]string{"config", "duplicate field", `config.flags[0].meta.x`},
			nil,
		},
		{
			"malformed number in extra field",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
				"meta":{"n":1e+}}]}`,
			"f",
			[]string{"config: invalid JSON"},
			[]string{"duplicate field"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.config, `{}`)
			var stdout, stderr bytes.Buffer
			if code := run(h.evalArgs(tc.key), &stdout, &stderr); code == 0 {
				t.Fatalf("expected non-zero exit, stdout=%q", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty on error, got %q", stdout.String())
			}
			msg := stderr.String()
			for _, want := range tc.wantSub {
				if !strings.Contains(msg, want) {
					t.Fatalf("stderr=%q, want substring %q", msg, want)
				}
			}
			for _, not := range tc.notSub {
				if strings.Contains(msg, not) {
					t.Fatalf("stderr=%q, must not contain %q", msg, not)
				}
			}
		})
	}
}
