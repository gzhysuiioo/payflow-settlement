package payflow

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件为 evaluate --explain 补充“附加信息不参与求值”的回归保障：用户在配置
// 顶层、开关、规则或条件上增加、修改、删除合法附加字段（负责人 owner、备注
// note、标签数组、嵌套对象、布尔、null，以及语法合法但超出浮点范围的 1e400
// 与 1 后接 400 个 0 的整数）时，只要开关定义、规则顺序、比较值与上下文不变，
// 普通模式的四字段结果与解释模式的完整解释（含 outcome、每条被考虑规则及其
// 全部条件的 attribute/op/compareValue/actualValue/missing/match）都必须与没有
// 这些附加字段时逐字节一致：附加信息既不出现在结果或解释里，也不能被当成上下
// 文属性去补齐缺失值。求值场景同时区分“靠前规则已命中并返回 false（解释只到
// 该规则）”与“靠前规则未成立、后续规则才命中（解释保留此前未命中规则及其全部
// 条件）”。整份配置先校验的边界也保留：未被选中或已关闭开关的附加对象里，
// 合法大数字之后重复声明同名字段仍拒绝整份配置。
//
// extraExplainBase 不含任何附加字段。开关 f 按顺序含三条规则：
//   - r-early-false（在前，返回 false）：plan eq "pro" 与 tier in
//     ["","a","a"] 两个条件 AND；命中时立即以 false 定案，后续规则不出现；
//   - r-later-true（居中，返回 true）：region eq "cn"；
//   - r-unseen（在后）：仅当没有更早的规则命中时才会被考虑，用于观察解释是否
//     平白多出本不该考虑的后续规则。
//
// 另有已关闭开关 off（即使其规则本会命中也固定 false、解释规则列表为空）与
// 无规则开关 plain（默认值定案）。
const extraExplainBase = `{"flags":[
	{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-early-false","value":false,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"tier","op":"in","value":["","a","a"]}]},
		{"id":"r-later-true","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]},
		{"id":"r-unseen","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"us"}]}]},
	{"key":"off","enabled":false,"default":true,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]},
	{"key":"plain","enabled":true,"default":false,"rules":[]}]}`

// extraExplainHugeInt 是 1 后接 400 个 0 的合法 JSON 整数（远超 float64 范围）。
var extraExplainHugeInt = "1" + strings.Repeat("0", 400)

// extraExplainFull 在顶层、开关、规则、条件四级都放满合法附加信息：负责人、
// 备注、布尔、null、数组（数组里再嵌套对象与 1e400）、嵌套对象（内含 1e400
// 与 400 位整数）。这些内容与求值无关，且其文字标记（pay-*、附加备注、
// 1e400）绝不能出现在任何结果或解释中。
var extraExplainFull = `{"meta":{"owner":"pay-alice","note":"top-附加备注","big":1e400,"huge":` + extraExplainHugeInt + `,
	"tags":["x",1e400,{"n":1e400}],"ok":true,"nil":null},"flags":[
	{"key":"f","enabled":true,"default":true,"owner":"pay-bob","note":"flag-附加备注","flag_meta":{"big":1e400},"rules":[
		{"id":"r-early-false","value":false,"owner":"pay-carol","note":"rule-附加备注","rule_meta":[1e400,{"x":1}],"conditions":[
			{"attribute":"plan","op":"eq","value":"pro","owner":"pay-dave","note":"cond-附加备注","weight":1e400},
			{"attribute":"tier","op":"in","value":["","a","a"],"owner":"pay-eve","labels":["y",1e400],"deep":{"big":1e400}}]},
		{"id":"r-later-true","value":true,"owner":"pay-frank","note":"later-附加备注","conditions":[
			{"attribute":"region","op":"eq","value":"cn","note":"region-附加备注","extra":1e400}]},
		{"id":"r-unseen","value":true,"owner":"pay-grace","conditions":[
			{"attribute":"region","op":"eq","value":"us","owner":"pay-heidi"}]}]},
	{"key":"off","enabled":false,"default":true,"owner":"pay-off","note":"off-附加备注","meta":{"big":1e400},"rules":[
		{"id":"r-never","value":true,"owner":"pay-hidden","note":"never-附加备注","conditions":[
			{"attribute":"plan","op":"eq","value":"pro","note":"off-cond-附加备注"}]}]},
	{"key":"plain","enabled":true,"default":false,"owner":"pay-plain","note":"plain-附加备注","rules":[]}]}`

