package payflow

import (
	"fmt"
	"strings"
	"testing"
)

func mustParseConfig(t *testing.T, raw string) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig failed: %v\nconfig: %s", err, raw)
	}
	return cfg
}

func mustParseContext(t *testing.T, raw string) *Context {
	t.Helper()
	ctx, err := ParseContext([]byte(raw))
	if err != nil {
		t.Fatalf("ParseContext failed: %v\ncontext: %s", err, raw)
	}
	return ctx
}

func eval(t *testing.T, cfgRaw, key, ctxRaw string) EvalResult {
	t.Helper()
	cfg := mustParseConfig(t, cfgRaw)
	ctx := mustParseContext(t, ctxRaw)
	flag := cfg.Find(key)
	if flag == nil {
		t.Fatalf("flag %q not found", key)
	}
	return flag.Evaluate(ctx)
}

func TestEvaluateDisabled(t *testing.T) {
	// 即使 default=true 且规则命中，关闭的开关固定 false。
	cfg := `{"flags":[{"key":"f","enabled":false,"default":true,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	got := eval(t, cfg, "f", `{"plan":"pro"}`)
	want := EvalResult{Key: "f", Value: false, Reason: EvalDisabled, RuleID: nil}
	if !got.Equals(want) {
		t.Fatalf("disabled: got %+v, want %+v", got, want)
	}
}

func TestEvaluateRuleMatchOrder(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]},
		{"id":"r2","value":false,"conditions":[{"attribute":"tier","op":"in","value":["a","b"]}]}]}]}`
	// 首条全中规则获胜，r1 命中即采用其 value=true，r2 不再考虑。
	got := eval(t, cfg, "f", `{"plan":"pro","tier":"a"}`)
	id := "r1"
	want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &id}
	if !got.Equals(want) {
		t.Fatalf("first-match: got %+v, want %+v", got, want)
	}

	// r1 不成立，r2 成立。
	got = eval(t, cfg, "f", `{"plan":"free","tier":"b"}`)
	id = "r2"
	want = EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &id}
	if !got.Equals(want) {
		t.Fatalf("second-rule: got %+v, want %+v", got, want)
	}
}

func TestEvaluateRulePriorityFalseWinsAndIsImmediate(t *testing.T) {
	// 已启用的开关，default=true：r-deny 在前、返回 false，含一个 eq 与一个 in
	// 条件（AND）；r-allow 在后、返回 true，且两条规则可以同时成立。
	// 公开规则：按数组顺序，第一条所有条件都成立的规则立即定案——即使它返回
	// false，也不能被后面的 true 规则或 true 默认值翻转。
	rDenyFirst := `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-deny","value":false,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"tier","op":"in","value":["a","b"]}]},
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]}]}]}`
	denyID, allowID := "r-deny", "r-allow"

	// 上下文同时满足两条规则：前面的 r-deny 获胜，结果为 false；reason=rule、
	// ruleId 指向前一条，与后面的规则及默认值同为 true 无关。
	got := eval(t, rDenyFirst, "f", `{"plan":"pro","tier":"a","region":"cn"}`)
	want := EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &denyID}
	if !got.Equals(want) {
		t.Fatalf("both match, earlier false must decide: got %+v, want %+v", got, want)
	}

	// 只交换两条规则的配置位置，规则内容与上下文完全相同：改由原来的后一条
	// r-allow 返回 true，命中原因与规则编号跟随获胜规则变化。
	rAllowFirst := `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]},
		{"id":"r-deny","value":false,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"tier","op":"in","value":["a","b"]}]}]}]}`
	got = eval(t, rAllowFirst, "f", `{"plan":"pro","tier":"a","region":"cn"}`)
	want = EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &allowID}
	if !got.Equals(want) {
		t.Fatalf("swap order, earlier true must decide: got %+v, want %+v", got, want)
	}

	// 前面的规则只有部分条件成立（eq 成立但 in 不成立）：任何单个条件成立都
	// 不算命中，也不能因前一条返回 false 就停止；完整成立的后一条 r-allow 获胜。
	got = eval(t, rDenyFirst, "f", `{"plan":"pro","tier":"c","region":"cn"}`)
	want = EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &allowID}
	if !got.Equals(want) {
		t.Fatalf("partial match must fall through: got %+v, want %+v", got, want)
	}
	// 前一条的 eq 属性缺失（而非不等）：同样只是不命中，继续检查后一条。
	got = eval(t, rDenyFirst, "f", `{"tier":"a","region":"cn"}`)
	if !got.Equals(want) {
		t.Fatalf("missing attr on earlier rule must fall through: got %+v, want %+v", got, want)
	}

	// 两条规则都不成立：采用 default=true，reason=default、ruleId=null，
	// 与“某条规则返回 true”的命中（reason=rule 且带 ruleId）明确区分。
	got = eval(t, rDenyFirst, "f", `{"plan":"free","tier":"c","region":"eu"}`)
	want = EvalResult{Key: "f", Value: true, Reason: EvalDefault, RuleID: nil}
	if !got.Equals(want) {
		t.Fatalf("neither matches must use default: got %+v, want %+v", got, want)
	}
}

func TestEvaluateRulePriorityEmptyStringMatchVsMissing(t *testing.T) {
	// 前一条规则返回 false 且用 eq 比较空字符串，后一条返回 true 且可同时成立。
	// 属性缺失表示该条件不成立（继续检查后一条）；显式提供 "" 是合法值，
	// 前一条所有条件成立时真正获胜并立即定案为 false。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-empty","value":false,"conditions":[
			{"attribute":"note","op":"eq","value":""},
			{"attribute":"tier","op":"in","value":["a","b"]}]},
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]}]}]}`
	emptyID, allowID := "r-empty", "r-allow"

	// note 缺失：r-empty 不命中，即使 tier 落在 in 列表内；继续到 r-allow。
	got := eval(t, cfg, "f", `{"tier":"a","region":"cn"}`)
	want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &allowID}
	if !got.Equals(want) {
		t.Fatalf("missing attribute must continue to later rule: got %+v, want %+v", got, want)
	}

	// note 显式为空字符串且另一条件成立：r-empty 完整命中，立即定案 false，
	// 同时成立的 r-allow 与 true 默认值都不能改变结果。
	got = eval(t, cfg, "f", `{"note":"","tier":"a","region":"cn"}`)
	want = EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &emptyID}
	if !got.Equals(want) {
		t.Fatalf("explicit empty string makes earlier rule win: got %+v, want %+v", got, want)
	}

	// note 显式为空字符串但另一条件不成立：r-empty 仍不命中，继续到 r-allow，
	// 不能把 eq 条件单独成立当成命中。
	got = eval(t, cfg, "f", `{"note":"","tier":"c","region":"cn"}`)
	want = EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &allowID}
	if !got.Equals(want) {
		t.Fatalf("empty eq alone must not match the rule: got %+v, want %+v", got, want)
	}
}

func TestEvaluateDefault(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r1","value":false,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	got := eval(t, cfg, "f", `{"plan":"free"}`)
	want := EvalResult{Key: "f", Value: true, Reason: EvalDefault, RuleID: nil}
	if !got.Equals(want) {
		t.Fatalf("default true: got %+v, want %+v", got, want)
	}

	// 无规则时也采用 default。
	cfg = `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[]}]}`
	got = eval(t, cfg, "g", `{}`)
	want = EvalResult{Key: "g", Value: false, Reason: EvalDefault, RuleID: nil}
	if !got.Equals(want) {
		t.Fatalf("empty rules: got %+v, want %+v", got, want)
	}
}

func TestEqSemantics(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"Pro"}]}]}]}`
	cases := map[string]string{
		`{"plan":"Pro"}`:  "match",
		`{"plan":"pro"}`:  "case",       // 区分大小写
		`{"plan":" Pro"}`: "whitespace", // 不裁剪空白
		`{}`:              "missing",
	}
	for ctxRaw, label := range cases {
		got := eval(t, cfg, "f", ctxRaw)
		matched := got.Reason == EvalRule
		wantMatch := label == "match"
		if matched != wantMatch {
			t.Errorf("%s (%s): match=%v want %v", label, ctxRaw, matched, wantMatch)
		}
	}

	// 空字符串是合法属性值，可以被 eq 命中。
	cfgEmpty := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"note","op":"eq","value":""}]}]}]}`
	got := eval(t, cfgEmpty, "f", `{"note":""}`)
	if got.Reason != EvalRule || !got.Value {
		t.Fatalf("empty string eq: got %+v", got)
	}
	// 属性缺失与属性为空字符串不同。
	got = eval(t, cfgEmpty, "f", `{}`)
	if got.Reason != EvalDefault {
		t.Fatalf("missing attr vs empty: got %+v", got)
	}
}

func TestInSemantics(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"tier","op":"in","value":["a","b",""]}]}]}]}`
	hit := eval(t, cfg, "f", `{"tier":"a"}`)
	if hit.Reason != EvalRule {
		t.Fatalf("in a: got %+v", hit)
	}
	miss := eval(t, cfg, "f", `{"tier":"c"}`)
	if miss.Reason != EvalDefault {
		t.Fatalf("in c: got %+v", miss)
	}
	emptyHit := eval(t, cfg, "f", `{"tier":""}`)
	if emptyHit.Reason != EvalRule {
		t.Fatalf("in empty string: got %+v", emptyHit)
	}
	missing := eval(t, cfg, "f", `{"other":"a"}`)
	if missing.Reason != EvalDefault {
		t.Fatalf("in missing attr: got %+v", missing)
	}
}

// --- in 条件：解码后精确相等的回归保障 ----------------------------------------
//
// 下列测试固定 in 运算符的成员判断边界：配置列表中的候选值与上下文中的属性值
// 都先按 JSON 解码成真实文字，成员关系只承认解码后的字符串逐码点精确相等。
// 直接字符与合法 Unicode 转义（含代理项对）是同一文字的不同写法；大小写、
// 首尾空白、预组合字符与“基字符＋组合附加符号”的序列不做任何归一化。
//
// 为避免测试源码本身的转义写法在阅读时与“JSON 文本中的转义”混淆，所有转义
// 外形都在运行时由 uesc 与反斜杠字面量拼出，直接字符则照常以 UTF-8 写出。

