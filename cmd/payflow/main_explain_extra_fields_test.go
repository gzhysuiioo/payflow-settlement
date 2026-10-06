package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件从命令行入口为 evaluate --explain 补充“附加信息不参与求值”的端到端
// 回归保障：配置顶层、开关、规则或条件上的负责人、备注、嵌套对象、数组以及
// 1e400 / 1 后接 400 个 0 的合法大数字，不改变普通模式的四字段输出，也不改变
// --explain 的结果与完整解释（字节级一致）；附加信息既不出现在 stdout，也不
// 会被当作上下文属性补齐缺失值。整份配置先校验：未被选中或已关闭开关的附加
// 对象在合法大数字之后重复声明同名字段时，解释命令也必须非零退出、stdout
// 为空、stderr 以重复字段及其精确位置报错，不能误报数字过大，也不能先输出
// 目标开关的结果或半份解释。
//
// cliExtraExplainBase 不含附加字段。开关 f 按顺序含三条规则：
//   - r-early-false（在前，返回 false）：plan eq "pro" 且 tier in
//     ["","a","a"]；命中即定案，解释只到它；
//   - r-later-true（居中，返回 true）：region eq "cn"；
//   - r-unseen（在后）：用于观察解释是否多出后续未考虑的规则。
const cliExtraExplainBase = `{"flags":[
	{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-early-false","value":false,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"tier","op":"in","value":["","a","a"]}]},
		{"id":"r-later-true","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]},
		{"id":"r-unseen","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"us"}]}]},
	{"key":"off","enabled":false,"default":true,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]},
	{"key":"plain","enabled":true,"default":false,"rules":[]}]}`

var cliExtraHugeInt = "1" + strings.Repeat("0", 400)

// cliExtraExplainFull 在四级都放满合法附加信息（负责人、备注、嵌套对象、数组、
// 布尔、null、1e400 与 400 位整数），业务定义与 cliExtraExplainBase 完全相同。
var cliExtraExplainFull = `{"meta":{"owner":"pay-owner","note":"top-附加备注","big":1e400,"huge":` + cliExtraHugeInt + `,
	"tags":["x",1e400,{"n":1e400}],"ok":true,"nil":null},"flags":[
	{"key":"f","enabled":true,"default":true,"owner":"pay-flag","note":"flag-附加备注","flag_meta":{"big":1e400},"rules":[
		{"id":"r-early-false","value":false,"owner":"pay-rule","note":"rule-附加备注","rule_meta":[1e400],"conditions":[
			{"attribute":"plan","op":"eq","value":"pro","owner":"pay-cond","note":"plan-附加备注","weight":1e400},
			{"attribute":"tier","op":"in","value":["","a","a"],"labels":["y",1e400],"deep":{"big":1e400}}]},
		{"id":"r-later-true","value":true,"owner":"pay-rule-2","note":"later-附加备注","conditions":[
			{"attribute":"region","op":"eq","value":"cn","note":"region-附加备注","extra":1e400}]},
		{"id":"r-unseen","value":true,"owner":"pay-rule-3","conditions":[
			{"attribute":"region","op":"eq","value":"us","owner":"pay-cond-3"}]}]},
	{"key":"off","enabled":false,"default":true,"owner":"pay-off","note":"off-附加备注","meta":{"big":1e400},"rules":[
		{"id":"r-never","value":true,"owner":"pay-never","conditions":[
			{"attribute":"plan","op":"eq","value":"pro","note":"off-cond-附加备注"}]}]},
	{"key":"plain","enabled":true,"default":false,"owner":"pay-plain","note":"plain-附加备注","rules":[]}]}`

