package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// evalExplainArgs 与 h.evalArgs 相同，但在第四个参数位置附上 --explain。
func (h *harness) evalExplainArgs(key string) []string {
	return []string{"evaluate", h.configPath, key, h.contextPath, "--explain"}
}

// explainObject 运行解释模式并把唯一的 JSON 对象解码返回；同时断言退出码 0、
// stderr 为空、stdout 只有一行 JSON。
func explainObject(t *testing.T, args []string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr must be empty on success: %q", stderr.String())
	}
	out := stdout.String()
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("stdout must contain exactly one JSON object line: %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %q (%v)", out, err)
	}
	return got
}

// asMap / asSlice 把 any 断言为 map[string]any / []any，失败即终止。
func asMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s must be an object, got %T (%v)", what, v, v)
	}
	return m
}

func asSlice(t *testing.T, v any, what string) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%s must be an array, got %T (%v)", what, v, v)
	}
	return s
}

func TestCLIEvaluateExplainRuleHitStopsAtFirstMatch(t *testing.T) {
	h := newHarness(t, validConfig, `{"plan":"pro","region":"cn"}`)
	got := explainObject(t, h.evalExplainArgs("feature-a"))
	if got["key"] != "feature-a" || got["value"] != true ||
		got["reason"] != "rule" || got["ruleId"] != "r-pro" {
		t.Fatalf("four result fields wrong: %v", got)
	}
	rules := asSlice(t, asMap(t, got["explanation"], "explanation")["rules"], "explanation.rules")
	// feature-a 只有一条规则，命中即停。
	if len(rules) != 1 {
		t.Fatalf("expected 1 considered rule, got %d", len(rules))
	}
	r0 := asMap(t, rules[0], "rules[0]")
	if r0["ruleId"] != "r-pro" || r0["matched"] != true {
		t.Fatalf("rule record wrong: %v", r0)
	}
	conds := asSlice(t, r0["conditions"], "conditions")
	if len(conds) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(conds))
	}
	plan := asMap(t, conds[0], "conditions[0]")
	if plan["attribute"] != "plan" || plan["op"] != "eq" || plan["expected"] != "pro" ||
		plan["actual"] != "pro" || plan["matched"] != true {
		t.Fatalf("plan condition record wrong: %v", plan)
	}
	region := asMap(t, conds[1], "conditions[1]")
	if region["op"] != "in" || region["actual"] != "cn" || region["matched"] != true {
		t.Fatalf("region condition record wrong: %v", region)
	}
	list := asSlice(t, region["expected"], "in expected")
	if len(list) != 2 || list[0] != "cn" || list[1] != "us" {
		t.Fatalf("in expected list wrong: %v", list)
	}
}

