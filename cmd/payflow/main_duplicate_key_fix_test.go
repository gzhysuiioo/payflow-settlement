package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 本文件从命令入口（run）为“两个开关使用同一开关键、后一个开关自身又不合法”
// 的逐处修正过程补充端到端回归保障，并要求普通求值与 --explain 走出完全相同的
// 修正体验：
//
//	后一个开关先后缺少 default、其 in 列表含非字符串项，且与前一个开关重名。
//	每修一处，两种调用都非零退出、stdout 为空（解释模式也不留结果字段或半份
//	explanation）、stderr 只报告当前这一处配置问题；直到后一个开关键改成不同
//	名称，原先请求的合法开关才按既有规则成功求值，两种模式的 key、value、
//	reason、ruleId 完全一致，解释模式继续附带原有 explanation。
//
// 重名按解码后的文字判断，因此直接字符与合法 Unicode 转义（含代理项对）都要
// 覆盖；只调整对象内部字段书写顺序或文档空白，报错阶段、位置与原因保持一致；
// 后一个开关即使已关闭，或用户正在求值另一个合法开关，也必须经过同样的完整
// 校验。求值功能与名称唯一性规则不在本文件修改。

type dupFixSpellingRow struct {
	name    string
	decoded string
	first   string
	second  string
}

var dupFixSpellingRows = []dupFixSpellingRow{
	{"identical direct names", "café", `café`, `café`},
	{"direct then unicode escape", "café", `café`, `café`},
	{"unicode escape then direct", "café", `café`, `café`},
	{"surrogate pair direct then escaped", "a🚀", `a🚀`, `a🚀`},
	{"surrogate pair escaped then direct", "a🚀", `a🚀`, `a🚀`},
}

// dupFixStage 与包内同名概念一致：后一个开关在修正过程中所处的阶段。
type dupFixStage struct {
	missingDefault bool
	badList        bool
	renamed        bool
}

var (
	dupFixStageMissingDefault = dupFixStage{missingDefault: true, badList: true}
	dupFixStageBadList        = dupFixStage{badList: true}
	dupFixStageDuplicateOnly  = dupFixStage{}
	dupFixStageRenamed        = dupFixStage{renamed: true}
)

// dupFixFirstFlag 是始终合法的第一份定义：plan=pro 命中返回 true 的 r-pro。
func dupFixFirstFlag(keyJSON string) string {
	return `{"key":"` + keyJSON + `","enabled":true,"default":false,"rules":[` +
		`{"id":"r-pro","value":true,"conditions":[` +
		`{"attribute":"plan","op":"eq","value":"pro"}]}]}`
}

// dupFixSecondFlag 是逐处修正的对象；renamed 后 key 改为 "other"，其 r-region
// 在 region 属于 ["cn","us"] 时返回 false。
func dupFixSecondFlag(keyJSON string, enabled bool, st dupFixStage) string {
	key := keyJSON
	if st.renamed {
		key = "other"
	}
	var b strings.Builder
	b.WriteString(`{"key":"`)
	b.WriteString(key)
	fmt.Fprintf(&b, `","enabled":%t`, enabled)
	if !st.missingDefault {
		b.WriteString(`,"default":false`)
	}
	b.WriteString(`,"rules":[{"id":"r-region","value":false,"conditions":[`)
	b.WriteString(`{"attribute":"region","op":"in","value":["cn",`)
	if st.badList {
		b.WriteString(`2]}`)
	} else {
		b.WriteString(`"us"]}`)
	}
	b.WriteString(`]}]}`)
	return b.String()
}

// dupFixConfig 按规范紧凑排版拼出两份开关定义；数组内第一份始终在前。
func dupFixConfig(row dupFixSpellingRow, enabled bool, st dupFixStage) string {
	return `{"flags":[` + dupFixFirstFlag(row.first) + `,` +
		dupFixSecondFlag(row.second, enabled, st) + `]}`
}

