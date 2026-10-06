package payflow

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件为 --explain 判断路径补特殊字符串的自动化回归保障：保护合法字符串
// 经过输入读取（JSON 解码）、规则判断（eq/in）与 JSON 输出（MarshalExplain）
// 后仍保持原有含义。开关键、规则编号、属性名以及 eq/in 的比较内容都可以含
// 中文、重音字符与表情字符；直接书写字符与合法 Unicode 转义（BMP 外字符用
// 一对代理项转义）表示同一文字时，必须选中同一开关、查找同一属性、得到相同
// 判断；解释里的 attribute、compareValue、actualValue 与 ruleId 一律展示
// 解码后的文字，而不是它们的转义外形。
//
// 下列常量沿用本包既有写法，用反引号原始字符串拼出 JSON 转义文本，使
// “转义写法”与“直接字符”在源码层面就是两类不同的输入。
const (
	// explEscEA 是 é（U+00E9）的转义写法。
	explEscEA = `\u` + `00E9`
	// explEscCB 是 "e" 后接组合附加符号 U+0301 的转义写法（与预组合 é 不同）。
	explEscCB = `e\u` + `0301`
	// explEscGrin 是 😀（U+1F600）的一对代理项转义写法。
	explEscGrin = (`\u` + `D83D`) + (`\u` + `DE00`)
	// explEscCN 是“中”（U+4E2D）的转义写法。
	explEscCN = `\u` + `4E2D`
	// explEscDI/explEscQU 分别是“地”（U+5730）与“区”（U+533A）的转义写法，
	// 合起来即属性名“地区”，与直接书写的属性名解码后同名。
	explEscDI = `\u` + `5730`
	explEscQU = `\u` + `533A`
	// explEscParty 是 🎉（U+1F389）的一对代理项转义写法。
	explEscParty = (`\u` + `D83C`) + (`\u` + `DF89`)
	// explEscPlan 是属性名 plan 四个字母各自转义后的写法。
	explEscPlan = (`\u` + `0070`) + (`\u` + `006C`) + (`\u` + `0061`) + (`\u` + `006E`)
)

// explSpecCfg 拼出特殊字符串专用配置：开关键、规则 id 与属性名均可含
// 中文/重音/表情，规则覆盖 eq 与 in 两类比较。flagKey 是开关键的 JSON 字符串
// 内容（不含引号）；condAttr 是条件属性名的 JSON 字符串内容。
//
// 开关含三条规则（顺序即优先级）：
//   - r-中é😀（在前，返回 false）：attrCN eq "中" 与 attrEA in ["é","","a","a"]；
//   - r-pro（居中，返回 true）：plan eq "pro"；
//   - r-unseen（在后）：前两条任一定案后都不应出现在解释里。
func explSpecCfg(flagKey, condAttr string) string {
	return `{"flags":[{"key":"` + flagKey + `","enabled":true,"default":true,"rules":[
		{"id":"r-中é😀","value":false,"conditions":[
			{"attribute":"` + condAttr + `","op":"eq","value":"中"},
			{"attribute":"emoji🎨","op":"in","value":["é","😀","","a","a"]}]},
		{"id":"r-pro","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]},
		{"id":"r-unseen","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"never"}]}]}]}`
}

// 直接字符与各种转义写法解码后应指向同一个开关键、同一组规则与属性名。
var explSpecKeySpellings = []namedConfig{
	{"direct", explSpecCfg(`支付开关🎉`, `地区`)},
	{"escaped", explSpecCfg(`支付开关`+explEscParty, explEscDI+explEscQU)},
	{"mixed", explSpecCfg(`支付开关`+explEscParty, `地`+explEscQU)},
}

const explSpecDecodedKey = "支付开关🎉"

