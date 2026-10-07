package payflow

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件为“两个开关使用同一开关键、后一个开关自身又不合法”时的逐处修正过程
// 补充回归保障。现有测试已分别覆盖单个字段错误与单纯的重名；这里锁定的是它们
// 叠加时的报错推进次序：
//
//	前一个开关定义合法，后一个开关与它重名，同时缺少 default，且其某条规则的
//	in 比较列表含非字符串项。用户逐处修正时必须按固定阶段看到——
//	  1. 先指出后一个开关缺少 default；
//	  2. 补上合法布尔值后，指出列表成员的类型错误，位置一路精确到成员下标
//	     （开关、规则、条件、成员）；
//	  3. 列表改成非空字符串数组后，才报告重复开关键，指向后一次定义并说明
//	     第一次出现的位置；
//	  4. 把后一个开关键改成不同名称，两份定义才被共同接受。
//
// 重名按 JSON 解码后的文字判断，因此直接字符与合法 Unicode 转义（含代理项对）
// 的每种组合都要走出同一次序、同一位置与同一原因；只调整对象内部字段的书写
// 顺序或文档空白同样不得改变报错。求值功能与名称唯一性规则不在本文件修改。

// dupKeySpellingRow 是一对“解码后同名”的开关键写法（JSON 字符串内容，不含
// 两侧引号）。
type dupKeySpellingRow struct {
	name    string
	decoded string
	first   string
	second  string
}

var dupKeySpellingRows = []dupKeySpellingRow{
	{"identical direct names", "café", `café`, `café`},
	{"direct then unicode escape", "café", `café`, `caf\u00E9`},
	{"unicode escape then direct", "café", `caf\u00E9`, `café`},
	{"surrogate pair direct then escaped", "a🚀", `a🚀`, `a\uD83D\uDE80`},
	{"surrogate pair escaped then direct", "a🚀", `a\uD83D\uDE80`, `a🚀`},
}

// dupKeyStage 描述修正过程中后一个开关的状态：缺 default、列表含非字符串项、
// 是否已把开关键改成不同名称。
type dupKeyStage struct {
	missingDefault bool
	badList        bool
	renamed        bool
}

var (
	dupStageMissingDefault = dupKeyStage{missingDefault: true, badList: true}
	dupStageBadList        = dupKeyStage{badList: true}
	dupStageDuplicateOnly  = dupKeyStage{}
	dupStageRenamed        = dupKeyStage{renamed: true}
)

// dupKeyFirstFlag 是始终合法的第一份定义：启用、default=false，一条 plan=pro
// 命中时返回 true 的 eq 规则。它与第二份定义的规则指纹不同，修正完成后可用来
// 证明原开关键仍属于第一份定义。
func dupKeyFirstFlag(keyJSON string) string {
	return `{"key":"` + keyJSON + `","enabled":true,"default":false,"rules":[` +
		`{"id":"r-pro","value":true,"conditions":[` +
		`{"attribute":"plan","op":"eq","value":"pro"}]}]}`
}

// dupKeySecondFlag 是逐处修正的对象：key 与第一份解码后同名（renamed 后改成
// "other"），default 与 in 列表按阶段变化；其规则 r-region 在 region 属于
// ["cn","us"] 时返回 false。enabled 允许单独设置，用于验证已关闭开关也要经过
// 同样的完整校验。
func dupKeySecondFlag(keyJSON string, enabled bool, st dupKeyStage) string {
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
		b.WriteString(`2]}`) // 下标 1 的成员是非字符串（数字）
	} else {
		b.WriteString(`"us"]}`)
	}
	b.WriteString(`]}]}`)
	return b.String()
}

// dupKeyConfig 按规范紧凑排版拼出两份开关定义的配置；开关数组内第一份始终在前。
func dupKeyConfig(row dupKeySpellingRow, secondEnabled bool, st dupKeyStage) string {
	return `{"flags":[` + dupKeyFirstFlag(row.first) + `,` +
		dupKeySecondFlag(row.second, secondEnabled, st) + `]}`
}

// dupKeyConfigRearranged 与 dupKeyConfig 表达完全相同的内容，但调整了每个对象
// 内部字段的书写顺序（开关：rules→default→enabled→key；规则：
// conditions→value→id；条件：value→op→attribute），并改用换行、制表符、冒号前
// 空白、空行等不同文档空白。开关数组次序、规则/条件次序与列表成员次序保持
// 不变——成员下标 [1] 仍指向同一个（非字符串）成员。
func dupKeyConfigRearranged(row dupKeySpellingRow, secondEnabled bool, st dupKeyStage) string {
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
	fmt.Fprintf(&second, "\t\t\t\"enabled\": %t,\n", secondEnabled)
	second.WriteString("\t\t\t\"key\": \"" + key + "\"\n\t\t}\n\t")
	return "{\n\t\"flags\" : [\n " + first + ",\n" + second.String() + " ]\n}\n\n"
}

