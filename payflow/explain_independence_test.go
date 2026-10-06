package payflow

import (
	"bytes"
	"strings"
	"testing"
)

// 本文件只补一项回归保障：EvaluateExplain 直接返回的 Go 解释对象属于调用方。
// 调用方保存多份记录用于对比、或改写手里记录中的文字与列表（候选成员、次序、
// 实际值、规则编号、条件说明、命中标记、依据文字）时，改动只能落在这份记录上：
// 开关配置、求值上下文、其他已经取得的记录都不能随之变化；下一次求值仍只由
// 传入的配置与上下文决定，普通结果与解释中的最终结果一致。反向同样成立：
// 调整配置或只换上下文后，旧记录仍保留保存时的值，重新输出旧 Go 对象也不变。
//
// 场景沿用 explainCfg：r-deny 在前（plan eq "pro" 且 tier in ["","a","a"]，
// 返回 false），r-allow 在后（region eq "cn"，返回 true），default=true。
// 列表自带空字符串、重复成员与固定次序；上下文可以同时让“前面规则未命中、
// 后面规则命中”，也可以让返回 false 的前一条直接定案。
var (
	indepCtxBoth    = `{"plan":"pro","tier":"a","region":"cn"}` // r-deny 命中并定案 false
	indepCtxLater   = `{"plan":"pro","tier":"b","region":"cn"}` // r-deny 未命中、r-allow 命中
	indepCtxMissing = `{"tier":"a","region":"cn"}`              // plan 缺失（null/missing）
	indepCtxEmpty   = `{"plan":"pro","tier":"","region":"cn"}`  // tier 显式空字符串
	indepCtxNeither = `{"plan":"free","region":"eu"}`           // 无规则命中，取默认值
)

// adjustedIndepCfg 是调整后的同键配置：两条规则改了编号与返回值（后一条改为
// false），eq 比较值改为 enterprise、in 列表改为 ["z"]，默认值改为 false。
const adjustedIndepCfg = `{"flags":[
	{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-deny-v2","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"enterprise"},
			{"attribute":"tier","op":"in","value":["z"]}]},
		{"id":"r-allow-v2","value":false,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]}]}]}`

// snapshotExplained 把一份解释记录按既有输出含义序列化为快照，供“重新输出旧
// 对象时内容仍与保存时一致”的逐字节比较。
func snapshotExplained(t *testing.T, r ExplainedResult) []byte {
	t.Helper()
	b, err := MarshalExplain(r)
	if err != nil {
		t.Fatalf("MarshalExplain failed: %v\nresult: %+v", err, r)
	}
	return b
}

// wantInCompareList 校验 in 条件解释里的候选列表：成员内容、空字符串、重复
// 成员与次序都必须与 want 完全一致。
func wantInCompareList(t *testing.T, c ConditionExplanation, want []string) {
	t.Helper()
	got, ok := c.CompareValue.([]string)
	if !ok {
		t.Fatalf("compareValue must be []string, got %#v", c.CompareValue)
	}
	if len(got) != len(want) {
		t.Fatalf("in-list length = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("in-list mismatch at %d: got %v, want %v", i, got, want)
		}
	}
}