// uesc 把若干码点渲染成 JSON 的 \uxxxx 转义外形（BMP 之外的码点输出一对
// 高/低代理项转义）。返回的是供 JSON 解析器消费的原文片段，如
// uesc('é') 的 6 个字符解码后是 é，uesc('😀') 的 12 个字符解码后是 😀。
func uesc(rs ...rune) string {
	var b strings.Builder
	for _, r := range rs {
		if r > 0xFFFF {
			lo := int(r) - 0x10000
			hi := 0xD800 + (lo >> 10)
			low := 0xDC00 + (lo & 0x3FF)
			fmt.Fprintf(&b, "\\u%04x\\u%04x", hi, low)
		} else {
			fmt.Fprintf(&b, "\\u%04x", r)
		}
	}
	return b.String()
}

// inRuleConfig 拼出一份单开关、单 in 规则的配置：开关 f 启用、default=false，
// 规则 id 固定 r-in、value=true。attrJSON 是 attribute 的 JSON 字符串内容
// （不含外层引号），listJSON 是完整的候选数组 JSON 文本。
func inRuleConfig(attrJSON, listJSON string) string {
	return `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[` +
		`{"id":"r-in","value":true,"conditions":[` +
		`{"attribute":"` + attrJSON + `","op":"in","value":` + listJSON + `}]}]}]}`
}

// inContext 拼出只有一个字符串属性的上下文；keyJSON、valueJSON 都是 JSON 字符串
// 内容（不含外层引号），可直接写字符，也可传 uesc 生成的转义外形。
func inContext(keyJSON, valueJSON string) string {
	return `{"` + keyJSON + `":"` + valueJSON + `"}`
}

// assertInMembership 断言单 in 规则配置对上下文的求值结果：wantMatch=true 时
// 必须命中 r-in（value=true），否则必须落到 default（ruleId 为 null）。
func assertInMembership(t *testing.T, cfgRaw, ctxRaw string, wantMatch bool) {
	t.Helper()
	got := eval(t, cfgRaw, "f", ctxRaw)
	if wantMatch {
		if got.Reason != EvalRule || !got.Value {
			t.Fatalf("ctx %s: expected exact in-match, got %+v", ctxRaw, got)
		}
		return
	}
	if got.Reason != EvalDefault || got.Value || got.RuleID != nil {
		t.Fatalf("ctx %s: lookalike must not be a member, got %+v", ctxRaw, got)
	}
}

// TestInDecodedSpellingsEquivalent 保护“解码后的字符串精确相等才算属于列表”：
// 属性名、列表候选值、上下文属性名与值分别用直接字符或合法 Unicode 转义书写
// （含 BMP 之外字符所需的代理项对）时，配置与上下文的写法可以任意组合，命中
// 的都是同一条规则，key、value、reason、ruleId 及序列化后的输出逐字节一致。
func TestInDecodedSpellingsEquivalent(t *testing.T) {
	id := "r-in"
	want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &id}
	wantOut := `{"key":"f","value":true,"reason":"rule","ruleId":"r-in"}`

	const (
		eacute = 'é' // U+00E9
		emoji  = '😀' // U+1F600，JSON 转义外形需要一对代理项
	)
	nfdEacute := "e" + string(rune(0x0301)) // e ＋ 组合用锐音符，与 é 外形几乎相同

	// 候选列表的三种 JSON 写法：全直接、全转义、混合；解码后都是
	// ["pro","é","😀"]。
	listVariants := []struct {
		name string
		json string
	}{
		{"direct", `["pro","é","😀"]`},
		{"escaped", `["` + uesc('p') + `ro","` + uesc(eacute) + `","` + uesc(emoji) + `"]`},
		{"mixed", `["pro","` + uesc(eacute) + `","😀"]`},
	}
	// 属性名 tier 的两种写法。
	attrVariants := []struct {
		name string
		json string
	}{
		{"direct", `tier`},
		{"escaped", uesc('t') + "ier"},
	}
	keyVariants := attrVariants
	valueVariants := []struct {
		name string
		json string
	}{
		{"ascii-direct", `pro`},
		{"ascii-escaped", uesc('p') + "ro"},
		{"eacute-direct", `é`},
		{"eacute-escaped", uesc(eacute)},
		{"emoji-direct", `😀`},
		{"emoji-surrogate-pair", uesc(emoji)},
	}
	for _, lv := range listVariants {
		for _, av := range attrVariants {
			cfgRaw := inRuleConfig(av.json, lv.json)
			for _, kv := range keyVariants {
				for _, vv := range valueVariants {
					t.Run(lv.name+"/"+av.name+"/"+kv.name+"/"+vv.name, func(t *testing.T) {
						got := eval(t, cfgRaw, "f", inContext(kv.json, vv.json))
						if !got.Equals(want) {
							t.Fatalf("got %+v, want %+v", got, want)
						}
						out, err := MarshalResult(got)
						if err != nil {
							t.Fatal(err)
						}
						if string(out) != wantOut {
							t.Fatalf("marshaled output %q, want %q", out, wantOut)
						}
					})
				}
			}
			// 等价只限于“同一文字的不同 JSON 写法”：外形相近但码点不同的值，
			// 在任何一种列表写法下都不能混进列表。
			lookalikes := []struct {
				name string
				json string
			}{
				{"uppercase", `Pro`},
				{"leading space", ` pro`},
				{"trailing space", `pro `},
				{"nfd eacute", nfdEacute},
				{"nfd eacute escaped combining mark", "e" + uesc(rune(0x0301))},
				{"absent value", `web`},
				{"empty string", ``},
				{"candidate prefix", `éx`},
				{"other emoji", `😃`},
			}
			for _, ll := range lookalikes {
				t.Run(lv.name+"/miss/"+ll.name, func(t *testing.T) {
					assertInMembership(t, cfgRaw, inContext("tier", ll.json), false)
				})
			}
		}
	}
}

// TestInEscapedBackslashTextNotReDecoded 保护转义边界：JSON 中已转义的反斜杠
// 之后即使跟着 u 与十六进制数字，解码后也只是一段普通文本，不会再被当作 Unicode
// 转义解码一次；它只与相同的字面文本相等，与“真正解码出的字符”互不相识。
// 属性名同样遵守这条边界。
func TestInEscapedBackslashTextNotReDecoded(t *testing.T) {
	bs := string(rune(0x5C))
	// fragEacute 是 JSON 文本片段 \\uxxxx（双反斜杠外形），解码后为 6 个
	// 普通字符：一个真实反斜杠后接 u00e9。
	fragEacute := bs + bs + "u00e9"
	// 同一字面文本的另一种 JSON 写法：第一个反斜杠用 u005c 转义写出。
	fragEacuteAlt := uesc(rune(0x5C)) + "u00e9"

	// 列表唯一候选就是这段字面文本。
	cfgLiteral := inRuleConfig("tier", `["`+fragEacute+`"]`)
	// 两种 JSON 写法在上下文一侧解码为同一字面文本，精确命中。
	assertInMembership(t, cfgLiteral, inContext("tier", fragEacute), true)
	assertInMembership(t, cfgLiteral, inContext("tier", fragEacuteAlt), true)
	// 真正的 é——无论直接写出还是用 Unicode 转义——都不是这个文本。
	assertInMembership(t, cfgLiteral, inContext("tier", `é`), false)
	assertInMembership(t, cfgLiteral, inContext("tier", uesc('é')), false)
	// 十六进制位的大小写差异也是不同的字面文本。
	assertInMembership(t, cfgLiteral, inContext("tier", bs+bs+"u00C9"), false)

	// 反向：列表里只有真正的 é 时，反斜杠字面文本不能命中，转义外形可以。
	cfgReal := inRuleConfig("tier", `["é"]`)
	assertInMembership(t, cfgReal, inContext("tier", fragEacute), false)
	assertInMembership(t, cfgReal, inContext("tier", `é`), true)
	assertInMembership(t, cfgReal, inContext("tier", uesc('é')), true)

	// 属性名同理：fragAttr 在 JSON 中写成双反斜杠外形，解码后是 8 个普通字符
	// 的属性名（反斜杠后接 u0074ier），与属性 tier 无关。
	fragAttr := bs + bs + "u0074ier"
	fragAttrAlt := uesc(rune(0x5C)) + "u0074ier" // 同名字面文本的另一种写法
	cfgAttr := inRuleConfig(fragAttr, `["pro"]`)
	assertInMembership(t, cfgAttr, inContext(fragAttr, "pro"), true)
	assertInMembership(t, cfgAttr, inContext(fragAttrAlt, "pro"), true)
	assertInMembership(t, cfgAttr, inContext("tier", "pro"), false)
	// 注意：u0074 的 Unicode 转义解码后就是普通属性名 tier，仍不与字面
	// 反斜杠文本同名，不能读到另一个属性。
	assertInMembership(t, cfgAttr, inContext(uesc('t')+"ier", "pro"), false)
}

// TestInComposedVsDecomposedCandidatesStayDistinct 保护 Unicode 归一化边界：
// 列表只有预组合字符时，“基字符＋组合附加符号”的上下文值不能命中；列表明确
// 包含组合序列时只有该序列可匹配。两种序列是不同的字符串，共存不构成重复配置。
func TestInComposedVsDecomposedCandidatesStayDistinct(t *testing.T) {
	nfdEacute := "e" + string(rune(0x0301))
	nfdEacuteEsc := "e" + uesc(rune(0x0301))

	// 列表只有预组合的 é（U+00E9）：é 的两种 JSON 写法都命中，NFD 序列不行。
	cfgComposed := inRuleConfig("tier", `["é"]`)
	cfgComposedEsc := inRuleConfig("tier", `["`+uesc('é')+`"]`)
	for _, cfgRaw := range []string{cfgComposed, cfgComposedEsc} {
		assertInMembership(t, cfgRaw, inContext("tier", `é`), true)
		assertInMembership(t, cfgRaw, inContext("tier", uesc('é')), true)
		assertInMembership(t, cfgRaw, inContext("tier", nfdEacute), false)
		assertInMembership(t, cfgRaw, inContext("tier", nfdEacuteEsc), false)
		assertInMembership(t, cfgRaw, inContext("tier", `É`), false) // 大小写不折叠
		assertInMembership(t, cfgRaw, inContext("tier", ` é`), false)
		assertInMembership(t, cfgRaw, inContext("tier", `é `), false)
	}

	// 列表明确只含 NFD 序列：该序列（两种 JSON 写法等价）命中，预组合字符不行。
	cfgDecomposed := inRuleConfig("tier", `["`+nfdEacute+`"]`)
	cfgDecomposedEsc := inRuleConfig("tier", `["`+nfdEacuteEsc+`"]`)
	for _, cfgRaw := range []string{cfgDecomposed, cfgDecomposedEsc} {
		assertInMembership(t, cfgRaw, inContext("tier", nfdEacute), true)
		assertInMembership(t, cfgRaw, inContext("tier", nfdEacuteEsc), true)
		assertInMembership(t, cfgRaw, inContext("tier", `é`), false)
	}

	// 同一列表同时收纳两个不同序列是合法的（不是重复配置），且各自精确命中。
	both := inRuleConfig("tier", `["é","`+nfdEacute+`"]`)
	mustParseConfig(t, both)
	assertInMembership(t, both, inContext("tier", `é`), true)
	assertInMembership(t, both, inContext("tier", nfdEacute), true)

	// 同形异码：西里尔 а（U+0430）与拉丁 a 是不同的码点，不互相归并。
	cyrA := string(rune(0x0430))
	cfgCyrillic := inRuleConfig("tier", `["p`+cyrA+`y"]`)
	assertInMembership(t, cfgCyrillic, inContext("tier", `p`+cyrA+`y`), true)
	assertInMembership(t, cfgCyrillic, inContext("tier", `pay`), false)
}

