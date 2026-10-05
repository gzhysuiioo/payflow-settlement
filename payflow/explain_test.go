package payflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// explainEval 解析输入并用解释模式求值，失败即终止测试。
func explainEval(t *testing.T, cfgRaw, key, ctxRaw string) ExplainedResult {
	t.Helper()
	cfg := mustParseConfig(t, cfgRaw)
	ctx := mustParseContext(t, ctxRaw)
	flag := cfg.Find(key)
	if flag == nil {
		t.Fatalf("flag %q not found", key)
	}
	return flag.EvaluateWithExplanation(ctx)
}

// explainConfig 含三条规则：r-miss（tier eq gold，value=false）、
// r-block（region=cn 且 platform in [ios,android]，value=false）、r-pro
// （plan=pro，value=true）；default=true。三个触发属性互不重叠，用于检验
// 按序记录、前序未命中保留与首条命中即停（含返回 false 的规则）。
const explainConfig = `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
	{"id":"r-miss","value":false,"conditions":[
		{"attribute":"tier","op":"eq","value":"gold"}]},
	{"id":"r-block","value":false,"conditions":[
		{"attribute":"region","op":"eq","value":"cn"},
		{"attribute":"platform","op":"in","value":["ios","android"]}]},
	{"id":"r-pro","value":true,"conditions":[
		{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`

func TestExplainRuleHitStopsAndKeepsEarlierMisses(t *testing.T) {
	// r-miss 不命中（tier 缺失）被保留；r-block 全部条件成立并返回
	// false：它决定结果，即使其后的 r-pro 也会命中，也不进入解释记录。
	got := explainEval(t, explainConfig, "f",
		`{"plan":"pro","region":"cn","platform":"ios"}`)
	if got.Reason != EvalRule || got.Value != false || got.RuleID == nil || *got.RuleID != "r-block" {
		t.Fatalf("first fully matching rule must decide even with value=false: %+v", got.EvalResult)
	}
	rules := got.Explanation.Rules
	if len(rules) != 2 {
		t.Fatalf("rules must stop at the first match (earlier misses kept), got %d: %+v", len(rules), rules)
	}
	if rules[0].RuleID != "r-miss" || rules[0].Matched {
		t.Fatalf("first record must be the retained miss: %+v", rules[0])
	}
	if len(rules[0].Conditions) != 1 || rules[0].Conditions[0].Matched {
		t.Fatalf("r-miss condition must be recorded as not matched: %+v", rules[0].Conditions)
	}
	if rules[1].RuleID != "r-block" || !rules[1].Matched {
		t.Fatalf("second record must be the matching r-block: %+v", rules[1])
	}
	for i, c := range rules[1].Conditions {
		if !c.Matched {
			t.Fatalf("r-block condition %d must be matched: %+v", i, c)
		}
	}
}

func TestExplainFirstRuleHitImmediate(t *testing.T) {
	// 第一条规则（r-miss，value=false）立即成立：它立刻定案为 false，
	// 解释中只有它一条，其后的规则即使也会命中也不出现。
	got := explainEval(t, explainConfig, "f", `{"tier":"gold","plan":"pro"}`)
	if got.Value != false || got.Reason != EvalRule || *got.RuleID != "r-miss" {
		t.Fatalf("first rule must decide (even returning false): %+v", got.EvalResult)
	}
	if len(got.Explanation.Rules) != 1 || got.Explanation.Rules[0].RuleID != "r-miss" ||
		!got.Explanation.Rules[0].Matched {
		t.Fatalf("only the first rule must be recorded: %+v", got.Explanation.Rules)
	}
}

func TestExplainLaterTrueRuleWins(t *testing.T) {
	// 前两条都不成立、最后的 r-pro 成立：记录全部三条，结果取 r-pro 的 true。
	got := explainEval(t, explainConfig, "f", `{"plan":"pro","region":"us","platform":"web"}`)
	if got.Value != true || got.Reason != EvalRule || *got.RuleID != "r-pro" {
		t.Fatalf("later true rule must win: %+v", got.EvalResult)
	}
	if len(got.Explanation.Rules) != 3 {
		t.Fatalf("all three rules must be recorded up to the match: %+v", got.Explanation.Rules)
	}
	if !got.Explanation.Rules[2].Matched || got.Explanation.Rules[0].Matched || got.Explanation.Rules[1].Matched {
		t.Fatalf("only the last rule must be matched: %+v", got.Explanation.Rules)
	}
}

func TestExplainAllRulesMissUseDefault(t *testing.T) {
	got := explainEval(t, explainConfig, "f", `{"plan":"free","region":"us","platform":"web","tier":"silver"}`)
	if got.Reason != EvalDefault || got.Value != true || got.RuleID != nil {
		t.Fatalf("no rule matches must use default: %+v", got.EvalResult)
	}
	rules := got.Explanation.Rules
	if len(rules) != 3 {
		t.Fatalf("all considered rules must be recorded on default, got %d", len(rules))
	}
	wantOrder := []string{"r-miss", "r-block", "r-pro"}
	for i, r := range rules {
		if r.RuleID != wantOrder[i] {
			t.Fatalf("rule %d id = %q, want %q", i, r.RuleID, wantOrder[i])
		}
		if r.Matched {
			t.Fatalf("rule %s must not be marked matched on default path", r.RuleID)
		}
	}
}