// TestExplainEditsToOneRecordDoNotLeak 验证调用方整理手里这份解释记录时，
// 改动不会泄漏到开关配置、求值上下文、另一次求值或另一份已取得的记录。
func TestExplainEditsToOneRecordDoNotLeak(t *testing.T) {
	flag := explainFlag(t, "f")
	ctxBoth := mustParseContext(t, indepCtxBoth)
	ctxLater := mustParseContext(t, indepCtxLater)

	// 先取得另一份记录（前面规则未命中、后面规则命中），稍后用于交叉检查。
	sibling := flag.EvaluateExplain(ctxLater)
	siblingSnap := snapshotExplained(t, sibling)

	first := flag.EvaluateExplain(ctxBoth)
	if first.Value != false || first.RuleID == nil || *first.RuleID != "r-deny" || first.Reason != EvalRule {
		t.Fatalf("baseline result: %+v", first.EvalResult)
	}
	if ids := explainedIDs(first); len(ids) != 1 || ids[0] != "r-deny" {
		t.Fatalf("baseline must stop at the first matching rule, got %v", ids)
	}

	// 调用方按自己的展示需要改写手里这份记录：
	// 候选成员（含空字符串与重复成员）、成员次序、列表长度；实际值；缺失与
	// 命中标记；条件的属性名、运算符与比较值；规则编号与规则命中标记；依据
	// 文字；顶层 ruleId/value；再追加一条本不存在的“规则”。
	tier := &first.Explanation.Rules[0].Conditions[1]
	list := tier.CompareValue.([]string)
	list[2] = "b"                       // 改掉一个重复成员
	list[0], list[1] = list[1], list[0] // 交换空字符串与第一个 "a" 的次序
	tier.CompareValue = append(list, "extra")
	*tier.ActualValue = "zzz"
	tier.Missing = true
	tier.Match = false
	plan := &first.Explanation.Rules[0].Conditions[0]
	plan.Attribute = "PLAN-X"
	plan.Op = "nin"
	plan.CompareValue = "CHANGED"
	first.Explanation.Rules[0].RuleID = "rule-tampered"
	first.Explanation.Rules[0].Match = false
	first.Explanation.Outcome = "rewritten by caller"
	*first.RuleID = "result-tampered"
	first.Value = true
	first.Explanation.Rules = append(first.Explanation.Rules, RuleExplanation{RuleID: "fake-rule"})

	// 开关配置不能被记录改写影响：原 in 列表仍是 ["","a","a"]。
	if in := flag.Rules[0].Conditions[1].inVal; len(in) != 3 ||
		in[0] != "" || in[1] != "a" || in[2] != "a" {
		t.Fatalf("config in-list leaked edits: %#v", in)
	}
	if v := flag.Rules[0].Conditions[0].strVal; v != "pro" {
		t.Fatalf("config eq compare value leaked edits: %q", v)
	}
	// 求值上下文不能被记录改写影响。
	if v, ok := ctxBoth.Get("tier"); !ok || v != "a" {
		t.Fatalf("context tier leaked edits: %q %v", v, ok)
	}

	// 下一次求值仍由传入的原配置与原上下文决定：r-deny 命中并立即定案 false，
	// 解释中的列表、实际值、标记与编号全部按配置重新生成。
	fresh := flag.EvaluateExplain(ctxBoth)
	if !fresh.EvalResult.Equals(flag.Evaluate(ctxBoth)) {
		t.Fatalf("plain and explained results diverged: %+v vs %+v", fresh.EvalResult, flag.Evaluate(ctxBoth))
	}
	if fresh.Value != false || fresh.RuleID == nil || *fresh.RuleID != "r-deny" {
		t.Fatalf("fresh result must follow the original config: %+v", fresh.EvalResult)
	}
	if ids := explainedIDs(fresh); len(ids) != 1 || ids[0] != "r-deny" {
		t.Fatalf("fresh explanation must list only r-deny, got %v", ids)
	}
	if !fresh.Explanation.Rules[0].Match {
		t.Fatalf("fresh deciding rule must be marked matched: %+v", fresh.Explanation.Rules[0])
	}
	freshPlan := condOf(t, fresh, 0, 0)
	if freshPlan.Attribute != "plan" || freshPlan.Op != "eq" || freshPlan.CompareValue != "pro" ||
		freshPlan.Missing || !freshPlan.Match || freshPlan.ActualValue == nil || *freshPlan.ActualValue != "pro" {
		t.Fatalf("fresh plan condition drifted: %+v", freshPlan)
	}
	freshTier := condOf(t, fresh, 0, 1)
	wantInCompareList(t, freshTier, []string{"", "a", "a"})
	if freshTier.Missing || !freshTier.Match || freshTier.ActualValue == nil || *freshTier.ActualValue != "a" {
		t.Fatalf("fresh tier condition drifted: %+v", freshTier)
	}
	freshOut := string(snapshotExplained(t, fresh))
	if strings.Contains(freshOut, "tampered") ||
		strings.Contains(freshOut, "CHANGED") || strings.Contains(freshOut, "fake-rule") {
		t.Fatalf("fresh explanation contains edits from the old record: %s", freshOut)
	}

	// 空字符串仍是原配置的合法成员（编辑没有删掉它）：显式 "" 仍由 r-deny
	// 命中并定案 false。
	empty := flag.EvaluateExplain(mustParseContext(t, indepCtxEmpty))
	if empty.Value != false || empty.RuleID == nil || *empty.RuleID != "r-deny" {
		t.Fatalf("empty-string member must still decide via r-deny: %+v", empty.EvalResult)
	}
	emptyTier := condOf(t, empty, 0, 1)
	if emptyTier.Missing || !emptyTier.Match || emptyTier.ActualValue == nil || *emptyTier.ActualValue != "" {
		t.Fatalf("explicit empty string must stay present and matched: %+v", emptyTier)
	}

	// 先取得的另一份记录不出现任何改动：编号列表、候选列表与实际值都保持原样。
	if got := snapshotExplained(t, sibling); !bytes.Equal(got, siblingSnap) {
		t.Fatalf("sibling record changed after another record was edited:\n%s\nwant\n%s", got, siblingSnap)
	}
	if ids := explainedIDs(sibling); len(ids) != 2 || ids[0] != "r-deny" || ids[1] != "r-allow" {
		t.Fatalf("sibling rule ids leaked edits: %v", ids)
	}
	sibTier := condOf(t, sibling, 0, 1)
	wantInCompareList(t, sibTier, []string{"", "a", "a"})
	if sibTier.Missing || sibTier.Match || sibTier.ActualValue == nil || *sibTier.ActualValue != "b" {
		t.Fatalf("sibling tier condition leaked edits: %+v", sibTier)
	}

	// 被调用方改写过的这份记录本身仍是调用方的副本，改动保留在它自己身上。
	editedOut := string(snapshotExplained(t, first))
	if !strings.Contains(editedOut, "rewritten by caller") ||
		!strings.Contains(editedOut, `"extra"`) || !strings.Contains(editedOut, "fake-rule") {
		t.Fatalf("caller edits must remain on the edited record: %s", editedOut)
	}
}