// TestInAttributeNameExactLookup 保护属性名一侧的精确查找：条件按解码后的属性
// 名读取上下文，名称外形接近的另一个属性即使携带列表中的值，也不能让条件成立。
func TestInAttributeNameExactLookup(t *testing.T) {
	nfdEacute := "e" + string(rune(0x0301))
	// 属性 région 的两种 JSON 写法（预组合 é 直接写出或用 Unicode 转义）。
	for _, attrJSON := range []string{
		`région`,
		"r" + uesc('é') + "gion",
	} {
		cfg := inRuleConfig(attrJSON, `["cn"]`)
		// 解码后同名即同一属性：直接字符与 Unicode 转义等价。
		assertInMembership(t, cfg, inContext(`région`, `cn`), true)
		assertInMembership(t, cfg, inContext("r"+uesc('é')+"gion", `cn`), true)
		// 外形接近的属性名一律按“属性缺失”处理，即使值正好在列表中。
		for _, keyJSON := range []string{
			`Région`,                 // 大小写不同
			`région `,                // 尾随空白
			` région`,                // 前导空白
			"r" + nfdEacute + "gion", // NFD 名称
		} {
			assertInMembership(t, cfg, inContext(keyJSON, `cn`), false)
		}
		// 属性缺失，或命中的是其他属性，条件都不成立。
		assertInMembership(t, cfg, `{}`, false)
		assertInMembership(t, cfg, `{"other":"cn"}`, false)
		assertInMembership(t, cfg, `{"other":"cn","région":"us"}`, false)
	}
}

// TestInListOrderAndDuplicateCandidatesDoNotMatter 保护成员判断对列表写法的
// 稳定性：调整候选顺序或重复列入已有候选值，既不改变任何值的成员判断，也不会
// 被误报为重复配置（数组元素重复与对象字段重复是两回事）。
func TestInListOrderAndDuplicateCandidatesDoNotMatter(t *testing.T) {
	variants := []string{
		`["a","b","c"]`,
		`["c","a","b"]`,
		`["a","b","c","c","a","b"]`,
		`["b","a","c"]`,
	}
	probe := []struct {
		name   string
		value  string
		member bool
	}{
		{"a", "a", true},
		{"b", "b", true},
		{"c direct", "c", true},
		{"c escaped spelling", uesc('c'), true}, // 转义写法跨写法成员判断一致
		{"uppercase", "A", false},               // 大小写不折叠
		{"leading space", " a", false},          // 空白不裁剪
		{"absent", "d", false},
		{"empty not listed", "", false}, // 列表不含空字符串
	}
	for _, listJSON := range variants {
		cfgRaw := inRuleConfig("tier", listJSON)
		mustParseConfig(t, cfgRaw) // 重复候选不得构成配置错误
		for _, p := range probe {
			assertInMembership(t, cfgRaw, inContext("tier", p.value), p.member)
		}
	}
}

// TestInEarlierFalseRuleDecidesOnlyOnExactMembership 保护命中/未命中与规则优先级
// 的配合：前一条只含 in 条件的规则返回 false、后一条规则返回 true 时，上下文值
// 只有与列表精确相等，前面的规则才以 reason=rule、ruleId 指向前一条立即定案为
// false；任何外形相近的值都属于不匹配，求值继续采用后面的规则。两条规则都不
// 成立时仍采用 default，ruleId 为 null。
func TestInEarlierFalseRuleDecidesOnlyOnExactMembership(t *testing.T) {
	bs := string(rune(0x5C))
	litFrag := bs + bs + "u00e9" // JSON 中解码为反斜杠＋u00e9 的普通文本
	nfdEacute := "e" + string(rune(0x0301))
	// 列表 ["é","ios"]，é 用 Unicode 转义外形写出，确保读取侧先解码。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-deny","value":false,"conditions":[
			{"attribute":"tier","op":"in","value":["` + uesc('é') + `","ios"]}]},
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	const noRule = ""
	cases := []struct {
		name   string
		ctx    string
		value  bool
		reason EvalReason
		ruleID string // 空串表示 ruleId 必须为 null
	}{
		{"exact member satisfies both rules", `{"tier":"é","plan":"pro"}`, false, EvalRule, "r-deny"},
		{"escaped spelling is the same exact member", `{"tier":"` + uesc('é') + `","plan":"pro"}`, false, EvalRule, "r-deny"},
		{"other exact candidate decides false", `{"tier":"ios","plan":"pro"}`, false, EvalRule, "r-deny"},
		{"nfd lookalike is not a member and falls through", `{"tier":"` + nfdEacute + `","plan":"pro"}`, true, EvalRule, "r-allow"},
		{"nfd escaped combining mark falls through", `{"tier":"e` + uesc(rune(0x0301)) + `","plan":"pro"}`, true, EvalRule, "r-allow"},
		{"uppercase lookalike falls through", `{"tier":"IOS","plan":"pro"}`, true, EvalRule, "r-allow"},
		{"whitespace lookalike falls through", `{"tier":" ios","plan":"pro"}`, true, EvalRule, "r-allow"},
		{"literal escape text falls through", `{"tier":"` + litFrag + `","plan":"pro"}`, true, EvalRule, "r-allow"},
		{"missing attribute falls through", `{"plan":"pro"}`, true, EvalRule, "r-allow"},
		{"exact member decides false even when later rule misses", `{"tier":"ios"}`, false, EvalRule, "r-deny"},
		{"non-member and later rule misses uses default", `{"tier":"web","plan":"free"}`, true, EvalDefault, noRule},
		{"nothing present uses default", `{}`, true, EvalDefault, noRule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := eval(t, cfg, "f", tc.ctx)
			if got.Value != tc.value || got.Reason != tc.reason {
				t.Fatalf("got %+v, want value=%v reason=%s", got, tc.value, tc.reason)
			}
			switch {
			case tc.ruleID == noRule && got.RuleID != nil:
				t.Fatalf("ruleId must be null, got %q", *got.RuleID)
			case tc.ruleID != noRule && (got.RuleID == nil || *got.RuleID != tc.ruleID):
				gotID := "<nil>"
				if got.RuleID != nil {
					gotID = *got.RuleID
				}
				t.Fatalf("ruleId=%q, want %q", gotID, tc.ruleID)
			}
		})
	}
}

// TestInEmptyStringCandidateVsMissingAttribute 保护空字符串候选：列表显式包含
// 空字符串时，属性明确给出空字符串才算成员；属性缺失（即使其他属性为空）条件
// 不成立。在前 false/后 true 的规则次序中，两种情形也要分别定案与放行。
func TestInEmptyStringCandidateVsMissingAttribute(t *testing.T) {
	cfg := inRuleConfig("tier", `["","x"]`)
	assertInMembership(t, cfg, `{"tier":""}`, true)
	assertInMembership(t, cfg, `{}`, false)
	assertInMembership(t, cfg, `{"other":""}`, false)

	cfgTwo := `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-empty","value":false,"conditions":[
			{"attribute":"tier","op":"in","value":[""]}]},
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	emptyID, allowID := "r-empty", "r-allow"
	// 显式空字符串：前一条精确命中并定案 false，即使后一条同时成立。
	got := eval(t, cfgTwo, "f", `{"tier":"","plan":"pro"}`)
	if !got.Equals(EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &emptyID}) {
		t.Fatalf("explicit empty string must decide false: got %+v", got)
	}
	// 属性缺失：前一条不成立，继续采用后一条。
	got = eval(t, cfgTwo, "f", `{"plan":"pro"}`)
	if !got.Equals(EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &allowID}) {
		t.Fatalf("missing attribute must fall through: got %+v", got)
	}
	// 显式空字符串但后一条不成立：仍是前一条精确命中并定案 false。
	got = eval(t, cfgTwo, "f", `{"tier":""}`)
	if !got.Equals(EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &emptyID}) {
		t.Fatalf("empty string alone must decide false: got %+v", got)
	}
}

// TestInInvalidListPointsAtPosition 保护配置接受范围：空列表与含非字符串项的
// 列表仍是配置错误，错误要指出具体条件值或元素下标。
func TestInInvalidListPointsAtPosition(t *testing.T) {
	cases := []struct {
		name     string
		listJSON string
		want     string
	}{
		{"empty array", `[]`, "in-list must contain at least one item"},
		{"null only", `[null]`, `conditions[0].value[0]: must be a string`},
		{"bool first item", `[true]`, `conditions[0].value[0]: must be a string`},
		{"number first item", `[1]`, `conditions[0].value[0]: must be a string`},
		{"number after valid item", `["ok",1]`, `conditions[0].value[1]: must be a string`},
		{"nested array item", `["ok",["nested"]]`, `conditions[0].value[1]: must be a string`},
		{"object item", `["ok",{}]`, `conditions[0].value[1]: must be a string`},
		{"not an array", `"x"`, `conditions[0].value: must be an array when op is "in"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(inRuleConfig("tier", tc.listJSON)))
			if err == nil {
				t.Fatalf("list %s: expected config error", tc.listJSON)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestConditionsAreAND(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"region","op":"in","value":["cn","us"]}]}]}]}`
	if got := eval(t, cfg, "f", `{"plan":"pro","region":"cn"}`); got.Reason != EvalRule {
		t.Fatalf("both hold: got %+v", got)
	}
	if got := eval(t, cfg, "f", `{"plan":"pro"}`); got.Reason != EvalDefault {
		t.Fatalf("one missing: got %+v", got)
	}
	if got := eval(t, cfg, "f", `{"plan":"pro","region":"eu"}`); got.Reason != EvalDefault {
		t.Fatalf("one mismatch: got %+v", got)
	}
	// 其他属性不参与判断。
	if got := eval(t, cfg, "f", `{"plan":"pro","region":"us","extra":"whatever"}`); got.Reason != EvalRule {
		t.Fatalf("extra attrs: got %+v", got)
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"tier","op":"in","value":["a","b"]}]}]}]}`
	c := mustParseConfig(t, cfg)
	ctx := mustParseContext(t, `{"tier":"b","x":"1","y":"2","z":"3"}`)
	flag := c.Find("f")
	var first EvalResult
	for i := 0; i < 100; i++ {
		got := flag.Evaluate(ctx)
		if i == 0 {
			first = got
			continue
		}
		if !got.Equals(first) {
			t.Fatalf("iteration %d: %+v != %+v", i, got, first)
		}
	}
}

