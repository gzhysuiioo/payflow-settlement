package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件在命令行端到端层面为 evaluate --explain 补特殊字符串的回归保障：
//   - 开关键、规则编号、属性名与 eq/in 比较内容含中文、重音与表情字符时，
//     直接字符与合法 \uXXXX 转义写法选中同一开关、得到相同解释；
//   - 命令行开关键按字面查找，反斜杠加 u 和数字的普通文本不会被再次解码；
//   - 引号、反斜杠与转义表示的换行/制表符进入字符串后，解释输出仍是单行、
//     可解析的一个 JSON 对象，往返后文字与输入解码内容相同；
//   - 同一输入普通模式与解释模式的 key/value/reason/ruleId 一致；
//   - 不能组成完整字符的代理项转义在解释模式同样被拒绝：非零退出、stdout
//     为空、stderr 指出位置与“转义不完整”的原因，不留半份解释。

// explCLIKey 是解码后的特殊开关键；其 JSON 转义写法（含 🎉 的代理项对）如下。
const explCLIKey = "支付开关🎉"

// 本文件复用 main_test.go 已有的 cliEscEA（é）与 cliEscGR（😀 代理项对）；
// 这里再补几个本文件需要的转义片段，均为 JSON 字符串“内容”（不含引号）。
const (
	// explCLIParty 是 🎉（U+1F389）的一对代理项转义写法。
	explCLIParty = (`\u` + `D83C`) + (`\u` + `DF89`)
	// explCLIReject 是“拒绝”的转义写法；explCLIRegion 是“地区”；
	// explCLIMiddle 是“中”；explCLILevel 是“等级”。
	explCLIReject = (`\u` + `62D2`) + (`\u` + `7EDD`)
	explCLIRegion = (`\u` + `5730`) + (`\u` + `533A`)
	explCLIMiddle = `\u` + `4E2D`
	explCLILevel  = (`\u` + `7B49`) + (`\u` + `7EA7`)
)