func TestExplainDisabledHasNoRulesAndNoDefaultBasis(t *testing.T) {
	// default=true 且规则本会命中；关闭固定 false，解释规则为空。
	cfg := `{"flags":[{"key":"off","enabled":false,"default":true,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	got := explainEval(t, cfg, "off", `{"plan":"pro"}`)
	if got.Reason != EvalDisabled || got.Value != false || got.RuleID != nil {
		t.Fatalf("disabled result wrong: %+v", got.EvalResult)
	}
	if got.Explanation.Rules == nil || len(got.Explanation.Rules) != 0 {
		t.Fatalf("disabled flag must have an empty rules array, got %#v", got.Explanation.Rules)
	}
}

func TestExplainEmptyRules(t *testing.T) {
	cfg := `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[]}]}`
	got := explainEval(t, cfg, "g", `{}`)
	if got.Reason != EvalDefault || got.Value != false {
		t.Fatalf("empty rules must use default: %+v", got.EvalResult)
	}
	if got.Explanation.Rules == nil || len(got.Explanation.Rules) != 0 {
		t.Fatalf("empty rules must serialize as [], got %#v", got.Explanation.Rules)
	}
}

func TestExplainConditionMissingVsEmptyString(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-eq","value":true,"conditions":[{"attribute":"note","op":"eq","value":""}]},
		{"id":"r-in","value":true,"conditions":[{"attribute":"tag","op":"in","value":["","a"]}]}]}]}`

	// 两个属性都缺失：actual 必须是 nil（序列化为 null），matched=false，
	// expected 分别是 "" 与列表，缺失不能与显式空串混淆。
	missing := explainEval(t, cfg, "f", `{}`)
	for _, r := range missing.Explanation.Rules {
		for _, c := range r.Conditions {
			if c.Actual != nil {
				t.Fatalf("%s/%s: missing attr must have nil actual, got %q", r.RuleID, c.Attribute, *c.Actual)
			}
			if c.Matched {
				t.Fatalf("%s/%s: missing attr must not match", r.RuleID, c.Attribute)
			}
		}
	}
	if e := missing.Explanation.Rules[0].Conditions[0].Expected; e != "" {
		t.Fatalf("eq expected must be the decoded empty string, got %#v", e)
	}
	list, ok := missing.Explanation.Rules[1].Conditions[0].Expected.([]string)
	if !ok || len(list) != 2 || list[0] != "" || list[1] != "a" {
		t.Fatalf("in expected must be [\"\", a], got %#v", missing.Explanation.Rules[1].Conditions[0].Expected)
	}

	// 显式给出空字符串：actual 指向 ""，两条规则都成立；首条命中即停。
	empty := explainEval(t, cfg, "f", `{"note":"","tag":""}`)
	if empty.Reason != EvalRule || *empty.RuleID != "r-eq" || !empty.Value {
		t.Fatalf("explicit empty string must match eq: %+v", empty.EvalResult)
	}
	c0 := empty.Explanation.Rules[0].Conditions[0]
	if c0.Actual == nil || *c0.Actual != "" || !c0.Matched {
		t.Fatalf("explicit \"\" must appear as actual:\"\" and matched=true: %+v", c0)
	}
	if len(empty.Explanation.Rules) != 1 {
		t.Fatalf("explanation must stop at first match, got %d rules", len(empty.Explanation.Rules))
	}
}

func TestExplainConditionMismatchKinds(t *testing.T) {
	// 区分三种不成立：属性缺失、值不相等（eq）、不在候选列表（in）。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"tier","op":"in","value":["gold",""]}]}]}]}`
	got := explainEval(t, cfg, "f", `{"plan":"PRO","tier":"silver","extra":"x"}`)
	r1 := got.Explanation.Rules[0]
	if r1.Matched {
		t.Fatal("rule with two failing conditions must not match")
	}
	eq := r1.Conditions[0]
	if eq.Attribute != "plan" || eq.Op != "eq" || eq.Expected != "pro" ||
		eq.Actual == nil || *eq.Actual != "PRO" || eq.Matched {
		t.Fatalf("eq mismatch detail wrong: %+v", eq)
	}
	in := r1.Conditions[1]
	if in.Op != "in" || in.Actual == nil || *in.Actual != "silver" || in.Matched {
		t.Fatalf("in non-member detail wrong: %+v", in)
	}
	list := in.Expected.([]string)
	if len(list) != 2 || list[0] != "gold" || list[1] != "" {
		t.Fatalf("in expected list detail wrong: %#v", in.Expected)
	}
}

