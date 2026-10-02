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
