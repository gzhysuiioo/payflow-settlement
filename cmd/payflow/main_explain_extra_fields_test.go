package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件从命令行入口为 evaluate --explain 补充“配置附加信息（负责人、备注、
// 标签等治理字段）不参与求值”的端到端回归保障。业务行为由 payflow 包的单测
// 保障，这里锁定进程级契约：
//   - 开关定义、规则顺序、比较值与上下文不变，只增删改合法附加字段时，
//     --explain 的成功 stdout 必须逐字节一致（四字段结果与完整 explanation），
//     获胜规则与被考虑规则集合不变，条件的比较值/实际值/缺失标记/判断不变；
//   - 附加信息不出现在结果或解释里，也不会被当作上下文属性补齐缺失值；
//     上下文显式空字符串与属性缺失严格区分；普通模式仍只输出四个既有字段；
//   - 嵌套对象/数组与 1e400 这样的合法大数字作为附加信息被接受；
//   - 整份配置先校验：未被选中或已关闭开关的附加对象在合法大数字之后重复声明
//     同名字段时，--explain 必须非零退出、stdout 为空（不先输出结果或半份
//     解释），stderr 点名重复字段与具体位置，且不误报为数字过大。

// explainExtraBaseConfig 的业务形态：f 启用、default=true，r-block 在前返回
// false（region eq "cn" 与 plan eq "pro" 两个条件 AND），r-allow 在后返回
// true（tier in ["","a","a"]，含空字符串与重复成员）；off 关闭，含一条本会
// 命中的 r-never。两条 f 规则可同时成立，用于区分“靠前规则命中 false 立即
// 定案、解释只到首条命中规则”与“靠前规则不成立、后续才命中、解释保留此前
// 未命中规则及其全部条件”。
const explainExtraBaseConfig = `{"flags":[
	{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-block","value":false,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"},
			{"attribute":"plan","op":"eq","value":"pro"}]},
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"tier","op":"in","value":["","a","a"]}]}]},
	{"key":"off","enabled":false,"default":false,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`