// cliExtraExplainChanged 改写全量附加字段的内容（改名、改数组、移除大数字），
// 业务定义不变；cliExtraExplainSparse 删除大部分附加字段，只留少数。
const cliExtraExplainChanged = `{"meta":{"owner":"pay-owner-2","note":"top-changed","tags":["z",{"n":2}]},"flags":[
	{"key":"f","enabled":true,"default":true,"owner":"pay-flag-2","note":"flag-changed","rules":[
		{"id":"r-early-false","value":false,"owner":"pay-rule-2x","note":"rule-changed","conditions":[
			{"attribute":"plan","op":"eq","value":"pro","owner":"pay-cond-2","weight":9},
			{"attribute":"tier","op":"in","value":["","a","a"],"labels":["q"]}]},
		{"id":"r-later-true","value":true,"owner":"pay-rule-2y","conditions":[
			{"attribute":"region","op":"eq","value":"cn","extra":4}]},
		{"id":"r-unseen","value":true,"owner":"pay-rule-2z","conditions":[
			{"attribute":"region","op":"eq","value":"us"}]}]},
	{"key":"off","enabled":false,"default":true,"owner":"pay-off-2","rules":[
		{"id":"r-never","value":true,"owner":"pay-never-2","conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]},
	{"key":"plain","enabled":true,"default":false,"owner":"pay-plain-2","rules":[]}]}`

const cliExtraExplainSparse = `{"meta":{"owner":"pay-owner","note":"top-附加备注"},"flags":[
	{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-early-false","value":false,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"tier","op":"in","value":["","a","a"],"note":"only-this-condition"}]},
		{"id":"r-later-true","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"}]},
		{"id":"r-unseen","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"us"}]}]},
	{"key":"off","enabled":false,"default":true,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]},
	{"key":"plain","enabled":true,"default":false,"rules":[]}]}`

// cliExtraExplainShadow 让附加字段（含嵌套附加对象 context/fallback/defaults/
// backup）在每一级都与规则所需的 plan/tier/region 同名并持有命中值。
const cliExtraExplainShadow = `{"context":{"plan":"pro","tier":"a","region":"cn"},"plan":"pro","tier":"a","region":"cn","flags":[
	{"key":"f","enabled":true,"default":true,"plan":"pro","tier":"a","region":"cn","fallback":{"plan":"pro","tier":"a","region":"cn"},"rules":[
		{"id":"r-early-false","value":false,"plan":"pro","tier":"a","defaults":{"plan":"pro","tier":"a"},"conditions":[
			{"attribute":"plan","op":"eq","value":"pro","plan":"pro","backup":{"plan":"pro"}},
			{"attribute":"tier","op":"in","value":["","a","a"],"tier":"a","backup":{"tier":"a"}}]},
		{"id":"r-later-true","value":true,"region":"cn","defaults":{"region":"cn"},"conditions":[
			{"attribute":"region","op":"eq","value":"cn","region":"cn","backup":{"region":"cn"}}]},
		{"id":"r-unseen","value":true,"conditions":[
			{"attribute":"region","op":"eq","value":"us"}]}]},
	{"key":"off","enabled":false,"default":true,"plan":"pro","rules":[
		{"id":"r-never","value":true,"plan":"pro","conditions":[
			{"attribute":"plan","op":"eq","value":"pro","plan":"pro"}]}]},
	{"key":"plain","enabled":true,"default":false,"region":"cn","rules":[]}]}`

// cliExtraExplainScenarios 覆盖靠前命中返回 false、靠前未命中后续命中（成员不
// 命中/缺失/显式空串）、全部不命中走默认、关闭开关与无规则开关。
var cliExtraExplainScenarios = []struct {
	name string
	key  string
	ctx  string
}{
	{"earlier rule hits and returns false", "f", `{"plan":"pro","tier":"a","region":"cn"}`},
	{"earlier rule misses by membership, later hits", "f", `{"plan":"pro","tier":"b","region":"cn"}`},
	{"required attrs missing, later rule hits", "f", `{"region":"cn"}`},
	{"explicit empty strings, later rule hits", "f", `{"plan":"","tier":"","region":"cn"}`},
	{"plan missing while tier is a member", "f", `{"tier":"a","region":"cn"}`},
	{"no rule matches, default used", "f", `{}`},
	{"disabled flag fixed false", "off", `{"plan":"pro"}`},
	{"flag without rules uses default", "plain", `{}`},
}

