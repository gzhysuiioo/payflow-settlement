package payflow

import (
	"strings"
	"testing"
)

// 本文件为“开关键冲突与开关自身字段错误同时存在”补充 ParseConfig 层面的回归
// 保障：在一份语法合法、没有重复 JSON 字段的配置里，前一个开关定义合法，
// 后一个开关与它重名（按解码后的文字判断，直接字符与合法 \uXXXX 转义同义），
// 同时后一个开关缺少 default、其某条规则的 in 比较列表还含非字符串成员。
// 用户逐处修正输入时，报错必须按固定次序推进，且每次只暴露当前这一处：
//
//  1. 先指出后一个开关缺少 default（位置 flags[1].default），此时不提列表
//     类型错误，也不提重名；
//  2. 补上合法布尔值后，指出 in 列表成员的类型错误，位置贯穿开关、规则、
//     条件与成员下标（flags[1].rules[0].conditions[0].value[1]）；
//  3. 列表修正为非空字符串数组后，才报告重复开关键，指向后一次定义
//     flags[1].key 并说明第一次出现的位置 flags[0]；
//  4. 只有把后一个开关键改成不同名称，两份定义才被共同接受：原开关键仍
//     对应第一份定义（不能被后一次定义覆盖），两份定义都不能被悄悄丢弃。
//
// 只调整对象内部字段的书写顺序或文档空白，四个阶段的报错阶段、位置与原因
// 保持一致；开关数组的顺序不变，第一次定义始终是重复错误的参照。后一个
// 开关即使 enabled=false，也必须经过同样的完整校验。本文件只补充测试，
// 不改变求值功能与名称唯一性规则。

// dupFixEscE 是 é（U+00E9）的 JSON 转义写法；dupFixEscA 是 a（U+0061）的
// JSON 转义写法。用反引号字符串拼接，保证配置原文里出现的就是 \uXXXX
// 序列，而不是解码后的字符本身。
const (
	dupFixEscE = `\u` + `00E9`
	dupFixEscA = `\u` + `0061`
)

// dupFixFirstFlagText 拼出始终合法的第一份定义：启用、default=false、一条
// eq 规则 r1（plan 等于 "pro" 时返回 true）。keyJSON 是 key 的 JSON 字符串
// 内容（不含引号），可写直接字符或合法 Unicode 转义。
func dupFixFirstFlagText(keyJSON string) string {
	return `{"key":"` + keyJSON + `","enabled":true,"default":false,"rules":[` +
		`{"id":"r1","value":true,"conditions":[` +
		`{"attribute":"plan","op":"eq","value":"pro"}]}]}`
}

// dupFixSecondFlagText 拼出后一个开关：
//   - keyJSON：key 的 JSON 字符串内容，与第一份解码后同名，或改成新名字；
//   - enabledJSON：enabled 的布尔原文（"true"/"false"）；
//   - defaultJSON：""/"false"/"true"，空串表示省略 default 字段；
//   - listJSON：r2 规则唯一 in 条件的比较列表原文；
//   - reordered：为 true 时把开关、规则、条件对象内部的字段按另一种顺序书写，
//     报错阶段与位置不应受影响。
//
// r2 在 region 属于列表时返回 false，与第一份的 r1（返回 true）明确区分。
func dupFixSecondFlagText(keyJSON, enabledJSON, defaultJSON, listJSON string, reordered bool) string {
	cond := `{"attribute":"region","op":"in","value":` + listJSON + `}`
	rule := `{"id":"r2","value":false,"conditions":[` + cond + `]}`
	if reordered {
		cond = `{"value":` + listJSON + `,"op":"in","attribute":"region"}`
		rule = `{"conditions":[` + cond + `],"value":false,"id":"r2"}`
		s := `{"rules":[` + rule + `],"enabled":` + enabledJSON + `,"key":"` + keyJSON + `"`
		if defaultJSON != "" {
			s += `,"default":` + defaultJSON
		}
		return s + `}`
	}
	s := `{"key":"` + keyJSON + `","enabled":` + enabledJSON + `,`
	if defaultJSON != "" {
		s += `"default":` + defaultJSON + `,`
	}
	return s + `"rules":[` + rule + `]}`
}