// dupFixConfigRearranged 与 dupFixConfig 内容相同，但调整了每个对象内部字段的
// 书写顺序并改用换行、制表符、冒号前空白等不同文档空白；开关、规则、条件与
// 列表成员的次序不变，下标 [1] 仍指向同一个（非字符串）成员。
func dupFixConfigRearranged(row dupFixSpellingRow, enabled bool, st dupFixStage) string {
	key := row.second
	if st.renamed {
		key = "other"
	}
	list := `"cn", "us"`
	if st.badList {
		list = `"cn", 2`
	}
	first := "\n\t\t{\n" +
		"\t\t\t\"rules\": [{\"conditions\": [{\"value\": \"pro\", \"op\": \"eq\", \"attribute\": \"plan\"}], \"value\": true, \"id\": \"r-pro\"}],\n" +
		"\t\t\t\"default\": false,\n" +
		"\t\t\t\"enabled\": true,\n" +
		"\t\t\t\"key\": \"" + row.first + "\"\n\t\t}"
	var second strings.Builder
	second.WriteString("\n\t\t{\n")
	second.WriteString("\t\t\t\"rules\": [\n")
	second.WriteString("\t\t\t\t{\"conditions\": [{\"value\": [" + list + "], \"op\": \"in\", \"attribute\": \"region\"}], \"value\": false, \"id\": \"r-region\"}\n")
	second.WriteString("\t\t\t],\n")
	if !st.missingDefault {
		second.WriteString("\t\t\t\"default\": false,\n")
	}
	fmt.Fprintf(&second, "\t\t\t\"enabled\": %t,\n", enabled)
	second.WriteString("\t\t\t\"key\": \"" + key + "\"\n\t\t}\n\t")
	return "{\n\t\"flags\" : [\n " + first + ",\n" + second.String() + " ]\n}\n\n"
}

// dupFixWantError 返回后一个开关位于 flags[flagIndex]、第一份定义位于
// flags[firstIndex] 时各失败阶段应逐字出现的报错。
func dupFixWantError(decoded string, flagIndex, firstIndex int, st dupFixStage) string {
	switch {
	case st.missingDefault:
		return fmt.Sprintf("config.flags[%d].default: field is required", flagIndex)
	case st.badList:
		return fmt.Sprintf("config.flags[%d].rules[0].conditions[0].value[1]: must be a string", flagIndex)
	default:
		return fmt.Sprintf("config.flags[%d].key: duplicate flag key %q (first at flags[%d])",
			flagIndex, decoded, firstIndex)
	}
}

// dupFixContext 同时满足两份定义的规则；三个失败阶段配置先失败，上下文是否能
// 命中规则不影响报错，改名成功后则让第一份定义命中 r-pro。
const dupFixContext = `{"plan":"pro","region":"cn"}`

// dupFixAssertFailureStage 在普通与 --explain 两种模式下各运行一次，要求：
// 非零退出、stdout 为空、stderr 逐字等于当前阶段唯一的报错（两种模式一致），
// 且既不提前暴露后续阶段，也不与“同一对象重复字段”混淆。
func dupFixAssertFailureStage(t *testing.T, h *harness, key, want string) {
	t.Helper()
	plain := strings.TrimRight(evalFailure(t, h, key), "\n")
	if plain != want {
		t.Fatalf("normal mode stderr = %q\nwant %q", plain, want)
	}
	explained := strings.TrimRight(explainFailure(t, h, key), "\n")
	if explained != want {
		t.Fatalf("--explain mode stderr = %q\nwant %q", explained, want)
	}
	if plain != explained {
		t.Fatalf("normal and --explain must report the same error:\nnormal:  %q\nexplain: %q", plain, explained)
	}
	for _, not := range []string{"duplicate field", `"key":"`, `"value":`, "explanation"} {
		if strings.Contains(plain, not) {
			t.Fatalf("error output must not contain %q: %q", not, plain)
		}
	}
}