func TestCLIEvaluateExplainStopsEvenWhenWinnerIsFalse(t *testing.T) {
	// 第一条规则返回 false 且全部条件成立：它定案并停止，后续规则不得出现。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-deny","value":false,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]},
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]}]}]}`
	h := newHarness(t, cfg, `{"plan":"pro","region":"cn"}`)
	got := explainObject(t, h.evalExplainArgs("f"))
	if got["value"] != false || got["reason"] != "rule" || got["ruleId"] != "r-deny" {
		t.Fatalf("earlier false rule must decide: %v", got)
	}
	rules := asSlice(t, asMap(t, got["explanation"], "explanation")["rules"], "rules")
	if len(rules) != 1 || asMap(t, rules[0], "rules[0]")["ruleId"] != "r-deny" {
		t.Fatalf("explanation must stop at the false-winning rule: %v", rules)
	}
}

func TestCLIEvaluateExplainKeepsEarlierMissesThenDefault(t *testing.T) {
	// 两条规则都不命中：前面的未命中记录全部保留，并说明采用 default。
	h := newHarness(t, validConfig, `{"plan":"pro","region":"eu"}`)
	got := explainObject(t, h.evalExplainArgs("feature-a"))
	if got["reason"] != "default" || got["value"] != false || got["ruleId"] != nil {
		t.Fatalf("no match must use default: %v", got)
	}
	rules := asSlice(t, asMap(t, got["explanation"], "explanation")["rules"], "rules")
	if len(rules) != 1 {
		t.Fatalf("validConfig feature-a has one rule; expected 1 recorded, got %d", len(rules))
	}
	r0 := asMap(t, rules[0], "rules[0]")
	if r0["ruleId"] != "r-pro" || r0["matched"] != false {
		t.Fatalf("missed rule record wrong: %v", r0)
	}
	conds := asSlice(t, r0["conditions"], "conditions")
	// plan=pro 成立，region=eu 不在 [cn,us]：规则整体不成立，但各条件独立记录。
	plan, region := asMap(t, conds[0], "c0"), asMap(t, conds[1], "c1")
	if plan["matched"] != true || region["matched"] != false || region["actual"] != "eu" {
		t.Fatalf("per-condition results wrong: plan=%v region=%v", plan, region)
	}
}

func TestCLIEvaluateExplainDisabledAndEmptyRules(t *testing.T) {
	// 关闭开关：固定 false，规则解释为空数组，不得把 default 或本会命中的
	// 规则写成结果依据。
	h := newHarness(t, validConfig, `{"plan":"pro","region":"cn"}`)
	got := explainObject(t, h.evalExplainArgs("feature-b"))
	if got["value"] != false || got["reason"] != "disabled" || got["ruleId"] != nil {
		t.Fatalf("disabled result wrong: %v", got)
	}
	rules := asMap(t, got["explanation"], "explanation")["rules"]
	if sl, ok := rules.([]any); !ok || len(sl) != 0 {
		t.Fatalf("disabled explanation.rules must be [], got %#v", rules)
	}

	// 空规则列表：默认结果 + 空解释。
	got = explainObject(t, h.evalExplainArgs("feature-c"))
	if got["reason"] != "default" || got["value"] != true {
		t.Fatalf("empty-rules default result wrong: %v", got)
	}
	rules = asMap(t, got["explanation"], "explanation")["rules"]
	if sl, ok := rules.([]any); !ok || len(sl) != 0 {
		t.Fatalf("empty-rules explanation.rules must be [], got %#v", rules)
	}
}

func TestCLIEvaluateExplainMissingAttributeIsDistinctFromEmptyString(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[
			{"attribute":"note","op":"eq","value":""},
			{"attribute":"tag","op":"in","value":["","x"]}]}]}]}`

	// 属性缺失：actual 必须是 null，matched=false，与显式空字符串区分。
	h := newHarness(t, cfg, `{}`)
	got := explainObject(t, h.evalExplainArgs("f"))
	r0 := asMap(t, asSlice(t, asMap(t, got["explanation"], "explanation")["rules"], "rules")[0], "r0")
	for i, c := range asSlice(t, r0["conditions"], "conditions") {
		cm := asMap(t, c, "condition")
		if _, present := cm["actual"]; !present {
			t.Fatalf("condition %d must carry an actual field (null)", i)
		}
		if cm["actual"] != nil || cm["matched"] != false {
			t.Fatalf("missing attribute condition %d wrong: %v", i, cm)
		}
	}

	// 显式给出空字符串：actual 是 ""，两个条件都成立，首条规则命中。
	h = newHarness(t, cfg, `{"note":"","tag":""}`)
	got = explainObject(t, h.evalExplainArgs("f"))
	if got["reason"] != "rule" || got["value"] != true || got["ruleId"] != "r1" {
		t.Fatalf("explicit empty strings must match: %v", got)
	}
	r0 = asMap(t, asSlice(t, asMap(t, got["explanation"], "explanation")["rules"], "rules")[0], "r0")
	for i, c := range asSlice(t, r0["conditions"], "conditions") {
		cm := asMap(t, c, "condition")
		if cm["actual"] != "" || cm["matched"] != true {
			t.Fatalf("explicit empty-string condition %d wrong: %v", i, cm)
		}
	}
}

