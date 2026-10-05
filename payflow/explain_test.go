package payflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// explainCfg 含两条可同时成立、返回值相反的规则：r-deny 在前返回 false，
// r-allow 在后返回 true；另有 eq/in 两类条件与默认值，覆盖解释的各条路径。
const explainCfg = `{"flags":[
	{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-deny","value":false,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"tier","op":"in","value":["","a","a"]}]},
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]}]},
	{"key":"off","enabled":false,"default":true,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]},
	{"key":"plain","enabled":true,"default":false,"rules":[]}]}`

func explainFlag(t *testing.T, key string) *Flag {
	t.Helper()
	flag := mustParseConfig(t, explainCfg).Find(key)
	if flag == nil {
		t.Fatalf("flag %q not found", key)
	}
	return flag
}

// condOf 取第 ruleIdx 条规则解释中的第 condIdx 个条件。
func condOf(t *testing.T, r ExplainedResult, ruleIdx, condIdx int) ConditionExplanation {
	t.Helper()
	if ruleIdx >= len(r.Explanation.Rules) {
		t.Fatalf("rule index %d out of range in %+v", ruleIdx, r.Explanation.Rules)
	}
	cs := r.Explanation.Rules[ruleIdx].Conditions
	if condIdx >= len(cs) {
		t.Fatalf("condition index %d out of range in %+v", condIdx, cs)
	}
	return cs[condIdx]
}

func TestExplainFirstMatchingFalseRuleStops(t *testing.T) {
	// 两条规则都成立：第一条 r-deny（返回 false）定案，解释只包含 r-deny，
	// 后面的 r-allow 不得出现。
	flag := explainFlag(t, "f")
	ctx := mustParseContext(t, `{"plan":"pro","tier":"a","region":"cn"}`)
	got := flag.EvaluateExplain(ctx)

	if got.Value != false || got.Reason != EvalRule || got.RuleID == nil || *got.RuleID != "r-deny" {
		t.Fatalf("unexpected result: %+v", got.EvalResult)
	}
	if ids := explainedIDs(got); len(ids) != 1 || ids[0] != "r-deny" {
		t.Fatalf("explanation must stop at the first match, got %v", ids)
	}
	if !got.Explanation.Rules[0].Match {
		t.Fatalf("deciding rule must be marked matched: %+v", got.Explanation.Rules[0])
	}
}

func TestExplainEarlierMissKeptLaterMatchWins(t *testing.T) {
	// 第一条规则因 tier 不命中而整体不成立：它仍要出现在解释里（含全部条件
	// 的逐条判断），求值继续到 r-allow 并命中。
	flag := explainFlag(t, "f")
	ctx := mustParseContext(t, `{"plan":"pro","tier":"b","region":"cn"}`)
	got := flag.EvaluateExplain(ctx)

	if got.Reason != EvalRule || got.RuleID == nil || *got.RuleID != "r-allow" || !got.Value {
		t.Fatalf("unexpected result: %+v", got.EvalResult)
	}
	if ids := explainedIDs(got); len(ids) != 2 || ids[0] != "r-deny" || ids[1] != "r-allow" {
		t.Fatalf("both considered rules must be listed, got %v", ids)
	}
	if got.Explanation.Rules[0].Match || !got.Explanation.Rules[1].Match {
		t.Fatalf("match flags wrong: %+v", got.Explanation.Rules)
	}

	// 第一条规则的两个条件都给出判断：plan eq 成立；tier in 不成立。
	plan := condOf(t, got, 0, 0)
	if plan.Attribute != "plan" || plan.Op != "eq" || plan.CompareValue != "pro" ||
		plan.ActualValue == nil || *plan.ActualValue != "pro" || plan.Missing || !plan.Match {
		t.Fatalf("plan condition: %+v", plan)
	}
	tier := condOf(t, got, 0, 1)
	if tier.Attribute != "tier" || tier.Op != "in" || tier.Missing || tier.Match {
		t.Fatalf("tier condition should miss by membership: %+v", tier)
	}
	if list, ok := tier.CompareValue.([]string); !ok || len(list) != 3 ||
		list[0] != "" || list[1] != "a" || list[2] != "a" {
		t.Fatalf("compareValue must keep empty string, order and duplicates: %#v", tier.CompareValue)
	}
	if tier.ActualValue == nil || *tier.ActualValue != "b" {
		t.Fatalf("tier actual value: %+v", tier.ActualValue)
	}
}

func TestExplainMissingAttributeDistinctFromEmptyString(t *testing.T) {
	flag := explainFlag(t, "f")

	// 属性缺失：missing=true、actualValue 为 nil；规则不命中。
	missing := flag.EvaluateExplain(mustParseContext(t, `{"region":"cn"}`))
	tier := condOf(t, missing, 0, 1)
	if !tier.Missing || tier.ActualValue != nil || tier.Match {
		t.Fatalf("missing attribute must use missing=true/nil actualValue: %+v", tier)
	}
	plan := condOf(t, missing, 0, 0)
	if !plan.Missing || plan.ActualValue != nil {
		t.Fatalf("missing plan attribute: %+v", plan)
	}

	// 显式空字符串命中 eq "" 与 in [""]：missing=false、actualValue 指向 ""。
	emptyEq := `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"note","op":"eq","value":""}]}]}]}`
	g := mustParseConfig(t, emptyEq).Find("g")
	hit := g.EvaluateExplain(mustParseContext(t, `{"note":""}`))
	c := hit.Explanation.Rules[0].Conditions[0]
	if c.Missing || c.ActualValue == nil || *c.ActualValue != "" || !c.Match {
		t.Fatalf("explicit empty string must match eq \"\": %+v", c)
	}
	miss := g.EvaluateExplain(mustParseContext(t, `{}`))
	c = miss.Explanation.Rules[0].Conditions[0]
	if !c.Missing || c.ActualValue != nil || c.Match {
		t.Fatalf("missing note must not match eq \"\": %+v", c)
	}
}