func TestMarshalResultShape(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-1","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`
	got := eval(t, cfg, "f", `{"a":"x"}`)
	out, err := MarshalResult(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"f","value":true,"reason":"rule","ruleId":"r-1"}`
	if string(out) != want {
		t.Fatalf("marshal rule: got %s want %s", out, want)
	}

	disabled := eval(t, `{"flags":[{"key":"f","enabled":false,"default":true,"rules":[]}]}`, "f", `{}`)
	out, _ = MarshalResult(disabled)
	want = `{"key":"f","value":false,"reason":"disabled","ruleId":null}`
	if string(out) != want {
		t.Fatalf("marshal disabled: got %s want %s", out, want)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 错误信息需包含的定位片段
	}{
		{"not object", `[1,2]`, "top level"},
		{"flags missing", `{}`, "config.flags"},
		{"flags not array", `{"flags":{}}`, "config.flags"},
		{"flags null", `{"flags":null}`, "config.flags"},
		{"flag not object", `{"flags":[1]}`, "flags[0]"},
		{"key missing", `{"flags":[{"enabled":true,"default":false,"rules":[]}]}`, `flags[0].key`},
		{"key empty", `{"flags":[{"key":"","enabled":true,"default":false,"rules":[]}]}`, `flags[0].key`},
		{"key wrong type", `{"flags":[{"key":1,"enabled":true,"default":false,"rules":[]}]}`, `flags[0].key`},
		{"enabled missing must not default to false", `{"flags":[{"key":"f","default":false,"rules":[]}]}`, `flags[0].enabled`},
		{"enabled wrong type", `{"flags":[{"key":"f","enabled":"yes","default":false,"rules":[]}]}`, `flags[0].enabled`},
		{"enabled numeric", `{"flags":[{"key":"f","enabled":1,"default":false,"rules":[]}]}`, `flags[0].enabled`},
		{"default missing", `{"flags":[{"key":"f","enabled":true,"rules":[]}]}`, `flags[0].default`},
		{"rules missing", `{"flags":[{"key":"f","enabled":true,"default":false}]}`, `flags[0].rules`},
		{"rules not array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":1}]}`, `flags[0].rules`},
		{"rule not object", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[1]}]}`, `rules[0]`},
		{"rule id missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].id`},
		{"rule id empty", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].id`},
		{"rule value missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].value`},
		{"rule value wrong type", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":"true","conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].value`},
		{"conditions missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true}]}]}`, `rules[0].conditions`},
		{"conditions empty", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[]}]}]}`, `rules[0].conditions`},
		{"condition not object", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[1]}]}]}`, `conditions[0]`},
		{"attribute missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"op":"eq","value":"x"}]}]}]}`, `conditions[0].attribute`},
		{"attribute empty", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"","op":"eq","value":"x"}]}]}]}`, `conditions[0].attribute`},
		{"op missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","value":"x"}]}]}]}`, `conditions[0].op`},
		{"op unknown", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"neq","value":"x"}]}]}]}`, `conditions[0].op`},
		{"value missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq"}]}]}]}`, `conditions[0].value`},
		{"eq value non-string", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":1}]}]}]}`, `conditions[0].value`},
		{"eq value bool", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":true}]}]}]}`, `conditions[0].value`},
		{"in value not array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":"x"}]}]}]}`, `conditions[0].value`},
		{"in value empty array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":[]}]}]}]}`, `conditions[0].value`},
		{"in item non-string", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":["x",1]}]}]}]}`, `conditions[0].value[1]`},
		{"duplicate flag key", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[]},
			{"key":"f","enabled":true,"default":false,"rules":[]}]}`, `flags[1].key`},
		{"duplicate rule id", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},
			{"id":"r","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`, `rules[1].id`},
		{"invalid json", `{"flags":[`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestRuleIDUniqueAcrossFlagsIsAllowed(t *testing.T) {
	cfg := `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]},
		{"key":"g","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`
	c := mustParseConfig(t, cfg)
	if len(c.Flags) != 2 {
		t.Fatalf("got %d flags", len(c.Flags))
	}
}

func TestUnselectedAndDisabledFlagsAreValidated(t *testing.T) {
	// 请求第一个开关，但第二个开关有错误：仍须整体校验失败。
	raw := `{"flags":[
		{"key":"good","enabled":true,"default":false,"rules":[]},
		{"key":"bad","enabled":true,"rules":[]}]}`
	if _, err := ParseConfig([]byte(raw)); err == nil {
		t.Fatal("expected validation of unselected flag to fail")
	}
	// 关闭的开关规则非法同样要报错。
	raw = `{"flags":[
		{"key":"off","enabled":false,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[]}]}]}`
	if _, err := ParseConfig([]byte(raw)); err == nil {
		t.Fatal("expected validation of disabled flag rules to fail")
	}
}

func TestContextValidation(t *testing.T) {
	if _, err := ParseContext([]byte(`{"a":"x"}`)); err != nil {
		t.Fatalf("valid context: %v", err)
	}
	if _, err := ParseContext([]byte(`{}`)); err != nil {
		t.Fatalf("empty context: %v", err)
	}
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`[]`, "top level"},
		{`{"a":1}`, "context.a"},
		{`{"a":true}`, "context.a"},
		{`{"a":null}`, "context.a"},
		{`{"a":{}}`, "context.a"},
		{`{"a":[]}`, "context.a"},
		{`{bad`, "invalid JSON"},
	} {
		if _, err := ParseContext([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseContext(%s): err=%v want substring %q", tc.raw, err, tc.want)
		}
	}
}

func TestContextValueTypeErrorIsFirstInSourceOrder(t *testing.T) {
	// 多个非字符串属性：报告原文中最先出现的一个，且每次运行一致，
	// 不随 Go map 遍历顺序漂移。
	raw := `{"zeta":false,"plan":"pro","alpha":7}`
	var first string
	for i := 0; i < 20; i++ {
		_, err := ParseContext([]byte(raw))
		if err == nil {
			t.Fatalf("iter %d: expected error", i)
		}
		if i == 0 {
			first = err.Error()
			if !strings.Contains(first, "context.zeta") ||
				!strings.Contains(first, "value must be a string") {
				t.Fatalf("first error = %q, want context.zeta string-type error", first)
			}
			continue
		}
		if err.Error() != first {
			t.Fatalf("iter %d: %q != %q", i, err.Error(), first)
		}
	}

	// 把 alpha 放到 zeta 前面：应改为报告 context.alpha，前面的合法属性不影响选择。
	_, err := ParseContext([]byte(`{"alpha":7,"plan":"pro","zeta":false}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("reordered: err=%v want context.alpha", err)
	}

	// 用户修正第一处错误后再次求值，应看到剩余属性中最先出现的类型错误。
	_, err = ParseContext([]byte(`{"zeta":"z","plan":"pro","alpha":7}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("after fixing zeta: err=%v want context.alpha", err)
	}

	// 全部属性值合法后才返回求值结果；看起来像数字的字符串仍是合法字符串值。
	ctx, err := ParseContext([]byte(`{"zeta":"false","plan":"pro","alpha":"7"}`))
	if err != nil {
		t.Fatalf("all string values must parse: %v", err)
	}
	if v, ok := ctx.Get("alpha"); !ok || v != "7" {
		t.Fatalf("numeric-looking string should stay a string: %q %v", v, ok)
	}
}

func TestContextNonStringValueShapes(t *testing.T) {
	// 数字、布尔、null、数组、对象都不能作为顶层属性值；对象/数组值的错误
	// 指向所属的顶层属性，不指向内部成员。
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"number", `{"a":"ok","b":1}`, "context.b"},
		{"bool", `{"a":"ok","b":false}`, "context.b"},
		{"null", `{"a":"ok","b":null}`, "context.b"},
		{"array", `{"a":"ok","b":["x"]}`, "context.b"},
		{"object", `{"a":"ok","b":{"inner":1}}`, "context.b"},
		{"object inner is string too", `{"obj":{"inner":"x"},"later":1}`, "context.obj"},
		{"array with strings only", `{"arr":["x","y"]}`, "context.arr"},
		{"first of mixed kinds", `{"z":{"x":1},"a":[2],"m":true}`, "context.z"},
		{"nested bad member still points to top key", `{"a":"x","obj":{"inner":1},"b":2}`, "context.obj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseContext([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "value must be a string") {
				t.Errorf("ParseContext(%s): err=%v want location %q", tc.raw, err, tc.want)
			}
		})
	}
}