// extraExplainChanged 与 extraExplainFull 同名附加字段但内容全部改写（负责人
// 改名、标签数组与嵌套对象改变、大数字移除、备注换文），用于覆盖“修改合法
// 附加字段”这一情形：输出仍须与没有附加字段时一致。
const extraExplainChanged = `{"meta":{"owner":"pay-alice-2","note":"top-changed","tags":["z",{"n":2}],"ok":false},"flags":[
	{"key":"f","enabled":true,"default":true,"owner":"pay-bob-2","note":"flag-changed","flag_meta":{"keep":1},"rules":[
		{"id":"r-early-false","value":false,"owner":"pay-carol-2","note":"rule-changed","rule_meta":[1,2],"conditions":[
			{"attribute":"plan","op":"eq","value":"pro","owner":"pay-dave-2","note":"cond-changed","weight":7},
			{"attribute":"tier","op":"in","value":["","a","a"],"owner":"pay-eve-2","labels":["q"],"deep":{"big":9}}]},
		{"id":"r-later-true","value":true,"owner":"pay-frank-2","conditions":[
			{"attribute":"region","op":"eq","value":"cn","note":"region-changed","extra":3}]},
		{"id":"r-unseen","value":true,"owner":"pay-grace-2","conditions":[
			{"attribute":"region","op":"eq","value":"us","owner":"pay-heidi-2"}]}]},
	{"key":"off","enabled":false,"default":true,"owner":"pay-off-2","meta":{"big":2},"rules":[
		{"id":"r-never","value":true,"owner":"pay-hidden-2","conditions":[
			{"attribute":"plan","op":"eq","value":"pro","note":"off-cond-changed"}]}]},
	{"key":"plain","enabled":true,"default":false,"owner":"pay-plain-2","rules":[]}]}`

// extraExplainSparse 只在少数层级保留附加字段（顶层与其中一个条件），用于
// 覆盖“删除大部分附加字段”：它与 full 携带的业务定义完全相同，输出也必须
// 逐字节一致。
const extraExplainSparse = `{"meta":{"owner":"pay-alice","note":"top-附加备注"},"flags":[
	{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-early-false","value":false,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"tier","op":"in","value":["","a","a"],"note":"only-this-condition"}]},
		{"id":"r-later-true","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]},
		{"id":"r-unseen","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"us"}]}]},
	{"key":"off","enabled":false,"default":true,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]},
	{"key":"plain","enabled":true,"default":false,"rules":[]}]}`

// extraExplainShadow 专门覆盖“附加信息与上下文属性同名”：附加字段及嵌套附加
// 对象里写满规则所需的 plan/tier/region 和能够命中的值（"pro"、"a"、"cn"），
// 出现在顶层、顶层附加对象 context、开关、开关附加对象 fallback、规则、规则
// 附加对象 defaults、条件、条件附加对象 backup 每一处。上下文没有该属性时，
// 这些值绝不能被拿来补齐缺失值。
const extraExplainShadow = `{"context":{"plan":"pro","tier":"a","region":"cn"},"plan":"pro","tier":"a","region":"cn","flags":[
	{"key":"f","enabled":true,"default":true,"plan":"pro","tier":"a","region":"cn","fallback":{"plan":"pro","tier":"a","region":"cn"},"rules":[
		{"id":"r-early-false","value":false,"plan":"pro","tier":"a","defaults":{"plan":"pro","tier":"a"},"conditions":[
			{"attribute":"plan","op":"eq","value":"pro","plan":"pro","backup":{"plan":"pro","tier":"a"}},
			{"attribute":"tier","op":"in","value":["","a","a"],"tier":"a","backup":{"tier":"a","plan":"pro"}}]},
		{"id":"r-later-true","value":true,"region":"cn","defaults":{"region":"cn"},"conditions":[
			{"attribute":"region","op":"eq","value":"cn","region":"cn","backup":{"region":"cn"}}]},
		{"id":"r-unseen","value":true,"region":"us","conditions":[
			{"attribute":"region","op":"eq","value":"us","region":"us"}]}]},
	{"key":"off","enabled":false,"default":true,"plan":"pro","rules":[
		{"id":"r-never","value":true,"plan":"pro","conditions":[
			{"attribute":"plan","op":"eq","value":"pro","plan":"pro"}]}]},
	{"key":"plain","enabled":true,"default":false,"region":"cn","rules":[]}]}`

