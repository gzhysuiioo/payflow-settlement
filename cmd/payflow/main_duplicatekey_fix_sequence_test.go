package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件从命令行端到端回归“开关键冲突与后一个开关自身错误并存”时，用户逐处
// 修正输入所看到的变化。语法合法、没有重复 JSON 字段的配置里：第一份开关
// 定义合法（café，启用、default=false、规则 r1 在 plan=pro 时返回 true）；
// 后一份开关与它解码后同名（直接字符或合法 \uXXXX 转义），同时缺少 default，
// 其规则 r2 的 in 比较列表含有非字符串成员。修正过程分四步：
//
//  1. 先报后一个开关缺少 default（config.flags[1].default）；
//  2. 补上合法布尔值后，报列表成员类型错误，位置包含开关、规则、条件与
//     成员下标（config.flags[1].rules[0].conditions[0].value[1]）；
//  3. 列表改成非空字符串数组后，才报重复开关键，指向 flags[1].key 并说明
//     第一次出现在 flags[0]；
//  4. 后一个开关键改成不同名字（beta）后，两份定义共同被接受。
//
// 配置仍有错误时，普通求值与 --explain 都必须非零退出、stdout 为空（解释
// 模式也不输出结果或半份 explanation），stderr 逐字一致地报告当前这一处
// 配置问题。请求重名键、请求另一个合法开关 good、以及后一个开关已关闭，
// 都不能绕过完整校验。全部修正后，原先请求的合法开关按既有规则成功求值，
// 两种模式的 key、value、reason、ruleId 相同，解释模式继续附带原有
// explanation。本文件只补充测试，不改变求值功能与名称唯一性规则。

// cliDupEscE 是 é（U+00E9）的 JSON 转义写法；cliDupEscA 是 a（U+0061）的
// JSON 转义写法。反引号字符串拼接保证配置原文里确实是 \uXXXX 序列。
const (
	cliDupEscE = `\u` + `00E9`
	cliDupEscA = `\u` + `0061`
)

// cliDupGoodFlag 是与冲突无关的第三个合法开关：启用、无规则、default=true，
// 用于证明“用户正在求值另一个合法开关”时整份配置仍被完整校验。
const cliDupGoodFlag = `{"key":"good","enabled":true,"default":true,"rules":[]}`

// cliDupFirstFlag 拼出始终合法的第一份定义。keyJSON 为 key 的字符串内容。
func cliDupFirstFlag(keyJSON string) string {
	return `{"key":"` + keyJSON + `","enabled":true,"default":false,"rules":[` +
		`{"id":"r1","value":true,"conditions":[` +
		`{"attribute":"plan","op":"eq","value":"pro"}]}]}`
}