func TestContextTypeErrorKeyDecoding(t *testing.T) {
	// 属性名采用解码后的文字定位：合法 Unicode 转义与直接字符同义。
	escA := `"\u` + `0061lpha"` // 解码后为 "alpha"
	_, err := ParseContext([]byte(`{` + escA + `:7}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("escaped key: err=%v want context.alpha", err)
	}
	// 转义形式与直接字符匹配同一属性（Get 使用解码后的名字）。
	escP := `"\u` + `0070lan"` // 解码后为 "plan"
	ctx := mustParseContext(t, `{`+escP+`:"pro"}`)
	if v, ok := ctx.Get("plan"); !ok || v != "pro" {
		t.Fatalf("escaped key lookup: %q %v", v, ok)
	}
	// 大小写与空白按原样保留，不改变属性匹配与定位；含空格的名字走方括号定位。
	_, err = ParseContext([]byte(`{"Alpha ":7}`))
	if err == nil || !strings.Contains(err.Error(), `context["Alpha "]`) {
		t.Fatalf("case/space key: err=%v", err)
	}
	// 配对的代理项转义解码后定位到同一属性名；非 ASCII 名字用方括号 JSON 字符串。
	emojiKey := `"\u` + `D83D\u` + `DE00"` // 解码后为 "😀"
	_, err = ParseContext([]byte(`{"a":"x",` + emojiKey + `:2}`))
	if err == nil || !strings.Contains(err.Error(), `context["😀"]`) {
		t.Fatalf("surrogate pair key: err=%v", err)
	}
	// 直接写出的同一 emoji 名字必须得到相同定位（解码后文字一致）。
	_, err = ParseContext([]byte(`{"a":"x","😀":2}`))
	if err == nil || !strings.Contains(err.Error(), `context["😀"]`) {
		t.Fatalf("literal emoji key: err=%v", err)
	}
}

func TestFieldPathFormatting(t *testing.T) {
	// 字段位置统一规则：简单字段名（ASCII 字母/下划线开头，仅含 ASCII 字母数字
	// 下划线）继续用点号；其他名字在父位置后用方括号包住一个 JSON 字符串。
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"dotted name is bracketed", `{"user.name":7}`, `context["user.name"]`},
		{"empty name is bracketed", `{"":7}`, `context[""]`},
		{"newline in name is json escaped", "{\"a\\nb\":7}", `context["a\nb"]`},
		{"tab in name is json escaped", "{\"a\\tb\":7}", `context["a\tb"]`},
		{"quote in name is json escaped", `{"a\"b":7}`, `context["a\"b"]`},
		{"backslash in name is json escaped", `{"a\\b":7}`, `context["a\\b"]`},
		{"brackets are not array indices", `{"names[0]":7}`, `context["names[0]"]`},
		{"leading digit is bracketed", `{"0ab":7}`, `context["0ab"]`},
		{"dash is bracketed", `{"a-b":7}`, `context["a-b"]`},
		{"simple underscore stays dotted", `{"_":7}`, `context._`},
		{"simple alnum stays dotted", `{"plan_2":7}`, `context.plan_2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseContext([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected type error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "value must be a string") {
				t.Fatalf("err=%q want location %q", err.Error(), tc.want)
			}
			// 控制字符必须转义显示：定位文字本身不能被拆成多行。
			if strings.Contains(strings.SplitN(err.Error(), ": ", 2)[0], "\n") {
				t.Fatalf("location text must stay on one line: %q", err.Error())
			}
		})
	}

	// 合法 Unicode 转义先解码再定位：. 与直接写出的点号得到同一位置。
	_, err := ParseContext([]byte(`{"user.name":7}`))
	if err == nil || !strings.Contains(err.Error(), `context["user.name"]`) {
		t.Fatalf("escaped dot key: err=%v", err)
	}

	// 重复字段同样按统一规则定位；名字渲染为 JSON 字符串。
	_, err = ParseContext([]byte(`{"user.name":1,"user.name":2}`))
	if err == nil || !strings.Contains(err.Error(), `context["user.name"]: duplicate field "user.name"`) {
		t.Fatalf("duplicate dotted key: err=%v", err)
	}
	// 转义形式与直接字符是同名字段：重复检查与定位都按解码后的文字。
	_, err = ParseContext([]byte(`{"x.y":1,"x.y":2}`))
	if err == nil || !strings.Contains(err.Error(), `context["x.y"]: duplicate field "x.y"`) {
		t.Fatalf("duplicate escaped key: err=%v", err)
	}
	_, err = ParseContext([]byte(`{"":1,"":2}`))
	if err == nil || !strings.Contains(err.Error(), `context[""]: duplicate field ""`) {
		t.Fatalf("duplicate empty key: err=%v", err)
	}

	// 配置附加字段名为 meta.info，其中嵌套对象的 owner 重复：
	// 应显示 config.flags[0]["meta.info"].owner。
	rawConfig := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"meta.info":{"owner":"a","owner":"b"}}]}`
	_, err = ParseConfig([]byte(rawConfig))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0]["meta.info"].owner`) ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("nested duplicate under dotted extra field: err=%v", err)
	}
	// 附加字段数组元素里的点号字段名同样加方括号。
	rawConfig = `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"tags":[{"v":1},{"a.b":1,"a.b":2}]}]}`
	_, err = ParseConfig([]byte(rawConfig))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].tags[1]["a.b"]`) ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate in extra array element: err=%v", err)
	}
	// 顶层附加的点号字段名。
	_, err = ParseConfig([]byte(`{"flags":[],"x.y":1,"x.y":2}`))
	if err == nil || !strings.Contains(err.Error(), `config["x.y"]: duplicate field "x.y"`) {
		t.Fatalf("duplicate dotted top-level extra field: err=%v", err)
	}

	// 点号字段名的值含不完整 Unicode 转义：错误位置用方括号包住字段名。
	_, err = ParseContext([]byte(`{"user.name":"\uD800"}`))
	if err == nil || !strings.Contains(err.Error(), `context["user.name"]`) ||
		!strings.Contains(err.Error(), "does not form a complete character") {
		t.Fatalf("bad escape under dotted key: err=%v", err)
	}
}

func TestBadEscapeInFieldNameKeepsRawEscapeAndObjectPath(t *testing.T) {
	// 字段名自身含不完整 Unicode 转义时：仍报告它不能组成完整字符，保留可辨认的
	// 原始转义内容（\uD800 不被替换成别的字符），位置准确指向所属对象。
	_, err := ParseContext([]byte(`{"bad\uD800name":1}`))
	if err == nil {
		t.Fatal("expected invalid Unicode escape error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does not form a complete character") ||
		!strings.Contains(msg, `\uD800`) {
		t.Fatalf("context bad field name: err=%v", msg)
	}
	// 所属对象是根对象；非法名字不能被渲染进定位（不能出现替换字符定位）。
	if !strings.HasPrefix(strings.SplitN(msg, ": ", 2)[0], "context") ||
		strings.Contains(msg, "context[") || strings.Contains(msg, "context.") {
		t.Fatalf("location must point at the root object only: %q", msg)
	}

	// 配置里开关附加字段名含坏转义：所属对象位置为 config.flags[0]。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"met\uD800a":1}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0]`) ||
		!strings.Contains(err.Error(), `\uD800`) ||
		!strings.Contains(err.Error(), "does not form a complete character") {
		t.Fatalf("config bad extra field name: err=%v", err)
	}

	// 嵌套附加对象内的字段名含坏转义：位置指向该嵌套对象 config.flags[0].meta。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"meta":{"o\uD800wner":1}}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].meta`) ||
		!strings.Contains(err.Error(), `\uD800`) {
		t.Fatalf("bad field name in nested object: err=%v", err)
	}

	// 数组元素对象内的字段名含坏转义：位置指向该元素对象。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"tags":[{"k\uDC00":1}]}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].tags[0]`) ||
		!strings.Contains(err.Error(), `\uDC00`) {
		t.Fatalf("bad field name in array element: err=%v", err)
	}
}

func TestContextValidationPrecedence(t *testing.T) {
	// 非法 UTF-8 先于属性类型错误报告。
	if _, err := ParseContext(append([]byte(`{"a":1}`), 0xff)); err == nil ||
		!strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("utf8 precedence: %v", err)
	}
	// 不能组成完整字符的 Unicode 转义不能被较早出现的属性类型错误遮住。
	// 属性类型错误在 zeta，转义错误在其后的 plan 值中：仍报转义错误。
	if _, err := ParseContext([]byte(`{"zeta":false,"plan":"\uD800"}`)); err == nil ||
		!strings.Contains(err.Error(), "does not form a complete character") ||
		!strings.Contains(err.Error(), "context.plan") {
		t.Fatalf("unicode escape precedence: %v", err)
	}
	// 重复字段先于属性类型错误。
	if _, err := ParseContext([]byte(`{"b":1,"b":2}`)); err == nil ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate field precedence: %v", err)
	}
	// JSON 语法错误先于属性类型错误。
	if _, err := ParseContext([]byte(`{"a":1,`)); err == nil ||
		!strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("syntax precedence: %v", err)
	}
}

func TestUnusedContextAttributesValidatedEvenWhenFlagDisabled(t *testing.T) {
	// 开关关闭且规则不引用任何属性：未被规则使用的属性仍必须全部校验。
	cfg := `{"flags":[{"key":"f","enabled":false,"default":true,"rules":[]}]}`
	flag := mustParseConfig(t, cfg).Find("f")
	if _, err := ParseContext([]byte(`{"anything":42}`)); err == nil ||
		!strings.Contains(err.Error(), "context.anything") {
		t.Fatalf("unused attr must still be validated even for a disabled flag: %v", err)
	}
	ctx := mustParseContext(t, `{"anything":"42"}`)
	if got := flag.Evaluate(ctx); got.Reason != EvalDisabled {
		t.Fatalf("valid unused attrs with disabled flag: %+v", got)
	}
}

func TestContextOutOfRangeNumbersAreTypeErrors(t *testing.T) {
	// 1 后接 400 个 0：远超 float64 可表示范围的合法 JSON 整数。它与 1e400、
	// -1e400 一样是语法合法的 JSON 数字——用户把应为字符串的属性写成数字时，
	// 必须报该属性的类型错误，而不是被解码阶段当成 JSON 语法错误。
	hugeInt := "1" + strings.Repeat("0", 400)
	cases := []struct {
		name         string
		raw          string
		wantAttr     string
		topLevelOnly bool // 对象/数组值的错误不得指向内部成员
	}{
		{"exponent overflow", `{"plan":1e400}`, "context.plan", false},
		{"negative exponent overflow", `{"plan":-1e400}`, "context.plan", false},
		{"400-digit integer", `{"plan":` + hugeInt + `}`, "context.plan", false},
		{"object holding big number", `{"plan":{"inner":1e400}}`, "context.plan", true},
		{"array holding big numbers", `{"plan":[1e400,-1e400]}`, "context.plan", true},
		{"nested object then later scalar", `{"a":"x","obj":{"inner":` + hugeInt + `},"b":2}`, "context.obj", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseContext([]byte(tc.raw))
			if err == nil {
				t.Fatalf("ParseContext(%s): expected error, got nil", tc.raw)
			}
			msg := err.Error()
			if strings.Contains(msg, "invalid JSON") {
				t.Fatalf("a legal big number must not be reported as a JSON syntax error: %q", msg)
			}
			if !strings.Contains(msg, tc.wantAttr) || !strings.Contains(msg, "value must be a string") {
				t.Fatalf("err=%q, want %s string-type error", msg, tc.wantAttr)
			}
			if tc.topLevelOnly && strings.Contains(msg, "inner") {
				t.Fatalf("object/array value error must stay on the top-level attribute: %q", msg)
			}
		})
	}

	// 规则不引用该属性：上下文校验独立于配置与求值，同样拒绝并按属性类型错误报告。
	_, err := ParseContext([]byte(`{"unreferenced":1e400}`))
	if err == nil || !strings.Contains(err.Error(), "context.unreferenced") ||
		!strings.Contains(err.Error(), "value must be a string") ||
		strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("unreferenced big-number attr: err=%v", err)
	}
}