// extraExplainScenarios 覆盖不同的定案路径：靠前规则命中返回 false、靠前规则
// 未命中后续规则命中（成员不命中/属性缺失/显式空串三类原因）、无规则命中走
// 默认值（所有规则都应保留在解释里），以及关闭开关与无规则开关。
var extraExplainScenarios = []struct {
	name string
	key  string
	ctx  string
}{
	{"earlier rule hits and returns false", "f", `{"plan":"pro","tier":"a","region":"cn"}`},
	{"earlier rule misses by in membership, later rule hits", "f", `{"plan":"pro","tier":"b","region":"cn"}`},
	{"required attrs missing, later rule hits", "f", `{"region":"cn"}`},
	{"explicit empty strings present, later rule hits", "f", `{"plan":"","tier":"","region":"cn"}`},
	{"plan missing while tier is a member, later rule hits", "f", `{"tier":"a","region":"cn"}`},
	{"no rule matches, default is used", "f", `{}`},
	{"disabled flag fixed false", "off", `{"plan":"pro"}`},
	{"flag without rules uses default", "plain", `{}`},
}

// explainRuleByID 在解释记录中按编号找规则；不存在返回 nil。
func explainRuleByID(r ExplainedResult, id string) *RuleExplanation {
	for i := range r.Explanation.Rules {
		if r.Explanation.Rules[i].RuleID == id {
			return &r.Explanation.Rules[i]
		}
	}
	return nil
}

// TestExplainExtraFieldsDoNotChangeResultOrExplanation 是核心回归：对每一种附加
// 字段配置（全量增、改、删成稀疏、与上下文属性同名的 shadow）与每一种定案
// 场景，普通结果与完整解释都必须与无附加字段的基线逐字节一致；普通求值与解释
// 模式的四字段结果也必须一致。
func TestExplainExtraFieldsDoNotChangeResultOrExplanation(t *testing.T) {
	variants := map[string]string{
		"full":    extraExplainFull,
		"changed": extraExplainChanged,
		"sparse":  extraExplainSparse,
		"shadow":  extraExplainShadow,
	}
	for vname, vraw := range variants {
		t.Run(vname, func(t *testing.T) {
			for _, sc := range extraExplainScenarios {
				t.Run(sc.name, func(t *testing.T) {
					baseCfg := mustParseConfig(t, extraExplainBase)
					gotCfg := mustParseConfig(t, vraw)
					ctx := mustParseContext(t, sc.ctx)
					baseFlag, gotFlag := baseCfg.Find(sc.key), gotCfg.Find(sc.key)
					if baseFlag == nil || gotFlag == nil {
						t.Fatalf("flag %q missing", sc.key)
					}

					basePlain := baseFlag.Evaluate(ctx)
					gotPlain := gotFlag.Evaluate(ctx)
					if !gotPlain.Equals(basePlain) {
						t.Fatalf("plain result changed by extra fields: got %+v, want %+v", gotPlain, basePlain)
					}
					plainOut, err := MarshalResult(gotPlain)
					if err != nil {
						t.Fatalf("MarshalResult: %v", err)
					}
					basePlainOut, err := MarshalResult(basePlain)
					if err != nil {
						t.Fatalf("MarshalResult baseline: %v", err)
					}
					if !bytes.Equal(plainOut, basePlainOut) {
						t.Fatalf("plain output changed:\ngot:  %s\nwant: %s", plainOut, basePlainOut)
					}

					baseExplained := baseFlag.EvaluateExplain(ctx)
					gotExplained := gotFlag.EvaluateExplain(ctx)
					if !gotExplained.EvalResult.Equals(gotPlain) {
						t.Fatalf("explained four fields %+v != plain %+v", gotExplained.EvalResult, gotPlain)
					}
					if !gotExplained.EvalResult.Equals(baseExplained.EvalResult) {
						t.Fatalf("explained result changed: got %+v, want %+v", gotExplained.EvalResult, baseExplained.EvalResult)
					}
					gotOut := mustMarshalExplain(t, gotExplained)
					baseOut := mustMarshalExplain(t, baseExplained)
					if !bytes.Equal(gotOut, baseOut) {
						t.Fatalf("full explanation changed by extra fields:\ngot:  %s\nwant: %s", gotOut, baseOut)
					}
				})
			}
		})
	}
}

