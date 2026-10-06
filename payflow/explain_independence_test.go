package payflow

import (
	"bytes"
	"strings"
	"testing"
)

// 本文件只补一项回归保障：调用方取得 EvaluateExplain 的 Go 解释对象后，可以
// 长期保存、相互对比、任意改写其中的文字与列表供自己展示；这些改动只属于手里
// 这一份记录，不得回流到开关配置、求值上下文，不得串到其他已经取得的记录，
// 也不得改变之后任何一次求值。反过来，保存解释之后再调整配置或换用上下文，
// 新结果必须只反映新的输入，旧 Go 对象重新输出时仍与保存当时逐字一致。
//
// independenceCfg 的 f 开关按顺序含三条规则，专门覆盖“前面规则未命中、后面
// 规则命中并返回 false”的既有行为：
//   - r-miss（在前，返回 true）：plan eq "pro" 与 tier in ["","a","a"] 两个
//     条件 AND；in 列表同时含空字符串与重复成员，用于保护候选内容与次序；
//   - r-false（居中，返回 false）：region eq "cn"，r-miss 不成立而它成立时
//     立即定案为 false；
//   - r-unseen（在后）：命中定案后不应被考虑，解释里也不得出现。
const independenceCfg = `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
	{"id":"r-miss","value":true,"conditions":[
		{"attribute":"plan","op":"eq","value":"pro"},
		{"attribute":"tier","op":"in","value":["","a","a"]}]},
	{"id":"r-false","value":false,"conditions":[
		{"attribute":"region","op":"eq","value":"cn"}]},
	{"id":"r-unseen","value":true,"conditions":[
		{"attribute":"plan","op":"eq","value":"never"}]}]}]}`

const (
	independenceCtxLaterFalse = `{"plan":"pro","tier":"b","region":"cn"}`
	independenceCtxMissing    = `{"region":"cn"}`
	independenceCtxEmpty      = `{"plan":"free","tier":"","region":"cn"}`
)

func independenceFlag(t *testing.T) *Flag {
	t.Helper()
	flag := mustParseConfig(t, independenceCfg).Find("f")
	if flag == nil {
		t.Fatalf("flag %q not found", "f")
	}
	return flag
}

func mustMarshalExplain(t *testing.T, r ExplainedResult) []byte {
	t.Helper()
	b, err := MarshalExplain(r)
	if err != nil {
		t.Fatalf("MarshalExplain failed: %v", err)
	}
	return b
}

// assertLaterFalseBaseline 按 independenceCtxLaterFalse 当次求值的既有结果
// 逐条核对解释对象：最终值 false、reason=rule、ruleId=r-false；解释保留
// r-miss（未命中、两个条件逐条在案）与 r-false（命中），r-unseen 不出现；
// in 候选保持空字符串、重复成员与原次序；实际值 pro/b/cn 各自在案。
func assertLaterFalseBaseline(t *testing.T, label string, r ExplainedResult) {
	t.Helper()
	falseID := "r-false"
	wantResult := EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &falseID}
	if !r.EvalResult.Equals(wantResult) {
		t.Fatalf("%s: result = %+v, want %+v", label, r.EvalResult, wantResult)
	}
	if ids := explainedIDs(r); len(ids) != 2 || ids[0] != "r-miss" || ids[1] != "r-false" {
		t.Fatalf("%s: considered rules = %v, want [r-miss r-false] (r-unseen must not appear)", label, ids)
	}
	if r.Explanation.Rules[0].Match || !r.Explanation.Rules[1].Match {
		t.Fatalf("%s: match flags wrong: %+v", label, r.Explanation.Rules)
	}
	if !strings.Contains(r.Explanation.Outcome, "r-false") {
		t.Fatalf("%s: outcome must name the deciding rule r-false: %q", label, r.Explanation.Outcome)
	}

	missConds := r.Explanation.Rules[0].Conditions
	if len(missConds) != 2 {
		t.Fatalf("%s: r-miss must keep both condition records, got %d", label, len(missConds))
	}
	plan := missConds[0]
	if plan.Attribute != "plan" || plan.Op != "eq" || plan.CompareValue != "pro" ||
		plan.ActualValue == nil || *plan.ActualValue != "pro" || plan.Missing || !plan.Match {
		t.Fatalf("%s: plan condition record wrong: %+v", label, plan)
	}
	tier := missConds[1]
	list, ok := tier.CompareValue.([]string)
	if !ok || len(list) != 3 || list[0] != "" || list[1] != "a" || list[2] != "a" {
		t.Fatalf("%s: in-list must keep empty member, order and duplicates %#v", label, tier.CompareValue)
	}
	if tier.Attribute != "tier" || tier.Op != "in" || tier.Missing || tier.Match {
		t.Fatalf("%s: tier condition should be a present non-member: %+v", label, tier)
	}
	if tier.ActualValue == nil || *tier.ActualValue != "b" {
		t.Fatalf("%s: tier actual value = %+v, want %q", label, tier.ActualValue, "b")
	}

	falseConds := r.Explanation.Rules[1].Conditions
	if len(falseConds) != 1 {
		t.Fatalf("%s: r-false must list its one condition, got %d", label, len(falseConds))
	}
	region := falseConds[0]
	if region.Attribute != "region" || region.Op != "eq" || region.CompareValue != "cn" ||
		region.ActualValue == nil || *region.ActualValue != "cn" || region.Missing || !region.Match {
		t.Fatalf("%s: region condition record wrong: %+v", label, region)
	}
}