func TestExplainEqMismatchAndCaseSensitive(t *testing.T) {
	cfg := `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	g := mustParseConfig(t, cfg).Find("g")
	for _, raw := range []string{`{"plan":"PRO"}`, `{"plan":"pro "}`} {
		got := g.EvaluateExplain(mustParseContext(t, raw))
		c := got.Explanation.Rules[0].Conditions[0]
		if c.Missing || c.Match || c.ActualValue == nil {
			t.Fatalf("%s must be a present-but-unequal value: %+v", raw, c)
		}
	}
}

func TestExplainDefaultWhenNoRuleMatches(t *testing.T) {
	// 所有规则都不命中：解释列出全部规则及其条件，结果采用 default=true。
	flag := explainFlag(t, "f")
	got := flag.EvaluateExplain(mustParseContext(t, `{"plan":"free","region":"eu"}`))
	if got.Reason != EvalDefault || got.RuleID != nil || got.Value != true {
		t.Fatalf("unexpected result: %+v", got.EvalResult)
	}
	if ids := explainedIDs(got); len(ids) != 2 || ids[0] != "r-deny" || ids[1] != "r-allow" {
		t.Fatalf("all rules must be shown when none match, got %v", ids)
	}
	for _, r := range got.Explanation.Rules {
		if r.Match {
			t.Fatalf("no rule may be marked matched: %+v", r)
		}
	}
	if !strings.Contains(strings.ToLower(got.Explanation.Outcome), "default") {
		t.Fatalf("outcome must explain the default fallback: %q", got.Explanation.Outcome)
	}
}

func TestExplainEmptyRuleList(t *testing.T) {
	flag := explainFlag(t, "plain")
	got := flag.EvaluateExplain(mustParseContext(t, `{}`))
	if got.Reason != EvalDefault || got.Value != false || got.RuleID != nil {
		t.Fatalf("unexpected result: %+v", got.EvalResult)
	}
	if got.Explanation.Rules == nil || len(got.Explanation.Rules) != 0 {
		t.Fatalf("empty rule list must yield an empty (not nil) explanation slice: %#v", got.Explanation.Rules)
	}
}

func TestExplainDisabledFlagHasEmptyRules(t *testing.T) {
	// 关闭开关：固定 false；不评估任何规则，解释中的规则列表为空，依据只
	// 说明关闭导致固定 false，不能把 default 或本会命中的规则写成依据。
	flag := explainFlag(t, "off")
	got := flag.EvaluateExplain(mustParseContext(t, `{"plan":"pro"}`))
	if got.Reason != EvalDisabled || got.Value != false || got.RuleID != nil {
		t.Fatalf("unexpected result: %+v", got.EvalResult)
	}
	if len(got.Explanation.Rules) != 0 {
		t.Fatalf("disabled flag must not evaluate or list rules: %+v", got.Explanation.Rules)
	}
	if !strings.Contains(strings.ToLower(got.Explanation.Outcome), "disabled") {
		t.Fatalf("outcome must explain the disabled fixed-false result: %q", got.Explanation.Outcome)
	}
}

func TestExplainResultMatchesPlainEvaluation(t *testing.T) {
	// 解释模式的 key/value/reason/ruleId 必须与普通 Evaluate 完全一致。
	cases := []struct {
		key string
		ctx string
	}{
		{"f", `{"plan":"pro","tier":"a","region":"cn"}`},
		{"f", `{"plan":"pro","region":"cn"}`},
		{"f", `{"plan":"free"}`},
		{"off", `{"plan":"pro"}`},
		{"plain", `{}`},
	}
	for _, tc := range cases {
		cfg := mustParseConfig(t, explainCfg)
		flag := cfg.Find(tc.key)
		ctx := mustParseContext(t, tc.ctx)
		plain := flag.Evaluate(ctx)
		explained := flag.EvaluateExplain(ctx)
		if !explained.EvalResult.Equals(plain) {
			t.Fatalf("%s/%s: explained %+v != plain %+v", tc.key, tc.ctx, explained.EvalResult, plain)
		}
	}
}

func TestMarshalExplainShape(t *testing.T) {
	flag := explainFlag(t, "f")
	got := flag.EvaluateExplain(mustParseContext(t, `{"plan":"free"}`))
	out, err := MarshalExplain(got)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("explain output is not JSON: %s (%v)", out, err)
	}
	for _, k := range []string{"key", "value", "reason", "ruleId", "explanation"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing field %q in %s", k, out)
		}
	}
	// 字段顺序：前四个字段在 explanation 之前。
	s := string(out)
	ei := strings.Index(s, `"explanation"`)
	for _, k := range []string{`"key"`, `"value"`, `"reason"`, `"ruleId"`} {
		if p := strings.Index(s, k); p < 0 || p > ei {
			t.Fatalf("field %s must appear before explanation in %s", k, s)
		}
	}

	// 缺失属性渲染为 actualValue:null、missing:true；显式空串为 actualValue:""。
	disabled := explainFlag(t, "off").EvaluateExplain(mustParseContext(t, `{}`))
	dout, _ := MarshalExplain(disabled)
	if !strings.Contains(string(dout), `"reason":"disabled"`) ||
		!strings.Contains(string(dout), `"rules":[]`) {
		t.Fatalf("disabled explain output wrong: %s", dout)
	}
}

func explainedIDs(r ExplainedResult) []string {
	ids := make([]string, 0, len(r.Explanation.Rules))
	for _, rr := range r.Explanation.Rules {
		ids = append(ids, rr.RuleID)
	}
	return ids
}