// TestExplainExtraFieldsEarlyFalseStopsAtFirstRule 验证“靠前规则已经命中并
// 返回 false”时，附加字段不改变获胜规则，解释只到首条命中规则为止，不把后续
// r-later-true/r-unseen 带出来，也不改变该规则两个条件的比较值、实际值与判断。
func TestExplainExtraFieldsEarlyFalseStopsAtFirstRule(t *testing.T) {
	for _, vraw := range []string{extraExplainFull, extraExplainChanged, extraExplainSparse, extraExplainShadow} {
		cfg := mustParseConfig(t, vraw)
		got := cfg.Find("f").EvaluateExplain(mustParseContext(t, `{"plan":"pro","tier":"a","region":"cn"}`))

		falseID := "r-early-false"
		want := EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &falseID}
		if !got.EvalResult.Equals(want) {
			t.Fatalf("result = %+v, want %+v", got.EvalResult, want)
		}
		if ids := explainedIDs(got); len(ids) != 1 || ids[0] != "r-early-false" {
			t.Fatalf("explanation must stop at the first hit, got %v", ids)
		}
		rule := explainRuleByID(got, "r-early-false")
		if rule == nil || !rule.Match || len(rule.Conditions) != 2 {
			t.Fatalf("winning rule record wrong: %+v", rule)
		}
		plan := rule.Conditions[0]
		if plan.Attribute != "plan" || plan.Op != "eq" || plan.CompareValue != "pro" ||
			plan.ActualValue == nil || *plan.ActualValue != "pro" || plan.Missing || !plan.Match {
			t.Fatalf("plan condition record wrong: %+v", plan)
		}
		tier := rule.Conditions[1]
		if list, ok := tier.CompareValue.([]string); !ok || len(list) != 3 ||
			list[0] != "" || list[1] != "a" || list[2] != "a" {
			t.Fatalf("tier compareValue must keep empty/duplicates/order: %#v", tier.CompareValue)
		}
		if tier.Attribute != "tier" || tier.Op != "in" || tier.Missing || !tier.Match ||
			tier.ActualValue == nil || *tier.ActualValue != "a" {
			t.Fatalf("tier condition record wrong: %+v", tier)
		}
		if !strings.Contains(got.Explanation.Outcome, "r-early-false") {
			t.Fatalf("outcome must name the deciding rule: %q", got.Explanation.Outcome)
		}
		if explainRuleByID(got, "r-unseen") != nil || explainRuleByID(got, "r-later-true") != nil {
			t.Fatalf("later rules must not be considered: %v", explainedIDs(got))
		}
	}
}

// TestExplainExtraFieldsLaterRuleKeepsMissedRecords 验证“靠前规则未成立、后续
// 规则才命中”时：附加字段不改变获胜规则；解释保留此前未命中的 r-early-false
// 及其全部条件（plan 成立、tier 因非成员不成立的逐条判断都在案），r-unseen
// 仍不出现；条件的比较值、实际值、缺失标记与判断结果不变。
func TestExplainExtraFieldsLaterRuleKeepsMissedRecords(t *testing.T) {
	for _, vraw := range []string{extraExplainFull, extraExplainChanged, extraExplainSparse, extraExplainShadow} {
		cfg := mustParseConfig(t, vraw)
		got := cfg.Find("f").EvaluateExplain(mustParseContext(t, `{"plan":"pro","tier":"b","region":"cn"}`))

		trueID := "r-later-true"
		want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &trueID}
		if !got.EvalResult.Equals(want) {
			t.Fatalf("result = %+v, want %+v", got.EvalResult, want)
		}
		if ids := explainedIDs(got); len(ids) != 2 || ids[0] != "r-early-false" || ids[1] != "r-later-true" {
			t.Fatalf("considered rules = %v, want [r-early-false r-later-true]", ids)
		}
		early := explainRuleByID(got, "r-early-false")
		if early == nil || early.Match {
			t.Fatalf("missed earlier rule must be kept and marked non-match: %+v", early)
		}
		if len(early.Conditions) != 2 {
			t.Fatalf("missed rule must keep all its condition records: %+v", early.Conditions)
		}
		plan := early.Conditions[0]
		if plan.CompareValue != "pro" || plan.ActualValue == nil || *plan.ActualValue != "pro" ||
			plan.Missing || !plan.Match {
			t.Fatalf("plan record wrong: %+v", plan)
		}
		tier := early.Conditions[1]
		if list, ok := tier.CompareValue.([]string); !ok || len(list) != 3 ||
			list[0] != "" || list[1] != "a" || list[2] != "a" {
			t.Fatalf("tier compareValue wrong: %#v", tier.CompareValue)
		}
		if tier.ActualValue == nil || *tier.ActualValue != "b" || tier.Missing || tier.Match {
			t.Fatalf("tier record must show present non-member: %+v", tier)
		}
		later := explainRuleByID(got, "r-later-true")
		if later == nil || !later.Match || len(later.Conditions) != 1 {
			t.Fatalf("deciding later rule record wrong: %+v", later)
		}
		region := later.Conditions[0]
		if region.CompareValue != "cn" || region.ActualValue == nil || *region.ActualValue != "cn" ||
			region.Missing || !region.Match {
			t.Fatalf("region record wrong: %+v", region)
		}
		if explainRuleByID(got, "r-unseen") != nil {
			t.Fatalf("r-unseen must not appear after an earlier rule hit: %v", explainedIDs(got))
		}
	}
}