// TestCLIEvaluateDuplicateKeyFixProgression 端到端锁定完整修正过程。每种同名
// 写法、规范与重排两种排版都要依次经历：缺 default → 列表成员类型错误（位置到
// 成员下标）→ 重复开关键（指向后一次定义并注明第一次出现位置）→ 改名成功。
// 失败阶段两种模式逐字一致；成功后普通模式与 --explain 的 key、value、reason、
// ruleId 完全相同，解释模式另附 explanation。
func TestCLIEvaluateDuplicateKeyFixProgression(t *testing.T) {
	stages := []dupFixStage{dupFixStageMissingDefault, dupFixStageBadList, dupFixStageDuplicateOnly}
	for _, row := range dupFixSpellingRows {
		t.Run(row.name, func(t *testing.T) {
			for _, layout := range []struct {
				name string
				doc  func(dupFixStage) string
			}{
				{"canonical", func(st dupFixStage) string { return dupFixConfig(row, true, st) }},
				{"rearranged", func(st dupFixStage) string { return dupFixConfigRearranged(row, true, st) }},
			} {
				t.Run(layout.name, func(t *testing.T) {
					for _, st := range stages {
						h := newHarness(t, layout.doc(st), dupFixContext)
						dupFixAssertFailureStage(t, h, row.decoded, dupFixWantError(row.decoded, 1, 0, st))
					}

					// 改名后：两种模式都成功。普通模式逐字锁定四字段对象。
					h := newHarness(t, layout.doc(dupFixStageRenamed), dupFixContext)
					plainGot, plainRaw := evalSuccess(t, h, row.decoded)
					if plainGot["key"] != row.decoded || plainGot["value"] != true ||
						plainGot["reason"] != "rule" || plainGot["ruleId"] != "r-pro" {
						t.Fatalf("original key must evaluate to the first definition's r-pro: %v", plainGot)
					}
					if strings.Contains(plainRaw, "explanation") {
						t.Fatalf("normal mode must not include explanation: %q", plainRaw)
					}
					// 解释模式：四字段与普通模式逐项一致，并附带原有 explanation。
					explainedGot := explainSuccess(t, h, row.decoded)
					for _, k := range []string{"key", "value", "reason", "ruleId"} {
						if explainedGot[k] != plainGot[k] {
							t.Fatalf("field %s differs between modes: normal=%v explain=%v",
								k, plainGot[k], explainedGot[k])
						}
					}
					exp, ok := explainedGot["explanation"].(map[string]any)
					if !ok {
						t.Fatalf("explain mode must add an explanation object: %v", explainedGot)
					}
					rules, ok := exp["rules"].([]any)
					if !ok || len(rules) != 1 {
						t.Fatalf("explanation must list the single deciding rule: %v", exp["rules"])
					}
					r0 := rules[0].(map[string]any)
					if r0["ruleId"] != "r-pro" || r0["match"] != true {
						t.Fatalf("explanation rule record wrong: %v", r0)
					}
					if !strings.Contains(exp["outcome"].(string), "r-pro") {
						t.Fatalf("outcome must name the deciding rule: %v", exp["outcome"])
					}

					// 后一份改名定义独立存在，不覆盖第一份定义。
					other, _ := evalSuccess(t, h, "other")
					if other["key"] != "other" || other["value"] != false ||
						other["reason"] != "rule" || other["ruleId"] != "r-region" {
						t.Fatalf("renamed second definition must evaluate independently: %v", other)
					}
				})
			}
		})
	}
}

// TestCLIEvaluateDuplicateKeyFixDisabledLaterFlag 锁定后一个开关即使已关闭也
// 必须经过同样的完整校验：三个失败阶段在两种模式下的报错与启用时逐字一致；
// 改名后请求原开关键仍按第一份定义命中 r-pro，关闭的 "other" 固定 disabled。
func TestCLIEvaluateDuplicateKeyFixDisabledLaterFlag(t *testing.T) {
	for _, row := range dupFixSpellingRows {
		t.Run(row.name, func(t *testing.T) {
			for _, st := range []dupFixStage{dupFixStageMissingDefault, dupFixStageBadList, dupFixStageDuplicateOnly} {
				hEnabled := newHarness(t, dupFixConfig(row, true, st), dupFixContext)
				hDisabled := newHarness(t, dupFixConfig(row, false, st), dupFixContext)
				want := dupFixWantError(row.decoded, 1, 0, st)
				dupFixAssertFailureStage(t, hEnabled, row.decoded, want)
				dupFixAssertFailureStage(t, hDisabled, row.decoded, want)
			}
			h := newHarness(t, dupFixConfig(row, false, dupFixStageRenamed), dupFixContext)
			got, _ := evalSuccess(t, h, row.decoded)
			if got["value"] != true || got["reason"] != "rule" || got["ruleId"] != "r-pro" {
				t.Fatalf("first definition must be unaffected by the disabled second one: %v", got)
			}
			off, _ := evalSuccess(t, h, "other")
			if off["value"] != false || off["reason"] != "disabled" || off["ruleId"] != nil {
				t.Fatalf("renamed disabled flag must stay fixed false: %v", off)
			}
			offExplain := explainSuccess(t, h, "other")
			for _, k := range []string{"key", "value", "reason", "ruleId"} {
				if offExplain[k] != off[k] {
					t.Fatalf("field %s differs between modes: normal=%v explain=%v", k, off[k], offExplain[k])
				}
			}
			rules, _ := offExplain["explanation"].(map[string]any)["rules"].([]any)
			if len(rules) != 0 {
				t.Fatalf("disabled flag must explain with an empty rule list: %v", rules)
			}
		})
	}
}

