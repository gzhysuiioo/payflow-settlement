package payflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 本文件为 evaluate --explain 的判断记录补充特殊字符串的回归保障：开关键、
// 规则编号、属性名与 eq/in 的比较内容含中文、重音字符、表情、引号、反斜杠及
// 换行/制表符时，经过输入读取、规则判断与 JSON 输出后原有含义不变——直接字符
// 与合法 \uXXXX 转义表示同一文字时选中同一开关、查找同一属性、得到相同判断，
// 解释里的 attribute、compareValue、actualValue 与 ruleId 保留解码后的文字。

const (
	specialKey        = "结算🚀开关"
	specialRuleMiss   = "r-跳过é🚀"
	specialRuleDecide = "r-定案🚀"
	specialRuleUnseen = "r-隐身"
	specialAttrPlan   = "套餐é"
	specialAttrTag    = "标签🏷"
	specialAttrRegion = "地区"
	specialPlanValue  = "专业版 🚀"
	specialRegionHit  = "北京🚀"
)

// specialInList 同时含空字符串、重复成员与表情文字，排列顺序本身有意义。
var specialInList = []string{"", "中文", "中文", "café 🚀"}

// escapeJSONString 把文字写成 JSON \uXXXX 转义序列（非 BMP 字符用代理项对，
// 引号、反斜杠与控制字符按 JSON 规则转义），得到与直接书写字符同义的 JSON
// 字符串内容（不含两侧引号）。
func escapeJSONString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04X`, r)
		case r < 0x80:
			b.WriteRune(r)
		case r <= 0xFFFF:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04X\u%04X`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		}
	}
	return b.String()
}

// specialJSON 按写法把文字写入 JSON 字符串字面量：direct 直接书写字符，
// escaped 把非 ASCII 与控制字符全部写成 \uXXXX——两种写法解码后是同一文字。
func specialJSON(t *testing.T, spelling, s string) string {
	t.Helper()
	switch spelling {
	case "direct":
		return jsonQuote(s)
	case "escaped":
		return `"` + escapeJSONString(s) + `"`
	default:
		t.Fatalf("unknown spelling %q", spelling)
		return ""
	}
}

// specialExplainCfg 的开关按顺序含三条规则，覆盖“前面规则未命中、后面首条
// 成立的规则返回 false 并定案、其后规则不出现”的既有行为，且所有名称与比较
// 内容都含中文、重音字符与表情：
//   - r-跳过é🚀（在前，返回 true）：套餐é eq "专业版 🚀" 与 标签🏷 in
//     ["","中文","中文","café 🚀"] 两个条件 AND；上下文里 标签🏷 为“其他”，
//     规则整体不命中，但两个条件的逐条判断都要保留在解释里；
//   - r-定案🚀（居中，返回 false）：地区 eq "北京🚀" 成立，立即定案为 false；
//   - r-隐身（在后）：定案后不再被考虑，解释里不得出现。
func specialExplainCfg(t *testing.T, spelling string) string {
	t.Helper()
	j := func(s string) string { return specialJSON(t, spelling, s) }
	in := make([]string, 0, len(specialInList))
	for _, v := range specialInList {
		if v == "" {
			in = append(in, `""`)
		} else {
			in = append(in, j(v))
		}
	}
	return `{"flags":[{"key":` + j(specialKey) + `,"enabled":true,"default":true,"rules":[` +
		`{"id":` + j(specialRuleMiss) + `,"value":true,"conditions":[` +
		`{"attribute":` + j(specialAttrPlan) + `,"op":"eq","value":` + j(specialPlanValue) + `},` +
		`{"attribute":` + j(specialAttrTag) + `,"op":"in","value":[` + strings.Join(in, ",") + `]}]},` +
		`{"id":` + j(specialRuleDecide) + `,"value":false,"conditions":[` +
		`{"attribute":` + j(specialAttrRegion) + `,"op":"eq","value":` + j(specialRegionHit) + `}]},` +
		`{"id":` + j(specialRuleUnseen) + `,"value":true,"conditions":[` +
		`{"attribute":` + j(specialAttrRegion) + `,"op":"eq","value":` + j("上海") + `}]}]}]}`
}

// specialExplainCtx 让首条规则因 标签🏷 不命中而整体不成立、第二条规则命中。
func specialExplainCtx(t *testing.T, spelling string) string {
	t.Helper()
	j := func(s string) string { return specialJSON(t, spelling, s) }
	return `{` + j(specialAttrPlan) + `:` + j(specialPlanValue) + `,` +
		j(specialAttrTag) + `:` + j("其他") + `,` +
		j(specialAttrRegion) + `:` + j(specialRegionHit) + `}`
}