// TestExplainShadowExtrasNeverFillMissingAttributes 是同名附加信息的关键保障：
// 即使附加字段和嵌套附加对象在每一级都写了 plan/tier/region 的命中值，上下文
// 没有该属性时解释仍必须显示 actualValue=null、missing=true、条件不成立，求值
// 继续按既有规则走到后续规则（或默认值）。
func TestExplainShadowExtrasNeverFillMissingAttributes(t *testing.T) {
	cfg := mustParseConfig(t, extraExplainShadow)

	// 上下文只有 region：r-early-false 的 plan、tier 都必须按缺失处理，规则
	// 不成立；r-later-true 凭真正的上下文值 region=cn 命中。
	got := cfg.Find("f").EvaluateExplain(mustParseContext(t, `{"region":"cn"}`))
	if got.Reason != EvalRule || got.RuleID == nil || *got.RuleID != "r-later-true" || !got.Value {
		t.Fatalf("shadow extras must not make r-early-false win: %+v", got.EvalResult)
	}
	early := explainRuleByID(got, "r-early-false")
	if early == nil || early.Match {
		t.Fatalf("earlier rule must be kept and miss: %+v", early)
	}
	for i, attr := range []string{"plan", "tier"} {
		c := early.Conditions[i]
		if c.Attribute != attr || !c.Missing || c.ActualValue != nil || c.Match {
			t.Fatalf("missing %s must stay null/true/non-match despite shadow extras: %+v", attr, c)
		}
	}
	planC, tierC := early.Conditions[0], early.Conditions[1]
	if planC.CompareValue != "pro" {
		t.Fatalf("plan compareValue changed: %+v", planC)
	}
	if list, ok := tierC.CompareValue.([]string); !ok || len(list) != 3 || list[0] != "" || list[1] != "a" || list[2] != "a" {
		t.Fatalf("tier compareValue changed: %#v", tierC.CompareValue)
	}
	later := explainRuleByID(got, "r-later-true")
	if later == nil || !later.Match {
		t.Fatalf("r-later-true must win via the real context: %+v", got.Explanation.Rules)
	}

	// 上下文完全为空：连 region 也缺失，任何规则都不能凭附加信息命中，结果走
	// 默认值；三条规则都按缺失逐条在案。
	none := cfg.Find("f").EvaluateExplain(mustParseContext(t, `{}`))
	if none.Reason != EvalDefault || none.RuleID != nil || !none.Value {
		t.Fatalf("empty context must use the default despite shadow extras: %+v", none.EvalResult)
	}
	if ids := explainedIDs(none); len(ids) != 3 {
		t.Fatalf("all three rules must be considered when none matches: %v", ids)
	}
	for _, rid := range []string{"r-early-false", "r-later-true", "r-unseen"} {
		r := explainRuleByID(none, rid)
		if r == nil || r.Match {
			t.Fatalf("rule %s must be present and miss: %+v", rid, r)
		}
		for _, c := range r.Conditions {
			if !c.Missing || c.ActualValue != nil || c.Match {
				t.Fatalf("rule %s condition must stay missing: %+v", rid, c)
			}
		}
	}
	if out := mustMarshalExplain(t, none); bytes.Count(out, []byte(`"missing":true`)) != 4 {
		// plan、tier、region、region 共四个条件全部缺失。
		t.Fatalf("all four condition records must render missing=true: %s", out)
	}

	// 关闭开关同样不能被开关/规则/条件上的同名附加字段“启用”或命中。
	off := cfg.Find("off").EvaluateExplain(mustParseContext(t, `{}`))
	if off.Reason != EvalDisabled || off.Value || off.RuleID != nil {
		t.Fatalf("disabled flag stays disabled: %+v", off.EvalResult)
	}
	if len(off.Explanation.Rules) != 0 {
		t.Fatalf("disabled flag must not list rules, even shadowed ones: %+v", off.Explanation.Rules)
	}
}