func TestContextBigNumberTypeErrorIsFirstInSourceOrder(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	// 前面的布尔值错误不能被后面的大数字盖住；且多次运行报告同一处，
	// 不随 map 遍历顺序在 zeta 与大数字属性之间漂移。
	raw := `{"zeta":false,"plan":"pro","alpha":1e400,"omega":` + hugeInt + `}`
	var first string
	for i := 0; i < 20; i++ {
		_, err := ParseContext([]byte(raw))
		if err == nil {
			t.Fatalf("iter %d: expected error", i)
		}
		if i == 0 {
			first = err.Error()
			if !strings.Contains(first, "context.zeta") ||
				!strings.Contains(first, "value must be a string") {
				t.Fatalf("first error = %q, want context.zeta type error", first)
			}
			continue
		}
		if err.Error() != first {
			t.Fatalf("iter %d: %q != %q", i, err.Error(), first)
		}
	}

	// 调整属性顺序、让大数字排到最前：应报告新的第一处错误。
	_, err := ParseContext([]byte(`{"alpha":1e400,"zeta":false}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("reordered: err=%v want context.alpha", err)
	}
	// 把第一处修正为合法字符串后：报告剩余属性中最先出现的类型错误。
	_, err = ParseContext([]byte(`{"zeta":"z","plan":"pro","alpha":1e400}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("after fixing first attr: err=%v want context.alpha", err)
	}
	// 两处都是大数字时同样取原文第一处。
	_, err = ParseContext([]byte(`{"a":1e400,"b":-1e400}`))
	if err == nil || !strings.Contains(err.Error(), "context.a") ||
		strings.Contains(err.Error(), "context.b") {
		t.Fatalf("two big numbers: err=%v want context.a only", err)
	}
}

func TestContextBigNumberValidationPrecedence(t *testing.T) {
	// 完整输入的检查优先于属性类型检查：大数字属性之后重复声明同名字段时，
	// 必须报重复字段及其所属位置，而不是较早属性的类型错误。
	_, err := ParseContext([]byte(`{"plan":1e400,"plan":"x"}`))
	if err == nil {
		t.Fatal("expected duplicate field error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "duplicate field") || !strings.Contains(msg, "context.plan") {
		t.Fatalf("err=%q, want duplicate field at context.plan", msg)
	}
	if strings.Contains(msg, "value must be a string") {
		t.Fatalf("duplicate field must win over the earlier type error: %q", msg)
	}
	// 大数字属性之后、后面对象内部的重复字段同理，位置指向内部成员。
	_, err = ParseContext([]byte(`{"a":1e400,"b":{"x":1,"x":2}}`))
	if err == nil || !strings.Contains(err.Error(), "context.b.x") ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("nested duplicate must win with its own location: err=%v", err)
	}
	// 数字写法本身无效（1e+ 缺少指数数字）：仍是 JSON 语法错误，
	// 与合法大数字的属性类型错误明确区分。
	_, err = ParseContext([]byte(`{"plan":1e+}`))
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("malformed number must be a JSON syntax error: err=%v", err)
	}
}

func TestQuotedBigNumberStaysString(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	// 同样的数字文本加上引号后就是普通字符串，按原文参与 eq 匹配，
	// 不能被数值化、改写或继续拒绝。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-exp","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"1e400"}]},
		{"id":"r-neg","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"-1e400"}]},
		{"id":"r-int","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"` + hugeInt + `"}]}]}]}`
	for _, tc := range []struct {
		ctxRaw string
		ruleID string
		reason EvalReason
	}{
		{`{"plan":"1e400"}`, "r-exp", EvalRule},
		{`{"plan":"-1e400"}`, "r-neg", EvalRule},
		{`{"plan":"` + hugeInt + `"}`, "r-int", EvalRule},
		{`{"plan":"1e401"}`, "", EvalDefault}, // 不做数值换算："1e401" != "1e400"
	} {
		got := eval(t, cfg, "f", tc.ctxRaw)
		if got.Reason != tc.reason ||
			(tc.reason == EvalRule && (got.RuleID == nil || *got.RuleID != tc.ruleID)) {
			t.Fatalf("ctx=%s: got %+v, want reason=%s rule=%s", tc.ctxRaw, got, tc.reason, tc.ruleID)
		}
	}

	// 1e400 与 1 后接 400 个 0 在数值上相等，但字符串写法不同：不得互相命中。
	cfgInt := `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[
		{"id":"r-int","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"` + hugeInt + `"}]}]}]}`
	if got := eval(t, cfgInt, "g", `{"plan":"1e400"}`); got.Reason != EvalDefault {
		t.Fatalf("numerically equal spellings must not match as strings: %+v", got)
	}
	if got := eval(t, cfgInt, "g", `{"plan":"`+hugeInt+`"}`); got.Reason != EvalRule {
		t.Fatalf("identical spelling must match verbatim: %+v", got)
	}

	// ParseContext 保留引号内的原文，不做任何转换。
	ctx := mustParseContext(t, `{"plan":"1e400"}`)
	if v, ok := ctx.Get("plan"); !ok || v != "1e400" {
		t.Fatalf("quoted big number was rewritten: %q %v", v, ok)
	}
}

func TestBigNumberRejectedEvenForDisabledFlag(t *testing.T) {
	// 请求的是关闭的开关：文件仍须先完整校验，非法上下文不能借 disabled 路径通过。
	cfg := `{"flags":[{"key":"off","enabled":false,"default":true,"rules":[]}]}`
	flag := mustParseConfig(t, cfg).Find("off")
	if _, err := ParseContext([]byte(`{"plan":1e400}`)); err == nil ||
		!strings.Contains(err.Error(), "context.plan") ||
		strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("disabled flag must not admit big-number input: err=%v", err)
	}
	// 修正为合法字符串后，关闭开关仍固定 false（reason=disabled）。
	ctx := mustParseContext(t, `{"plan":"1e400"}`)
	if got := flag.Evaluate(ctx); got.Reason != EvalDisabled || got.Value {
		t.Fatalf("disabled flag with valid quoted string: %+v", got)
	}
}

func TestInvalidUTF8(t *testing.T) {
	bad := []byte("{\"flags\":[]}\xff")
	if _, err := ParseConfig(bad); err == nil {
		t.Error("expected invalid UTF-8 config error")
	}
	if _, err := ParseContext(append([]byte("{\"a\":\"x\"}"), 0xfe)); err == nil {
		t.Error("expected invalid UTF-8 context error")
	}
}

func TestDuplicateFieldsRejected(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 错误信息需包含的定位片段
	}{
		{"flag enabled", `{"flags":[{"key":"f","enabled":false,"enabled":true,"default":false,"rules":[]}]}`,
			`config.flags[0].enabled`},
		{"second flag", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[]},
			{"key":"g","enabled":true,"enabled":false,"default":false,"rules":[]}]}`,
			`config.flags[1].enabled`},
		{"top level", `{"flags":[],"flags":[]}`, `config.flags`},
		{"rule value", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"value":false,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`,
			`config.flags[0].rules[0].value`},
		{"condition op", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","op":"in","value":"x"}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].op`},
		{"nested extra field", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"owner":"a","owner":"b"}}]}`,
			`config.flags[0].meta.owner`},
		{"deeply nested extra field", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"team":{"name":"x","name":"y"}}}]}`,
			`config.flags[0].meta.team.name`},
		{"extra field nested in array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"tags":[{"v":1,"v":2}]}]}`,
			`config.flags[0].tags[0].v`},
		{"identical values still duplicate", `{"flags":[{"key":"f","enabled":true,"default":false,"default":false,"rules":[]}]}`,
			`config.flags[0].default`},
		{"duplicate in extra top-level field", `{"flags":[],"x":1,"x":2}`, `config.x`},
		{"disabled flag not exempt", `{"flags":[{"key":"f","enabled":false,"enabled":false,"default":false,"rules":[]}]}`,
			`config.flags[0].enabled`},
		{"unselected flag not exempt", `{"flags":[
			{"key":"good","enabled":true,"default":false,"rules":[]},
			{"key":"bad","enabled":true,"default":false,"default":true,"rules":[]}]}`,
			`config.flags[1].default`},
		{"first duplicate in file order wins", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[]},
			{"key":"g","enabled":true,"enabled":true,"default":false,"default":false,"rules":[]}]}`,
			`config.flags[1].enabled`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected duplicate field error, got nil\nconfig: %s", tc.raw)
			}
			if !strings.Contains(err.Error(), "duplicate field") {
				t.Fatalf("error = %q, want %q wording", err.Error(), "duplicate field")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want location %q", err.Error(), tc.want)
			}
		})
	}
}

func TestDuplicateFieldEscapedName(t *testing.T) {
	// 字段名按解码后的字符串比较："enabled" 与 "énabled" 是同名。
	raw := `{"flags":[{"key":"f","enabled":false,"\u0065nabled":true,"default":false,"rules":[]}]}`
	_, err := ParseConfig([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].enabled`) {
		t.Fatalf("escaped duplicate: err=%v", err)
	}
	if !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("escaped duplicate wording: err=%v", err)
	}
}

func TestDuplicateFieldsInContext(t *testing.T) {
	_, err := ParseContext([]byte(`{"plan":"free","plan":"pro"}`))
	if err == nil || !strings.Contains(err.Error(), `context.plan`) ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("context duplicate: err=%v", err)
	}
	// 转义后同名也算重复。
	_, err = ParseContext([]byte(`{"plan":"free","\u0070lan":"pro"}`))
	if err == nil || !strings.Contains(err.Error(), `context.plan`) {
		t.Fatalf("context escaped duplicate: err=%v", err)
	}
}

func TestDuplicateFieldLookalikesAllowed(t *testing.T) {
	// 大小写不同的字段名是不同字段。
	cfg := mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"Enabled":false,"default":false,"rules":[]}]}`)
	if !cfg.Flags[0].Enabled {
		t.Fatal("lowercase enabled should win as the real field")
	}
	// 名字中的空白不裁剪，"enabled " 与 "enabled" 不同名。
	mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"enabled ":false,"default":false,"rules":[]}]}`)
	// 同名字段分别出现在不同开关、不同规则里是合法的。
	mustParseConfig(t, `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]},
		{"key":"g","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`)
	// 字符串数组中的重复元素不属于重复字段。
	mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":["x","x"]}]}]}]}`)
	// 上下文中不同名的字段互不影响。
	mustParseContext(t, `{"plan":"pro","Plan":"free"}`)
}