// assertSpecialDecideBaseline 核对 specialExplainCfg × specialExplainCtx 的既有
// 结果：最终值 false、reason=rule、ruleId=r-定案🚀；解释保留 r-跳过é🚀（未命中、
// 两个条件逐条在案）与 r-定案🚀（命中），r-隐身不出现；所有文字为解码后的内容，
// in 候选保持空字符串、重复成员与原次序。
func assertSpecialDecideBaseline(t *testing.T, label string, got ExplainedResult) {
	t.Helper()
	decideID := specialRuleDecide
	want := EvalResult{Key: specialKey, Value: false, Reason: EvalRule, RuleID: &decideID}
	if !got.EvalResult.Equals(want) {
		t.Fatalf("%s: result = %+v, want %+v", label, got.EvalResult, want)
	}
	rules := got.Explanation.Rules
	if len(rules) != 2 || rules[0].RuleID != specialRuleMiss || rules[1].RuleID != specialRuleDecide {
		t.Fatalf("%s: considered rules = %v, want [%s %s]（%s 不得出现）",
			label, explainedIDs(got), specialRuleMiss, specialRuleDecide, specialRuleUnseen)
	}
	if rules[0].Match || !rules[1].Match {
		t.Fatalf("%s: match flags wrong: %+v", label, rules)
	}
	if len(rules[0].Conditions) != 2 || len(rules[1].Conditions) != 1 {
		t.Fatalf("%s: condition records incomplete: %+v", label, rules)
	}

	plan := rules[0].Conditions[0]
	if plan.Attribute != specialAttrPlan || plan.Op != "eq" ||
		plan.CompareValue != specialPlanValue ||
		plan.ActualValue == nil || *plan.ActualValue != specialPlanValue ||
		plan.Missing || !plan.Match {
		t.Fatalf("%s: plan condition must hold decoded text: %+v", label, plan)
	}
	tag := rules[0].Conditions[1]
	list, ok := tag.CompareValue.([]string)
	if !ok || len(list) != len(specialInList) {
		t.Fatalf("%s: in compareValue = %#v, want %#v", label, tag.CompareValue, specialInList)
	}
	for i, w := range specialInList {
		if list[i] != w {
			t.Fatalf("%s: in compareValue[%d] = %q, want %q（空串/重复/次序须保留）", label, i, list[i], w)
		}
	}
	if tag.Attribute != specialAttrTag || tag.Op != "in" || tag.Missing || tag.Match ||
		tag.ActualValue == nil || *tag.ActualValue != "其他" {
		t.Fatalf("%s: tag condition should be a present non-member: %+v", label, tag)
	}

	region := rules[1].Conditions[0]
	if region.Attribute != specialAttrRegion || region.Op != "eq" ||
		region.CompareValue != specialRegionHit ||
		region.ActualValue == nil || *region.ActualValue != specialRegionHit ||
		region.Missing || !region.Match {
		t.Fatalf("%s: region condition must hold decoded text: %+v", label, region)
	}
}

// TestExplainSpecialStringsSpellingEquivalence 验证直接书写字符与合法 \uXXXX
// 转义表示同一文字时，配置与上下文各自独立地选任一写法都选中同一开关、查找
// 同一属性并得到相同判断；解释里的 attribute、compareValue、actualValue 与
// ruleId 保留解码后的文字，序列化输出逐字节一致；同一输入下普通模式与解释
// 模式的 key/value/reason/ruleId 四项一致。
func TestExplainSpecialStringsSpellingEquivalence(t *testing.T) {
	var reference []byte
	for _, cfgSpelling := range []string{"direct", "escaped"} {
		for _, ctxSpelling := range []string{"direct", "escaped"} {
			t.Run(cfgSpelling+"-config/"+ctxSpelling+"-context", func(t *testing.T) {
				cfg := mustParseConfig(t, specialExplainCfg(t, cfgSpelling))
				flag := cfg.Find(specialKey)
				if flag == nil {
					t.Fatalf("flag %q not found (key spelling %s)", specialKey, cfgSpelling)
				}
				ctx := mustParseContext(t, specialExplainCtx(t, ctxSpelling))
				got := flag.EvaluateExplain(ctx)
				assertSpecialDecideBaseline(t, cfgSpelling+"/"+ctxSpelling, got)

				out := mustMarshalExplain(t, got)
				// 输出中的名称与比较内容是解码后的文字本身，不是转义写法。
				for _, decoded := range []string{specialKey, specialRuleDecide, specialAttrTag, specialPlanValue} {
					if !bytes.Contains(out, []byte(decoded)) {
						t.Fatalf("output must carry decoded text %q: %s", decoded, out)
					}
				}
				if reference == nil {
					reference = out
				} else if !bytes.Equal(out, reference) {
					t.Fatalf("spelling changed explain output:\n%s\n!= reference:\n%s", out, reference)
				}

				if plain := flag.Evaluate(ctx); !got.EvalResult.Equals(plain) {
					t.Fatalf("explained %+v != plain %+v", got.EvalResult, plain)
				}
			})
		}
	}
}