// TestExplainShadowExtrasKeepExplicitEmptyString 区分缺失与显式空串：上下文
// 显式给出 "" 时，即使附加对象里有可命中的非空值，解释也必须保留空字符串、
// missing=false；其中 tier="" 是 in 列表的合法成员（该条件成立），plan="" 不
// 等于 "pro"（该条件不成立），规则整体仍不命中，由后续规则定案。
func TestExplainShadowExtrasKeepExplicitEmptyString(t *testing.T) {
	cfg := mustParseConfig(t, extraExplainShadow)
	got := cfg.Find("f").EvaluateExplain(mustParseContext(t, `{"plan":"","tier":"","region":"cn"}`))
	if got.Reason != EvalRule || got.RuleID == nil || *got.RuleID != "r-later-true" {
		t.Fatalf("empty strings must not be replaced by shadow hits: %+v", got.EvalResult)
	}
	early := explainRuleByID(got, "r-early-false")
	if early == nil || early.Match {
		t.Fatalf("earlier rule must miss as a whole: %+v", early)
	}
	plan := early.Conditions[0]
	if plan.Missing || plan.ActualValue == nil || *plan.ActualValue != "" || plan.Match {
		t.Fatalf("plan explicit \"\" must stay present and unequal to \"pro\": %+v", plan)
	}
	tier := early.Conditions[1]
	// 空串是 ["","a","a"] 的成员：该条件成立，但规则因 plan 不成立仍不命中。
	if tier.Missing || tier.ActualValue == nil || *tier.ActualValue != "" || !tier.Match {
		t.Fatalf("tier explicit \"\" must stay present and be a list member: %+v", tier)
	}
	out := mustMarshalExplain(t, got)
	if !bytes.Contains(out, []byte(`"actualValue":"","missing":false`)) {
		t.Fatalf("empty strings must render as \"\"/false, not be confused with missing: %s", out)
	}
	if bytes.Contains(out, []byte(`"actualValue":null,"missing":false`)) {
		t.Fatalf("a present value must never render null/missing=false: %s", out)
	}
}