// TestExplainRecordEditsDoNotRedirectLaterEvaluation 验证改写旧记录中的规则
// 编号、条件命中标记与顶层定案字段，不能让后续求值改用另一条规则，也不能影响
// 更早取得的其他记录。
func TestExplainRecordEditsDoNotRedirectLaterEvaluation(t *testing.T) {
	flag := explainFlag(t, "f")
	ctxBoth := mustParseContext(t, indepCtxBoth)
	ctxLater := mustParseContext(t, indepCtxLater)

	recFalse := flag.EvaluateExplain(ctxBoth)
	falseSnap := snapshotExplained(t, recFalse)
	old := flag.EvaluateExplain(ctxLater)
	if !old.Value || old.RuleID == nil || *old.RuleID != "r-allow" || old.Reason != EvalRule {
		t.Fatalf("baseline later-hit result: %+v", old.EvalResult)
	}
	if ids := explainedIDs(old); len(ids) != 2 || ids[0] != "r-deny" || ids[1] != "r-allow" {
		t.Fatalf("baseline considered rules: %v", ids)
	}

	// 把未命中的 r-deny 伪造成命中、把命中的 r-allow 伪造成未命中，改写两条
	// 规则编号、tier 条件的命中标记与实际值、依据文字以及顶层 ruleId/value。
	old.Explanation.Rules[0].RuleID = "r-deny-edited"
	old.Explanation.Rules[0].Match = true
	old.Explanation.Rules[0].Conditions[1].Match = true
	*old.Explanation.Rules[0].Conditions[1].ActualValue = "a"
	old.Explanation.Rules[1].RuleID = "r-allow-edited"
	old.Explanation.Rules[1].Match = false
	old.Explanation.Outcome = "edited outcome"
	*old.RuleID = "r-deny-edited"
	old.Value = false
	editedSnap := snapshotExplained(t, old)

	// 后续求值不能改用另一条规则：仍是 r-allow 按原配置定案 true。
	fresh := flag.EvaluateExplain(ctxLater)
	if !fresh.EvalResult.Equals(flag.Evaluate(ctxLater)) {
		t.Fatalf("plain and explained results diverged after edits: %+v", fresh.EvalResult)
	}
	if !fresh.Value || fresh.RuleID == nil || *fresh.RuleID != "r-allow" {
		t.Fatalf("later evaluation must still be decided by r-allow: %+v", fresh.EvalResult)
	}
	if ids := explainedIDs(fresh); len(ids) != 2 || ids[0] != "r-deny" || ids[1] != "r-allow" {
		t.Fatalf("fresh rule ids leaked edits: %v", ids)
	}
	if fresh.Explanation.Rules[0].Match || !fresh.Explanation.Rules[1].Match {
		t.Fatalf("fresh match marks leaked edits: %+v", fresh.Explanation.Rules)
	}
	freshTier := condOf(t, fresh, 0, 1)
	if freshTier.Match || freshTier.Missing || freshTier.ActualValue == nil || *freshTier.ActualValue != "b" {
		t.Fatalf("fresh tier judgment leaked edits: %+v", freshTier)
	}
	wantInCompareList(t, freshTier, []string{"", "a", "a"})

	// 同上下文仍由前一条 r-deny 立即定案 false，且解释只含它一条。
	freshFalse := flag.EvaluateExplain(ctxBoth)
	if freshFalse.Value != false || freshFalse.RuleID == nil || *freshFalse.RuleID != "r-deny" {
		t.Fatalf("r-deny decision changed after record edits: %+v", freshFalse.EvalResult)
	}
	if ids := explainedIDs(freshFalse); len(ids) != 1 || ids[0] != "r-deny" {
		t.Fatalf("rules after the deciding rule must still be absent: %v", ids)
	}

	// 更早取得的记录保持原样；被编辑的记录重新输出仍等于保存时（编辑后）的
	// 内容——后续求值不会回写覆盖任何一份旧记录。
	if got := snapshotExplained(t, recFalse); !bytes.Equal(got, falseSnap) {
		t.Fatalf("earlier record changed after a later record was edited:\n%s\nwant\n%s", got, falseSnap)
	}
	if got := snapshotExplained(t, old); !bytes.Equal(got, editedSnap) {
		t.Fatalf("edited record was overwritten by later evaluation:\n%s\nwant\n%s", got, editedSnap)
	}
}

