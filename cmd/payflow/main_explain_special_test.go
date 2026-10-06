package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 本文件从命令行入口为 evaluate --explain 补充特殊字符串的回归保障：含中文、
// 重音字符、表情、引号、反斜杠及换行/制表符的开关键、规则编号、属性名与比较
// 内容，经过输入读取、规则判断与 JSON 输出后原有含义不变；命令行开关键仍按
// 字面查找；非法代理项转义按现有约定拒绝且不留半份解释。

const (
	cliSpecialKey        = "结算🚀开关"
	cliSpecialRuleMiss   = "r-跳过é🚀"
	cliSpecialRuleDecide = "r-定案🚀"
)

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

// cliSpecialJSON 按写法把文字写入 JSON 字符串字面量：direct 直接书写字符，
// escaped 把非 ASCII 与控制字符全部写成 \uXXXX——两种写法解码后是同一文字。
func cliSpecialJSON(t *testing.T, spelling, s string) string {
	t.Helper()
	switch spelling {
	case "direct":
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %q: %v", s, err)
		}
		return string(b)
	case "escaped":
		return `"` + escapeJSONString(s) + `"`
	default:
		t.Fatalf("unknown spelling %q", spelling)
		return ""
	}
}

// cliSpecialConfig 的开关按顺序含三条规则：r-跳过é🚀 因 标签🏷 不命中而整体
// 不成立（两个条件的逐条判断都要保留），r-定案🚀 命中并返回 false 定案，
// r-隐身 不再被考虑。所有名称与比较内容都含中文、重音字符与表情。
func cliSpecialConfig(t *testing.T, spelling string) string {
	t.Helper()
	j := func(s string) string { return cliSpecialJSON(t, spelling, s) }
	return `{"flags":[{"key":` + j(cliSpecialKey) + `,"enabled":true,"default":true,"rules":[` +
		`{"id":` + j(cliSpecialRuleMiss) + `,"value":true,"conditions":[` +
		`{"attribute":` + j("套餐é") + `,"op":"eq","value":` + j("专业版 🚀") + `},` +
		`{"attribute":` + j("标签🏷") + `,"op":"in","value":["",` + j("中文") + `,` + j("中文") + `,` + j("café 🚀") + `]}]},` +
		`{"id":` + j(cliSpecialRuleDecide) + `,"value":false,"conditions":[` +
		`{"attribute":` + j("地区") + `,"op":"eq","value":` + j("北京🚀") + `}]},` +
		`{"id":"r-隐身","value":true,"conditions":[` +
		`{"attribute":` + j("地区") + `,"op":"eq","value":"上海"}]}]}]}`
}

// cliSpecialContext 让首条规则因 标签🏷 不命中而整体不成立、第二条规则命中。
func cliSpecialContext(t *testing.T, spelling string) string {
	t.Helper()
	j := func(s string) string { return cliSpecialJSON(t, spelling, s) }
	return `{` + j("套餐é") + `:` + j("专业版 🚀") + `,` +
		j("标签🏷") + `:` + j("其他") + `,` +
		j("地区") + `:` + j("北京🚀") + `}`
}