// TestExplainExtraFieldContentNeverSerialized 验证附加信息的具体内容（负责人、
// 备注、大数字）不出现在任何输出里：解释输出的顶层恰有五个字段、每个条件恰有
// 六个字段；普通模式仍只有原来的四个字段且与基线逐字节一致。
func TestExplainExtraFieldContentNeverSerialized(t *testing.T) {
	cfg := mustParseConfig(t, extraExplainFull)
	ctx := mustParseContext(t, `{"plan":"pro","tier":"b","region":"cn"}`)
	flag := cfg.Find("f")

	out := mustMarshalExplain(t, flag.EvaluateExplain(ctx))
	for _, leak := range []string{"owner", "pay-alice", "pay-bob", "pay-carol", "附加备注",
		"1e400", extraExplainHugeInt, "weight", "labels", "tags", "meta"} {
		if bytes.Contains(out, []byte(leak)) {
			t.Fatalf("extra content %q leaked into explanation: %s", leak, out)
		}
	}
	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("explanation is not JSON: %v", err)
	}
	if len(top) != 5 {
		t.Fatalf("explain output must keep exactly five top-level fields: %v", top)
	}
	for _, k := range []string{"key", "value", "reason", "ruleId", "explanation"} {
		if _, ok := top[k]; !ok {
			t.Fatalf("missing field %q in %v", k, top)
		}
	}
	rules := top["explanation"].(map[string]any)["rules"].([]any)
	for _, r0 := range rules {
		for _, c0 := range r0.(map[string]any)["conditions"].([]any) {
			cond := c0.(map[string]any)
			if len(cond) != 6 {
				t.Fatalf("condition must keep exactly six fields, extra data leaked: %v", cond)
			}
			for _, k := range []string{"attribute", "op", "compareValue", "actualValue", "missing", "match"} {
				if _, ok := cond[k]; !ok {
					t.Fatalf("missing condition field %q in %v", k, cond)
				}
			}
		}
	}

	// 普通模式：仍是原来的四字段单行对象，不含 explanation 或任何附加字段。
	plainOut, err := MarshalResult(flag.Evaluate(ctx))
	if err != nil {
		t.Fatalf("MarshalResult: %v", err)
	}
	wantPlain := `{"key":"f","value":true,"reason":"rule","ruleId":"r-later-true"}`
	if string(plainOut) != wantPlain {
		t.Fatalf("normal mode output = %s, want %s", plainOut, wantPlain)
	}
	var plain map[string]any
	if err := json.Unmarshal(plainOut, &plain); err != nil {
		t.Fatalf("plain output is not JSON: %v", err)
	}
	if len(plain) != 4 {
		t.Fatalf("normal mode must keep exactly four fields: %v", plain)
	}

	// 关闭与无规则开关的解释同样不含附加内容，且规则列表按既有约定为空列表。
	for _, tc := range []struct {
		key string
		ctx string
	}{{"off", `{"plan":"pro"}`}, {"plain", `{}`}} {
		small := mustMarshalExplain(t, cfg.Find(tc.key).EvaluateExplain(mustParseContext(t, tc.ctx)))
		if bytes.Contains(small, []byte("owner")) || bytes.Contains(small, []byte("备注")) ||
			bytes.Contains(small, []byte("pay-")) {
			t.Fatalf("extra content leaked for %s: %s", tc.key, small)
		}
		if !bytes.Contains(small, []byte(`"rules":[]`)) {
			t.Fatalf("%s must keep an empty rules list: %s", tc.key, small)
		}
	}
}

// TestParseConfigExtraFieldsAcceptLegalNestedValuesAndBigNumbers 单独锁定接受面：
// 附加信息中的嵌套对象、数组、布尔、null、1e400 与 400 位整数都是合法配置，
// 必须解析成功并可正常求值；它们不会成为任何规则的比较值。
func TestParseConfigExtraFieldsAcceptLegalNestedValuesAndBigNumbers(t *testing.T) {
	cfg := mustParseConfig(t, extraExplainFull)
	if len(cfg.Flags) != 3 {
		t.Fatalf("got %d flags, want 3", len(cfg.Flags))
	}
	flag := cfg.Find("f")
	// 配置中的业务比较值原样保留，没有被附加数据污染。
	got, want := flag.Rules[0].Conditions[1].inVal, []string{"", "a", "a"}
	if len(got) != len(want) {
		t.Fatalf("in compare list changed: %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("in compare list changed: %#v", got)
		}
	}
	r := flag.EvaluateExplain(mustParseContext(t, `{"plan":"pro","tier":"b","region":"cn"}`))
	if r.RuleID == nil || *r.RuleID != "r-later-true" {
		t.Fatalf("evaluation with legal extras must follow the real rules: %+v", r.EvalResult)
	}
}