// dupFixConfigText 把两份开关组成一份 flags 数组文档；padded 时在文档前后
// 加入 JSON 允许的空白，报错与接受结果都不应改变。
func dupFixConfigText(first, second string, padded bool) string {
	doc := `{"flags":[` + first + `,` + second + `]}`
	if padded {
		return " \t\r\n" + doc + "\n  \t"
	}
	return doc
}

// dupFixAssertError 要求 ParseConfig 失败，且错误信息包含全部 wantSub、
// 不包含任何 notSub（用于锁定“当前只报这一处”，不提前暴露后续阶段问题）。
func dupFixAssertError(t *testing.T, label string, raw string, wantSub, notSub []string) {
	t.Helper()
	_, err := ParseConfig([]byte(raw))
	if err == nil {
		t.Fatalf("%s: expected config error, config=%s", label, raw)
	}
	msg := err.Error()
	for _, want := range wantSub {
		if !strings.Contains(msg, want) {
			t.Fatalf("%s: error=%q, want substring %q\nconfig: %s", label, msg, want, raw)
		}
	}
	for _, not := range notSub {
		if strings.Contains(msg, not) {
			t.Fatalf("%s: error=%q, must not contain %q\nconfig: %s", label, msg, not, raw)
		}
	}
}

// TestDuplicateFlagKeyFixSequenceErrorStages 沿用户逐处修正的前三个阶段，
// 锁定报错的推进次序与精确定位；key 写法（直接字符 / 合法 Unicode 转义）、
// 对象内部字段顺序、文档空白各自组合后，三个阶段的报错都不变。
func TestDuplicateFlagKeyFixSequenceErrorStages(t *testing.T) {
	spellings := []struct {
		name string
		json string
	}{
		{"direct", "café"},
		{"escaped", "caf" + dupFixEscE},
	}
	stages := []struct {
		name    string
		defJSON string
		list    string
		wantSub []string
		notSub  []string
	}{
		{
			name:    "missing default reported first",
			defJSON: "",
			list:    `["eu",5]`,
			wantSub: []string{`config.flags[1].default`, "field is required"},
			notSub:  []string{"must be a string", "duplicate flag key"},
		},
		{
			name:    "in-list member type error after default is fixed",
			defJSON: "false",
			list:    `["eu",5]`,
			wantSub: []string{
				`config.flags[1].rules[0].conditions[0].value[1]`,
				"must be a string",
			},
			notSub: []string{"field is required", "duplicate flag key"},
		},
		{
			name:    "duplicate key reported only after the flag is otherwise valid",
			defJSON: "false",
			list:    `["eu","cn"]`,
			wantSub: []string{
				`config.flags[1].key`,
				"duplicate flag key",
				`"café"`,
				"first at flags[0]",
			},
			notSub: []string{"must be a string", "field is required"},
		},
	}
	for _, sp := range spellings {
		for _, reordered := range []bool{false, true} {
			for _, padded := range []bool{false, true} {
				for _, st := range stages {
					label := "key=" + sp.name + "/reordered=" +
						boolLabel(reordered) + "/padded=" + boolLabel(padded) + "/" + st.name
					raw := dupFixConfigText(
						dupFixFirstFlagText("café"),
						dupFixSecondFlagText(sp.json, "true", st.defJSON, st.list, reordered),
						padded)
					dupFixAssertError(t, label, raw, st.wantSub, st.notSub)
				}
			}
		}
	}
}