// TestExplainSpecialStringsExactMatchBoundaries 验证特殊文字参与实际判断时，
// 大小写、首尾空白及外观相近但字符序列不同的内容仍按原样区分：命中与不命中
// 都发生，不命中时 actualValue 原样保留、结果落到默认值。
func TestExplainSpecialStringsExactMatchBoundaries(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-é🚀","value":true,"conditions":[
			{"attribute":"标签","op":"eq","value":"café 🚀"}]}]}]}`
	flag := mustParseConfig(t, cfg).Find("f")
	cases := []struct {
		name    string
		actual  string
		wantHit bool
	}{
		{"exact hit", "café 🚀", true},
		{"case differs", "CAFÉ 🚀", false},
		{"leading space", " café 🚀", false},
		{"trailing space", "café 🚀 ", false},
		{"precomposed vs combining", "café 🚀", false}, // e + 组合符 U+0301，与预组合的 é 不同
		{"cyrillic lookalike", "cаfé 🚀", false},        // 西里尔 а（U+0430）替换拉丁 a
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := mustParseContext(t, `{"标签":`+jsonQuote(tc.actual)+`}`)
			got := flag.EvaluateExplain(ctx)
			c := got.Explanation.Rules[0].Conditions[0]
			if c.Match != tc.wantHit || c.Missing {
				t.Fatalf("match = %v, want %v: %+v", c.Match, tc.wantHit, c)
			}
			if c.ActualValue == nil || *c.ActualValue != tc.actual {
				t.Fatalf("actualValue must keep the original text %q: %+v", tc.actual, c)
			}
			if c.Attribute != "标签" || c.CompareValue != "café 🚀" {
				t.Fatalf("attribute/compareValue must keep decoded text: %+v", c)
			}
			wantReason := EvalDefault
			if tc.wantHit {
				wantReason = EvalRule
			}
			if got.Reason != wantReason {
				t.Fatalf("reason = %v, want %v: %+v", got.Reason, wantReason, got.EvalResult)
			}
			if plain := flag.Evaluate(ctx); !got.EvalResult.Equals(plain) {
				t.Fatalf("explained %+v != plain %+v", got.EvalResult, plain)
			}
		})
	}
}

// TestExplainQuotesBackslashControlRoundTrip 验证引号、反斜杠以及合法转义表示
// 的换行和制表符进入字符串后：成功输出仍是单行、可解析的一个 JSON 对象；读取
// 输出得到的文字与输入解码后的内容相同，不丢字符、不重复转义，字符串内容也
// 不会变成新字段。配置与上下文各自用直接/转义两种写法，输出逐字节一致。
func TestExplainQuotesBackslashControlRoundTrip(t *testing.T) {
	attr := "属\"性\\名"
	value := "行一\n行二\t制表\"引号\\反斜杠🚀"
	ruleID := "r-引号\"反斜杠\\"
	var reference []byte
	for _, cfgSpelling := range []string{"direct", "escaped"} {
		for _, ctxSpelling := range []string{"direct", "escaped"} {
			t.Run(cfgSpelling+"-config/"+ctxSpelling+"-context", func(t *testing.T) {
				j := func(s string) string { return specialJSON(t, cfgSpelling, s) }
				jc := func(s string) string { return specialJSON(t, ctxSpelling, s) }
				cfgRaw := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[` +
					`{"id":` + j(ruleID) + `,"value":true,"conditions":[` +
					`{"attribute":` + j(attr) + `,"op":"eq","value":` + j(value) + `},` +
					`{"attribute":"标签","op":"in","value":["",` + j(value) + `,` + j(value) + `]}]}]}]}`
				ctxRaw := `{` + jc(attr) + `:` + jc(value) + `,"标签":` + jc(value) + `}`
				flag := mustParseConfig(t, cfgRaw).Find("f")
				got := flag.EvaluateExplain(mustParseContext(t, ctxRaw))
				if got.Reason != EvalRule || got.RuleID == nil || *got.RuleID != ruleID || !got.Value {
					t.Fatalf("unexpected result: %+v", got.EvalResult)
				}

				out := mustMarshalExplain(t, got)
				// 单行：换行/制表符只能以转义形式出现，输出不得含裸换行。
				if bytes.ContainsAny(out, "\n") {
					t.Fatalf("output must be a single line: %q", out)
				}
				var m map[string]any
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("output is not one parseable JSON object: %v\n%s", err, out)
				}
				// 字符串内容不得变成新字段：顶层恰好五个既有字段。
				if len(m) != 5 {
					t.Fatalf("top-level fields changed by string content: %v", m)
				}
				for _, k := range []string{"key", "value", "reason", "ruleId", "explanation"} {
					if _, ok := m[k]; !ok {
						t.Fatalf("missing field %q in %v", k, m)
					}
				}
				if m["ruleId"] != ruleID {
					t.Fatalf("ruleId decoded text wrong: %q want %q", m["ruleId"], ruleID)
				}

				exp := m["explanation"].(map[string]any)
				rules := exp["rules"].([]any)
				rule0 := rules[0].(map[string]any)
				if rule0["ruleId"] != ruleID {
					t.Fatalf("explanation ruleId decoded text wrong: %q want %q", rule0["ruleId"], ruleID)
				}
				conds := rule0["conditions"].([]any)
				c0 := conds[0].(map[string]any)
				// 读回的文字与输入解码后的内容相同：不丢字符、不重复转义。
				if c0["attribute"] != attr || c0["compareValue"] != value || c0["actualValue"] != value {
					t.Fatalf("decoded text drifted: %v", c0)
				}
				if len(c0) != 6 {
					t.Fatalf("condition fields changed by string content: %v", c0)
				}
				c1 := conds[1].(map[string]any)
				list := c1["compareValue"].([]any)
				if len(list) != 3 || list[0] != "" || list[1] != value || list[2] != value {
					t.Fatalf("in list must keep empty member, duplicates and order: %v", list)
				}

				if reference == nil {
					reference = out
				} else if !bytes.Equal(out, reference) {
					t.Fatalf("spelling changed output:\n%s\n!= reference:\n%s", out, reference)
				}
			})
		}
	}
}