func TestUniquenessErrorsDistinctFromDuplicateFields(t *testing.T) {
	// 两个开关使用相同 key 仍按唯一性规则报告，措辞与重复字段不同。
	_, err := ParseConfig([]byte(`{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[]},
		{"key":"f","enabled":true,"default":false,"rules":[]}]}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate flag key") {
		t.Fatalf("flag key uniqueness: err=%v", err)
	}
	if strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("uniqueness error must not read as duplicate field: %v", err)
	}
	// 同一开关内两条规则使用相同 id 同理。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},
		{"id":"r","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate rule id") {
		t.Fatalf("rule id uniqueness: err=%v", err)
	}
	if strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("uniqueness error must not read as duplicate field: %v", err)
	}
}

func TestInvalidUnicodeEscapesRejected(t *testing.T) {
	// encoding/json 会把孤立代理项转义静默替换为 U+FFFD；这些输入必须明确失败，
	// 且错误指出文件（config/context）、字段或数组位置，并说明转义不能组成完整字符。
	const incomplete = "does not form a complete character"
	configCases := []struct {
		name string
		raw  string
		want string
	}{
		{"eq value lone high surrogate", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].value`},
		{"eq value lone low surrogate", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uDC00"}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].value`},
		{"reversed pair", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uDC00\uD800"}]}]}]}`,
			`conditions[0].value`},
		{"high then non-surrogate escape", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800A"}]}]}]}`,
			`conditions[0].value`},
		{"high then high", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800\uD800"}]}]}]}`,
			`conditions[0].value`}, {"high at end of string", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x\uD800"}]}]}]}`,
			`conditions[0].value`},
		{"in-list element", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":["ok","\uD800"]}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].value[1]`},
		{"flag key string", `{"flags":[{"key":"\uD800","enabled":true,"default":false,"rules":[]}]}`,
			`config.flags[0].key`},
		{"extra field value", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"note":"\uDFFF"}}]}`,
			`config.flags[0].meta.note`},
		{"disabled flag not exempt", `{"flags":[{"key":"f","enabled":false,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`conditions[0].value`},
		{"unselected flag not exempt", `{"flags":[
			{"key":"good","enabled":true,"default":false,"rules":[]},
			{"key":"bad","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`config.flags[1].rules[0].conditions[0].value`},
		{"first error in file order wins", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]},
			{"key":"g","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uDC00"}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].value`},
	}
	for _, tc := range configCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected invalid Unicode escape error, got nil\nconfig: %s", tc.raw)
			}
			if !strings.Contains(err.Error(), incomplete) {
				t.Fatalf("error = %q, want wording %q", err.Error(), incomplete)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want location %q", err.Error(), tc.want)
			}
		})
	}

	// 字段名本身非法：指出所属对象，并保留可辨认的原始转义内容。
	_, err := ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"met\uD800a":1}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0]`) ||
		!strings.Contains(err.Error(), `\uD800`) || !strings.Contains(err.Error(), incomplete) {
		t.Fatalf("bad field name in config: err=%v", err)
	}

	// 上下文同样检查值与字段名。
	_, err = ParseContext([]byte(`{"plan":"\uD800"}`))
	if err == nil || !strings.Contains(err.Error(), `context.plan`) ||
		!strings.Contains(err.Error(), incomplete) {
		t.Fatalf("context value: err=%v", err)
	}
	_, err = ParseContext([]byte(`{"pl\uDC00an":"pro"}`))
	if err == nil || !strings.Contains(err.Error(), `context`) ||
		!strings.Contains(err.Error(), `\uDC00`) || !strings.Contains(err.Error(), incomplete) {
		t.Fatalf("context field name: err=%v", err)
	}
}

func TestInvalidUnicodeEscapeNotEquatedWithReplacementChar(t *testing.T) {
	// 规则要求属性值等于真正的 "�"：上下文提供 "\uD800" 不能在替换后命中规则，
	// 整个求值必须失败。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"�"}]}]}]}`
	if _, err := ParseContext([]byte(`{"plan":"\uD800"}`)); err == nil {
		t.Fatal("context with lone surrogate must be rejected, not replaced by U+FFFD")
	}
	c := mustParseConfig(t, cfg)
	ctx := mustParseContext(t, `{"plan":"�"}`)
	if got := c.Find("f").Evaluate(ctx); got.Reason != EvalRule {
		t.Fatalf("literal U+FFFD still matches: %+v", got)
	}
	// 字段名被替换后也不能误报重复字段："\uD800x" 与 "�x" 本不应同名，
	// 但非法转义必须先于重复字段检查以自身名义报错。
	_, err := ParseContext([]byte(`{"\uD800x":"a","�x":"b"}`))
	if err == nil || strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("lone surrogate in field name must not surface as duplicate field: %v", err)
	}
}

func TestValidUnicodeEscapesUnchanged(t *testing.T) {
	// 正确配对的代理项转义与直接写出的同一字符按同一字符串比较。
	// emojiEsc 是 "😀" 的代理项对转义形式（分两段书写仅为避免源码层面的转义歧义）。
	emojiEsc := `\uD83D` + `\uDE00`
	cfgEsc := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"emoji","op":"eq","value":"` + emojiEsc + `"}]}]}]}`
	if got := eval(t, cfgEsc, "f", `{"emoji":"😀"}`); got.Reason != EvalRule {
		t.Fatalf("paired escape equals literal: %+v", got)
	}
	cfgLit := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"emoji","op":"eq","value":"😀"}]}]}]}`
	if got := eval(t, cfgLit, "f", `{"emoji":"`+emojiEsc+`"}`); got.Reason != EvalRule {
		t.Fatalf("literal equals paired escape: %+v", got)
	}
	// 真正的 "�" 与 "�" 仍是合法字符串，不能一概禁止替换字符。
	mustParseContext(t, `{"a":"�","b":"\u`+`FFFD"}`)
	if got := eval(t, cfgLit, "f", `{"emoji":"�"}`); got.Reason != EvalDefault {
		t.Fatalf("U+FFFD is just another string: %+v", got)
	}
	// 反斜杠转义后的 "\uD800" 是普通文本（内容为 \uD800 六个字符）。
	mustParseContext(t, `{"a":"\\uD800"}`)
	cfgText := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\\uD800"}]}]}]}`
	if got := eval(t, cfgText, "f", `{"a":"\\uD800"}`); got.Reason != EvalRule {
		t.Fatalf("escaped backslash text: %+v", got)
	}
}

// singleDocConfig 与 singleDocContext 自身满足全部字段规则：以它们为第一份
// 文档的解析失败只能来自文件边界（尾随内容），不会与业务字段错误混淆。
const singleDocConfig = `{"flags":[
	{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]},
	{"key":"off","enabled":false,"default":true,"rules":[]}]}`
const singleDocContext = `{"plan":"pro"}`

func TestParseRejectsSecondTopLevelValue(t *testing.T) {
	// 完整对象之后只允许 JSON 空白；第二个顶层值（即使本身合法）、未写完的
	// 内容或不能构成 JSON 的字符都必须让整个文件失败，不能只返回前半段的结果。
	// 尾随内容本身不含重复字段或非法转义，避免触发其他检查而掩盖边界错误。
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
			_, err := ParseConfig([]byte(singleDocConfig + tc.suffix))
			if err == nil || !strings.Contains(err.Error(), "config: invalid JSON") {
				t.Fatalf("err=%v, want config invalid JSON", err)
			}
		})
		t.Run("context/"+tc.name, func(t *testing.T) {
			_, err := ParseContext([]byte(singleDocContext + tc.suffix))
			if err == nil || !strings.Contains(err.Error(), "context: invalid JSON") {
				t.Fatalf("err=%v, want context invalid JSON", err)
			}
		})
	}
}

func TestWhitespaceAroundDocumentIgnored(t *testing.T) {
	// 文件开头与结尾的空格、制表符、回车、换行不影响解析与求值结果。
	paddedCfg := " \t\r\n" + singleDocConfig + "\t \r\n"
	paddedCtx := "\n\t " + singleDocContext + " \r\n"
	got := eval(t, paddedCfg, "f", paddedCtx)
	want := eval(t, singleDocConfig, "f", singleDocContext)
	if !got.Equals(want) {
		t.Fatalf("padded: got %+v, want %+v", got, want)
	}
	id := "r1"
	if !got.Equals(EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &id}) {
		t.Fatalf("padded result changed: %+v", got)
	}
}

func TestStructuralCharsInsideStringsAreNotSecondDocument(t *testing.T) {
	// 字符串里的大括号、方括号、转义引号与换行转义是合法内容，
	// 不能被误判成文档结束后的第二个顶层值。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x}{[\"}\n"}]}]}]}`
	ctx := `{"a":"x}{[\"}\n","note":"tail } { ] [ \" \n still one doc"}`
	got := eval(t, cfg, "f", ctx)
	if got.Reason != EvalRule || !got.Value {
		t.Fatalf("string content must not end the document: %+v", got)
	}
}

func TestTrailingContentRemovalRestoresEvaluation(t *testing.T) {
	// 同一份业务输入：带尾随内容时整个文件被拒绝；移除后恢复正常求值，
	// 继续遵守关闭开关固定 false、首条命中规则决定结果、无命中取默认值。
	if _, err := ParseConfig([]byte(singleDocConfig + ` {}`)); err == nil {
		t.Fatal("config with trailing content must be rejected")
	}
	if _, err := ParseContext([]byte(singleDocContext + ` {}`)); err == nil {
		t.Fatal("context with trailing content must be rejected")
	}
	cfg := mustParseConfig(t, singleDocConfig)
	ctx := mustParseContext(t, singleDocContext)
	if got := cfg.Find("off").Evaluate(ctx); got.Reason != EvalDisabled || got.Value {
		t.Fatalf("disabled flag: %+v", got)
	}
	id := "r1"
	if got := cfg.Find("f").Evaluate(ctx); !got.Equals(EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &id}) {
		t.Fatalf("first matching rule: %+v", got)
	}
	defaultCtx := mustParseContext(t, `{"plan":"free"}`)
	if got := cfg.Find("f").Evaluate(defaultCtx); got.Reason != EvalDefault || got.Value {
		t.Fatalf("default fallback: %+v", got)
	}
}

func TestEmptyFlagListAndFind(t *testing.T) {
	cfg := mustParseConfig(t, `{"flags":[]}`)
	if cfg.Find("missing") != nil {
		t.Error("expected nil flag")
	}
	var nilCfg *Config
	if nilCfg.Find("x") != nil {
		t.Error("nil config Find should be nil")
	}
	flag := (&Flag{Key: "f", Enabled: false})
	if got := flag.Evaluate(nil); got.Reason != EvalDisabled {
		t.Errorf("nil context on disabled flag: %+v", got)
	}
}