// TestDuplicateFlagKeyFixSequenceFirstFlagEscaped 覆盖反向拼写：第一份定义
// 用合法 Unicode 转义书写、后一份用直接字符。三个阶段的报错与解码后同名
// 的直接写法逐字一致，重复错误始终指向 flags[1] 并以 flags[0] 为第一次
// 出现的参照。
func TestDuplicateFlagKeyFixSequenceFirstFlagEscaped(t *testing.T) {
	first := dupFixFirstFlagText("caf" + dupFixEscE)
	cases := []struct {
		name    string
		defJSON string
		list    string
		wantSub []string
		notSub  []string
	}{
		{"stage1 missing default", "", `["eu",5]`,
			[]string{`config.flags[1].default`, "field is required"},
			[]string{"must be a string", "duplicate flag key"}},
		{"stage2 bad list member", "false", `["eu",5]`,
			[]string{`config.flags[1].rules[0].conditions[0].value[1]`, "must be a string"},
			[]string{"field is required", "duplicate flag key"}},
		{"stage3 duplicate key", "false", `["eu","cn"]`,
			[]string{`config.flags[1].key`, "duplicate flag key", `"café"`, "first at flags[0]"},
			[]string{"must be a string", "field is required"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := dupFixConfigText(first,
				dupFixSecondFlagText("café", "true", tc.defJSON, tc.list, false), false)
			dupFixAssertError(t, tc.name, raw, tc.wantSub, tc.notSub)
		})
	}

	// 改名后两份共同接受；用解码后的名字找到的是第一份（转义书写的）定义，
	// plan=pro 命中它的 r1。
	raw := dupFixConfigText(first,
		dupFixSecondFlagText("beta", "true", "true", `["eu","cn"]`, false), false)
	cfg := mustParseConfig(t, raw)
	if got := len(cfg.Flags); got != 2 {
		t.Fatalf("both definitions must be kept, got %d flags", got)
	}
	flag := cfg.Find("café")
	if flag != &cfg.Flags[0] {
		t.Fatalf("decoded name must resolve to the first (escaped) definition")
	}
	if r := flag.Evaluate(mustParseContext(t, `{"plan":"pro"}`)); r.Value != true ||
		r.Reason != EvalRule || r.RuleID == nil || *r.RuleID != "r1" {
		t.Fatalf("first definition must stay in charge: %+v", r)
	}
}

// TestDuplicateFlagKeyFixSequenceRenamingAcceptsBothDefinitions 锁定第四阶段：
// 后一个开关改成不同名字后，两份定义都被接受——开关顺序不变；原开关键仍
// 解析到第一份定义（第一份 default=false、只有 r1；第二份 default=true、
// 只有 r2，二者不可互相覆盖）；新名字按第二份定义求值；任何一份都不能被
// 悄悄丢弃。写法、字段顺序与空白变体下接受结果一致。
func TestDuplicateFlagKeyFixSequenceRenamingAcceptsBothDefinitions(t *testing.T) {
	renamedSpellings := []struct {
		name string
		json string
	}{
		{"direct", "beta"},
		{"escaped", "bet" + dupFixEscA},
	}
	var canonical *Config
	for _, sp := range renamedSpellings {
		for _, reordered := range []bool{false, true} {
			for _, padded := range []bool{false, true} {
				label := "key=" + sp.name + "/reordered=" + boolLabel(reordered) +
					"/padded=" + boolLabel(padded)
				raw := dupFixConfigText(
					dupFixFirstFlagText("café"),
					dupFixSecondFlagText(sp.json, "true", "true", `["eu","cn"]`, reordered),
					padded)
				cfg := mustParseConfig(t, raw)
				if len(cfg.Flags) != 2 {
					t.Fatalf("%s: both definitions must be kept, got %d", label, len(cfg.Flags))
				}
				if cfg.Flags[0].Key != "café" || cfg.Flags[1].Key != "beta" {
					t.Fatalf("%s: flag order/keys changed: %q, %q", label, cfg.Flags[0].Key, cfg.Flags[1].Key)
				}
				if sp.name == "direct" && !reordered && !padded {
					canonical = cfg
				}
			}
		}
	}

	// 详细身份与求值断言只在标准写法上做一次。
	first := &canonical.Flags[0]
	second := &canonical.Flags[1]
	if canonical.Find("café") != first || canonical.Find("beta") != second {
		t.Fatal("Find must not be shadowed by the later definition, and both flags stay reachable")
	}
	if !first.Enabled || first.Default != false || len(first.Rules) != 1 ||
		first.Rules[0].ID != "r1" || first.Rules[0].Value != true {
		t.Fatalf("first definition altered: %+v", first)
	}
	if !second.Enabled || second.Default != true || len(second.Rules) != 1 ||
		second.Rules[0].ID != "r2" || second.Rules[0].Value != false {
		t.Fatalf("second definition altered: %+v", second)
	}

	// plan=pro：第一份的 r1 命中并返回 true；第二份根本不引用 plan，
	// 不可能给出这个结果——证明原开关键没有被后一次定义覆盖。
	planPro := mustParseContext(t, `{"plan":"pro"}`)
	r := first.Evaluate(planPro)
	if r.Key != "café" || r.Value != true || r.Reason != EvalRule ||
		r.RuleID == nil || *r.RuleID != "r1" {
		t.Fatalf("café must evaluate by the first definition's r1: %+v", r)
	}
	// 解释模式走同一条定案路径，记录的也只是第一份的 r1。
	ex := first.EvaluateExplain(planPro)
	if ex.Value != true || ex.Reason != EvalRule || ex.RuleID == nil || *ex.RuleID != "r1" {
		t.Fatalf("explain must also use the first definition: %+v", ex.EvalResult)
	}
	if len(ex.Explanation.Rules) != 1 || ex.Explanation.Rules[0].RuleID != "r1" ||
		!ex.Explanation.Rules[0].Match {
		t.Fatalf("explanation must record the first definition's r1: %+v", ex.Explanation)
	}

	// 空上下文是两份 default 的直接对照：第一份 default=false；若第二份
	// 覆盖了同名开关，这里会错误地得到 true。
	empty := mustParseContext(t, `{}`)
	if r := first.Evaluate(empty); r.Value != false || r.Reason != EvalDefault || r.RuleID != nil {
		t.Fatalf("café must use the first default=false: %+v", r)
	}
	// 第二份在空上下文使用自己的 default=true。
	if r := second.Evaluate(empty); r.Value != true || r.Reason != EvalDefault || r.RuleID != nil {
		t.Fatalf("beta must use its own default=true: %+v", r)
	}

	// region=eu：第二份的 r2 命中并返回 false；第一份不引用 region，只能
	// 落回 default=false。两个名字在同一份配置里各自给出属于自己定义的结果。
	regionEU := mustParseContext(t, `{"region":"eu"}`)
	if r := second.Evaluate(regionEU); r.Key != "beta" || r.Value != false ||
		r.Reason != EvalRule || r.RuleID == nil || *r.RuleID != "r2" {
		t.Fatalf("beta must evaluate by the second definition's r2: %+v", r)
	}
	if r := first.Evaluate(regionEU); r.Value != false || r.Reason != EvalDefault {
		t.Fatalf("café must not react to region: %+v", r)
	}
}