func TestCLIEvaluateExplainPreservesListContents(t *testing.T) {
	// 比较列表中的空字符串、重复成员和次序都原样保留，不裁剪空白、不合并大小写。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[
			{"attribute":"tier","op":"in","value":["b","","a","b"," A"]}]}]}]}`
	h := newHarness(t, cfg, `{"tier":"z"}`)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalExplainArgs("f"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	raw := stdout.String()
	want := `"expected":["b","","a","b"," A"]`
	if !strings.Contains(raw, want) {
		t.Fatalf("list must be preserved verbatim:\n%s\nwant substring %s", raw, want)
	}
	// 不命中时 actual 仍是上下文实际值。
	if !strings.Contains(raw, `"actual":"z"`) {
		t.Fatalf("actual context value must be shown: %s", raw)
	}
}

func TestCLIEvaluateWithoutExplainKeepsExactlyFourFields(t *testing.T) {
	// 不带选项时输出仍是原有四字段对象，键集合不增不减，逐字节不含 explanation。
	h := newHarness(t, validConfig, `{"plan":"pro","region":"cn"}`)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalArgs("feature-a"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	raw := stdout.String()
	if strings.Contains(raw, "explanation") {
		t.Fatalf("plain mode must not include explanation: %s", raw)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("plain mode must have exactly 4 fields, got %d: %v", len(got), got)
	}
	want := `{"key":"feature-a","value":true,"reason":"rule","ruleId":"r-pro"}`
	if strings.TrimSpace(raw) != want {
		t.Fatalf("plain output changed:\n%s\nwant %s", raw, want)
	}
}

func TestCLIEvaluateExplainArgumentBoundaries(t *testing.T) {
	goodCfg, goodCtx := validConfig, `{}`
	cases := []struct {
		name    string
		prepare func(t *testing.T) []string
		wantSub string
	}{
		{
			"unknown fourth argument",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, goodCtx)
				return []string{"evaluate", h.configPath, "feature-a", h.contextPath, "--verbose"}
			},
			"unknown extra argument",
		},
		{
			"fifth argument rejected",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, goodCtx)
				return []string{"evaluate", h.configPath, "feature-a", h.contextPath, "--explain", "extra"}
			},
			"expected 3 arguments",
		},
		{
			"fourth positional value is not the option",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, goodCtx)
				return []string{"evaluate", h.configPath, "feature-a", h.contextPath, "explain"}
			},
			"unknown extra argument",
		},
		{
			"two args even when second is --explain",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, goodCtx)
				return []string{"evaluate", h.configPath, "--explain"}
			},
			"expected 3 arguments",
		},
		{
			"--explain as config position is a path",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, goodCtx)
				return []string{"evaluate", "--explain", "feature-a", h.contextPath}
			},
			"cannot read config file",
		},
		{
			"--explain as key position is a flag key",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, goodCtx)
				return []string{"evaluate", h.configPath, "--explain", h.contextPath}
			},
			"not found",
		},
		{
			"--explain as context position is a path",
			func(t *testing.T) []string {
				h := newHarness(t, goodCfg, "")
				return []string{"evaluate", h.configPath, "feature-a", "--explain"}
			},
			"cannot read context file",
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
				t.Fatalf("stdout must be empty on argument error: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.wantSub) {
				t.Fatalf("stderr=%q, want substring %q", stderr.String(), tc.wantSub)
			}
		})
	}
}

func TestCLIEvaluateExplainStillValidatesEverything(t *testing.T) {
	// 解释模式不豁免任何校验：未选中/已关闭开关里的非法配置、非法上下文都让
	// 命令非零退出、stdout 为空、原因写入 stderr，不输出半份解释。
	t.Run("invalid rule in unselected flag with --explain", func(t *testing.T) {
		cfg := `{"flags":[
			{"key":"good","enabled":true,"default":false,"rules":[]},
			{"key":"bad","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"a","op":"nope","value":"x"}]}]}]}`
		h := newHarness(t, cfg, `{}`)
		var stdout, stderr bytes.Buffer
		if code := run(h.evalExplainArgs("good"), &stdout, &stderr); code == 0 {
			t.Fatalf("expected non-zero exit")
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must be empty: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), `conditions[0].op`) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("invalid context with --explain even for disabled flag", func(t *testing.T) {
		h := newHarness(t, validConfig, `{"plan":1}`)
		var stdout, stderr bytes.Buffer
		if code := run(h.evalExplainArgs("feature-b"), &stdout, &stderr); code == 0 {
			t.Fatalf("disabled flag must not bypass context validation")
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must be empty: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), "context.plan") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("flag not found with --explain", func(t *testing.T) {
		h := newHarness(t, validConfig, `{}`)
		var stdout, stderr bytes.Buffer
		if code := run(h.evalExplainArgs("missing"), &stdout, &stderr); code == 0 {
			t.Fatalf("expected non-zero exit")
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must be empty: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), "not found") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
}