// explSpecHitCtx 让前一条规则的两个条件都成立（地区=中、emoji🎨=😀），
// 同时 plan=pro 也满足第二条：靠前的规则即使返回 false 也立即定案。
var explSpecHitCtxSpelling = []struct {
	name string
	raw  string
}{
	{"direct", `{"地区":"中","emoji🎨":"😀","plan":"pro"}`},
	{"escaped values", `{"` + explEscDI + explEscQU + `":"` + explEscCN + `","emoji🎨":"` + explEscGrin + `","` + explEscPlan + `":"pro"}`},
	{"mixed", `{"地区":"` + explEscCN + `","emoji🎨":"` + explEscGrin + `","plan":"pro"}`},
}

// TestExplainSpecialStringsDecodedSpellingEquivalent 验证：无论配置的开关键、
// 规则编号、属性名、eq/in 比较值，还是上下文的属性名与值，用直接字符还是
// 合法 Unicode 转义（含代理项对）书写，解释模式都按解码后的同一文字求值，
// 得到逐字节一致的解释输出；解释中的 attribute、compareValue、actualValue
// 与 ruleId 展示的都是解码后的文字。
func TestExplainSpecialStringsDecodedSpellingEquivalent(t *testing.T) {
	var reference []byte
	for ci, cfg := range explSpecKeySpellings {
		for _, cx := range explSpecHitCtxSpelling {
			t.Run(cfg.name+"/"+cx.name, func(t *testing.T) {
				flag := mustParseConfig(t, cfg.raw).Find(explSpecDecodedKey)
				if flag == nil {
					t.Fatalf("decoded flag key %q must be found regardless of JSON spelling", explSpecDecodedKey)
				}
				got := flag.EvaluateExplain(mustParseContext(t, cx.raw))

				// 前一条规则全中并以 false 定案，ruleId 是解码后的特殊文字。
				if got.Key != explSpecDecodedKey || got.Value != false || got.Reason != EvalRule {
					t.Fatalf("unexpected result: %+v", got.EvalResult)
				}
				if got.RuleID == nil || *got.RuleID != "r-中é😀" {
					t.Fatalf("ruleId must carry decoded text: %+v", got.RuleID)
				}
				if ids := explainedIDs(got); len(ids) != 1 || ids[0] != "r-中é😀" {
					t.Fatalf("only the deciding rule may appear, got %v", ids)
				}

				// 解释中的属性名与比较值是解码后的文字。
				conds := got.Explanation.Rules[0].Conditions
				if len(conds) != 2 {
					t.Fatalf("deciding rule must keep both conditions: %d", len(conds))
				}
				if conds[0].Attribute != "地区" || conds[0].CompareValue != "中" ||
					conds[0].ActualValue == nil || *conds[0].ActualValue != "中" ||
					conds[0].Missing || !conds[0].Match {
					t.Fatalf("eq condition record wrong: %+v", conds[0])
				}
				list, ok := conds[1].CompareValue.([]string)
				if !ok || len(list) != 5 ||
					list[0] != "é" || list[1] != "😀" || list[2] != "" || list[3] != "a" || list[4] != "a" {
					t.Fatalf("in compareValue must decode to text and keep empty/dup/order: %#v", conds[1].CompareValue)
				}
				if conds[1].Attribute != "emoji🎨" ||
					conds[1].ActualValue == nil || *conds[1].ActualValue != "😀" ||
					conds[1].Missing || !conds[1].Match {
					t.Fatalf("in condition record wrong: %+v", conds[1])
				}

				// outcome 一句话中引用的规则编号也是解码后的文字。
				if !strings.Contains(got.Explanation.Outcome, "r-中é😀") {
					t.Fatalf("outcome must name the decoded rule id: %q", got.Explanation.Outcome)
				}

				out := mustMarshalExplain(t, got)
				// 输出必须直接携带解码后的非 ASCII 文字，而不是 \uXXXX 外形。
				for _, frag := range []string{explSpecDecodedKey, `"r-中é😀"`, `"地区"`, `"中"`, `"emoji🎨"`, `"😀"`, `"é"`} {
					if !bytes.Contains(out, []byte(frag)) {
						t.Fatalf("explain output must carry decoded text %q: %s", frag, out)
					}
				}
				if bytes.Contains(out, []byte(`\u`)) {
					t.Fatalf("decoded text must not be re-escaped as \\uXXXX in output: %s", out)
				}
				if ci == 0 && cx.name == "direct" {
					reference = out
				} else if !bytes.Equal(out, reference) {
					t.Fatalf("spelling changed explain output:\n%s\n!= reference:\n%s", out, reference)
				}
			})
		}
	}
}