// --- 附加字段（不参与求值的额外信息）-------------------------------------------

// namedConfig 把一份配置文本与其变体名绑定，便于子测试定位。
type namedConfig struct {
	name string
	raw  string
}

// extraFieldConfigVariants 返回三份开关定义完全相同的配置：不含附加字段的基准、
// 在顶层/开关/规则/条件上附加了对象、数组、布尔、null 与合法大数字（1e400、
// 1 后接 400 个 0 的整数）的完整版，以及修改部分附加值、移除一部分、新增另一
// 部分的变体。三份配置对同一开关键与上下文必须给出完全一致的求值结果。
func extraFieldConfigVariants() []namedConfig {
	hugeInt := "1" + strings.Repeat("0", 400)
	base := `{"flags":[
		{"key":"f","enabled":true,"default":true,"rules":[
			{"id":"r-deny","value":false,"conditions":[
				{"attribute":"plan","op":"eq","value":"pro"},
				{"attribute":"tier","op":"in","value":["a","b"]}]},
			{"id":"r-allow","value":true,"conditions":[
				{"attribute":"region","op":"eq","value":"cn"}]}]},
		{"key":"off","enabled":false,"default":true,"rules":[]}]}`
	rich := `{"meta":{"owner":"pay","big":1e400,"huge":` + hugeInt + `,"tags":["x",1e400],"ok":true,"nil":null},
		"flags":[
		{"key":"f","enabled":true,"default":true,"note":"主开关","labels":["a","b"],"reviewed":true,"archived":null,"rules":[
			{"id":"r-deny","value":false,"comment":{"text":"先拒绝","n":1e400},"conditions":[
				{"attribute":"plan","op":"eq","value":"pro","hint":"套餐"},
				{"attribute":"tier","op":"in","value":["a","b"],"weight":1e400}]},
			{"id":"r-allow","value":true,"conditions":[
				{"attribute":"region","op":"eq","value":"cn"}]}]},
		{"key":"off","enabled":false,"default":true,"rules":[],"meta":{"big":` + hugeInt + `}}]}`
	alt := `{"meta":{"owner":"growth","big":-1e400},
		"flags":[
		{"key":"f","enabled":true,"default":true,"note":"改名","rules":[
			{"id":"r-deny","value":false,"conditions":[
				{"attribute":"plan","op":"eq","value":"pro"},
				{"attribute":"tier","op":"in","value":["a","b"],"weight":2}]},
			{"id":"r-allow","value":true,"since":"2026","conditions":[
				{"attribute":"region","op":"eq","value":"cn"}]}]},
		{"key":"off","enabled":false,"default":true,"rules":[]}],"footer":[1,2,3]}`
	return []namedConfig{
		{"no extra fields", base},
		{"extra fields added", rich},
		{"extra fields modified and removed", alt},
	}
}

func TestExtraFieldsDoNotAffectEvaluation(t *testing.T) {
	// 对同一份开关定义与合法上下文，增加、修改或移除附加字段后，结果中的
	// 开关键、布尔值、命中原因与规则编号必须保持一致：附加信息不改变首条
	// 完整命中规则的选择，也不会在没有规则命中时替代默认值。
	denyID, allowID := "r-deny", "r-allow"
	cases := []struct {
		name string
		key  string
		ctx  string
		want EvalResult
	}{
		// 上下文同时满足 r-deny 与 r-allow：靠前的 r-deny 立即定案为 false。
		{"first matching rule wins", "f", `{"plan":"pro","tier":"a","region":"cn"}`,
			EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &denyID}},
		{"later rule when first misses", "f", `{"plan":"free","region":"cn"}`,
			EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &allowID}},
		{"default when no rule matches", "f", `{"plan":"free","region":"eu"}`,
			EvalResult{Key: "f", Value: true, Reason: EvalDefault, RuleID: nil}},
		{"disabled flag stays false", "off", `{"plan":"pro","tier":"a","region":"cn"}`,
			EvalResult{Key: "off", Value: false, Reason: EvalDisabled, RuleID: nil}},
	}
	for _, variant := range extraFieldConfigVariants() {
		for _, tc := range cases {
			t.Run(variant.name+"/"+tc.name, func(t *testing.T) {
				got := eval(t, variant.raw, tc.key, tc.ctx)
				if !got.Equals(tc.want) {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
			})
		}
	}
}

func TestExtraFieldBigNumbersAccepted(t *testing.T) {
	// 附加信息里语法合法但超出浮点范围的数字（1e400、-1e400、1 后接 400 个 0
	// 的整数）必须能成功读取，不能被当成 JSON 无效；它们可以出现在顶层、
	// 开关、规则、条件以及附加对象/数组内部。
	hugeInt := "1" + strings.Repeat("0", 400)
	cfg := `{"meta":[1e400,` + hugeInt + `],"flags":[
		{"key":"f","enabled":true,"default":false,"big":1e400,"rules":[
			{"id":"r","value":true,"huge":` + hugeInt + `,"conditions":[
				{"attribute":"plan","op":"eq","value":"pro","neg":-1e400}]}]}]}`
	c := mustParseConfig(t, cfg)
	ctx := mustParseContext(t, `{"plan":"pro"}`)
	id := "r"
	want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &id}
	if got := c.Find("f").Evaluate(ctx); !got.Equals(want) {
		t.Fatalf("big numbers in extra fields must not disturb evaluation: got %+v, want %+v", got, want)
	}
}

func TestExtraFieldDuplicateAfterBigNumber(t *testing.T) {
	// 附加信息仍属于被完整检查的配置：合法大数字之后，同一附加对象再次声明
	// 同名字段时必须报重复字段并指向它在配置中的位置，不能误报为大数字的
	// 语法错误。
	hugeInt := "1" + strings.Repeat("0", 400)
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"after exponent overflow", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"n":1e400,"n":2}}]}`, `config.flags[0].meta.n`},
		{"after 400-digit integer", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"n":` + hugeInt + `,"n":2}}]}`, `config.flags[0].meta.n`},
		{"top-level extra object", `{"flags":[],"meta":{"n":1e400,"n":2}}`, `config.meta.n`},
		{"nested inside extra array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"tags":[{"n":1e400,"n":2}]}]}`, `config.flags[0].tags[0].n`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected duplicate field error, got nil\nconfig: %s", tc.raw)
			}
			msg := err.Error()
			if !strings.Contains(msg, "duplicate field") || !strings.Contains(msg, tc.want) {
				t.Fatalf("err=%q, want duplicate field at %s", msg, tc.want)
			}
			if strings.Contains(msg, "invalid JSON") {
				t.Fatalf("legal big number must not be misreported as a syntax error: %q", msg)
			}
		})
	}
}

func TestExtraFieldDuplicateInUnselectedOrDisabledFlag(t *testing.T) {
	// 请求的开关本身完全合法、能直接得到结果，也不能跳过其他开关附加信息的
	// 检查：未选中开关附加对象里的重复字段仍须整体失败。
	_, err := ParseConfig([]byte(`{"flags":[
		{"key":"good","enabled":true,"default":false,"rules":[]},
		{"key":"bad","enabled":true,"default":false,"rules":[],"meta":{"x":1,"x":2}}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[1].meta.x`) ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("unselected flag extra duplicate: err=%v", err)
	}
	// 已关闭开关的附加对象同理。
	_, err = ParseConfig([]byte(`{"flags":[
		{"key":"off","enabled":false,"default":false,"rules":[],"meta":{"x":1,"x":2}}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].meta.x`) ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("disabled flag extra duplicate: err=%v", err)
	}
}

func TestExtraFieldSameNameInDifferentObjectsAllowed(t *testing.T) {
	// 同名字段出现在两个不同对象中不算重复：不同开关的附加对象、同一附加
	// 对象的嵌套层、顶层与开关级都各自独立。
	mustParseConfig(t, `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[],"meta":{"owner":"a"}},
		{"key":"g","enabled":true,"default":false,"rules":[],"meta":{"owner":"b"}}]}`)
	mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"meta":{"owner":"a","nested":{"owner":"b"}}}]}`)
	mustParseConfig(t, `{"meta":{"owner":"a"},"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[],"meta":{"owner":"b"}}]}`)
}

func TestExtraFieldMalformedNumberRejected(t *testing.T) {
	// 附加字段把数字写成缺少指数数字的 1e+ 时不是合法 JSON 数字：按 JSON
	// 语法错误拒绝，与合法大数字被接受的行为明确区分。
	for _, raw := range []string{
		`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"n":1e+}}]}`,
		`{"flags":[],"meta":1e+}`,
	} {
		_, err := ParseConfig([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Fatalf("malformed number in extra field: err=%v\nconfig: %s", err, raw)
		}
		if strings.Contains(err.Error(), "duplicate field") {
			t.Fatalf("malformed number must not read as duplicate field: %v", err)
		}
	}
}

func TestExtraFieldsDoNotRelaxBusinessFields(t *testing.T) {
	// 附加字段的兼容性不扩展到业务字段：开关、规则、条件的必填项、值类型与
	// 唯一性要求在附加字段合法存在时仍按原约定执行。
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"enabled missing", `{"flags":[{"key":"f","default":false,"rules":[],"meta":{"x":1}}]}`,
			`flags[0].enabled`},
		{"enabled wrong type", `{"flags":[{"key":"f","enabled":"yes","default":false,"rules":[],"meta":{"x":1}}]}`,
			`flags[0].enabled`},
		{"default null", `{"flags":[{"key":"f","enabled":true,"default":null,"rules":[],"meta":{"x":1}}]}`,
			`flags[0].default`},
		{"rule id empty", `{"flags":[{"key":"f","enabled":true,"default":false,"meta":{"x":1},"rules":[
			{"id":"","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`,
			`rules[0].id`},
		{"eq value non-string", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":1,"hint":"h"}]}]}]}`,
			`conditions[0].value`},
		{"duplicate flag key", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[],"meta":{"x":1}},
			{"key":"f","enabled":true,"default":false,"rules":[],"meta":{"x":2}}]}`,
			`duplicate flag key`},
		{"duplicate rule id", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"note":"a","conditions":[{"attribute":"a","op":"eq","value":"x"}]},
			{"id":"r","value":false,"note":"b","conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`,
			`duplicate rule id`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want substring %q", err, tc.want)
			}
		})
	}
	// 上下文属性仍只允许字符串，与配置是否携带附加字段无关。
	if _, err := ParseContext([]byte(`{"plan":1}`)); err == nil ||
		!strings.Contains(err.Error(), "context.plan") {
		t.Fatalf("context attributes must stay string-only: err=%v", err)
	}
}