// TestExplainExtraFieldDuplicatesRejectWholeConfig 保留“整份配置先校验”的边界：
// 未被选中或已经关闭的开关，其附加对象在合法大数字之后重复声明同名字段时，
// 解释功能也必须拒绝整份配置——以重复字段及其精确位置报错，不能误报成 JSON
// 无效或数字过大；配置不可用意味着目标开关的结果或半份解释都不可能产生。
func TestExplainExtraFieldDuplicatesRejectWholeConfig(t *testing.T) {
	goodTarget := `{"key":"f","enabled":true,"default":false,"rules":[]}`
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			"duplicate after 1e400 in unselected flag extra object",
			`{"flags":[` + goodTarget + `,
				{"key":"later","enabled":true,"default":false,"rules":[
					{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}],
						"note":{"v":1e400,"v":2}}]}]}`,
			`config.flags[1].rules[0].note.v: duplicate field "v"`,
		},
		{
			"duplicate after 400-digit integer in unselected flag extra object",
			`{"flags":[` + goodTarget + `,
				{"key":"later","enabled":true,"default":false,
					"meta":{"n":` + extraExplainHugeInt + `,"n":2},"rules":[]}]}`,
			`config.flags[1].meta.n: duplicate field "n"`,
		},
		{
			"duplicate after 1e400 in disabled flag extra object",
			`{"flags":[` + goodTarget + `,
				{"key":"off","enabled":false,"default":true,"rules":[],
					"meta":{"n":1e400,"n":2}}]}`,
			`config.flags[1].meta.n: duplicate field "n"`,
		},
		{
			"duplicate after huge integer in disabled flag nested extra",
			`{"flags":[` + goodTarget + `,
				{"key":"off","enabled":false,"default":true,"rules":[],
					"meta":{"team":{"n":` + extraExplainHugeInt + `,"n":2}}}]}`,
			`config.flags[1].meta.team.n: duplicate field "n"`,
		},
		{
			"duplicate after 1e400 in top-level extra object",
			`{"flags":[` + goodTarget + `],"meta":{"n":1e400,"n":2}}`,
			`config.meta.n: duplicate field "n"`,
		},
		{
			"duplicate after big number inside extra array element",
			`{"flags":[` + goodTarget + `,
				{"key":"later","enabled":true,"default":false,"rules":[],
					"tags":[{"v":1e400,"v":2}]}]}`,
			`config.flags[1].tags[0].v: duplicate field "v"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("whole config must be rejected, got config with %d flags", len(cfg.Flags))
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want substring %q", msg, tc.want)
			}
			// 合法大数字是合法 JSON 标量：重复字段必须先以自身名义报错，不能被
			// 误报成 JSON 语法错误；配置附加字段也没有“数字过大”的类型规则。
			if strings.Contains(msg, "invalid JSON") {
				t.Fatalf("legal big number must not be reported as invalid JSON: %q", msg)
			}
			if strings.Contains(msg, "must be") {
				t.Fatalf("extra-field big numbers have no type rule: %q", msg)
			}
		})
	}
}

// TestExplainExtraFieldDuplicatesRejectedBeforeAnyEvaluation 进一步保证拒绝发生
// 在任何求值之前：对同一份含重复附加字段的配置，既无法取得 Config，也就无法
// 对目标开关做普通求值或解释求值；把重复字段删掉（其余附加字段保留）后，
// 两种模式立即恢复既有输出。
func TestExplainExtraFieldDuplicatesRejectedBeforeAnyEvaluation(t *testing.T) {
	bad := `{"flags":[
		{"key":"f","enabled":true,"default":true,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]},
		{"key":"off","enabled":false,"default":false,"rules":[],
			"meta":{"x":1e400,"x":2}}]}`
	if _, err := ParseConfig([]byte(bad)); err == nil {
		t.Fatal("duplicate in disabled flag extra must fail the whole config")
	} else if !strings.Contains(err.Error(), `config.flags[1].meta.x: duplicate field "x"`) {
		t.Fatalf("error must point at the disabled flag's extra object: %v", err)
	}

	// 删除重复声明、保留合法附加字段与 1e400：配置可用，求值只认真实规则。
	fixed := `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]},
		{"key":"off","enabled":false,"default":false,"rules":[],
			"meta":{"x":1e400}}]}`
	cfg := mustParseConfig(t, fixed)
	ctx := mustParseContext(t, `{"plan":"pro"}`)
	id := "r"
	want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &id}
	if got := cfg.Find("f").Evaluate(ctx); !got.Equals(want) {
		t.Fatalf("plain evaluation after fixing extras: got %+v, want %+v", got, want)
	}
	explained := cfg.Find("f").EvaluateExplain(ctx)
	if !explained.EvalResult.Equals(want) || len(explained.Explanation.Rules) != 1 {
		t.Fatalf("explain after fixing extras wrong: %+v", explained)
	}
	if bytes.Contains(mustMarshalExplain(t, explained), []byte("1e400")) {
		t.Fatalf("legal extra big number must not appear in the explanation")
	}
}