// explCLIKeyConfig 拼出一个启用开关（key 与规则/属性/比较值的写法由各 JSON
// 片段给定），含两条规则：靠前的 r-拒绝🎉 返回 false（两个条件 AND），靠后
// 的 r-通过 返回 true。
func explCLIKeyConfig(keyJSON, idJSON, regionJSON, midJSON, levelJSON, eaJSON, grinJSON string) string {
	return `{"flags":[{"key":"` + keyJSON + `","enabled":true,"default":true,"rules":[
		{"id":"` + idJSON + `","value":false,"conditions":[
			{"attribute":"` + regionJSON + `","op":"eq","value":"` + midJSON + `"},
			{"attribute":"` + levelJSON + `","op":"in","value":["` + eaJSON + `","` + grinJSON + `","a"]}]},
		{"id":"r-通过","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
}

// TestCLIEvaluateExplainSpecialKeyAndRuleDecoded 端到端验证：配置的开关键、
// 规则编号、属性名与比较值无论用直接字符还是合法 Unicode 转义（含代理项对）
// 书写，命令行传解码后的真实文字都选中同一开关；解释中的 key、ruleId、
// attribute、compareValue、actualValue 全部是解码后的文字，且不同写法的
// 解释输出逐字节一致。
func TestCLIEvaluateExplainSpecialKeyAndRuleDecoded(t *testing.T) {
	configs := []struct {
		name string
		raw  string
	}{
		{"direct", explCLIKeyConfig(
			`支付开关🎉`, `r-拒绝🎉`, `地区`, `中`, `等级`, `é`, `😀`)},
		{"escaped", explCLIKeyConfig(
			`支付开关`+explCLIParty, `r-`+explCLIReject+explCLIParty,
			explCLIRegion, explCLIMiddle, explCLILevel, cliEscEA, cliEscGR)},
		{"mixed", explCLIKeyConfig(
			`支付开关`+explCLIParty, `r-拒绝`+explCLIParty,
			`地区`, explCLIMiddle, explCLILevel, `é`, cliEscGR)},
	}
	contexts := []struct {
		name string
		raw  string
	}{
		{"direct", `{"地区":"中","等级":"😀","plan":"pro"}`},
		{"escaped", `{"` + explCLIRegion + `":"` + explCLIMiddle + `","` + explCLILevel + `":"` + cliEscGR + `","plan":"pro"}`},
	}
	var reference string
	for ci, cfg := range configs {
		for _, cx := range contexts {
			t.Run(cfg.name+"/"+cx.name, func(t *testing.T) {
				h := newHarness(t, cfg.raw, cx.raw)
				var stdout, stderr bytes.Buffer
				if code := run(h.evalExplainArgs(explCLIKey), &stdout, &stderr); code != 0 {
					t.Fatalf("exit=%d stderr=%s", code, stderr.String())
				}
				out := stdout.String()
				var got map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatalf("output not parseable: %v\n%s", err, out)
				}
				if got["key"] != explCLIKey || got["value"] != false ||
					got["reason"] != "rule" || got["ruleId"] != "r-拒绝🎉" {
					t.Fatalf("unexpected payload: %v", got)
				}
				exp := got["explanation"].(map[string]any)
				rules := exp["rules"].([]any)
				if len(rules) != 1 || rules[0].(map[string]any)["ruleId"] != "r-拒绝🎉" {
					t.Fatalf("only the deciding rule may appear: %v", rules)
				}
				conds := rules[0].(map[string]any)["conditions"].([]any)
				first := conds[0].(map[string]any)
				if first["attribute"] != "地区" || first["compareValue"] != "中" ||
					first["actualValue"] != "中" || first["match"] != true {
					t.Fatalf("eq condition wrong: %v", first)
				}
				second := conds[1].(map[string]any)
				list := second["compareValue"].([]any)
				if len(list) != 3 || list[0] != "é" || list[1] != "😀" || list[2] != "a" ||
					second["actualValue"] != "😀" {
					t.Fatalf("in condition wrong: %v", second)
				}
				// 输出直接承载解码文字，不写成 \uXXXX。
				if !strings.Contains(out, explCLIKey) || !strings.Contains(out, "r-拒绝🎉") ||
					strings.Contains(out, `\u`) {
					t.Fatalf("output must carry decoded text, not escapes: %s", out)
				}
				if ci == 0 && cx.name == "direct" {
					reference = out
				} else if out != reference {
					t.Fatalf("spelling changed explain output:\n%s\n!= reference:\n%s", out, reference)
				}
			})
		}
	}
}

// TestCLIEvaluateExplainCommandLineBackslashLiteral 验证命令行开关键按字面
// 查找：参数里的反斜杠加 u00e9 等只是普通文本，不会被再次解码成 é/🎉；
// 它选中配置里用双反斜杠声明的“字面文本开关”，而解释中的 key/ruleId 也
// 保留这段字面文本（作为 JSON 输出时反斜杠再转义一次显示为 \\）。
func TestCLIEvaluateExplainCommandLineBackslashLiteral(t *testing.T) {
	// 第一个开关的 key 是 21 个普通字符 café🎉（JSON 中用
	// 双反斜杠声明）；第二个开关的 key 是真正的 café🎉。
	config := `{"flags":[
		{"key":"caf\\u00e9\\ud83c\\udf89","enabled":true,"default":false,"rules":[
			{"id":"r-literal\\u00e9","value":true,"conditions":[
				{"attribute":"plan","op":"eq","value":"pro"}]}]},
		{"key":"café🎉","enabled":true,"default":true,"rules":[]}]}`
	h := newHarness(t, config, `{"plan":"pro"}`)

	// 传解码后的真实文字：选中第二个开关（default，ruleId 为 null）。
	gotReal := explainSuccess(t, h, "café🎉")
	if gotReal["key"] != "café🎉" || gotReal["reason"] != "default" || gotReal["ruleId"] != nil {
		t.Fatalf("decoded real key must select the real flag: %v", gotReal)
	}

	// 传反斜杠字面文本：参数内容是反斜杠加 u 与数字本身（Go 双引号串里每个
	// 反斜杠要写两次），求值器按字面查找、不会再解码。
	literalKey := "caf\\u00e9\\ud83c\\udf89"
	literalID := "r-literal\\u00e9"
	var stdout, stderr bytes.Buffer
	if code := run(h.evalExplainArgs(literalKey), &stdout, &stderr); code != 0 {
		t.Fatalf("literal backslash key: exit=%d stderr=%s", code, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output not parseable: %v\n%s", err, stdout.String())
	}
	if got["key"] != literalKey || got["ruleId"] != literalID || got["value"] != true {
		t.Fatalf("backslash text must stay a literal key/id: %v", got)
	}
	// JSON 文本中真实反斜杠必须再转义为双反斜杠，且不能出现解码后的 é 外形混淆。
	out := stdout.String()
	if !strings.Contains(out, `caf\\u00e9\\ud83c\\udf89`) ||
		!strings.Contains(out, `r-literal\\u00e9`) {
		t.Fatalf("output must re-escape literal backslashes: %s", out)
	}

	// 把字面外形传给只认真实字符的查找会失败；--explain 下同样是未找到，且
	// 不产出任何解释。
	realOnly := newHarness(t, `{"flags":[{"key":"café🎉","enabled":true,"default":true,"rules":[]}]}`, `{}`)
	var out2, err2 bytes.Buffer
	if code := run(realOnly.evalExplainArgs(literalKey), &out2, &err2); code == 0 {
		t.Fatalf("backslash literal must not match the real-character flag, stdout=%q", out2.String())
	}
	if out2.Len() != 0 {
		t.Fatalf("stdout must be empty when key not found: %q", out2.String())
	}
	if !strings.Contains(err2.String(), "not found") {
		t.Fatalf("stderr must report not found: %q", err2.String())
	}
}

// TestCLIEvaluateExplainControlCharsSingleLineRoundTrip 端到端验证引号、
// 反斜杠及转义表示的换行、制表符进入字符串后：解释输出是单行、可解析的一个
// JSON 对象（仅既有五个字段），重新读取得到的文字与输入解码内容逐字相同，
// 不丢字符、不重复转义，字符串内容也不会变成结果对象的新字段。
func TestCLIEvaluateExplainControlCharsSingleLineRoundTrip(t *testing.T) {
	config := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-特殊\n编号","value":true,"conditions":[
			{"attribute":"行\n制表\t名","op":"eq","value":"a\"b\\c\n\t中😀"},
			{"attribute":"plan","op":"in","value":["x\ny","","a\"a","\\u0041"]}]}]}]}`
	context := `{"行\n制表\t名":"a\"b\\c\n\t中😀","plan":"x\ny"}`
	h := newHarness(t, config, context)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalExplainArgs("f"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	// 去掉 Fprintln 正常追加的行尾换行后，JSON 对象本身必须单行，且不含裸
	// 制表符；字符串内的换行/制表符只能以 \n、\t 转义形式出现。
	body := bytes.TrimRight(stdout.Bytes(), "\n")
	if bytes.ContainsAny(body, "\n\r") {
		t.Fatalf("output must stay on one line: %q", body)
	}
	if bytes.Contains(body, []byte("\t")) {
		t.Fatalf("raw tab must be escaped: %q", body)
	}
	raw := append([]byte{}, body...)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("output is not one parseable JSON object: %v\n%s", err, raw)
	}
	if len(obj) != 5 {
		t.Fatalf("string content must not become new fields: %v", obj)
	}
	if obj["ruleId"] != "r-特殊\n编号" {
		t.Fatalf("ruleId with newline must round-trip: %v", obj["ruleId"])
	}
	var parsed struct {
		Explanation struct {
			Rules []struct {
				RuleID     string `json:"ruleId"`
				Conditions []struct {
					Attribute    string  `json:"attribute"`
					CompareValue any     `json:"compareValue"`
					ActualValue  *string `json:"actualValue"`
					Missing      bool    `json:"missing"`
					Match        bool    `json:"match"`
				} `json:"conditions"`
			} `json:"rules"`
		} `json:"explanation"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	wantSpecial := "a\"b\\c\n\t中😀"
	c0 := parsed.Explanation.Rules[0].Conditions[0]
	if c0.Attribute != "行\n制表\t名" || c0.CompareValue != wantSpecial ||
		c0.ActualValue == nil || *c0.ActualValue != wantSpecial || !c0.Match {
		t.Fatalf("eq text did not round-trip: %+v", c0)
	}
	c1 := parsed.Explanation.Rules[0].Conditions[1]
	list := c1.CompareValue.([]any)
	// 末项 JSON "\\u0041" 解码为反斜杠普通文本（6 字符），不会再解码成 A。
	if len(list) != 4 || list[0] != "x\ny" || list[1] != "" || list[2] != "a\"a" ||
		list[3] != "\\u0041" || c1.ActualValue == nil || *c1.ActualValue != "x\ny" {
		t.Fatalf("in list did not round-trip: %+v", list)
	}
	// 反斜杠与控制字符在数据字段（attribute/compareValue/actualValue）里恰好
	// 转义一次。outcome 文案中的规则编号另有 Go %q 渲染，不在此断言范围内。
	s := string(raw)
	for _, frag := range []string{
		`"attribute":"行\n制表\t名"`,
		`"compareValue":"a\"b\\c\n\t中😀"`,
		`"actualValue":"a\"b\\c\n\t中😀"`,
		`"compareValue":["x\ny","","a\"a","\\u0041"]`,
	} {
		if !strings.Contains(s, frag) {
			t.Fatalf("output must contain properly escaped %q: %s", frag, s)
		}
	}
}

// TestCLIEvaluateExplainSpecialAttributeMissingVsEmpty 端到端验证含特殊字符
// 的属性名也区分缺失与空值：缺失时 actualValue 为 null、missing 为 true；
// 显式空字符串保留 ""、missing 为 false。
func TestCLIEvaluateExplainSpecialAttributeMissingVsEmpty(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[
			{"attribute":"emoji🎨","op":"eq","value":""}]}]}]}`

	// 属性缺失：null/missing=true。
	missing := explainSuccess(t, newHarness(t, cfg, `{"其他":""}`), "f")
	mc := rulesOf(t, missing)[0]["conditions"].([]any)[0].(map[string]any)
	if mc["attribute"] != "emoji🎨" || mc["actualValue"] != nil ||
		mc["missing"] != true || mc["match"] != false {
		t.Fatalf("missing special-named attribute: %v", mc)
	}

	// 显式空字符串：""/missing=false/match=true；属性名用转义写法也指向同一属性。
	empty := explainSuccess(t, newHarness(t, cfg, `{"emoji🎨":""}`), "f")
	ec := rulesOf(t, empty)[0]["conditions"].([]any)[0].(map[string]any)
	if ec["attribute"] != "emoji🎨" || ec["actualValue"] != "" ||
		ec["missing"] != false || ec["match"] != true {
		t.Fatalf("explicit empty value on special-named attribute: %v", ec)
	}
}

// TestCLIEvaluateExplainFourFieldsMatchPlainMode 端到端验证同一输入普通模式
// 与解释模式输出的 key、value、reason、ruleId 四项逐字一致，覆盖特殊字符串
// 的靠前 false 定案、文字不命中落到后一条、全部不命中取默认值。
func TestCLIEvaluateExplainFourFieldsMatchPlainMode(t *testing.T) {
	cfg := explCLIKeyConfig(
		`支付开关🎉`, `r-拒绝🎉`, `地区`, `中`, `等级`, `é`, `😀`)
	cases := []struct {
		name string
		ctx  string
	}{
		{"earlier false decides", `{"地区":"中","等级":"a","plan":"pro"}`},
		{"miss then later rule", `{"地区":"中","等级":"z","plan":"pro"}`},
		{"default fallback", `{"地区":"x","plan":"free"}`},
		{"missing attrs", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var plainOut, explainOut bytes.Buffer
			h1 := newHarness(t, cfg, tc.ctx)
			if code := run(h1.evalArgs(explCLIKey), &plainOut, &bytes.Buffer{}); code != 0 {
				t.Fatalf("plain mode failed")
			}
			h2 := newHarness(t, cfg, tc.ctx)
			if code := run(h2.evalExplainArgs(explCLIKey), &explainOut, &bytes.Buffer{}); code != 0 {
				t.Fatalf("explain mode failed")
			}
			// 解释输出截到 ,"explanation" 并补 }，应与普通模式整行逐字相同。
			e := explainOut.Bytes()
			i := bytes.Index(e, []byte(`,"explanation"`))
			if i < 0 {
				t.Fatalf("explain output missing explanation: %s", e)
			}
			head := append(append([]byte{}, e[:i]...), '}')
			// 普通输出末尾带一个换行（Fprintln），解释头部对齐时去掉。
			if !bytes.Equal(head, bytes.TrimRight(plainOut.Bytes(), "\n")) {
				t.Fatalf("four fields differ:\nexplain: %s\nplain:   %s", head, plainOut.String())
			}
		})
	}
}