// TestCLIEvaluateDuplicateKeyFixBlocksUnrelatedFlag 锁定用户正在求值另一个合法
// 开关也不能豁免这对重名开关：在它们前面放一个完全合法的 good（重名对移到
// flags[1]/flags[2]），三个阶段请求 good 都失败，位置随下标移动、第一次出现
// 参照仍是第一份定义（flags[1]）；两种模式一致。改名后 good 与原开关键分别按
// 各自定义成功求值。
func TestCLIEvaluateDuplicateKeyFixBlocksUnrelatedFlag(t *testing.T) {
	good := `{"key":"good","enabled":true,"default":true,"rules":[]}`
	for _, row := range dupFixSpellingRows {
		t.Run(row.name, func(t *testing.T) {
			for _, st := range []dupFixStage{dupFixStageMissingDefault, dupFixStageBadList, dupFixStageDuplicateOnly} {
				doc := `{"flags":[` + good + `,` +
					dupFixFirstFlag(row.first) + `,` +
					dupFixSecondFlag(row.second, true, st) + `]}`
				h := newHarness(t, doc, `{}`)
				dupFixAssertFailureStage(t, h, "good", dupFixWantError(row.decoded, 2, 1, st))
			}
			doc := `{"flags":[` + good + `,` +
				dupFixFirstFlag(row.first) + `,` +
				dupFixSecondFlag(row.second, true, dupFixStageRenamed) + `]}`
			h := newHarness(t, doc, dupFixContext)
			goodGot, _ := evalSuccess(t, h, "good")
			if goodGot["key"] != "good" || goodGot["value"] != true ||
				goodGot["reason"] != "default" || goodGot["ruleId"] != nil {
				t.Fatalf("the unrelated legal flag must evaluate normally after the fix: %v", goodGot)
			}
			firstGot, _ := evalSuccess(t, h, row.decoded)
			if firstGot["ruleId"] != "r-pro" || firstGot["value"] != true {
				t.Fatalf("original key must keep the first definition: %v", firstGot)
			}
			// 确认 stdout 只有一个 JSON 对象（不夹带半份解释或多输出）。
			firstExplain := explainSuccess(t, h, row.decoded)
			if firstExplain["ruleId"] != "r-pro" {
				t.Fatalf("explain mode must keep the same deciding rule: %v", firstExplain)
			}
			if _, ok := firstExplain["explanation"].(map[string]any); !ok {
				t.Fatalf("explain mode must attach the explanation: %v", firstExplain)
			}
		})
	}
}

// TestCLIEvaluateDuplicateKeyFixModesIdenticalOnSuccess 用逐字节输出核对：成功时
// 解释模式输出以普通模式的四字段对象（含键序）为前缀，只在其后追加 explanation。
func TestCLIEvaluateDuplicateKeyFixModesIdenticalOnSuccess(t *testing.T) {
	row := dupFixSpellingRows[1] // direct then unicode escape
	h := newHarness(t, dupFixConfig(row, true, dupFixStageRenamed), dupFixContext)

	var po, pe, xo, xe bytes.Buffer
	if code := run(h.evalArgs(row.decoded), &po, &pe); code != 0 {
		t.Fatalf("normal mode must succeed after the fix: exit=%d stderr=%s", code, pe.String())
	}
	if code := run(h.evalExplainArgs(row.decoded), &xo, &xe); code != 0 {
		t.Fatalf("explain mode must succeed after the fix: exit=%d stderr=%s", code, xe.String())
	}
	if pe.Len() != 0 || xe.Len() != 0 {
		t.Fatalf("stderr must be empty on success: plain=%q explain=%q", pe.String(), xe.String())
	}
	const wantPlain = `{"key":"café","value":true,"reason":"rule","ruleId":"r-pro"}`
	if strings.TrimSpace(po.String()) != wantPlain {
		t.Fatalf("normal mode output = %q, want %q", po.String(), wantPlain)
	}
	// 解释输出复用同一结果对象：去掉普通对象的收尾 '}' 后接上 ,"explanation":{...}}，
	// 即四字段（含键序与值）逐字保留，只追加 explanation。
	wantPrefix := strings.TrimSuffix(wantPlain, "}") + `,"explanation":`
	if !strings.HasPrefix(strings.TrimSpace(xo.String()), wantPrefix) {
		t.Fatalf("explain output must keep the four fields verbatim then add explanation:\n%s", xo.String())
	}
	var explained map[string]any
	if err := json.Unmarshal(xo.Bytes(), &explained); err != nil {
		t.Fatalf("explain output is not valid JSON: %v", err)
	}
	for k, v := range map[string]any{
		"key": "café", "value": true, "reason": "rule", "ruleId": "r-pro",
	} {
		if explained[k] != v {
			t.Fatalf("explain field %s = %v, want %v", k, explained[k], v)
		}
	}
}