// TestExplainSpecialStringsActualJudgmentHitAndMiss 让特殊字符串真正参与判断：
// 既包含精确命中（靠前的 false 规则定案），也包含文字不同导致的不命中
// （大小写、首尾空白、预组合 é vs e+U+0301、邻近表情、真字符 vs 反斜杠文本、
// 属性缺失）；不命中时前面的规则保留全部条件判断，求值继续到后一条规则。
func TestExplainSpecialStringsActualJudgmentHitAndMiss(t *testing.T) {
	cfg := mustParseConfig(t, explSpecCfg(explSpecDecodedKey, `地区`))
	flag := cfg.Find(explSpecDecodedKey)

	// 命中：r-中é😀 以 false 立即定案，r-pro/r-unseen 不出现。
	hit := flag.EvaluateExplain(mustParseContext(t, `{"地区":"中","emoji🎨":"a","plan":"pro"}`))
	if hit.Value != false || hit.Reason != EvalRule || hit.RuleID == nil || *hit.RuleID != "r-中é😀" {
		t.Fatalf("special-string match must decide false: %+v", hit.EvalResult)
	}
	if ids := explainedIDs(hit); len(ids) != 1 {
		t.Fatalf("later rules must not appear after a hit: %v", ids)
	}
	// 空字符串作为 in 成员同样命中。
	emptyHit := flag.EvaluateExplain(mustParseContext(t, `{"地区":"中","emoji🎨":""}`))
	if emptyHit.Value != false || emptyHit.RuleID == nil || *emptyHit.RuleID != "r-中é😀" {
		t.Fatalf("empty in member must still satisfy the rule: %+v", emptyHit.EvalResult)
	}

	// 不命中情形：前一条规则保留全部条件（含逐条在案），r-pro 因 plan=pro 命中
	// 而以 true 定案；r-unseen 不出现。badCond 标出前一条规则上判定不成立的
	// 条件（0=eq，1=in）；另一个条件在每个上下文里都成立，必须照样记录为 match。
	missCases := []struct {
		name string
		ctx  string
		// badCond 指向前一条规则上判定为不成立的条件下标（0=eq，1=in）。
		badCond int
	}{
		// eq（地区=中）不成立，in 仍成立：
		{"eq surrounding whitespace", `{"地区":" 中 ","emoji🎨":"😀","plan":"pro"}`, 0},
		{"eq different text", `{"地区":"国","emoji🎨":"é","plan":"pro"}`, 0},
		{"eq missing attribute", `{"emoji🎨":"a","plan":"pro"}`, 0},
		// in（emoji🎨 ∈ 列表）不成立，eq 仍成立：
		{"in uppercase member", `{"地区":"中","emoji🎨":"A","plan":"pro"}`, 1},
		{"in surrounding whitespace", `{"地区":"中","emoji🎨":" é","plan":"pro"}`, 1},
		{"in whitespace vs empty member", `{"地区":"中","emoji🎨":" ","plan":"pro"}`, 1},
		{"in neighboring emoji", `{"地区":"中","emoji🎨":"😃","plan":"pro"}`, 1},
		{"in combining vs precomposed", `{"地区":"中","emoji🎨":"` + explEscCB + `","plan":"pro"}`, 1},
		{"in backslash literal vs decoded", `{"地区":"中","emoji🎨":"\\u00E9","plan":"pro"}`, 1},
		{"in missing attribute", `{"地区":"中","plan":"pro"}`, 1},
	}
	for _, tc := range missCases {
		t.Run(tc.name, func(t *testing.T) {
			got := flag.EvaluateExplain(mustParseContext(t, tc.ctx))
			if got.Value != true || got.Reason != EvalRule || got.RuleID == nil || *got.RuleID != "r-pro" {
				t.Fatalf("special-text miss must continue to r-pro: %+v", got.EvalResult)
			}
			ids := explainedIDs(got)
			if len(ids) != 2 || ids[0] != "r-中é😀" || ids[1] != "r-pro" {
				t.Fatalf("the earlier missed rule must be kept with all conditions: %v", ids)
			}
			if got.Explanation.Rules[0].Match {
				t.Fatalf("earlier rule must be marked miss: %+v", got.Explanation.Rules[0])
			}
			bad := got.Explanation.Rules[0].Conditions[tc.badCond]
			if bad.Match {
				t.Fatalf("condition %d must be the recorded miss: %+v", tc.badCond, bad)
			}
			// 另一个条件若成立也要照样在案，不能只列到第一个失败条件。
			good := got.Explanation.Rules[0].Conditions[1-tc.badCond]
			if !good.Match {
				t.Fatalf("the other condition must still be evaluated and recorded: %+v", good)
			}
		})
	}

	// 三条规则都不成立：解释按配置顺序列出全部规则（含 r-unseen）的全部条件，
	// 取 default=true。
	none := flag.EvaluateExplain(mustParseContext(t, `{"地区":"其他","emoji🎨":"z","plan":"free"}`))
	if none.Reason != EvalDefault || none.RuleID != nil || none.Value != true {
		t.Fatalf("no rule holds must use default: %+v", none.EvalResult)
	}
	if ids := explainedIDs(none); len(ids) != 3 ||
		ids[0] != "r-中é😀" || ids[1] != "r-pro" || ids[2] != "r-unseen" {
		t.Fatalf("all rules must be shown when none match: %v", ids)
	}
}