// cliDupSecondFlag 拼出后一个开关：keyJSON 与第一份解码后同名或改为新名字；
// enabledJSON 为布尔原文；defaultJSON 为空串表示省略该字段；listJSON 是
// r2 唯一 in 条件的比较列表；reordered 时开关、规则、条件对象内部字段换一种
// 书写顺序，报错阶段与位置不应改变。r2 在 region 属于列表时返回 false。
func cliDupSecondFlag(keyJSON, enabledJSON, defaultJSON, listJSON string, reordered bool) string {
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

// cliDupConfig 把给定开关文本组成配置文档；padded 时在文档前后加 JSON 空白。
func cliDupConfig(flags []string, padded bool) string {
	doc := `{"flags":[` + strings.Join(flags, ",") + `]}`
	if padded {
		return " \t\r\n" + doc + "\n  \t"
	}
	return doc
}

// cliDupStages 是用户逐处修正的前三个阶段；wantSub/notSub 锁定“当前只报
// 这一处”：后续阶段的问题不得提前出现。
var cliDupStages = []struct {
	name    string
	defJSON string
	list    string
	wantSub []string
	notSub  []string
}{
	{
		name:    "missing default first",
		defJSON: "",
		list:    `["eu",5]`,
		wantSub: []string{`config.flags[1].default`, "field is required"},
		notSub:  []string{"must be a string", "duplicate flag key"},
	},
	{
		name:    "in-list member type error second",
		defJSON: "false",
		list:    `["eu",5]`,
		wantSub: []string{
			`config.flags[1].rules[0].conditions[0].value[1]`,
			"must be a string",
		},
		notSub: []string{"field is required", "duplicate flag key"},
	},
	{
		name:    "duplicate flag key last",
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

// TestCLIEvaluateDuplicateKeyFixSequenceReportsCurrentError 锁定修正前三阶段：
// 普通与 --explain 两种模式都非零退出、stdout 为空、stderr 逐字一致地报告
// 当前这一处配置问题。矩阵覆盖：重名键的直接字符/Unicode 转义写法、对象
// 字段书写顺序、文档空白、以及请求对象是重名键还是另一个合法开关 good。
func TestCLIEvaluateDuplicateKeyFixSequenceReportsCurrentError(t *testing.T) {
	requestSeries := []struct {
		name   string
		config func(first, second string, padded bool) string
		key    string
	}{
		{"requested=dup-key",
			func(first, second string, padded bool) string {
				return cliDupConfig([]string{first, second}, padded)
			}, "café"},
		{"requested=other-valid-flag",
			func(first, second string, padded bool) string {
				return cliDupConfig([]string{first, second, cliDupGoodFlag}, padded)
			}, "good"},
	}
	spellings := []struct {
		name string
		json string
	}{
		{"direct", "café"},
		{"escaped", "caf" + cliDupEscE},
	}
	for _, series := range requestSeries {
		for _, sp := range spellings {
			for _, reordered := range []bool{false, true} {
				for _, padded := range []bool{false, true} {
					for _, st := range cliDupStages {
						label := series.name + "/key=" + sp.name +
							"/reordered=" + cliDupBoolLabel(reordered) +
							"/padded=" + cliDupBoolLabel(padded) + "/" + st.name
						t.Run(label, func(t *testing.T) {
							cfg := series.config(
								cliDupFirstFlag("café"),
								cliDupSecondFlag(sp.json, "true", st.defJSON, st.list, reordered),
								padded)
							h := newHarness(t, cfg, `{"plan":"pro"}`)
							// 两种模式都失败且 stderr 逐字一致；helper 同时锁定
							// 非零退出与 stdout 为空（不留半份 explanation）。
							msg := bothModesFailure(t, h, series.key)
							assertStderr(t, msg, st.wantSub, st.notSub)
						})
					}
				}
			}
		}
	}
}

// TestCLIEvaluateDuplicateKeyFixSequenceDisabledLaterFlag 锁定：后一个开关
// 即使 enabled=false（关闭固定 false、不会被求值），三个阶段的校验一处都
// 不能少；请求重名键或另一个合法开关 good 结果相同。
func TestCLIEvaluateDuplicateKeyFixSequenceDisabledLaterFlag(t *testing.T) {
	requestSeries := []struct {
		name  string
		flags func(first, second string) []string
		key   string
	}{
		{"requested=dup-key",
			func(first, second string) []string { return []string{first, second} },
			"café"},
		{"requested=other-valid-flag",
			func(first, second string) []string { return []string{first, second, cliDupGoodFlag} },
			"good"},
	}
	for _, series := range requestSeries {
		for _, st := range cliDupStages {
			t.Run(series.name+"/"+st.name, func(t *testing.T) {
				cfg := cliDupConfig(series.flags(
					cliDupFirstFlag("café"),
					cliDupSecondFlag("café", "false", st.defJSON, st.list, false)), false)
				h := newHarness(t, cfg, `{"plan":"pro","region":"eu"}`)
				msg := bothModesFailure(t, h, series.key)
				assertStderr(t, msg, st.wantSub, st.notSub)
			})
		}
	}
}

// TestCLIEvaluateDuplicateKeyFixSequenceEscapedFirstFlag 覆盖反向拼写组合：
// 第一份用 é 转义书写、后一份用直接字符，前三阶段报错与直接写法逐字
// 一致（下标仍以数组位置为准，第一次出现仍是 flags[0]）。
func TestCLIEvaluateDuplicateKeyFixSequenceEscapedFirstFlag(t *testing.T) {
	for _, st := range cliDupStages {
		t.Run(st.name, func(t *testing.T) {
			cfg := cliDupConfig([]string{
				cliDupFirstFlag("caf" + cliDupEscE),
				cliDupSecondFlag("café", "true", st.defJSON, st.list, false),
			}, false)
			h := newHarness(t, cfg, `{}`)
			msg := bothModesFailure(t, h, "café")
			assertStderr(t, msg, st.wantSub, st.notSub)
		})
	}
}

// cliDupFixedConfig 拼出全部修正后的三开关配置：café（第一份定义）、
// beta（改名后的第二份定义）、good（无关的合法开关）。
func cliDupFixedConfig(secondKeyJSON string, reordered, padded bool) string {
	return cliDupConfig([]string{
		cliDupFirstFlag("café"),
		cliDupSecondFlag(secondKeyJSON, "true", "true", `["eu","cn"]`, reordered),
		cliDupGoodFlag,
	}, padded)
}

// assertCLIResultFourFields 要求解码结果恰有既有四字段的取值。
func assertCLIResultFourFields(t *testing.T, label string, got map[string]any,
	key string, value bool, reason, ruleID any) {
	t.Helper()
	if got["key"] != key || got["value"] != value || got["reason"] != reason || got["ruleId"] != ruleID {
		t.Fatalf("%s: unexpected payload: %v", label, got)
	}
}

// TestCLIEvaluateDuplicateKeyFixSequenceSuccessAfterAllFixes 锁定第四阶段：
// 全部修正后，普通模式与 --explain 都成功，原先请求的 café 按第一份定义
// 求值（plan=pro 命中 r1 返回 true）；两模式 key/value/reason/ruleId 逐字
// 一致，普通模式没有 explanation，解释模式继续附带原有 explanation（outcome
// 与 r1 的逐条条件判断）。beta 与 good 也各自按自己的定义求值。
func TestCLIEvaluateDuplicateKeyFixSequenceSuccessAfterAllFixes(t *testing.T) {
	h := newHarness(t, cliDupFixedConfig("beta", false, false), `{"plan":"pro"}`)

	// 普通模式：恰好四字段、无 explanation。
	plain, rawPlain := evalSuccess(t, h, "café")
	assertCLIResultFourFields(t, "normal", plain, "café", true, "rule", "r1")
	const wantNormal = `{"key":"café","value":true,"reason":"rule","ruleId":"r1"}`
	if strings.TrimSpace(rawPlain) != wantNormal {
		t.Fatalf("normal output must be exactly the four fields: %q", rawPlain)
	}
	if strings.Contains(rawPlain, "explanation") {
		t.Fatalf("normal mode must not include explanation: %q", rawPlain)
	}

	// 解释模式：前四字段相同，另带原有 explanation。
	explained := explainSuccess(t, h, "café")
	assertCLIResultFourFields(t, "explain", explained, "café", true, "rule", "r1")
	exp, ok := explained["explanation"].(map[string]any)
	if !ok {
		t.Fatalf("explain mode must add an explanation object: %v", explained)
	}
	outcome, _ := exp["outcome"].(string)
	if !strings.Contains(outcome, "r1") || !strings.Contains(strings.ToLower(outcome), "matching rule") {
		t.Fatalf("outcome must name the first matching rule r1: %v", exp["outcome"])
	}
	rules := rulesOf(t, explained)
	if len(rules) != 1 || rules[0]["ruleId"] != "r1" || rules[0]["match"] != true {
		t.Fatalf("explanation must keep only the deciding r1 record: %v", rules)
	}
	conds := rules[0]["conditions"].([]any)
	if len(conds) != 1 {
		t.Fatalf("r1 must show its one condition: %v", conds)
	}
	cond := conds[0].(map[string]any)
	if cond["attribute"] != "plan" || cond["op"] != "eq" ||
		cond["compareValue"] != "pro" || cond["actualValue"] != "pro" ||
		cond["missing"] != false || cond["match"] != true {
		t.Fatalf("r1 condition record changed: %v", cond)
	}

	// 两种模式的四字段逐项一致（对普通输出重新解码后比较）。
	var plainObj map[string]any
	if err := json.Unmarshal([]byte(wantNormal), &plainObj); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"key", "value", "reason", "ruleId"} {
		if explained[k] != plainObj[k] {
			t.Fatalf("field %s differs between modes: normal=%v explain=%v",
				k, plainObj[k], explained[k])
		}
	}

	// 身份保障的直接对照：plan=pro 上下文里第二份定义（beta）不引用 plan，
	// 只会落回 default=true；而 café 得到的是 true/rule/r1——若后一次定义
	// 覆盖了同名开关，就不可能出现这个结果。原开关键仍由第一份定义负责。
	h2 := newHarness(t, cliDupFixedConfig("beta", false, false), `{"region":"eu"}`)
	got, _ := evalSuccess(t, h2, "beta")
	assertCLIResultFourFields(t, "beta normal", got, "beta", false, "rule", "r2")
	gotE := explainSuccess(t, h2, "beta")
	assertCLIResultFourFields(t, "beta explain", gotE, "beta", false, "rule", "r2")
	if er := rulesOf(t, gotE); len(er) != 1 || er[0]["ruleId"] != "r2" {
		t.Fatalf("beta explanation must record r2: %v", er)
	}

	// 无关的第三个开关不受影响：无规则、default=true。
	h3 := newHarness(t, cliDupFixedConfig("beta", false, false), `{}`)
	got, _ = evalSuccess(t, h3, "good")
	assertCLIResultFourFields(t, "good normal", got, "good", true, "default", nil)
	gotE = explainSuccess(t, h3, "good")
	assertCLIResultFourFields(t, "good explain", gotE, "good", true, "default", nil)
	if er := rulesOf(t, gotE); len(er) != 0 {
		t.Fatalf("good must keep an empty explanation rule list: %v", er)
	}

	// 空上下文同样区分两份 default：café=false（第一份），beta=true（第二份）。
	h4 := newHarness(t, cliDupFixedConfig("beta", false, false), `{}`)
	got, _ = evalSuccess(t, h4, "café")
	assertCLIResultFourFields(t, "café default", got, "café", false, "default", nil)
	got, _ = evalSuccess(t, h4, "beta")
	assertCLIResultFourFields(t, "beta default", got, "beta", true, "default", nil)
}

// TestCLIEvaluateDuplicateKeyFixSequenceSuccessStableAcrossVariants 验证第四
// 阶段的成功对书写变体保持稳定：第二份的新名字无论用直接字符还是合法
// Unicode 转义（beta）、其对象字段是否重排、文档前后是否有空白，
// café 在两种模式下都按第一份定义成功且四字段完全一致；beta 同样可求值。
func TestCLIEvaluateDuplicateKeyFixSequenceSuccessStableAcrossVariants(t *testing.T) {
	spellings := []struct {
		name string
		json string
	}{
		{"direct", "beta"},
		{"escaped", "bet" + cliDupEscA},
	}
	for _, sp := range spellings {
		for _, reordered := range []bool{false, true} {
			for _, padded := range []bool{false, true} {
				label := "key=" + sp.name + "/reordered=" + cliDupBoolLabel(reordered) +
					"/padded=" + cliDupBoolLabel(padded)
				t.Run(label, func(t *testing.T) {
					h := newHarness(t,
						cliDupFixedConfig(sp.json, reordered, padded),
						`{"plan":"pro"}`)
					plain, raw := evalSuccess(t, h, "café")
					assertCLIResultFourFields(t, "normal", plain, "café", true, "rule", "r1")
					explained := explainSuccess(t, h, "café")
					assertCLIResultFourFields(t, "explain", explained, "café", true, "rule", "r1")
					if !strings.Contains(raw, `"ruleId":"r1"`) {
						t.Fatalf("normal output must name r1: %q", raw)
					}
					if er := rulesOf(t, explained); len(er) != 1 || er[0]["ruleId"] != "r1" {
						t.Fatalf("explain output must keep r1: %v", er)
					}
					// 改名后的第二份始终可达，没有被悄悄丢弃。
					h2 := newHarness(t,
						cliDupFixedConfig(sp.json, reordered, padded),
						`{"region":"cn"}`)
					got, _ := evalSuccess(t, h2, "beta")
					assertCLIResultFourFields(t, "beta", got, "beta", false, "rule", "r2")
				})
			}
		}
	}
}

// cliDupBoolLabel 给表驱动用例的布尔变体一个稳定的短标签。
func cliDupBoolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