// TestExplainOldRecordsSurviveConfigAndContextChanges 从反方向保障独立性：
// 保存解释后调整开关的规则编号、返回值与条件，新求值必须反映新配置；只换用
// 另一份上下文时，旧记录的实际值也不能被覆盖。检查对象是直接取得的 Go 解释
// 对象本身，而不仅是已输出的 JSON 文本。
func TestExplainOldRecordsSurviveConfigAndContextChanges(t *testing.T) {
	flag := explainFlag(t, "f")

	// 覆盖既有行为的五种旧记录：前未中后命中（r-allow true）、前一条直接定案
	// false、属性缺失、显式空字符串、无命中取默认值。
	recLater := flag.EvaluateExplain(mustParseContext(t, indepCtxLater))
	recFalse := flag.EvaluateExplain(mustParseContext(t, indepCtxBoth))
	recMissing := flag.EvaluateExplain(mustParseContext(t, indepCtxMissing))
	recEmpty := flag.EvaluateExplain(mustParseContext(t, indepCtxEmpty))
	recDefault := flag.EvaluateExplain(mustParseContext(t, indepCtxNeither))

	saved := []struct {
		name string
		rec  ExplainedResult
		snap []byte
	}{
		{"later-match", recLater, snapshotExplained(t, recLater)},
		{"false-decided", recFalse, snapshotExplained(t, recFalse)},
		{"missing-attr", recMissing, snapshotExplained(t, recMissing)},
		{"empty-string", recEmpty, snapshotExplained(t, recEmpty)},
		{"default", recDefault, snapshotExplained(t, recDefault)},
	}

	// 在同一个 Flag 上换成调整后的配置：规则编号、返回值、条件与默认值全部改变。
	adjusted := mustParseConfig(t, adjustedIndepCfg).Find("f")
	flag.Default = adjusted.Default
	flag.Rules = adjusted.Rules

	// 新求值必须反映新配置：原“两条都成立”的上下文上，plan=pro 不等于
	// enterprise、tier=a 不属于 ["z"]，r-deny-v2 不命中；region=cn 命中
	// r-allow-v2，按其新返回值定案 false。
	ctxBoth := mustParseContext(t, indepCtxBoth)
	got := flag.EvaluateExplain(ctxBoth)
	if !got.EvalResult.Equals(flag.Evaluate(ctxBoth)) {
		t.Fatalf("plain/explained mismatch on adjusted config: %+v", got.EvalResult)
	}
	if got.Value != false || got.Reason != EvalRule || got.RuleID == nil || *got.RuleID != "r-allow-v2" {
		t.Fatalf("new evaluation must follow the adjusted config: %+v", got.EvalResult)
	}
	if ids := explainedIDs(got); len(ids) != 2 || ids[0] != "r-deny-v2" || ids[1] != "r-allow-v2" {
		t.Fatalf("new evaluation must use adjusted rule ids: %v", ids)
	}
	if c := condOf(t, got, 0, 0); c.CompareValue != "enterprise" || c.Match {
		t.Fatalf("adjusted eq condition must be used: %+v", c)
	}
	wantInCompareList(t, condOf(t, got, 0, 1), []string{"z"})

	// 无规则命中时采用调整后的默认值 false（旧默认值为 true）。
	gotDefault := flag.EvaluateExplain(mustParseContext(t, indepCtxNeither))
	if gotDefault.Reason != EvalDefault || gotDefault.RuleID != nil || gotDefault.Value != false {
		t.Fatalf("adjusted default must be false: %+v", gotDefault.EvalResult)
	}

	// 旧 Go 记录直接读取，仍保留保存时的最终值、原因、规则编号、依据文字与
	// 逐条条件判断——不经过 JSON 文本中转。
	if recFalse.Value != false || recFalse.Reason != EvalRule ||
		recFalse.RuleID == nil || *recFalse.RuleID != "r-deny" {
		t.Fatalf("false-decided old result changed: %+v", recFalse.EvalResult)
	}
	if ids := explainedIDs(recFalse); len(ids) != 1 || ids[0] != "r-deny" {
		t.Fatalf("false-decided old record must keep only r-deny, got %v", ids)
	}
	if !recFalse.Explanation.Rules[0].Match || !strings.Contains(recFalse.Explanation.Outcome, `"r-deny"`) {
		t.Fatalf("false-decided old outcome changed: %+v / %q", recFalse.Explanation.Rules[0], recFalse.Explanation.Outcome)
	}
	falseTier := condOf(t, recFalse, 0, 1)
	wantInCompareList(t, falseTier, []string{"", "a", "a"})
	if falseTier.Missing || !falseTier.Match || falseTier.ActualValue == nil || *falseTier.ActualValue != "a" {
		t.Fatalf("false-decided old tier condition changed: %+v", falseTier)
	}

	// 前未中后命中的旧记录：r-deny 保留全部两个条件且标为未命中，r-allow
	// 命中；命中规则之后不出现其他规则。
	if recLater.Value != true || recLater.RuleID == nil || *recLater.RuleID != "r-allow" {
		t.Fatalf("later-match old result changed: %+v", recLater.EvalResult)
	}
	laterDeny := recLater.Explanation.Rules[0]
	if laterDeny.RuleID != "r-deny" || laterDeny.Match || len(laterDeny.Conditions) != 2 {
		t.Fatalf("old missed rule must keep id, mark and all conditions: %+v", laterDeny)
	}
	if c := laterDeny.Conditions[0]; !c.Match || c.ActualValue == nil || *c.ActualValue != "pro" {
		t.Fatalf("old matched plan condition changed: %+v", c)
	}
	if c := laterDeny.Conditions[1]; c.Match || c.Missing || c.ActualValue == nil || *c.ActualValue != "b" {
		t.Fatalf("old missed tier condition changed: %+v", c)
	}
	laterAllow := recLater.Explanation.Rules[1]
	if laterAllow.RuleID != "r-allow" || !laterAllow.Match ||
		laterAllow.Conditions[0].ActualValue == nil || *laterAllow.Conditions[0].ActualValue != "cn" {
		t.Fatalf("old deciding rule changed: %+v", laterAllow)
	}

	// 属性缺失仍是 null + missing=true；显式空字符串仍是指向 "" 的非 nil 值且
	// 不标缺失，二者不被新配置或新求值混淆。
	missPlan := condOf(t, recMissing, 0, 0)
	if !missPlan.Missing || missPlan.ActualValue != nil || missPlan.Match {
		t.Fatalf("old missing attribute must stay null/missing: %+v", missPlan)
	}
	if c := condOf(t, recMissing, 0, 1); c.Missing || !c.Match || c.ActualValue == nil || *c.ActualValue != "a" {
		t.Fatalf("old present tier next to missing plan changed: %+v", c)
	}
	if recMissing.Value != true || recMissing.RuleID == nil || *recMissing.RuleID != "r-allow" {
		t.Fatalf("missing-attr old result changed: %+v", recMissing.EvalResult)
	}
	emptyTier := condOf(t, recEmpty, 0, 1)
	if emptyTier.Missing || !emptyTier.Match || emptyTier.ActualValue == nil || *emptyTier.ActualValue != "" {
		t.Fatalf("old explicit empty string must stay present and matched: %+v", emptyTier)
	}
	if recEmpty.Value != false || recEmpty.RuleID == nil || *recEmpty.RuleID != "r-deny" {
		t.Fatalf("empty-string old result changed: %+v", recEmpty.EvalResult)
	}

	// 默认值旧记录保留 true/default、两条未命中规则与当时的依据文字。
	if recDefault.Value != true || recDefault.Reason != EvalDefault || recDefault.RuleID != nil {
		t.Fatalf("old default result changed: %+v", recDefault.EvalResult)
	}
	if ids := explainedIDs(recDefault); len(ids) != 2 || ids[0] != "r-deny" || ids[1] != "r-allow" {
		t.Fatalf("old default record must keep both rules: %v", ids)
	}
	for _, rr := range recDefault.Explanation.Rules {
		if rr.Match {
			t.Fatalf("old default record must keep every rule unmarked: %+v", rr)
		}
	}
	if !strings.Contains(strings.ToLower(recDefault.Explanation.Outcome), "default") {
		t.Fatalf("old default outcome changed: %q", recDefault.Explanation.Outcome)
	}

	// 重新输出旧 Go 对象：与保存时的 JSON 逐字节一致。
	for _, s := range saved {
		if got := snapshotExplained(t, s.rec); !bytes.Equal(got, s.snap) {
			t.Fatalf("%s: re-output changed after config adjustment:\n%s\nwant\n%s", s.name, got, s.snap)
		}
	}

	// --- 只换上下文：配置不动，旧记录的实际值不能被新上下文覆盖。 ---
	flag2 := explainFlag(t, "f")
	ctxLater := mustParseContext(t, indepCtxLater)
	old := flag2.EvaluateExplain(ctxLater)
	oldSnap := snapshotExplained(t, old)

	// 换一个无命中上下文：新结果取默认值 true。
	newCtx := mustParseContext(t, indepCtxNeither)
	newResult := flag2.EvaluateExplain(newCtx)
	if newResult.Reason != EvalDefault || !newResult.Value || newResult.RuleID != nil {
		t.Fatalf("new-context evaluation must follow the new context: %+v", newResult.EvalResult)
	}
	// 再换一个 plan 缺失的上下文。
	missResult := flag2.EvaluateExplain(mustParseContext(t, indepCtxMissing))
	if missResult.RuleID == nil || *missResult.RuleID != "r-allow" {
		t.Fatalf("missing-plan context must still decide via r-allow: %+v", missResult.EvalResult)
	}

	// 旧记录的实际值与标记不被任何新上下文覆盖。
	if c := condOf(t, old, 0, 0); c.Missing || c.ActualValue == nil || *c.ActualValue != "pro" {
		t.Fatalf("old plan actual value overwritten by new context: %+v", c)
	}
	if c := condOf(t, old, 0, 1); c.Missing || c.Match || c.ActualValue == nil || *c.ActualValue != "b" {
		t.Fatalf("old tier actual value overwritten by new context: %+v", c)
	}
	if c := old.Explanation.Rules[1].Conditions[0]; c.ActualValue == nil || *c.ActualValue != "cn" || !c.Match {
		t.Fatalf("old region actual value overwritten by new context: %+v", c)
	}
	if v, ok := ctxLater.Get("tier"); !ok || v != "b" {
		t.Fatalf("saved context mutated by later evaluations: %q %v", v, ok)
	}
	if got := snapshotExplained(t, old); !bytes.Equal(got, oldSnap) {
		t.Fatalf("old record re-output changed after switching contexts:\n%s\nwant\n%s", got, oldSnap)
	}
}