// TestExplainSpecialAttrMissingVsEmpty 验证含特殊字符的属性名准确区分缺失与
// 空值：未提供时 actualValue 为 null、missing 为 true；显式空字符串保留空
// 字符串、missing 为 false。属性名的转义写法与直接字符查找同一属性。
func TestExplainSpecialAttrMissingVsEmpty(t *testing.T) {
	// 配置里的属性名用代理项对转义写法（"标签🏷"），上下文里直接书写字符，
	// 两种写法仍查找同一属性。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[
			{"attribute":"标签\uD83C\uDFF7","op":"eq","value":""}]}]}]}`
	flag := mustParseConfig(t, cfg).Find("f")

	missing := flag.EvaluateExplain(mustParseContext(t, `{}`))
	c := missing.Explanation.Rules[0].Conditions[0]
	if c.Attribute != "标签🏷" || !c.Missing || c.ActualValue != nil || c.Match {
		t.Fatalf("missing attribute must use missing=true/nil actualValue: %+v", c)
	}
	if out := mustMarshalExplain(t, missing); !strings.Contains(string(out), `"actualValue":null,"missing":true`) {
		t.Fatalf("missing attribute must render as null/true: %s", out)
	}

	empty := flag.EvaluateExplain(mustParseContext(t, `{"标签🏷":""}`))
	c = empty.Explanation.Rules[0].Conditions[0]
	if c.Missing || c.ActualValue == nil || *c.ActualValue != "" || !c.Match {
		t.Fatalf("explicit empty string must be kept with missing=false: %+v", c)
	}
	if empty.Reason != EvalRule {
		t.Fatalf("empty string must match eq \"\": %+v", empty.EvalResult)
	}
	if out := mustMarshalExplain(t, empty); !strings.Contains(string(out), `"actualValue":"","missing":false`) {
		t.Fatalf("empty string must render as \"\"/false: %s", out)
	}
}