// TestExplainedResultEditsStayInTheRecord 验证调用方整理一份解释对象时，所有
// 改动都被关在这份记录里：改写顶层结果、依据文字、命中规则编号、规则列表与
// 条件列表中的编号/说明/命中标记，替换或就地改写 in 候选、追加伪造规则条目，
// 改写实际值指针——既不能动配置与上下文，也不能让后续普通/解释求值变样，
// 另一份已取得的记录同样保持原样。
func TestExplainedResultEditsStayInTheRecord(t *testing.T) {
	flag := independenceFlag(t)
	ctx := mustParseContext(t, independenceCtxLaterFalse)

	cur := flag.EvaluateExplain(ctx)
	assertLaterFalseBaseline(t, "baseline", cur)
	// 保存改写前的完整输出文本，作为“当次记录本来内容”的凭据。
	curSnap := mustMarshalExplain(t, cur)

	// 同配置同上下文的另一份记录，全程不被调用方改写。
	sibling := flag.EvaluateExplain(ctx)
	siblingSnap := mustMarshalExplain(t, sibling)

	// 属性缺失与显式空字符串各取一份：两者的实际值表示必须始终可区分。
	missing := flag.EvaluateExplain(mustParseContext(t, independenceCtxMissing))
	missingSnap := mustMarshalExplain(t, missing)
	empty := flag.EvaluateExplain(mustParseContext(t, independenceCtxEmpty))
	emptySnap := mustMarshalExplain(t, empty)
	mtBefore := missing.Explanation.Rules[0].Conditions[1]
	etBefore := empty.Explanation.Rules[0].Conditions[1]
	if !mtBefore.Missing || mtBefore.ActualValue != nil {
		t.Fatalf("setup: missing record must use missing=true/nil actual: %+v", mtBefore)
	}
	if etBefore.Missing || etBefore.ActualValue == nil || *etBefore.ActualValue != "" || !etBefore.Match {
		t.Fatalf("setup: empty record must keep actual \"\" with missing=false: %+v", etBefore)
	}

	// ---- 调用方按自己的展示需要大规模改写手里的 cur -----------------------
	hijackID := "r-evil"
	cur.RuleID = &hijackID
	cur.Value = true
	cur.Reason = EvalDefault
	cur.Explanation.Outcome = "rewritten by the caller for display"
	cur.Explanation.Rules[0].RuleID = "rule-tampered"
	cur.Explanation.Rules[0].Match = true
	cur.Explanation.Rules[1].RuleID = "decider-tampered"
	cur.Explanation.Rules[1].Match = false

	plan := &cur.Explanation.Rules[0].Conditions[0]
	plan.Attribute = "PLAN-X"
	plan.Op = "neq"
	plan.CompareValue = "ENTERPRISE"
	plan.Match = false
	*plan.ActualValue = "PRO-HACK"

	tier := &cur.Explanation.Rules[0].Conditions[1]
	// 先就地改写解释里原有的候选成员并调换次序（不替换切片本身）：若解释记录
	// 与配置共享 in 列表的底层数组，这一步就会污染配置。
	origList := tier.CompareValue.([]string)
	origList[0], origList[1], origList[2] = origList[2], origList[1], origList[0]
	origList[1] = "A-HACK"
	tier.Match = true
	*tier.ActualValue = "B-HACK"
	// 随后整体替换候选列表，并在替换后的切片上就地改成员、再追加成员，
	// 模拟调用方按自己展示需要整理列表。
	tier.CompareValue = []string{"z"}
	tamperedList := tier.CompareValue.([]string)
	tamperedList[0] = "zz"
	tier.CompareValue = append(tamperedList, "extra")

	region := &cur.Explanation.Rules[1].Conditions[0]
	region.Attribute = "REGION-X"
	*region.ActualValue = "CN-HACK"
	region.Match = false

	// 直接改写规则列表：追加一条本不存在的命中规则。
	cur.Explanation.Rules = append(cur.Explanation.Rules, RuleExplanation{RuleID: "r-fake", Match: true})

	// 缺失/空串两份记录也各自被调用方改写，验证二者不共享实际值存储。
	mt := &missing.Explanation.Rules[0].Conditions[1]
	missingHack := "missing-hijacked"
	mt.ActualValue = &missingHack
	mt.Missing = false
	mt.Match = true
	et := &empty.Explanation.Rules[0].Conditions[1]
	*et.ActualValue = "empty-hijacked"

	// 改写必须真的落在 cur 上，防止下面的隔离断言空转。
	if bytes.Equal(mustMarshalExplain(t, cur), curSnap) {
		t.Fatal("test setup: edits did not change the caller's own record")
	}
	if *cur.RuleID != "r-evil" {
		t.Fatalf("test setup: top-level rule id not rewritten: %+v", cur.EvalResult)
	}

	// ---- 改动不得回流到配置 ------------------------------------------------
	cfgList := flag.Rules[0].Conditions[1].inVal
	if len(cfgList) != 3 || cfgList[0] != "" || cfgList[1] != "a" || cfgList[2] != "a" {
		t.Fatalf("config in-list changed by explanation edits: %#v", cfgList)
	}
	if flag.Rules[0].ID != "r-miss" || flag.Rules[1].ID != "r-false" ||
		flag.Rules[0].Value != true || flag.Rules[1].Value != false {
		t.Fatalf("config rule id/value changed by explanation edits: %+v", flag.Rules)
	}
	if flag.Rules[0].Conditions[0].strVal != "pro" ||
		flag.Rules[1].Conditions[0].strVal != "cn" {
		t.Fatalf("config condition compare values changed: %+v", flag.Rules)
	}

	// ---- 改动不得回流到上下文 ----------------------------------------------
	if v, ok := ctx.Get("plan"); !ok || v != "pro" {
		t.Fatalf("context plan changed by ActualValue edit: %q ok=%v", v, ok)
	}
	if v, ok := ctx.Get("tier"); !ok || v != "b" {
		t.Fatalf("context tier changed by ActualValue edit: %q ok=%v", v, ok)
	}
	if v, ok := ctx.Get("region"); !ok || v != "cn" {
		t.Fatalf("context region changed by ActualValue edit: %q ok=%v", v, ok)
	}

	// ---- 后续求值仍由传入的配置和上下文决定，普通结果与解释最终结果一致 ------
	plain := flag.Evaluate(ctx)
	fresh := flag.EvaluateExplain(ctx)
	if !fresh.EvalResult.Equals(plain) {
		t.Fatalf("fresh explained result %+v != plain result %+v", fresh.EvalResult, plain)
	}
	assertLaterFalseBaseline(t, "fresh evaluation after edits", fresh)
	if b := mustMarshalExplain(t, fresh); !bytes.Equal(b, curSnap) {
		t.Fatalf("fresh evaluation drifted from the original record:\ngot:  %s\nwant: %s", b, curSnap)
	}

	// ---- 另一份未改写的记录不带任何改动 ------------------------------------
	if b := mustMarshalExplain(t, sibling); !bytes.Equal(b, siblingSnap) {
		t.Fatalf("sibling record was contaminated by the other record's edits:\ngot:  %s\nwant: %s", b, siblingSnap)
	}

	// ---- 缺失与空串记录互不覆盖；重新求值各自仍属当次配置 ------------------
	if mt.ActualValue == nil || *mt.ActualValue != "missing-hijacked" || mt.Missing {
		t.Fatalf("edits to the missing record did not stick to it: %+v", mt)
	}
	if et.ActualValue == nil || *et.ActualValue != "empty-hijacked" || et.Missing {
		t.Fatalf("edits to the empty record did not stick to it: %+v", et)
	}
	freshMissing := flag.EvaluateExplain(mustParseContext(t, independenceCtxMissing))
	fmTier := freshMissing.Explanation.Rules[0].Conditions[1]
	if !fmTier.Missing || fmTier.ActualValue != nil || fmTier.Match {
		t.Fatalf("fresh missing record must keep missing=true/nil actual: %+v", fmTier)
	}
	if b := mustMarshalExplain(t, freshMissing); !bytes.Equal(b, missingSnap) {
		t.Fatalf("missing record was contaminated:\ngot:  %s\nwant: %s", b, missingSnap)
	}
	freshEmpty := flag.EvaluateExplain(mustParseContext(t, independenceCtxEmpty))
	feTier := freshEmpty.Explanation.Rules[0].Conditions[1]
	if feTier.Missing || feTier.ActualValue == nil || *feTier.ActualValue != "" || !feTier.Match {
		t.Fatalf("fresh empty record must keep actual \"\"/missing=false: %+v", feTier)
	}
	if b := mustMarshalExplain(t, freshEmpty); !bytes.Equal(b, emptySnap) {
		t.Fatalf("empty record was contaminated:\ngot:  %s\nwant: %s", b, emptySnap)
	}
}