func TestExplainInListKeepsEmptyDuplicatesAndOrder(t *testing.T) {
	// 空串、重复成员与配置次序必须原样保留，不裁剪空白、不合并大小写。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[
			{"attribute":"tier","op":"in","value":["b","","a","b"," A"]}]}]}]}`
	got := explainEval(t, cfg, "f", `{}`)
	list := got.Explanation.Rules[0].Conditions[0].Expected.([]string)
	want := []string{"b", "", "a", "b", " A"}
	if len(list) != len(want) {
		t.Fatalf("list length changed: %#v", list)
	}
	for i := range want {
		if list[i] != want[i] {
			t.Fatalf("list not preserved at %d: %#v want %#v", i, list, want)
		}
	}
}

func TestExplainMatchesPlainEvaluation(t *testing.T) {
	// 解释模式的四个结果字段必须与普通 Evaluate 在各种路径上完全一致。
	cases := []struct {
		name   string
		cfg    string
		key    string
		ctxRaw string
	}{
		{"first rule false wins", explainConfig, "f", `{"tier":"gold"}`},
		{"earlier false wins", explainConfig, "f", `{"plan":"free","region":"cn","platform":"ios"}`},
		{"later true wins", explainConfig, "f", `{"plan":"pro","region":"us","platform":"web"}`},
		{"default", explainConfig, "f", `{}`},
		{"disabled", `{"flags":[{"key":"off","enabled":false,"default":true,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`, "off", `{"plan":"pro"}`},
		{"empty rules default false", `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[]}]}`, "g", `{}`},
		{"empty string eq", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"n","op":"eq","value":""}]}]}]}`, "f", `{"n":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParseConfig(t, tc.cfg)
			x := mustParseContext(t, tc.ctxRaw)
			plain := c.Find(tc.key).Evaluate(x)
			explained := c.Find(tc.key).EvaluateWithExplanation(x)
			if !explained.EvalResult.Equals(plain) {
				t.Fatalf("explained result %+v != plain %+v", explained.EvalResult, plain)
			}
		})
	}
}

func TestMarshalExplainedShape(t *testing.T) {
	got := explainEval(t, explainConfig, "f", `{"plan":"free","region":"cn","platform":"ios"}`)
	out, err := MarshalExplained(got)
	if err != nil {
		t.Fatal(err)
	}
	// 四字段在前、explanation 在后；紧凑 JSON、无多余换行。
	prefix := `{"key":"f","value":false,"reason":"rule","ruleId":"r-block","explanation":`
	if !strings.HasPrefix(string(out), prefix) {
		t.Fatalf("output prefix wrong:\n%s\nwant %s", out, prefix)
	}
	if strings.ContainsAny(string(out), "\n") {
		t.Fatalf("output must be one line: %q", out)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	for _, k := range []string{"key", "value", "reason", "ruleId", "explanation"} {
		if _, ok := decoded[k]; !ok {
			t.Fatalf("missing field %q in %s", k, out)
		}
	}

	// 缺失属性在 JSON 中明确是 null（而非 "" 或被省略）。
	miss := explainEval(t, explainConfig, "f", `{}`)
	missOut, err := MarshalExplained(miss)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(missOut), `"actual":null`) {
		t.Fatalf("missing attribute must render as actual:null: %s", missOut)
	}
}

func TestMarshalExplainedDisabledAndEmptyRules(t *testing.T) {
	cfg := `{"flags":[{"key":"off","enabled":false,"default":true,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	got := explainEval(t, cfg, "off", `{"plan":"pro"}`)
	out, err := MarshalExplained(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"off","value":false,"reason":"disabled","ruleId":null,"explanation":{"rules":[]}}`
	if string(out) != want {
		t.Fatalf("disabled explain:\n%s\nwant %s", out, want)
	}

	empty := explainEval(t, `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[]}]}`, "g", `{}`)
	out, _ = MarshalExplained(empty)
	want = `{"key":"g","value":false,"reason":"default","ruleId":null,"explanation":{"rules":[]}}`
	if string(out) != want {
		t.Fatalf("empty-rules explain:\n%s\nwant %s", out, want)
	}
}

func TestMarshalExplainedDoesNotEscapeHTML(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r<1>","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"a<b>"}]}]}]}`
	got := explainEval(t, cfg, "f", `{"plan":"a<b>"}`)
	out, err := MarshalExplained(got)
	if err != nil {
		t.Fatal(err)
	}
	// 与 MarshalResult 一致：<>& 按原样输出，不写成 < 之类的 Unicode 转义。
	escaped := strings.Contains(string(out), "\\u003c") || strings.Contains(string(out), "\\u003e")
	if escaped {
		t.Fatalf("explain output must not unicode-escape angle brackets: %s", out)
	}
	if !strings.Contains(string(out), `"a<b>"`) || !strings.Contains(string(out), `"r<1>"`) {
		t.Fatalf("angle brackets must appear verbatim: %s", out)
	}
}