// TestDuplicateFlagKeyFixSequenceDisabledLaterFlag 锁定：后一个开关即使
// enabled=false（不会被求值、关闭固定 false），也必须经过与启用时完全相同
// 的完整校验——三个阶段的报错位置与原因不变；改名后它与第一份共同被接受，
// 且关闭开关的求值固定为 disabled。
func TestDuplicateFlagKeyFixSequenceDisabledLaterFlag(t *testing.T) {
	first := dupFixFirstFlagText("café")
	cases := []struct {
		name    string
		defJSON string
		list    string
		wantSub []string
		notSub  []string
	}{
		{"stage1 missing default", "", `["eu",5]`,
			[]string{`config.flags[1].default`, "field is required"},
			[]string{"must be a string", "duplicate flag key"}},
		{"stage2 bad list member", "false", `["eu",5]`,
			[]string{`config.flags[1].rules[0].conditions[0].value[1]`, "must be a string"},
			[]string{"field is required", "duplicate flag key"}},
		{"stage3 duplicate key", "false", `["eu","cn"]`,
			[]string{`config.flags[1].key`, "duplicate flag key", `"café"`, "first at flags[0]"},
			[]string{"must be a string", "field is required"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := dupFixConfigText(first,
				dupFixSecondFlagText("café", "false", tc.defJSON, tc.list, false), false)
			dupFixAssertError(t, tc.name, raw, tc.wantSub, tc.notSub)
		})
	}

	// 改名后两份共同接受；第二个开关关闭，求值固定 disabled；原开关键仍是
	// 第一份启用的定义。
	raw := dupFixConfigText(first,
		dupFixSecondFlagText("beta", "false", "true", `["eu","cn"]`, false), false)
	cfg := mustParseConfig(t, raw)
	ctx := mustParseContext(t, `{"plan":"pro","region":"eu"}`)
	if r := cfg.Find("beta").Evaluate(ctx); r.Value != false || r.Reason != EvalDisabled || r.RuleID != nil {
		t.Fatalf("renamed disabled flag must stay fixed false: %+v", r)
	}
	if r := cfg.Find("café").Evaluate(ctx); r.Value != true ||
		r.Reason != EvalRule || r.RuleID == nil || *r.RuleID != "r1" {
		t.Fatalf("original key must still use the first enabled definition: %+v", r)
	}
}

// boolLabel 给表驱动用例的布尔变体一个稳定的短标签。
func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