// dupKeyExpectedError 返回各阶段应逐字出现的唯一报错。
func dupKeyExpectedError(decoded string, flagIndex, firstIndex int, st dupKeyStage) string {
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

// TestParseConfigDuplicateKeyFixProgression 锁定完整修正过程：缺 default →
// in 列表非字符串成员（位置精确到 flags[1].rules[0].conditions[0].value[1]）→
// 重复开关键（指向 flags[1] 并注明 first at flags[0]）→ 改名后两份定义共同
// 接受。直接字符与合法 Unicode 转义解码后同名的每种写法都要一致。
func TestParseConfigDuplicateKeyFixProgression(t *testing.T) {
	for _, row := range dupKeySpellingRows {
		t.Run(row.name, func(t *testing.T) {
			// 前三个阶段：逐字锁定每处报错，且字段顺序/空白排版给出逐字相同的结果。
			for _, st := range []dupKeyStage{dupStageMissingDefault, dupStageBadList, dupStageDuplicateOnly} {
				canonical := dupKeyConfig(row, true, st)
				rearranged := dupKeyConfigRearranged(row, true, st)
				_, errC := ParseConfig([]byte(canonical))
				_, errR := ParseConfig([]byte(rearranged))
				if errC == nil {
					t.Fatalf("stage %+v: expected error, config parses:\n%s", st, canonical)
				}
				want := dupKeyExpectedError(row.decoded, 1, 0, st)
				if errC.Error() != want {
					t.Fatalf("stage %+v:\nerr  = %q\nwant = %q", st, errC.Error(), want)
				}
				if errR == nil || errR.Error() != errC.Error() {
					t.Fatalf("stage %+v: field order/whitespace changed the error:\ncanonical:  %q\nrearranged: %q",
						st, errC, errR)
				}
				if strings.Contains(errC.Error(), "duplicate field") {
					t.Fatalf("stage %+v: must not be misread as an in-object duplicate field: %q",
						st, errC.Error())
				}
			}

			// 第四阶段：后一个开关键改名后，两份定义共同接受，排版变化不影响结果。
			for _, doc := range []string{
				dupKeyConfig(row, true, dupStageRenamed),
				dupKeyConfigRearranged(row, true, dupStageRenamed),
			} {
				dupKeyAssertBothDefinitionsAccepted(t, row.decoded, doc)
			}
		})
	}
}

// dupKeyAssertBothDefinitionsAccepted 验证改名后的配置：两个开关都保留、次序不
// 变；原开关键仍对应第一份定义（r-pro 命中返回 true），后一份定义以 "other"
// 独立存在（r-region 命中返回 false），不覆盖、不丢弃。普通求值与解释求值共用
// 同一定案结果。
func dupKeyAssertBothDefinitionsAccepted(t *testing.T, decoded, doc string) {
	t.Helper()
	cfg := mustParseConfig(t, doc)
	if len(cfg.Flags) != 2 {
		t.Fatalf("both definitions must be kept, got %d flags", len(cfg.Flags))
	}
	if cfg.Flags[0].Key != decoded || cfg.Flags[1].Key != "other" {
		t.Fatalf("flag order/keys changed: %q, %q", cfg.Flags[0].Key, cfg.Flags[1].Key)
	}
	// Find 必须按数组顺序返回第一份定义，不能被后一次定义覆盖。
	if first := cfg.Find(decoded); first != &cfg.Flags[0] {
		t.Fatalf("key %q must resolve to the first definition", decoded)
	}
	if second := cfg.Find("other"); second != &cfg.Flags[1] {
		t.Fatalf(`renamed key "other" must resolve to the second definition`)
	}

	ctx := mustParseContext(t, `{"plan":"pro","region":"cn"}`)
	proID, regionID := "r-pro", "r-region"
	first := cfg.Find(decoded).Evaluate(ctx)
	if !first.Equals(EvalResult{Key: decoded, Value: true, Reason: EvalRule, RuleID: &proID}) {
		t.Fatalf("original key must keep the first definition's r-pro decision: %+v", first)
	}
	second := cfg.Find("other").Evaluate(ctx)
	if !second.Equals(EvalResult{Key: "other", Value: false, Reason: EvalRule, RuleID: &regionID}) {
		t.Fatalf("renamed second definition must keep its own r-region decision: %+v", second)
	}

	// 解释模式与普通模式的四字段结果一致，并附带原有 explanation 判断记录。
	explained := cfg.Find(decoded).EvaluateExplain(ctx)
	if !explained.EvalResult.Equals(first) {
		t.Fatalf("explain result %+v != plain result %+v", explained.EvalResult, first)
	}
	if len(explained.Explanation.Rules) != 1 ||
		explained.Explanation.Rules[0].RuleID != "r-pro" || !explained.Explanation.Rules[0].Match {
		t.Fatalf("explanation must record the deciding r-pro rule: %+v", explained.Explanation.Rules)
	}
	if !strings.Contains(explained.Explanation.Outcome, "r-pro") {
		t.Fatalf("outcome must name the deciding rule: %q", explained.Explanation.Outcome)
	}
}

// TestParseConfigDuplicateKeyFixDisabledLaterFlag 锁定“后一个开关即使已关闭也
// 必须经过同样的完整校验”：把后一个开关的 enabled 改成 false，三个失败阶段的
// 报错必须与启用时逐字相同；改名后原开关键照常按第一份定义求值，而关闭的
// "other" 固定 disabled。
func TestParseConfigDuplicateKeyFixDisabledLaterFlag(t *testing.T) {
	ctx := mustParseContext(t, `{"plan":"pro","region":"cn"}`)
	for _, row := range dupKeySpellingRows {
		t.Run(row.name, func(t *testing.T) {
			for _, st := range []dupKeyStage{dupStageMissingDefault, dupStageBadList, dupStageDuplicateOnly} {
				enabledDoc := dupKeyConfig(row, true, st)
				disabledDoc := dupKeyConfig(row, false, st)
				_, errEnabled := ParseConfig([]byte(enabledDoc))
				_, errDisabled := ParseConfig([]byte(disabledDoc))
				if errEnabled == nil || errDisabled == nil {
					t.Fatalf("stage %+v: both variants must fail", st)
				}
				if errDisabled.Error() != errEnabled.Error() {
					t.Fatalf("stage %+v: disabling the later flag changed validation:\nenabled:  %q\ndisabled: %q",
						st, errEnabled.Error(), errDisabled.Error())
				}
			}
			// 改名后：关闭的后一份定义独立存在并固定 false；原开关键仍是第一份定义。
			cfg := mustParseConfig(t, dupKeyConfig(row, false, dupStageRenamed))
			if got := cfg.Find(row.decoded).Evaluate(ctx); got.Reason != EvalRule ||
				got.RuleID == nil || *got.RuleID != "r-pro" || !got.Value {
				t.Fatalf("first definition unaffected by the disabled second one: %+v", got)
			}
			if got := cfg.Find("other").Evaluate(ctx); got.Reason != EvalDisabled || got.Value || got.RuleID != nil {
				t.Fatalf("renamed disabled flag must stay fixed false: %+v", got)
			}
		})
	}
}

// TestParseConfigDuplicateKeyFixValidatedWhileAnotherFlagRequested 锁定“正在求值
// 另一个合法开关也不能豁免这对重名开关”：在它们前面再放一个完全合法的 good
// 开关，重名对移到 flags[1]/flags[2]。三个阶段仍逐一失败，位置随数组下标移动，
// 第一次出现的参照仍是第一份定义（flags[1]）；改名后三个开关共同保留。
func TestParseConfigDuplicateKeyFixValidatedWhileAnotherFlagRequested(t *testing.T) {
	good := `{"key":"good","enabled":true,"default":true,"rules":[]}`
	ctx := mustParseContext(t, `{}`)
	for _, row := range dupKeySpellingRows {
		t.Run(row.name, func(t *testing.T) {
			for _, st := range []dupKeyStage{dupStageMissingDefault, dupStageBadList, dupStageDuplicateOnly} {
				doc := `{"flags":[` + good + `,` +
					dupKeyFirstFlag(row.first) + `,` +
					dupKeySecondFlag(row.second, true, st) + `]}`
				_, err := ParseConfig([]byte(doc))
				want := dupKeyExpectedError(row.decoded, 2, 1, st)
				if err == nil || err.Error() != want {
					t.Fatalf("stage %+v:\nerr  = %q\nwant = %q", st, err, want)
				}
			}
			doc := `{"flags":[` + good + `,` +
				dupKeyFirstFlag(row.first) + `,` +
				dupKeySecondFlag(row.second, true, dupStageRenamed) + `]}`
			cfg := mustParseConfig(t, doc)
			if len(cfg.Flags) != 3 {
				t.Fatalf("all three definitions must be kept, got %d", len(cfg.Flags))
			}
			if got := cfg.Find("good").Evaluate(ctx); !got.Equals(
				(EvalResult{Key: "good", Value: true, Reason: EvalDefault})) {
				t.Fatalf("the unrelated legal flag must still evaluate normally: %+v", got)
			}
			if cfg.Find(row.decoded) != &cfg.Flags[1] || cfg.Find("other") != &cfg.Flags[2] {
				t.Fatalf("flag array order must be preserved: %+v", cfg.Flags)
			}
		})
	}
}