// TestExplainSpecialStringAttributeMissingVsEmpty 验证含特殊字符的属性名也
// 准确区分缺失与空值：未提供时 actualValue 为 null、missing 为 true；显式
// 空字符串保留 ""、missing 为 false。属性名分别用直接字符与转义两种写法，
// 查找必须指向同一属性。
func TestExplainSpecialStringAttributeMissingVsEmpty(t *testing.T) {
	cases := []struct {
		name string
		attr string // 属性名的 JSON 字符串内容
	}{
		{"chinese direct", `地区`},
		{"chinese escaped", explEscDI + explEscQU},
		{"emoji direct", `emoji🎨`},
		{"emoji escaped", `emoji` + explEscGrin},
		{"accent nfd", `caf` + explEscCB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[
					{"attribute":"` + tc.attr + `","op":"eq","value":""}]}]}]}`
			flag := mustParseConfig(t, cfg).Find("f")

			// 属性缺失：actualValue 为 nil、missing=true、match=false。
			missing := flag.EvaluateExplain(mustParseContext(t, `{"其他":"x"}`))
			c0 := missing.Explanation.Rules[0].Conditions[0]
			if !c0.Missing || c0.ActualValue != nil || c0.Match {
				t.Fatalf("missing special-named attribute: %+v", c0)
			}

			// 显式空字符串：actualValue 指向 ""、missing=false、match=true。
			// 属性名用与配置相同的解码文字（这里统一用直接字符）书写。
			decodedAttr := decodeJSONText(t, `"`+tc.attr+`"`)
			ctx := `{"` + decodedAttr + `":""}`
			empty := flag.EvaluateExplain(mustParseContext(t, ctx))
			c1 := empty.Explanation.Rules[0].Conditions[0]
			if c1.Missing || c1.ActualValue == nil || *c1.ActualValue != "" || !c1.Match {
				t.Fatalf("explicit empty value on special-named attribute: %+v", c1)
			}
		})
	}
}

// decodeJSONText 把一个 JSON 字符串字面量解码为 Go 字符串，仅供测试拼上下文用。
func decodeJSONText(t *testing.T, lit string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal([]byte(lit), &s); err != nil {
		t.Fatalf("decode %q: %v", lit, err)
	}
	return s
}

// TestExplainSpecialStringsSameResultAsPlainMode 验证同一输入在普通模式与
// 解释模式下 key、value、reason、ruleId 四项必须一致，覆盖特殊字符串的命中
// （含靠前 false 定案）、文字不命中而落到后一条、全部不命中取默认值等路径。
func TestExplainSpecialStringsSameResultAsPlainMode(t *testing.T) {
	contexts := []string{
		`{"地区":"中","emoji🎨":"é","plan":"pro"}`,                            // 前一条 false 定案
		`{"` + explEscDI + explEscQU + `":"中","emoji🎨":"z","plan":"pro"}`, // 前一条 in 不成立，r-pro 定案
		`{"地区":"中","emoji🎨":" A ","plan":"free"}`,                         // 都不成立取默认值
		`{"地区":"中","plan":"pro"}`,                                         // in 属性缺失：前一条不成立
		`{}`,                                                              // 全缺失：默认值
	}
	for i, ctxRaw := range contexts {
		t.Run(ctxRaw, func(t *testing.T) {
			cfg := mustParseConfig(t, explSpecKeySpellings[i%len(explSpecKeySpellings)].raw)
			flag := cfg.Find(explSpecDecodedKey)
			ctx := mustParseContext(t, ctxRaw)
			plain := flag.Evaluate(ctx)
			explained := flag.EvaluateExplain(ctx)
			if !explained.EvalResult.Equals(plain) {
				t.Fatalf("case %d: explained %+v != plain %+v", i, explained.EvalResult, plain)
			}
			// 两种模式各自序列化后，前四字段文本必须逐字一致。
			pOut := mustMarshalResult(t, plain)
			eOut := mustMarshalExplain(t, explained)
			// 解释输出在 ruleId 之后紧跟 ,"explanation"；截到该处并补 } 得到
			// 与普通模式相同的四字段对象文本。
			head := func(b []byte) []byte {
				if i := bytes.Index(b, []byte(`,"explanation"`)); i >= 0 {
					return append(append([]byte{}, b[:i]...), '}')
				}
				return b
			}
			if !bytes.Equal(head(eOut), pOut) {
				t.Fatalf("key/value/reason/ruleId differ:\nexplain: %s\nplain:   %s", head(eOut), pOut)
			}
		})
	}
}

// mustMarshalResult 是 MarshalResult 的测试辅助。
func mustMarshalResult(t *testing.T, r EvalResult) []byte {
	t.Helper()
	b, err := MarshalResult(r)
	if err != nil {
		t.Fatalf("MarshalResult failed: %v", err)
	}
	return b
}

// TestExplainSpecialStringsJSONRoundTrip 验证引号、反斜杠以及合法转义表示的
// 换行符与制表符进入字符串后，解释输出仍是单行、可解析的一个 JSON 对象；
// 重新读取得到的文字与输入解码后的内容完全相同——不丢字符、不重复转义，
// 字符串内容也不会变成结果对象的新字段。
func TestExplainSpecialStringsJSONRoundTrip(t *testing.T) {
	// 配置侧比较值与上下文字段名都覆盖：引号、反斜杠、换行、制表符。
	const special = `a"b\c` + "\n\t" + "中😀"
	cfg := mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-特殊","value":true,"conditions":[
			{"attribute":"line\nbreak\t名","op":"eq","value":"a\"b\\c\n\t中😀"},
			{"attribute":"plan","op":"in","value":["x\ny","","a\"a","\\u0041"]}]}]}]}`)
	flag := cfg.Find("f")
	ctx := mustParseContext(t, `{"line\nbreak\t名":"a\"b\\c\n\t中😀","plan":"x\ny"}`)

	got := flag.EvaluateExplain(ctx)
	if got.Reason != EvalRule || got.RuleID == nil || *got.RuleID != "r-特殊" {
		t.Fatalf("special strings must match: %+v", got.EvalResult)
	}
	out := mustMarshalExplain(t, got)

	// 输出为单行：除结尾换行（已被 TrimRight）外不得含裸换行；JSON 里的换行/
	// 制表符必须以 \n、\t 转义形式出现，引号与反斜杠也必须转义。
	if bytes.ContainsAny(out, "\n\r") {
		t.Fatalf("explain output must stay on one line: %q", out)
	}
	if bytes.Contains(out, []byte("\t")) {
		t.Fatalf("raw tab must be escaped in output: %q", out)
	}

	// 输出是单个可解析 JSON 对象，且只有既有字段，不新增任何字段。
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, out)
	}
	for _, k := range []string{"key", "value", "reason", "ruleId", "explanation"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("missing field %q in %s", k, out)
		}
	}
	if len(obj) != 5 {
		t.Fatalf("string content must not become new fields: %v", obj)
	}

	// 经 JSON 往返后，解释中的文字与输入解码后的内容逐字相同。
	conds := got.Explanation.Rules[0].Conditions
	if conds[0].Attribute != "line\nbreak\t名" {
		t.Fatalf("attribute did not survive: %q", conds[0].Attribute)
	}
	if conds[0].CompareValue != special ||
		conds[0].ActualValue == nil || *conds[0].ActualValue != special {
		t.Fatalf("eq compare/actual value did not survive: compare=%q actual=%v",
			conds[0].CompareValue, conds[0].ActualValue)
	}
	list := conds[1].CompareValue.([]string)
	// 末项 JSON 文本 "\\u0041" 解码后是“反斜杠加 u0041”的普通文本（6 个字符，
	// Go 中写作 "\\u0041"），不会被二次解码成 A——这正是“转义只发生一次”的边界。
	if len(list) != 4 || list[0] != "x\ny" || list[1] != "" || list[2] != `a"a` ||
		list[3] != "\\u0041" {
		t.Fatalf("in list did not survive decoding: %#v", list)
	}
	if *conds[1].ActualValue != "x\ny" || !conds[1].Match {
		t.Fatalf("in actual value did not survive: %+v", conds[1])
	}

	// 通过重新解析输出文本核对一次，防止只在 Go 对象层面相等、JSON 文本却损坏。
	var reparsed struct {
		Explanation struct {
			Rules []struct {
				Conditions []struct {
					Attribute    string  `json:"attribute"`
					CompareValue any     `json:"compareValue"`
					ActualValue  *string `json:"actualValue"`
				} `json:"conditions"`
			} `json:"rules"`
		} `json:"explanation"`
	}
	if err := json.Unmarshal(out, &reparsed); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	c0 := reparsed.Explanation.Rules[0].Conditions[0]
	if c0.Attribute != "line\nbreak\t名" || c0.CompareValue != special ||
		c0.ActualValue == nil || *c0.ActualValue != special {
		t.Fatalf("reparsed text drifted: %+v", c0)
	}

	// 数据字段中的反斜杠必须恰好转义一次：能看到 \n、\t、\"、\\ 形态的内容。
	// outcome 文案中的规则编号另有 Go %q 渲染，不在此断言范围内。
	s := string(out)
	for _, frag := range []string{
		`"attribute":"line\nbreak\t名"`,
		`"compareValue":"a\"b\\c\n\t中😀"`,
		`"actualValue":"a\"b\\c\n\t中😀"`,
		`"compareValue":["x\ny","","a\"a","\\u0041"]`,
	} {
		if !strings.Contains(s, frag) {
			t.Fatalf("output must contain properly escaped %q: %s", frag, s)
		}
	}
}