// TestCLIExplainSpecialStringsEndToEnd 验证直接字符与合法 \uXXXX 转义表示同一
// 文字时，配置与上下文各自独立地选任一写法都选中同一开关、查找同一属性、得到
// 相同判断：解释里的 attribute、compareValue、actualValue 与 ruleId 保留解码
// 后的文字，stdout 逐字节一致；同一输入在普通模式与解释模式下 key、value、
// reason、ruleId 四项一致。
func TestCLIExplainSpecialStringsEndToEnd(t *testing.T) {
	var reference string
	for _, cfgSpelling := range []string{"direct", "escaped"} {
		for _, ctxSpelling := range []string{"direct", "escaped"} {
			t.Run(cfgSpelling+"-config/"+ctxSpelling+"-context", func(t *testing.T) {
				h := newHarness(t, cliSpecialConfig(t, cfgSpelling), cliSpecialContext(t, ctxSpelling))

				var stdout, stderr bytes.Buffer
				if code := run(h.evalExplainArgs(cliSpecialKey), &stdout, &stderr); code != 0 {
					t.Fatalf("explain exit=%d stderr=%s", code, stderr.String())
				}
				out := strings.TrimSpace(stdout.String())
				if strings.Contains(out, "\n") {
					t.Fatalf("stdout must be a single line: %q", out)
				}
				var got map[string]any
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("stdout is not one JSON object: %v (%q)", err, out)
				}
				if got["key"] != cliSpecialKey || got["value"] != false ||
					got["reason"] != "rule" || got["ruleId"] != cliSpecialRuleDecide {
					t.Fatalf("unexpected payload: %v", got)
				}

				rules := rulesOf(t, got)
				if len(rules) != 2 || rules[0]["ruleId"] != cliSpecialRuleMiss ||
					rules[1]["ruleId"] != cliSpecialRuleDecide {
					t.Fatalf("considered rules wrong (r-隐身 must not appear): %v", rules)
				}
				if rules[0]["match"] != false || rules[1]["match"] != true {
					t.Fatalf("match flags wrong: %v", rules)
				}
				// 首条未命中规则的全部条件判断保留，文字为解码后的内容。
				conds := rules[0]["conditions"].([]any)
				if len(conds) != 2 {
					t.Fatalf("missed rule must keep all condition records: %v", conds)
				}
				plan := conds[0].(map[string]any)
				if plan["attribute"] != "套餐é" || plan["compareValue"] != "专业版 🚀" ||
					plan["actualValue"] != "专业版 🚀" || plan["missing"] != false || plan["match"] != true {
					t.Fatalf("plan condition must hold decoded text: %v", plan)
				}
				tag := conds[1].(map[string]any)
				list := tag["compareValue"].([]any)
				if len(list) != 4 || list[0] != "" || list[1] != "中文" || list[2] != "中文" || list[3] != "café 🚀" {
					t.Fatalf("in list must keep empty member, duplicates and order: %v", list)
				}
				if tag["attribute"] != "标签🏷" || tag["actualValue"] != "其他" ||
					tag["missing"] != false || tag["match"] != false {
					t.Fatalf("tag condition should be a present non-member: %v", tag)
				}
				decide := rules[1]["conditions"].([]any)[0].(map[string]any)
				if decide["attribute"] != "地区" || decide["compareValue"] != "北京🚀" ||
					decide["actualValue"] != "北京🚀" || decide["match"] != true {
					t.Fatalf("deciding condition must hold decoded text: %v", decide)
				}

				// 同一输入的普通模式：key/value/reason/ruleId 四项必须一致。
				var pout, perr bytes.Buffer
				if code := run(h.evalArgs(cliSpecialKey), &pout, &perr); code != 0 {
					t.Fatalf("plain exit=%d stderr=%s", code, perr.String())
				}
				var plain map[string]any
				if err := json.Unmarshal(pout.Bytes(), &plain); err != nil {
					t.Fatalf("plain stdout is not JSON: %v (%q)", err, pout.String())
				}
				for _, k := range []string{"key", "value", "reason", "ruleId"} {
					if plain[k] != got[k] {
						t.Fatalf("field %s differs: plain=%v explain=%v", k, plain[k], got[k])
					}
				}

				if reference == "" {
					reference = out
				} else if out != reference {
					t.Fatalf("spelling changed explain output:\n%s\n!= reference:\n%s", out, reference)
				}
			})
		}
	}
}

// TestCLIExplainKeyArgumentStaysLiteral 验证命令行开关键按字面查找：反斜杠加
// u 和数字组成的普通文本不能再次解码，也不与对应的真实字符混为一谈；解释里
// 的 ruleId 同样保持字面文本。
func TestCLIExplainKeyArgumentStaysLiteral(t *testing.T) {
	// 第一个开关的 key/id 都是带反斜杠的普通文本（JSON "\\u00E9" 解码为
	// 普通文本 u00E9）；若被再次解码，两个开关的 key 都会变成 "café"。
	config := `{"flags":[
		{"key":"caf\\u00E9","enabled":true,"default":false,"rules":[
			{"id":"r-\\uD83D\\uDE80","value":true,"conditions":[
				{"attribute":"plan","op":"eq","value":"pro"}]}]},
		{"key":"café","enabled":true,"default":true,"rules":[]}]}`
	h := newHarness(t, config, `{"plan":"pro"}`)

	// 字面文本 key 选中字面文本开关；解释里的 ruleId 保持字面文本，不变成 🚀。
	literalKey := `caf\u00E9`
	literalID := `r-\uD83D\uDE80`
	got := explainSuccess(t, h, literalKey)
	if got["key"] != literalKey || got["value"] != true ||
		got["reason"] != "rule" || got["ruleId"] != literalID {
		t.Fatalf("literal key must select the literal flag: %v", got)
	}
	rules := rulesOf(t, got)
	if len(rules) != 1 || rules[0]["ruleId"] != literalID {
		t.Fatalf("explanation must keep the literal rule id: %v", rules)
	}

	// 真实字符 key 选中另一个开关：二者互不混同。
	got = explainSuccess(t, h, "café")
	if got["key"] != "café" || got["value"] != true ||
		got["reason"] != "default" || got["ruleId"] != nil {
		t.Fatalf("decoded key must select the real café flag: %v", got)
	}
}