// explainExtraRichConfig 在顶层、开关、规则、条件四个位置都放附加字段：字符串、
// 布尔、null、嵌套对象/数组，以及 1e400 与 400 位整数。业务字段与基准逐字相同。
func explainExtraRichConfig() string {
	hugeInt := "1" + strings.Repeat("0", 400)
	return `{"owner":"payments-platform","schemaVersion":2,"topMeta":{"big":1e400},"topTags":[1,` + hugeInt + `,{"x":1e400}],"flags":[
	{"key":"f","enabled":true,"default":true,
		"owner":"@alice","labels":["checkout","rollout"],"rolloutWeight":1e400,"archived":null,"needsReview":true,
		"rules":[
		{"id":"r-block","value":false,"note":"命中即拒绝","review":{"required":true,"approvers":["@alice","@bob"],"budget":1e400},"conditions":[
			{"attribute":"region","op":"eq","value":"cn","rationale":null,"who":{"team":"growth","nodes":[1,2,{"deep":true}]}},
			{"attribute":"plan","op":"eq","value":"pro","hint":["p",1e400,{"deep":[true,null,{}]}]}]},
		{"id":"r-allow","value":true,"note":"允许放行","conditions":[
			{"attribute":"tier","op":"in","value":["","a","a"],"weight":` + hugeInt + `}]}]},
	{"key":"off","enabled":false,"default":false,"meta":{"big":1e400,"nested":{"a":[1,2,{"b":null}]}},"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
}

// explainExtraAltConfig 相对 rich 版修改、删除、新增了一批附加字段，业务字段
// 完全不变。
const explainExtraAltConfig = `{"owner":"growth-platform","footer":[1,2,3],"flags":[
	{"key":"f","enabled":true,"default":true,"owner":"@carol","rolloutWeight":-1e400,"rules":[
		{"id":"r-block","value":false,"review":{"required":false},"conditions":[
			{"attribute":"region","op":"eq","value":"cn","weight":2},
			{"attribute":"plan","op":"eq","value":"pro"}]},
		{"id":"r-allow","value":true,"since":"2026","conditions":[
			{"attribute":"tier","op":"in","value":["","a","a"]}]}]},
	{"key":"off","enabled":false,"default":false,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`

// explainExtraShadowConfig 的附加字段刻意与上下文属性同名：顶层、开关、规则、
// 条件以及嵌套对象/数组里都写了 region/plan/tier，值都是能直接命中规则的
// "cn"/"pro"/"a"。求值只能读上下文文件，这些影子值一个都不许补齐缺失属性。
const explainExtraShadowConfig = `{"region":"cn","plan":"pro","tier":"a","owner":{"region":"cn"},"flags":[
	{"key":"f","enabled":true,"default":true,
		"region":"cn","plan":"pro","tier":"a",
		"review":{"region":"cn","plan":"pro","tier":"a","approvers":[{"region":"cn"}]},
		"rules":[
		{"id":"r-block","value":false,"region":"cn","plan":"pro","conditions":[
			{"attribute":"region","op":"eq","value":"cn","region":"cn","meta":{"region":"cn"}},
			{"attribute":"plan","op":"eq","value":"pro","plan":"pro","hits":["region",{"plan":"pro"}]}]},
		{"id":"r-allow","value":true,"tier":"a","conditions":[
			{"attribute":"tier","op":"in","value":["","a","a"],"tier":"a"}]}]},
	{"key":"off","enabled":false,"default":false,"region":"cn","plan":"pro","rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`

// explainExtraLeakMarkers 是只可能来自附加信息的片段：业务字段与解释固定
// 文案中都不会出现。1e400 原样渲染会带来 "e400"；400 位整数会带来成串的 0。
func explainExtraLeakMarkers() []string {
	return []string{
		"@alice", "@carol", "payments-platform", "growth-platform",
		"rolloutWeight", "approvers", "rationale", "needsReview",
		"命中即拒绝", "允许放行", "e400", "E400", strings.Repeat("0", 50),
	}
}

// explainExtraConds 取出解释中某条规则的条件记录列表。
func explainExtraConds(t *testing.T, rule map[string]any) []map[string]any {
	t.Helper()
	raw, ok := rule["conditions"].([]any)
	if !ok {
		t.Fatalf("rule conditions must be an array: %v", rule)
	}
	conds := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		conds = append(conds, c.(map[string]any))
	}
	return conds
}

// explainExtraRun 以给定参数运行命令，返回退出码与 stdout/stderr 原文。
func explainExtraRun(t *testing.T, h *harness, argv []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(argv, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// explainExtraExpected 描述一条决策路径的结果形态。
type explainExtraExpected struct {
	name      string
	key       string
	ctx       string
	ruleID    any // nil 表示 ruleId 为 null
	value     bool
	reason    string
	ruleIDs   []string
	ruleMatch []bool
}

// TestCLIEvaluateExplainExtraFieldsByteStable 是核心端到端回归：在多种决策形态
// （靠前命中 false 立即停止、靠前未命中后续命中、属性缺失穿透、显式空字符串、
// 全部落空走 default、关闭固定 false）下，只增删改附加字段时 --explain 的
// stdout 必须与无附加字段时逐字节一致；同时检查四字段结果、被考虑规则集合、
// 条件的比较值/实际值/缺失标记/判断以及输出中不含任何附加信息。
func TestCLIEvaluateExplainExtraFieldsByteStable(t *testing.T) {
	cases := []explainExtraExpected{
		{"first rule hits false and stops", "f", `{"region":"cn","plan":"pro","tier":"a"}`,
			"r-block", false, "rule", []string{"r-block"}, []bool{true}},
		{"first rule misses later hits", "f", `{"region":"us","plan":"free","tier":"a"}`,
			"r-allow", true, "rule", []string{"r-block", "r-allow"}, []bool{false, true}},
		{"missing attribute falls through", "f", `{"plan":"pro","tier":"a"}`,
			"r-allow", true, "rule", []string{"r-block", "r-allow"}, []bool{false, true}},
		{"explicit empty strings", "f", `{"region":"","plan":"","tier":""}`,
			"r-allow", true, "rule", []string{"r-block", "r-allow"}, []bool{false, true}},
		{"no rule matches uses default", "f", `{"region":"us","plan":"x","tier":"z"}`,
			nil, true, "default", []string{"r-block", "r-allow"}, []bool{false, false}},
		{"disabled flag fixed false", "off", `{"region":"cn","plan":"pro","tier":"a"}`,
			nil, false, "disabled", []string{}, nil},
	}
	variants := []struct {
		name string
		raw  string
	}{
		{"rich", explainExtraRichConfig()},
		{"modified", explainExtraAltConfig},
		{"shadow", explainExtraShadowConfig},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseH := newHarness(t, explainExtraBaseConfig, tc.ctx)
			_, reference, refErr := explainExtraRun(t, baseH, baseH.evalExplainArgs(tc.key))
			if refErr != "" {
				t.Fatalf("baseline explain stderr must be empty: %q", refErr)
			}
			reference = strings.TrimSpace(reference)

			// 基准自身的结构断言。
			explainExtraAssertShape(t, "baseline", reference, tc)

			for _, v := range variants {
				t.Run(v.name, func(t *testing.T) {
					h := newHarness(t, v.raw, tc.ctx)

					// 解释模式：成功、单行、与基准逐字节一致。
					code, stdout, stderrStr := explainExtraRun(t, h, h.evalExplainArgs(tc.key))
					if code != 0 {
						t.Fatalf("%s: explain exit=%d stderr=%s", v.name, code, stderrStr)
					}
					if stderrStr != "" {
						t.Fatalf("%s: stderr must be empty on success: %q", v.name, stderrStr)
					}
					out := strings.TrimSpace(stdout)
					if strings.Contains(out, "\n") {
						t.Fatalf("%s: stdout must be a single line: %q", v.name, out)
					}
					if out != reference {
						t.Fatalf("%s: explain output drifted from no-extra baseline:\ngot:  %s\nwant: %s",
							v.name, out, reference)
					}
					explainExtraAssertShape(t, v.name, out, tc)
					for _, leak := range explainExtraLeakMarkers() {
						if strings.Contains(out, leak) {
							t.Fatalf("%s: explanation must not carry extra content %q: %s", v.name, leak, out)
						}
					}

					// 普通模式仍只有四个既有字段，且与解释模式前四字段逐字一致。
					pCode, pOut, pErr := explainExtraRun(t, h, h.evalArgs(tc.key))
					if pCode != 0 || pErr != "" {
						t.Fatalf("%s: plain exit=%d stderr=%q", v.name, pCode, pErr)
					}
					plain := strings.TrimSpace(pOut)
					if strings.Contains(plain, "explanation") {
						t.Fatalf("%s: plain mode must not include explanation: %s", v.name, plain)
					}
					var pm map[string]any
					if err := json.Unmarshal([]byte(plain), &pm); err != nil {
						t.Fatalf("%s: plain output is not JSON: %v (%s)", v.name, err, plain)
					}
					if len(pm) != 4 {
						t.Fatalf("%s: plain output must keep exactly 4 fields, got %d: %s",
							v.name, len(pm), plain)
					}
					var em map[string]any
					if err := json.Unmarshal([]byte(out), &em); err != nil {
						t.Fatalf("%s: explain output is not JSON: %v", v.name, err)
					}
					for _, k := range []string{"key", "value", "reason", "ruleId"} {
						// 用 JSON 再编码归一化后比较，避免类型表示差异。
						if jsonNorm(t, pm[k]) != jsonNorm(t, em[k]) {
							t.Fatalf("%s: field %s differs between plain and explain: %v vs %v",
								v.name, k, pm[k], em[k])
						}
					}
					for _, leak := range explainExtraLeakMarkers() {
						if strings.Contains(plain, leak) {
							t.Fatalf("%s: plain output must not carry extra content %q: %s",
								v.name, leak, plain)
						}
					}
				})
			}
		})
	}
}

// explainExtraAssertShape 解析一条解释输出并核对：顶层五个字段；四字段结果；
// explanation 恰有 outcome/rules 两个字段；规则编号、命中标记按序匹配；每条
// 规则恰三个字段、每个条件恰六个字段。
func explainExtraAssertShape(t *testing.T, label, out string, tc explainExtraExpected) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%s: stdout is not one JSON object: %v (%q)", label, err, out)
	}
	if len(got) != 5 {
		t.Fatalf("%s: top level must have exactly 5 fields, got %d: %s", label, len(got), out)
	}
	if got["key"] != tc.key || got["value"] != tc.value ||
		got["reason"] != tc.reason || !equalsJSON(got["ruleId"], tc.ruleID) {
		t.Fatalf("%s: result fields wrong: key=%v value=%v reason=%v ruleId=%v",
			label, got["key"], got["value"], got["reason"], got["ruleId"])
	}
	exp, ok := got["explanation"].(map[string]any)
	if !ok || len(exp) != 2 {
		t.Fatalf("%s: explanation must have exactly 2 fields: %s", label, out)
	}
	rules := rulesOf(t, got)
	if len(rules) != len(tc.ruleIDs) {
		t.Fatalf("%s: considered rules = %d, want %d (%v)", label, len(rules), len(tc.ruleIDs), rules)
	}
	for i, wantID := range tc.ruleIDs {
		if rules[i]["ruleId"] != wantID {
			t.Fatalf("%s: rules[%d].ruleId = %v, want %v", label, i, rules[i]["ruleId"], wantID)
		}
		if rules[i]["match"] != tc.ruleMatch[i] {
			t.Fatalf("%s: rules[%d].match = %v, want %v", label, i, rules[i]["match"], tc.ruleMatch[i])
		}
		if len(rules[i]) != 3 {
			t.Fatalf("%s: rule record must have exactly 3 fields: %v", label, rules[i])
		}
		for _, c := range explainExtraConds(t, rules[i]) {
			if len(c) != 6 {
				t.Fatalf("%s: condition record must have exactly 6 fields: %v", label, c)
			}
		}
	}
}

// equalsJSON 比较解码后的 ruleId（nil 对应 JSON null）。
func equalsJSON(got, want any) bool {
	return jsonNorm(nil, got) == jsonNorm(nil, want)
}

// jsonNorm 把解码后的值重新编码为 JSON 文本用于归一化比较。
func jsonNorm(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		if t != nil {
			t.Fatalf("marshal %v: %v", v, err)
		}
		return ""
	}
	return string(b)
}

// explainShadowCondExpectation 描述解释中 (规则下标, 条件下标) 对应条件应有的
// actualValue（nil 表示缺失）、missing 与 match。
type explainShadowCondExpectation struct {
	actual  any
	missing bool
	match   bool
}

// explainShadowExpected 描述影子附加信息场景下一个上下文上的完整期望。
type explainShadowExpected struct {
	name    string
	ctx     string
	reason  string
	value   bool
	ruleID  any
	ruleIDs []string
	conds   map[[2]int]explainShadowCondExpectation
}

// TestCLIEvaluateExplainShadowAttributeMissingVsEmpty 端到端锁定附加信息与上下
// 文属性同名时的边界：上下文缺什么属性，解释里就必须 actualValue=null、
// missing=true、该条件不成立，即使附加对象在四个位置及嵌套结构里都写了能命中
// 的值；显式空字符串则保留 ""、missing=false；影子版与无附加版输出逐字节一致。
func TestCLIEvaluateExplainShadowAttributeMissingVsEmpty(t *testing.T) {
	cases := []explainShadowExpected{
		{
			name:    "all missing despite shadow values",
			ctx:     `{}`,
			reason:  "default",
			value:   true,
			ruleID:  nil,
			ruleIDs: []string{"r-block", "r-allow"},
			conds: map[[2]int]explainShadowCondExpectation{
				{0, 0}: {nil, true, false}, // region 缺失
				{0, 1}: {nil, true, false}, // plan 缺失
				{1, 0}: {nil, true, false}, // tier 缺失
			},
		},
		{
			name:    "plan present region missing",
			ctx:     `{"plan":"pro","tier":"a"}`,
			reason:  "rule",
			value:   true,
			ruleID:  "r-allow",
			ruleIDs: []string{"r-block", "r-allow"},
			conds: map[[2]int]explainShadowCondExpectation{
				{0, 0}: {nil, true, false},
				{0, 1}: {"pro", false, true},
				{1, 0}: {"a", false, true},
			},
		},
		{
			name:    "explicit empty strings not missing",
			ctx:     `{"region":"","plan":"","tier":""}`,
			reason:  "rule",
			value:   true,
			ruleID:  "r-allow",
			ruleIDs: []string{"r-block", "r-allow"},
			conds: map[[2]int]explainShadowCondExpectation{
				{0, 0}: {"", false, false},
				{0, 1}: {"", false, false},
				{1, 0}: {"", false, true}, // "" 是 in 列表成员
			},
		},
		{
			name:    "region hits plan wrong tier missing",
			ctx:     `{"region":"cn","plan":"free"}`,
			reason:  "default",
			value:   true,
			ruleID:  nil,
			ruleIDs: []string{"r-block", "r-allow"},
			conds: map[[2]int]explainShadowCondExpectation{
				{0, 0}: {"cn", false, true},
				{0, 1}: {"free", false, false},
				{1, 0}: {nil, true, false},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 无附加字段基准的解释原文。
			baseH := newHarness(t, explainExtraBaseConfig, tc.ctx)
			bCode, bOutRaw, bErr := explainExtraRun(t, baseH, baseH.evalExplainArgs("f"))
			if bCode != 0 || bErr != "" {
				t.Fatalf("baseline explain failed: code=%d stderr=%q", bCode, bErr)
			}
			baseOut := strings.TrimSpace(bOutRaw)

			// 影子配置的解释：成功且与基准逐字节一致——影子值既没补齐缺失，
			// 也没改任何比较值/实际值/判断。
			h := newHarness(t, explainExtraShadowConfig, tc.ctx)
			sCode, sOutRaw, sErr := explainExtraRun(t, h, h.evalExplainArgs("f"))
			if sCode != 0 || sErr != "" {
				t.Fatalf("shadow explain failed: code=%d stderr=%q", sCode, sErr)
			}
			shadowOut := strings.TrimSpace(sOutRaw)
			if shadowOut != baseOut {
				t.Fatalf("shadow explain must equal no-extra baseline:\nshadow: %s\nbase:   %s",
					shadowOut, baseOut)
			}

			var got map[string]any
			if err := json.Unmarshal([]byte(shadowOut), &got); err != nil {
				t.Fatalf("shadow output is not JSON: %v (%q)", err, shadowOut)
			}
			if got["reason"] != tc.reason || got["value"] != tc.value ||
				!equalsJSON(got["ruleId"], tc.ruleID) {
				t.Fatalf("result fields wrong: %v", got)
			}
			rules := rulesOf(t, got)
			if len(rules) != len(tc.ruleIDs) {
				t.Fatalf("rules = %v, want %v", rules, tc.ruleIDs)
			}
			for i, id := range tc.ruleIDs {
				if rules[i]["ruleId"] != id {
					t.Fatalf("rules[%d].ruleId = %v, want %v", i, rules[i]["ruleId"], id)
				}
			}
			for key, want := range tc.conds {
				ri, ci := key[0], key[1]
				c := explainExtraConds(t, rules[ri])[ci]
				if c["missing"] != want.missing {
					t.Fatalf("rules[%d].conds[%d] missing = %v, want %v: %v",
						ri, ci, c["missing"], want.missing, c)
				}
				if c["match"] != want.match {
					t.Fatalf("rules[%d].conds[%d] match = %v, want %v: %v",
						ri, ci, c["match"], want.match, c)
				}
				if !equalsJSON(c["actualValue"], want.actual) {
					t.Fatalf("rules[%d].conds[%d] actualValue = %v, want %v (must not be filled from extra fields)",
						ri, ci, c["actualValue"], want.actual)
				}
			}
		})
	}
}

// TestCLIEvaluateExplainNestedExtraAndBigNumbersAccepted 显式锁定附加信息中的
// 嵌套对象、数组以及 1e400/-1e400/400 位整数都被 --explain 接受：退出码 0、
// stderr 为空、stdout 单行成功对象，且这些内容不成为比较值或解释内容。
func TestCLIEvaluateExplainNestedExtraAndBigNumbersAccepted(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	config := `{"top":{"deep":[1e400,{"k":[true,null,{}]}]},"flags":[
		{"key":"f","enabled":true,"default":false,"flagBig":1e400,"flagList":[` + hugeInt + `,-1e400],"rules":[
			{"id":"r-block","value":false,"ruleMeta":{"n":1e400,"arr":[{"x":` + hugeInt + `}]},"conditions":[
				{"attribute":"region","op":"eq","value":"cn","condMeta":1e400},
				{"attribute":"plan","op":"eq","value":"pro","deep":{"a":[1e400,{"b":-1e400}]}}]},
			{"id":"r-allow","value":true,"conditions":[
				{"attribute":"tier","op":"in","value":["","a","a"]}]}]}]}`
	h := newHarness(t, config, `{"plan":"pro","region":"cn","tier":"z"}`)

	code, stdout, stderrStr := explainExtraRun(t, h, h.evalExplainArgs("f"))
	if code != 0 {
		t.Fatalf("legal nested extras with big numbers must be accepted: exit=%d stderr=%s", code, stderrStr)
	}
	if stderrStr != "" {
		t.Fatalf("stderr must be empty: %q", stderrStr)
	}
	out := strings.TrimSpace(stdout)
	if strings.Contains(out, "\n") {
		t.Fatalf("stdout must be one line: %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if got["reason"] != "rule" || got["ruleId"] != "r-block" || got["value"] != false {
		t.Fatalf("r-block must win with the business fields unchanged: %v", got)
	}
	rules := rulesOf(t, got)
	if len(rules) != 1 || rules[0]["ruleId"] != "r-block" {
		t.Fatalf("explanation must stop at the first hit r-block: %v", rules)
	}
	conds := explainExtraConds(t, rules[0])
	if conds[0]["compareValue"] != "cn" || conds[1]["compareValue"] != "pro" {
		t.Fatalf("compare values must stay the business strings: %v", conds)
	}
	for _, leak := range []string{"e400", "E400", "flagBig", "condMeta", "ruleMeta", strings.Repeat("0", 50)} {
		if strings.Contains(out, leak) {
			t.Fatalf("big-number/nested extra content must not appear in explanation %q: %s", leak, out)
		}
	}
}

// TestCLIEvaluateExplainRejectsDuplicateAfterBigNumberWholeConfig 锁定整份配置
// 先校验的进程级边界：未被选中或已关闭开关的附加对象在合法大数字之后重复声明
// 同名字段时，--explain 必须非零退出、stdout 完全为空（不先给目标开关结果或
// 半份解释），stderr 指出重复字段原因与具体位置，不能误报为数字过大或 JSON
// 语法错误。普通模式与解释模式的拒绝行为一致。
func TestCLIEvaluateExplainRejectsDuplicateAfterBigNumberWholeConfig(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	goodFlag := `{"key":"good","enabled":true,"default":false,"rules":[
		{"id":"r-pro","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}`
	cases := []struct {
		name    string
		key     string
		config  string
		wantLoc string
	}{
		{
			"duplicate after 1e400 in unselected enabled flag",
			"good",
			`{"flags":[` + goodFlag + `,
				{"key":"bad","enabled":true,"default":false,"rules":[],
					"meta":{"n":1e400,"n":2}}]}`,
			`config.flags[1].meta.n`,
		},
		{
			"duplicate after 400-digit integer in selected disabled flag",
			"off",
			`{"flags":[
				{"key":"off","enabled":false,"default":false,"rules":[],
					"meta":{"n":` + hugeInt + `,"n":2}}]}`,
			`config.flags[0].meta.n`,
		},
		{
			"duplicate after 1e400 in unselected disabled flag",
			"good",
			`{"flags":[` + goodFlag + `,
				{"key":"off","enabled":false,"default":false,"rules":[],
					"meta":{"n":1e400,"n":2}}]}`,
			`config.flags[1].meta.n`,
		},
		{
			"duplicate after 1e400 deep in disabled flag condition extra",
			"good",
			`{"flags":[` + goodFlag + `,
				{"key":"off","enabled":false,"default":false,"rules":[
					{"id":"r-never","value":true,"conditions":[
						{"attribute":"plan","op":"eq","value":"pro",
							"hint":{"deep":{"n":1e400,"n":2}}}]}]}]}`,
			`config.flags[1].rules[0].conditions[0].hint.deep.n`,
		},
		{
			"duplicate after 1e400 in top-level extra object",
			"good",
			`{"meta":{"n":1e400,"n":2},"flags":[` + goodFlag + `]}`,
			`config.meta.n`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []struct {
				name string
				args func(h *harness) []string
			}{
				{"explain", func(h *harness) []string { return h.evalExplainArgs(tc.key) }},
				{"plain", func(h *harness) []string { return h.evalArgs(tc.key) }},
			} {
				t.Run(mode.name, func(t *testing.T) {
					h := newHarness(t, tc.config, `{"plan":"pro"}`)
					code, stdout, stderrStr := explainExtraRun(t, h, mode.args(h))
					if code == 0 {
						t.Fatalf("expected non-zero exit, stdout=%q", stdout)
					}
					// 关键契约：stdout 必须完全为空——不能先输出目标开关的结果，
					// 也不能输出半份解释。
					if stdout != "" {
						t.Fatalf("stdout must be empty on whole-config failure, got %q", stdout)
					}
					msg := stderrStr
					if !strings.Contains(msg, "duplicate field") {
						t.Fatalf("stderr must name a duplicate field: %q", msg)
					}
					if !strings.Contains(msg, tc.wantLoc) {
						t.Fatalf("stderr must point at %s, got %q", tc.wantLoc, msg)
					}
					// 不能误报为数字过大或 JSON 语法错误。
					if strings.Contains(msg, "invalid JSON") {
						t.Fatalf("legal big number must not be reported as invalid JSON: %q", msg)
					}
					lower := strings.ToLower(msg)
					for _, frag := range []string{"too large", "overflow", "out of range", "cannot unmarshal number"} {
						if strings.Contains(lower, frag) {
							t.Fatalf("legal 1e400 must not be reported as a number range error (%q): %q", frag, msg)
						}
					}
				})
			}
		})
	}
}

// TestCLIEvaluateExplainNormalModeFourFieldsExact 用携带丰富附加信息的配置锁定
// 普通模式的精确输出：任何决策形态下都仍是原来的四字段单行 JSON，不含
// explanation，也不含任何负责人/备注/大数字内容。
func TestCLIEvaluateExplainNormalModeFourFieldsExact(t *testing.T) {
	cases := []struct {
		name string
		key  string
		ctx  string
		want string
	}{
		{"first false rule", "f", `{"region":"cn","plan":"pro","tier":"a"}`,
			`{"key":"f","value":false,"reason":"rule","ruleId":"r-block"}`},
		{"later true rule", "f", `{"region":"us","plan":"free","tier":"a"}`,
			`{"key":"f","value":true,"reason":"rule","ruleId":"r-allow"}`},
		{"default", "f", `{}`,
			`{"key":"f","value":true,"reason":"default","ruleId":null}`},
		{"disabled", "off", `{"plan":"pro"}`,
			`{"key":"off","value":false,"reason":"disabled","ruleId":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, cfg := range []struct {
				name string
				raw  string
			}{
				{"rich", explainExtraRichConfig()},
				{"shadow", explainExtraShadowConfig},
			} {
				t.Run(cfg.name, func(t *testing.T) {
					h := newHarness(t, cfg.raw, tc.ctx)
					code, stdout, stderrStr := explainExtraRun(t, h, h.evalArgs(tc.key))
					if code != 0 || stderrStr != "" {
						t.Fatalf("exit=%d stderr=%q", code, stderrStr)
					}
					if got := strings.TrimSpace(stdout); got != tc.want {
						t.Fatalf("normal output with extra fields changed:\ngot:  %q\nwant: %q", got, tc.want)
					}
					for _, leak := range explainExtraLeakMarkers() {
						if strings.Contains(stdout, leak) {
							t.Fatalf("normal output must not carry extra content %q: %q", leak, stdout)
						}
					}
				})
			}
		})
	}
}