// TestCLIEvaluateExplainLoneSurrogateRejected 端到端验证：不能组成完整字符的
// 代理项转义出现在配置或上下文的任意位置时，--explain 同样非零退出、stdout
// 为空（不留半份解释），stderr 指出问题所属位置与“转义不完整”的原因。
func TestCLIEvaluateExplainLoneSurrogateRejected(t *testing.T) {
	const incomplete = "does not form a complete character"
	cases := []struct {
		name    string
		config  string
		context string
		key     string
		wantLoc string
	}{
		{
			"lone high surrogate in config eq value",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[
					{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`{}`, "f", `config.flags[0].rules[0].conditions[0].value`,
		},
		{
			"lone low surrogate in config in-list element",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[
					{"attribute":"a","op":"in","value":["ok","\uDC00"]}]}]}]}`,
			`{}`, "f", `config.flags[0].rules[0].conditions[0].value[1]`,
		},
		{
			"lone surrogate in config flag key value",
			`{"flags":[{"key":"中\uD800","enabled":true,"default":false,"rules":[]}]}`,
			`{}`, "f", `config.flags[0].key`,
		},
		{
			"lone surrogate in context value",
			`{"flags":[{"key":"f","enabled":true,"default":true,"rules":[]}]}`,
			`{"plan":"\uD800"}`, "f", `context.plan`,
		},
		{
			"lone surrogate in context field name",
			`{"flags":[{"key":"f","enabled":true,"default":true,"rules":[]}]}`,
			`{"pl\uD800an":"pro"}`, "f", `\uD800`,
		},
		{
			"reversed surrogate pair in config even for a hit-looking context",
			`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[
					{"attribute":"a","op":"eq","value":"\uDC00\uD800"}]}]}]}`,
			`{"a":"x"}`, "f", `conditions[0].value`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.config, tc.context)
			var stdout, stderr bytes.Buffer
			code := run(h.evalExplainArgs(tc.key), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("expected non-zero exit, stdout=%q", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("no half explanation may be printed on bad escape: %q", stdout.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, incomplete) {
				t.Fatalf("stderr must explain the incomplete escape: %q", msg)
			}
			if !strings.Contains(msg, tc.wantLoc) {
				t.Fatalf("stderr=%q, want location %q", msg, tc.wantLoc)
			}
			if strings.Contains(msg, "explanation") {
				t.Fatalf("error output must not leak an explanation: %q", msg)
			}
		})
	}
}