// TestCLIExplainQuotesBackslashControlSingleLine 验证引号、反斜杠以及合法转义
// 表示的换行和制表符进入字符串后：成功输出仍是单行、可解析的一个 JSON 对象；
// 读取输出得到的文字与输入解码后的内容相同，不丢字符、不重复转义，字符串
// 内容也不会变成新字段。
func TestCLIExplainQuotesBackslashControlSingleLine(t *testing.T) {
	attr := "属\"性\\名"
	value := "行一\n行二\t制表\"引号\\反斜杠🚀"
	ruleID := "r-引号\"反斜杠\\"
	js := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %q: %v", s, err)
		}
		return string(b)
	}
	config := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[` +
		`{"id":` + js(ruleID) + `,"value":true,"conditions":[` +
		`{"attribute":` + js(attr) + `,"op":"eq","value":` + js(value) + `},` +
		`{"attribute":"标签","op":"in","value":["",` + js(value) + `,` + js(value) + `]}]}]}]}`
	context := `{` + js(attr) + `:` + js(value) + `,"标签":` + js(value) + `}`
	h := newHarness(t, config, context)

	var stdout, stderr bytes.Buffer
	if code := run(h.evalExplainArgs("f"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	if strings.Contains(out, "\n") {
		t.Fatalf("output must stay a single line: %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not one parseable JSON object: %v (%q)", err, out)
	}
	// 字符串内容不得变成新字段：顶层恰好五个既有字段。
	if len(got) != 5 {
		t.Fatalf("top-level fields changed by string content: %v", got)
	}
	for _, k := range []string{"key", "value", "reason", "ruleId", "explanation"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing field %q in %v", k, got)
		}
	}
	// 读回的文字与输入解码后的内容相同：不丢字符、不重复转义。
	if got["ruleId"] != ruleID {
		t.Fatalf("ruleId decoded text wrong: %q want %q", got["ruleId"], ruleID)
	}
	rules := rulesOf(t, got)
	if rules[0]["ruleId"] != ruleID {
		t.Fatalf("explanation ruleId decoded text wrong: %q want %q", rules[0]["ruleId"], ruleID)
	}
	conds := rules[0]["conditions"].([]any)
	c0 := conds[0].(map[string]any)
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
}

// TestCLIExplainSpecialAttrMissingVsEmpty 验证含特殊字符的属性名准确区分缺失
// 与空值：未提供时 actualValue 为 null、missing 为 true；显式空字符串保留空
// 字符串、missing 为 false。属性名的转义写法与直接字符查找同一属性。
func TestCLIExplainSpecialAttrMissingVsEmpty(t *testing.T) {
	// 配置里的属性名用代理项对转义写法（"标签🏷"），上下文里直接书写字符。
	config := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[
			{"attribute":"标签\uD83C\uDFF7","op":"eq","value":""}]}]}]}`

	h := newHarness(t, config, `{}`)
	got := explainSuccess(t, h, "f")
	c := rulesOf(t, got)[0]["conditions"].([]any)[0].(map[string]any)
	if c["attribute"] != "标签🏷" || c["missing"] != true || c["actualValue"] != nil || c["match"] != false {
		t.Fatalf("missing special attribute must be null/true: %v", c)
	}

	h = newHarness(t, config, `{"标签🏷":""}`)
	got = explainSuccess(t, h, "f")
	c = rulesOf(t, got)[0]["conditions"].([]any)[0].(map[string]any)
	if c["missing"] != false || c["actualValue"] != "" || c["match"] != true {
		t.Fatalf("explicit empty string must be kept with missing=false: %v", c)
	}
	if got["reason"] != "rule" || got["value"] != true {
		t.Fatalf("empty string must match eq \"\": %v", got)
	}
}

// TestCLIExplainInvalidSurrogateRejected 验证不能组成完整字符的代理项转义在
// 解释模式下继续按现有约定拒绝：非零退出、标准输出为空（不留半份解释），
// 标准错误指出问题所属位置和转义不完整的原因。
func TestCLIExplainInvalidSurrogateRejected(t *testing.T) {
	const incomplete = "does not form a complete character"
	goodCfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	cases := []struct {
		name    string
		config  string
		context string
		wantSub []string
	}{
		{
			"config eq value lone high surrogate",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"\uD800"}]}]}]}`,
			`{"plan":"pro"}`,
			[]string{`config.flags[0].rules[0].conditions[0].value`, `\uD800`, incomplete},
		},
		{
			"config in-list lone low surrogate",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"in","value":["ok","\uDC00"]}]}]}]}`,
			`{"plan":"ok"}`,
			[]string{`config.flags[0].rules[0].conditions[0].value[1]`, `\uDC00`, incomplete},
		},
		{
			"config field name surrogate",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],"met\uD800a":1}]}`,
			`{}`,
			[]string{`config.flags[0]`, `\uD800`, incomplete},
		},
		{
			"context value surrogate",
			goodCfg, `{"plan":"\uD800"}`,
			[]string{`context.plan`, `\uD800`, incomplete},
		},
		{
			"context field name surrogate",
			goodCfg, `{"pl\uDC00an":"pro"}`,
			[]string{`context`, `\uDC00`, incomplete},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.config, tc.context)
			var stdout, stderr bytes.Buffer
			if code := run(h.evalExplainArgs("f"), &stdout, &stderr); code == 0 {
				t.Fatalf("expected non-zero exit, stdout=%q", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty (no half explanation): %q", stdout.String())
			}
			for _, want := range tc.wantSub {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr=%q, want substring %q", stderr.String(), want)
				}
			}
		})
	}
}