// runExplainOnce 运行一次 --explain，返回退出码与 stdout/stderr 原文。
func runExplainOnce(h *harness, key string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(h.evalExplainArgs(key), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestCLIExplainExtraFieldsByteIdenticalToBaseline 端到端锁定核心行为：在增加、
// 修改、删除合法附加字段（含与上下文属性同名的 shadow 变体）时，对同一场景，
// --explain 的 stdout 必须与无附加字段基线逐字节一致；普通模式四字段输出同样
// 逐字节一致，且成功时 stderr 为空。
func TestCLIExplainExtraFieldsByteIdenticalToBaseline(t *testing.T) {
	variants := map[string]string{
		"full":    cliExtraExplainFull,
		"changed": cliExtraExplainChanged,
		"sparse":  cliExtraExplainSparse,
		"shadow":  cliExtraExplainShadow,
	}
	for vname, vcfg := range variants {
		t.Run(vname, func(t *testing.T) {
			for _, sc := range cliExtraExplainScenarios {
				t.Run(sc.name, func(t *testing.T) {
					ctx := sc.ctx
					base := newHarness(t, cliExtraExplainBase, ctx)
					extra := newHarness(t, vcfg, ctx)

					bCode, bExplain, bErr := runExplainOnce(base, sc.key)
					if bCode != 0 {
						t.Fatalf("baseline explain exit=%d stderr=%s", bCode, bErr)
					}
					gCode, gExplain, gErr := runExplainOnce(extra, sc.key)
					if gCode != 0 {
						t.Fatalf("config with extras explain exit=%d stderr=%s", gCode, gErr)
					}
					if gErr != "" {
						t.Fatalf("stderr must be empty on success: %q", gErr)
					}
					if gExplain != bExplain {
						t.Fatalf("explain output changed by extra fields:\ngot:  %s\nwant: %s", gExplain, bExplain)
					}

					// 普通模式同样逐字节一致，且恰好四字段。
					var bp, gp, be, ge bytes.Buffer
					if code := run(base.evalArgs(sc.key), &bp, &be); code != 0 {
						t.Fatalf("baseline plain exit=%d %s", code, be.String())
					}
					if code := run(extra.evalArgs(sc.key), &gp, &ge); code != 0 {
						t.Fatalf("extras plain exit=%d %s", code, ge.String())
					}
					if gp.String() != bp.String() {
						t.Fatalf("plain output changed by extra fields: %q != %q", gp.String(), bp.String())
					}
					if strings.Contains(gp.String(), "explanation") {
						t.Fatalf("normal mode must keep only the four result fields: %q", gp.String())
					}
					var plain map[string]any
					if err := json.Unmarshal(gp.Bytes(), &plain); err != nil || len(plain) != 4 {
						t.Fatalf("plain output must be one four-field JSON object: %q", gp.String())
					}
				})
			}
		})
	}
}

// TestCLIExplainExtraFieldsEarlyFalseVsLaterWin 端到端区分两种定案：靠前规则命中
// 返回 false 时解释只含该规则；靠前未命中、后续命中时保留未命中规则及全部条件；
// 附加字段不能改变获胜规则，也不能让 r-unseen 出现。
func TestCLIExplainExtraFieldsEarlyFalseVsLaterWin(t *testing.T) {
	t.Run("earlier false rule stops explanation", func(t *testing.T) {
		h := newHarness(t, cliExtraExplainFull, `{"plan":"pro","tier":"a","region":"cn"}`)
		got := explainSuccess(t, h, "f")
		if got["key"] != "f" || got["value"] != false || got["reason"] != "rule" || got["ruleId"] != "r-early-false" {
			t.Fatalf("unexpected payload: %v", got)
		}
		rules := rulesOf(t, got)
		if len(rules) != 1 || rules[0]["ruleId"] != "r-early-false" || rules[0]["match"] != true {
			t.Fatalf("explanation must stop at the first hit: %v", rules)
		}
		conds := rules[0]["conditions"].([]any)
		if len(conds) != 2 {
			t.Fatalf("winning rule must show both conditions: %v", conds)
		}
		plan := conds[0].(map[string]any)
		if plan["attribute"] != "plan" || plan["compareValue"] != "pro" || plan["actualValue"] != "pro" ||
			plan["missing"] != false || plan["match"] != true {
			t.Fatalf("plan record wrong: %v", plan)
		}
		tier := conds[1].(map[string]any)
		list := tier["compareValue"].([]any)
		if len(list) != 3 || list[0] != "" || list[1] != "a" || list[2] != "a" {
			t.Fatalf("tier compareValue must keep empty/duplicates/order: %v", list)
		}
		if tier["actualValue"] != "a" || tier["missing"] != false || tier["match"] != true {
			t.Fatalf("tier record wrong: %v", tier)
		}
	})

	t.Run("earlier miss kept with all conditions, later rule wins", func(t *testing.T) {
		h := newHarness(t, cliExtraExplainFull, `{"plan":"pro","tier":"b","region":"cn"}`)
		got := explainSuccess(t, h, "f")
		if got["value"] != true || got["reason"] != "rule" || got["ruleId"] != "r-later-true" {
			t.Fatalf("unexpected payload: %v", got)
		}
		rules := rulesOf(t, got)
		if len(rules) != 2 || rules[0]["ruleId"] != "r-early-false" || rules[1]["ruleId"] != "r-later-true" {
			t.Fatalf("considered rules wrong: %v", rules)
		}
		if rules[0]["match"] != false || rules[1]["match"] != true {
			t.Fatalf("match flags wrong: %v", rules)
		}
		earlyConds := rules[0]["conditions"].([]any)
		if len(earlyConds) != 2 {
			t.Fatalf("missed rule must keep all condition records: %v", earlyConds)
		}
		plan := earlyConds[0].(map[string]any)
		if plan["compareValue"] != "pro" || plan["actualValue"] != "pro" || plan["missing"] != false || plan["match"] != true {
			t.Fatalf("plan record wrong: %v", plan)
		}
		tier := earlyConds[1].(map[string]any)
		if tier["actualValue"] != "b" || tier["missing"] != false || tier["match"] != false {
			t.Fatalf("tier must be a present non-member: %v", tier)
		}
		laterConds := rules[1]["conditions"].([]any)
		region := laterConds[0].(map[string]any)
		if region["compareValue"] != "cn" || region["actualValue"] != "cn" ||
			region["missing"] != false || region["match"] != true {
			t.Fatalf("region record wrong: %v", region)
		}
	})

	t.Run("all rules kept when none matches", func(t *testing.T) {
		h := newHarness(t, cliExtraExplainFull, `{}`)
		got := explainSuccess(t, h, "f")
		if got["reason"] != "default" || got["ruleId"] != nil || got["value"] != true {
			t.Fatalf("default fallback wrong: %v", got)
		}
		rules := rulesOf(t, got)
		if len(rules) != 3 {
			t.Fatalf("all three rules must be listed on default: %v", rules)
		}
	})
}

// TestCLIExplainShadowExtrasNeverFillMissing 端到端验证同名附加信息不补齐缺失：
// 附加对象里即使写了 plan/tier/region 的命中值，上下文没有该属性时解释仍显示
// actualValue=null、missing=true、match=false，求值继续按真实上下文处理。
func TestCLIExplainShadowExtrasNeverFillMissing(t *testing.T) {
	h := newHarness(t, cliExtraExplainShadow, `{"region":"cn"}`)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalExplainArgs("f"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if got["reason"] != "rule" || got["ruleId"] != "r-later-true" || got["value"] != true {
		t.Fatalf("shadow extras must not make the earlier rule win: %v", got)
	}
	rules := rulesOf(t, got)
	if len(rules) != 2 {
		t.Fatalf("only r-early-false and r-later-true must be considered: %v", rules)
	}
	conds := rules[0]["conditions"].([]any)
	plan := conds[0].(map[string]any)
	if plan["actualValue"] != nil || plan["missing"] != true || plan["match"] != false ||
		plan["compareValue"] != "pro" {
		t.Fatalf("missing plan must stay null/true despite shadow extras: %v", plan)
	}
	tier := conds[1].(map[string]any)
	if tier["actualValue"] != nil || tier["missing"] != true || tier["match"] != false {
		t.Fatalf("missing tier must stay null/true despite shadow extras: %v", tier)
	}
	// 缺失记录必须逐字渲染为 actualValue:null,"missing":true；附加内容不泄露。
	if !strings.Contains(out, `"actualValue":null,"missing":true`) {
		t.Fatalf("missing attributes must render null/true: %s", out)
	}
	for _, leak := range []string{"backup", "defaults", "fallback", `"context"`} {
		if strings.Contains(out, leak) {
			t.Fatalf("shadow extra object %q leaked into explanation: %s", leak, out)
		}
	}

	// 空上下文：所有规则都因缺失不命中，默认值定案；四个条件全部 null/true。
	h = newHarness(t, cliExtraExplainShadow, `{}`)
	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalExplainArgs("f"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out = stdout.String()
	if c := strings.Count(out, `"actualValue":null,"missing":true`); c != 4 {
		t.Fatalf("all four conditions must render missing, got %d: %s", c, out)
	}
	var empty map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &empty); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if empty["reason"] != "default" || empty["ruleId"] != nil {
		t.Fatalf("empty context must fall through to default: %v", empty)
	}

	// 关闭开关即使有同名附加字段也固定 false，规则解释为空。
	h = newHarness(t, cliExtraExplainShadow, `{}`)
	off := explainSuccess(t, h, "off")
	if off["reason"] != "disabled" || off["value"] != false || off["ruleId"] != nil {
		t.Fatalf("disabled flag stays disabled: %v", off)
	}
	if rs := rulesOf(t, off); len(rs) != 0 {
		t.Fatalf("disabled flag must have empty rule explanation: %v", rs)
	}
}

// TestCLIExplainShadowExtrasKeepExplicitEmptyString 端到端区分显式空串与缺失：
// 上下文给出 "" 时必须保留 ""、missing=false，不能被附加对象里的命中值替换，
// 也不能与 null 混淆。
func TestCLIExplainShadowExtrasKeepExplicitEmptyString(t *testing.T) {
	h := newHarness(t, cliExtraExplainShadow, `{"plan":"","tier":"","region":"cn"}`)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalExplainArgs("f"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	// plan="" 不等于 "pro"，r-early-false 整体不命中；r-later-true 命中。
	if got["reason"] != "rule" || got["ruleId"] != "r-later-true" {
		t.Fatalf("empty strings must not be replaced by shadow hits: %v", got)
	}
	rules := rulesOf(t, got)
	conds := rules[0]["conditions"].([]any)
	plan := conds[0].(map[string]any)
	if plan["actualValue"] != "" || plan["missing"] != false || plan["match"] != false {
		t.Fatalf("plan explicit \"\" must stay present and unequal: %v", plan)
	}
	tier := conds[1].(map[string]any)
	if tier["actualValue"] != "" || tier["missing"] != false || tier["match"] != true {
		t.Fatalf("tier explicit \"\" must stay present and match the empty list member: %v", tier)
	}
	if !strings.Contains(out, `"actualValue":"","missing":false`) {
		t.Fatalf("empty strings must render \"\"/false: %s", out)
	}
	if strings.Contains(out, `"actualValue":null,"missing":false`) {
		t.Fatalf("present values must never render null/missing=false: %s", out)
	}
}

// TestCLIExplainExtraFieldsContentNeverLeaks 验证负责人、备注、大数字等附加
// 内容绝不出现在解释 stdout：顶层恰好五个字段、条件恰好六个字段；普通模式
// 仍是原有四字段。
func TestCLIExplainExtraFieldsContentNeverLeaks(t *testing.T) {
	h := newHarness(t, cliExtraExplainFull, `{"plan":"pro","tier":"b","region":"cn"}`)
	var stdout, stderr bytes.Buffer
	if code := run(h.evalExplainArgs("f"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, leak := range []string{"owner", "pay-owner", "pay-flag", "附加备注",
		"1e400", cliExtraHugeInt, "weight", "labels", `"meta"`, "tags"} {
		if strings.Contains(out, leak) {
			t.Fatalf("extra content %q leaked into stdout: %s", leak, out)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("explain output must keep exactly five fields: %v", got)
	}
	for _, r0 := range rulesOf(t, got) {
		for _, c0 := range r0["conditions"].([]any) {
			cond := c0.(map[string]any)
			if len(cond) != 6 {
				t.Fatalf("condition must keep exactly six fields: %v", cond)
			}
		}
	}

	// 普通模式仍是原四字段。
	var pout bytes.Buffer
	if code := run(h.evalArgs("f"), &pout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("plain exit=%d", code)
	}
	if want := `{"key":"f","value":true,"reason":"rule","ruleId":"r-later-true"}`; strings.TrimSpace(pout.String()) != want {
		t.Fatalf("normal output = %q, want %q", pout.String(), want)
	}
}

// TestCLIExplainDuplicateInExtraFieldRejectsWholeConfig 端到端保留整份配置先
// 校验的边界：未被选中或已关闭开关的附加对象在合法大数字之后重复声明同名字段
// 时，--explain 必须非零退出、stdout 为空，stderr 以重复字段名义指出精确
// 位置——不能误报数字过大/JSON 无效，也不能先输出目标开关的结果或半份解释。
func TestCLIExplainDuplicateInExtraFieldRejectsWholeConfig(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		key     string
		wantSub []string
	}{
		{
			"duplicate after 1e400 in unselected flag extra",
			`{"flags":[
				{"key":"target","enabled":true,"default":false,"rules":[]},
				{"key":"later","enabled":true,"default":false,"rules":[],
					"meta":{"n":1e400,"n":2}}]}`,
			"target",
			[]string{`config.flags[1].meta.n`, "duplicate field", `"n"`},
		},
		{
			"duplicate after 400-digit integer in unselected flag rule extra",
			`{"flags":[
				{"key":"target","enabled":true,"default":false,"rules":[]},
				{"key":"later","enabled":true,"default":false,"rules":[
					{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}],
						"note":{"v":` + cliExtraHugeInt + `,"v":2}}]}]}`,
			"target",
			[]string{`config.flags[1].rules[0].note.v`, "duplicate field", `"v"`},
		},
		{
			"duplicate after 1e400 in disabled flag extra",
			`{"flags":[
				{"key":"target","enabled":true,"default":true,"rules":[]},
				{"key":"off","enabled":false,"default":true,"rules":[],
					"meta":{"n":1e400,"n":2}}]}`,
			"target",
			[]string{`config.flags[1].meta.n`, "duplicate field", `"n"`},
		},
		{
			"requesting the disabled flag itself still fails whole config",
			`{"flags":[
				{"key":"other","enabled":true,"default":false,"rules":[]},
				{"key":"off","enabled":false,"default":true,"rules":[],
					"meta":{"n":1e400,"n":2}}]}`,
			"off",
			[]string{`config.flags[1].meta.n`, "duplicate field", `"n"`},
		},
		{
			"duplicate after big number nested in extra array element",
			`{"flags":[
				{"key":"target","enabled":true,"default":false,"rules":[]},
				{"key":"later","enabled":true,"default":false,"rules":[],
					"tags":[{"v":1e400,"v":2}]}]}`,
			"target",
			[]string{`config.flags[1].tags[0].v`, "duplicate field", `"v"`},
		},
		{
			"duplicate after 1e400 in top-level extra object",
			`{"flags":[
				{"key":"target","enabled":true,"default":false,"rules":[]}],
				"meta":{"n":1e400,"n":2}}`,
			"target",
			[]string{`config.meta.n`, "duplicate field", `"n"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.config, `{}`)
			var stdout, stderr bytes.Buffer
			if code := run(h.evalExplainArgs(tc.key), &stdout, &stderr); code == 0 {
				t.Fatalf("expected non-zero exit, stdout=%q", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty (no target result, no half explanation): %q", stdout.String())
			}
			msg := stderr.String()
			for _, want := range tc.wantSub {
				if !strings.Contains(msg, want) {
					t.Fatalf("stderr=%q, want substring %q", msg, want)
				}
			}
			// 合法大数字不是 JSON 语法错误，配置附加字段也没有数值范围/类型规则：
			// 必须以重复字段名义拒绝，不能误报数字过大。
			if strings.Contains(msg, "invalid JSON") {
				t.Fatalf("legal big number must not be reported as invalid JSON: %q", msg)
			}
			for _, bad := range []string{"value must be", "must be a", "number", "too large", "range"} {
				if strings.Contains(msg, bad) {
					t.Fatalf("duplicate must not be misreported as a number problem (%q): %q", bad, msg)
				}
			}

			// 同一配置用普通模式也必须得到完全相同的拒绝形态。
			var pstdout, pstderr bytes.Buffer
			if code := run(h.evalArgs(tc.key), &pstdout, &pstderr); code == 0 {
				t.Fatalf("plain mode must also reject, stdout=%q", pstdout.String())
			}
			if pstdout.Len() != 0 || pstderr.String() != msg {
				t.Fatalf("plain and explain modes must fail identically: plain-stdout=%q plain-stderr=%q explain-stderr=%q",
					pstdout.String(), pstderr.String(), msg)
			}
		})
	}
}

// TestCLIExplainDuplicateExtraFieldFixRestoresOutput 验证把重复声明删掉（保留
// 其余合法附加字段与 1e400）后，--explain 立即恢复既有输出：真实规则定案、
// 解释不含附加内容；这条与拒绝用例互为正反，保证边界可恢复。
func TestCLIExplainDuplicateExtraFieldFixRestoresOutput(t *testing.T) {
	fixed := `{"meta":{"n":1e400},"flags":[
		{"key":"target","enabled":true,"default":false,"rules":[
			{"id":"r-hit","value":true,"conditions":[
				{"attribute":"plan","op":"eq","value":"pro"}]}]},
		{"key":"off","enabled":false,"default":true,"rules":[],
			"meta":{"n":1e400}}]}`
	h := newHarness(t, fixed, `{"plan":"pro"}`)
	got := explainSuccess(t, h, "target")
	if got["value"] != true || got["reason"] != "rule" || got["ruleId"] != "r-hit" {
		t.Fatalf("fixed config must evaluate by its real rule: %v", got)
	}
	rules := rulesOf(t, got)
	if len(rules) != 1 || rules[0]["ruleId"] != "r-hit" {
		t.Fatalf("explanation must contain only the hit rule: %v", rules)
	}
	// 已关闭开关在修正后固定 false，且不列出规则。
	off := explainSuccess(t, h, "off")
	if off["reason"] != "disabled" || off["value"] != false {
		t.Fatalf("disabled flag stays disabled after fix: %v", off)
	}
	if rs := rulesOf(t, off); len(rs) != 0 {
		t.Fatalf("disabled flag explanation must be empty: %v", rs)
	}
}