// TestExplainedResultKeptThroughConfigAndContextChanges 保障反方向：保存解释
// 之后调整规则条件、规则编号或返回值，再用调整后的配置求值时，新结果必须反映
// 新配置；旧 Go 对象仍保留保存当时的最终值、原因、规则编号与逐条条件判断，
// 重新输出与保存时逐字一致。只换上下文时，旧对象中的实际值也不能被覆盖。
func TestExplainedResultKeptThroughConfigAndContextChanges(t *testing.T) {
	t.Run("condition change makes the earlier rule win", func(t *testing.T) {
		flag := independenceFlag(t)
		ctx := mustParseContext(t, independenceCtxLaterFalse)
		old := flag.EvaluateExplain(ctx)
		assertLaterFalseBaseline(t, "saved record", old)
		oldSnap := mustMarshalExplain(t, old)

		// 调整 r-miss 的 in 条件：tier=b 成为成员后 r-miss 两个条件全中，
		// 提前以 true 定案，r-false 不再被考虑。
		flag.Rules[0].Conditions[1].inVal = []string{"b"}

		plain := flag.Evaluate(ctx)
		fresh := flag.EvaluateExplain(ctx)
		if !fresh.EvalResult.Equals(plain) {
			t.Fatalf("explained %+v != plain %+v after config change", fresh.EvalResult, plain)
		}
		missID := "r-miss"
		want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &missID}
		if !fresh.EvalResult.Equals(want) {
			t.Fatalf("new config must select r-miss: got %+v, want %+v", fresh.EvalResult, want)
		}
		if ids := explainedIDs(fresh); len(ids) != 1 || ids[0] != "r-miss" {
			t.Fatalf("only the earlier rule may be considered: %v", ids)
		}
		newTier := fresh.Explanation.Rules[0].Conditions[1]
		if list, ok := newTier.CompareValue.([]string); !ok || len(list) != 1 || list[0] != "b" || !newTier.Match {
			t.Fatalf("new explanation must show the adjusted condition: %+v", newTier)
		}

		// 旧记录仍是保存当时的样子：r-false 定案 false，候选仍是旧列表。
		assertLaterFalseBaseline(t, "old record after config change", old)
		if b := mustMarshalExplain(t, old); !bytes.Equal(b, oldSnap) {
			t.Fatalf("saved record changed after the config changed:\ngot:  %s\nwant: %s", b, oldSnap)
		}
	})

	t.Run("rule id and return value change", func(t *testing.T) {
		flag := independenceFlag(t)
		ctx := mustParseContext(t, independenceCtxLaterFalse)
		old := flag.EvaluateExplain(ctx)
		assertLaterFalseBaseline(t, "saved record", old)
		oldSnap := mustMarshalExplain(t, old)

		// 只调整定案规则的编号与返回值：r-miss 仍不命中，求值走到改名后的
		// r-v2，按它的新返回值 true 定案。
		flag.Rules[1].ID = "r-v2"
		flag.Rules[1].Value = true

		plain := flag.Evaluate(ctx)
		fresh := flag.EvaluateExplain(ctx)
		if !fresh.EvalResult.Equals(plain) {
			t.Fatalf("explained %+v != plain %+v after rule change", fresh.EvalResult, plain)
		}
		v2ID := "r-v2"
		want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &v2ID}
		if !fresh.EvalResult.Equals(want) {
			t.Fatalf("new config must use renamed rule and its new value: got %+v, want %+v", fresh.EvalResult, want)
		}
		if ids := explainedIDs(fresh); len(ids) != 2 || ids[0] != "r-miss" || ids[1] != "r-v2" {
			t.Fatalf("rule list must follow the renamed config: %v", ids)
		}

		// 旧记录保留当时的 false 结果、r-false 编号、依据与条件判断。
		assertLaterFalseBaseline(t, "old record after rule change", old)
		if b := mustMarshalExplain(t, old); !bytes.Equal(b, oldSnap) {
			t.Fatalf("saved record changed after rule id/value changed:\ngot:  %s\nwant: %s", b, oldSnap)
		}
	})

	t.Run("switching context does not overwrite saved actual values", func(t *testing.T) {
		flag := independenceFlag(t) // 配置始终不调整。
		ctxLater := mustParseContext(t, independenceCtxLaterFalse)
		ctxMissing := mustParseContext(t, independenceCtxMissing)

		// 先保存“属性齐全”的记录，再用属性缺失的上下文求值：旧记录的实际值
		// pro/b/cn 不能被新结果的 null 覆盖。
		oldLater := flag.EvaluateExplain(ctxLater)
		laterSnap := mustMarshalExplain(t, oldLater)
		freshMissing := flag.EvaluateExplain(ctxMissing)
		if plain := flag.Evaluate(ctxMissing); !freshMissing.EvalResult.Equals(plain) {
			t.Fatalf("missing-context explained %+v != plain %+v", freshMissing.EvalResult, plain)
		}
		missPlan := freshMissing.Explanation.Rules[0].Conditions[0]
		missTier := freshMissing.Explanation.Rules[0].Conditions[1]
		if !missPlan.Missing || missPlan.ActualValue != nil ||
			!missTier.Missing || missTier.ActualValue != nil {
			t.Fatalf("new missing-context record must mark plan/tier missing: %+v %+v", missPlan, missTier)
		}
		assertLaterFalseBaseline(t, "saved later-false record after context switch", oldLater)
		if b := mustMarshalExplain(t, oldLater); !bytes.Equal(b, laterSnap) {
			t.Fatalf("saved record was overwritten by the new context:\ngot:  %s\nwant: %s", b, laterSnap)
		}

		// 反方向：保存“属性缺失”的记录，再用属性齐全（含显式空串区分）的
		// 上下文求值；旧记录的 null/missing=true 不能被填上字符串。
		oldMissing := flag.EvaluateExplain(ctxMissing)
		missingSnap := mustMarshalExplain(t, oldMissing)
		freshLater := flag.EvaluateExplain(ctxLater)
		if plain := flag.Evaluate(ctxLater); !freshLater.EvalResult.Equals(plain) {
			t.Fatalf("later-context explained %+v != plain %+v", freshLater.EvalResult, plain)
		}
		assertLaterFalseBaseline(t, "fresh later-false evaluation", freshLater)
		oldPlan := oldMissing.Explanation.Rules[0].Conditions[0]
		oldTier := oldMissing.Explanation.Rules[0].Conditions[1]
		if !oldPlan.Missing || oldPlan.ActualValue != nil ||
			!oldTier.Missing || oldTier.ActualValue != nil {
			t.Fatalf("saved missing record must keep nil actual values: %+v %+v", oldPlan, oldTier)
		}
		if b := mustMarshalExplain(t, oldMissing); !bytes.Equal(b, missingSnap) {
			t.Fatalf("saved missing record changed after context switch:\ngot:  %s\nwant: %s", b, missingSnap)
		}
	})
}
