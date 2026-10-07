package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件为 evaluate 的“错误来源顺序”补充回归保障：用户提交配置、上下文两份
// 文件和一个开关键，当三者同时存在问题时，报错必须按固定次序推进——
//  1. 整份配置的校验失败（含未被选中、已关闭的开关）最先报告；
//  2. 配置合法后才报告上下文的属性类型错误；
//  3. 两份文件都合法后，才报告开关键不存在。
// 普通模式与 --explain 解释模式遵守同一次序；所有输入失败都非零退出、
// stdout 为空（解释模式也不留结果字段或半份 explanation），stderr 能辨认
// 当前失败来自配置、上下文还是开关查找。两份文件都修正后，请求那个合法的
// 关闭开关是成功的求值（退出码 0、value=false、reason=disabled），与输入
// 失败明确区分。本文件只补充测试，不改变求值规则或错误处理。

// precedenceBrokenConfig 含一个合法的关闭开关 off，以及另一个已关闭、但缺少
// 必填 default 字段的开关 half。half 既不会被选中也已关闭，但整份配置仍须
// 完整校验，它的缺失字段必须最先被报告，位置指向 flags[1]。
const precedenceBrokenConfig = `{"flags":[
	{"key":"off","enabled":false,"default":false,"rules":[]},
	{"key":"half","enabled":false,"rules":[]}]}`

// precedenceFixedConfig 只把 half 缺失的 default 补齐，其余与 broken 版逐字
// 相同；此时整份配置合法，报错应推进到上下文。
const precedenceFixedConfig = `{"flags":[
	{"key":"off","enabled":false,"default":false,"rules":[]},
	{"key":"half","enabled":false,"default":true,"rules":[]}]}`

// precedenceBrokenContext 的属性 plan 提供非字符串值；precedenceFixedContext
// 只把它改成合法字符串。
const (
	precedenceBrokenContext = `{"plan":5}`
	precedenceFixedContext  = `{"plan":"pro"}`
)

// explainFailure 运行带 --explain 的 evaluate 并要求失败：非零退出、stdout
// 为空（不留四字段结果或半份 explanation）。返回 stderr 原文。
func explainFailure(t *testing.T, h *harness, key string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(h.evalExplainArgs(key), &stdout, &stderr); code == 0 {
		t.Fatalf("key %q: expected non-zero exit, stdout=%q", key, stdout.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("key %q: stdout must be empty on error (no half explanation), got %q", key, stdout.String())
	}
	return stderr.String()
}

// bothModesFailure 以普通模式与 --explain 模式各运行一次 evaluate，要求两者
// 都失败且 stderr 逐字一致（同一输入失败在两种模式下给出同一报错，解释模式
// 不另设输出格式）。返回该 stderr 原文。
func bothModesFailure(t *testing.T, h *harness, key string) string {
	t.Helper()
	plain := evalFailure(t, h, key)
	explain := explainFailure(t, h, key)
	if plain != explain {
		t.Fatalf("key %q: normal and --explain modes must report the same error:\nnormal:  %q\nexplain: %q", key, plain, explain)
	}
	return plain
}

// assertStderr 要求 msg 包含全部 wantSub 片段、不含任何 notSub 片段。
func assertStderr(t *testing.T, msg string, wantSub, notSub []string) {
	t.Helper()
	for _, want := range wantSub {
		if !strings.Contains(msg, want) {
			t.Fatalf("stderr=%q, want substring %q", msg, want)
		}
	}
	for _, not := range notSub {
		if strings.Contains(msg, not) {
			t.Fatalf("stderr=%q, must not contain %q", msg, not)
		}
	}
}

// TestCLIEvaluateErrorSourceConfigBeatsContextAndKey 锁定第一优先：配置、上下文
// 与开关键同时有问题时，无论请求的是合法的关闭开关还是不存在的开关键，也无论
// 是否带 --explain，都先报告整份配置的校验失败——位置指向真正缺字段的那个开关
// （flags[1]），不能因请求对象已关闭就返回 disabled，也不能提前反馈上下文错误
// 或开关不存在。
func TestCLIEvaluateErrorSourceConfigBeatsContextAndKey(t *testing.T) {
	for _, key := range []string{"off", "no-such-flag"} {
		t.Run("key="+key, func(t *testing.T) {
			h := newHarness(t, precedenceBrokenConfig, precedenceBrokenContext)
			msg := bothModesFailure(t, h, key)
			assertStderr(t, msg,
				[]string{`config.flags[1].default`, "field is required"},
				// 不得提前暴露后续阶段的问题：上下文类型错误、开关未找到。
				[]string{"context.plan", "value must be a string", "not found"})
		})
	}
}

// TestCLIEvaluateErrorSourceContextBeatsKey 锁定第二优先：只补齐配置中的缺失
// 字段、保留上下文错误后，上述两种开关键请求都改为报告上下文属性必须为字符
// 串，并指出对应属性；仍不报告开关不存在。
func TestCLIEvaluateErrorSourceContextBeatsKey(t *testing.T) {
	for _, key := range []string{"off", "no-such-flag"} {
		t.Run("key="+key, func(t *testing.T) {
			h := newHarness(t, precedenceFixedConfig, precedenceBrokenContext)
			msg := bothModesFailure(t, h, key)
			assertStderr(t, msg,
				[]string{`context.plan`, "value must be a string"},
				[]string{"config.flags", "not found"})
		})
	}
}

// TestCLIEvaluateErrorSourceKeyLookupLast 锁定最后一环：两份文件都合法后，请求
// 不存在的开关键才报告未找到；错误沿用既有形式（引用开关键与配置路径），普通
// 模式与 --explain 一致。
func TestCLIEvaluateErrorSourceKeyLookupLast(t *testing.T) {
	h := newHarness(t, precedenceFixedConfig, precedenceFixedContext)
	msg := bothModesFailure(t, h, "no-such-flag")
	assertStderr(t, msg,
		[]string{`flag key "no-such-flag" not found`, "config"},
		[]string{"value must be a string", "field is required"})
}

// TestCLIEvaluateErrorSourceProgressesWithFixes 沿用户逐步修正输入的过程，串起
// 错误来源的完整迁移：配置错误 → 上下文错误 → 开关键未找到 → 成功。每一步都
// 在普通与 --explain 两种模式下验证。
func TestCLIEvaluateErrorSourceProgressesWithFixes(t *testing.T) {
	// 第一步：配置缺字段（flags[1]），上下文也有类型错误，开关键不存在——
	// 报配置错误。
	h := newHarness(t, precedenceBrokenConfig, precedenceBrokenContext)
	assertStderr(t, bothModesFailure(t, h, "no-such-flag"),
		[]string{`config.flags[1].default`, "field is required"},
		[]string{"context.plan", "not found"})

	// 第二步：只补齐配置缺失字段，保留上下文错误——报上下文属性类型错误。
	h = newHarness(t, precedenceFixedConfig, precedenceBrokenContext)
	assertStderr(t, bothModesFailure(t, h, "no-such-flag"),
		[]string{`context.plan`, "value must be a string"},
		[]string{"config.flags", "not found"})

	// 第三步：把该属性改成合法字符串，继续请求不存在的开关键——报未找到。
	h = newHarness(t, precedenceFixedConfig, precedenceFixedContext)
	assertStderr(t, bothModesFailure(t, h, "no-such-flag"),
		[]string{`flag key "no-such-flag" not found`},
		[]string{"value must be a string", "field is required"})

	// 第四步：输入不变，改为请求那个合法的关闭开关——成功求值。
	got, _ := evalSuccess(t, h, "off")
	if got["value"] != false || got["reason"] != "disabled" || got["ruleId"] != nil {
		t.Fatalf("disabled flag must evaluate to fixed false: %v", got)
	}
}

// TestCLIEvaluateDisabledSuccessDistinctFromInputFailure 锁定修正完成后的成功
// 结果：请求合法的关闭开关，普通模式退出码 0、stderr 为空、stdout 只有既有
// 四个结果字段（value=false、reason=disabled、ruleId=null）——这是一次成功
// 的求值，与上文所有输入失败（非零退出、stdout 为空）明确区分，不能把
// false 一律视为失败。--explain 保留相同结果并给出空的规则列表。
func TestCLIEvaluateDisabledSuccessDistinctFromInputFailure(t *testing.T) {
	h := newHarness(t, precedenceFixedConfig, precedenceFixedContext)

	// 普通模式：逐字锁定四字段输出，不多出 explanation 等任何字段。
	var stdout, stderr bytes.Buffer
	if code := run(h.evalArgs("off"), &stdout, &stderr); code != 0 {
		t.Fatalf("disabled flag is a successful evaluation: exit=%d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr must be empty on success: %q", stderr.String())
	}
	const wantNormal = `{"key":"off","value":false,"reason":"disabled","ruleId":null}`
	if strings.TrimSpace(stdout.String()) != wantNormal {
		t.Fatalf("normal mode must keep exactly the four result fields: %q", stdout.String())
	}

	// 解释模式：前四个字段与普通模式一致，explanation 的规则列表为空。
	got := explainSuccess(t, h, "off")
	if got["key"] != "off" || got["value"] != false ||
		got["reason"] != "disabled" || got["ruleId"] != nil {
		t.Fatalf("explain mode must keep the same result: %v", got)
	}
	exp, ok := got["explanation"].(map[string]any)
	if !ok {
		t.Fatalf("explain mode must add an explanation object: %v", got)
	}
	rules, ok := exp["rules"].([]any)
	if !ok || len(rules) != 0 {
		t.Fatalf("disabled flag must explain with an empty rule list: %v", exp["rules"])
	}
	if !strings.Contains(strings.ToLower(exp["outcome"].(string)), "disabled") {
		t.Fatalf("outcome must say the flag is disabled: %v", exp["outcome"])
	}

	// 两种模式的四字段结果逐项一致。
	var plain map[string]any
	if err := json.Unmarshal([]byte(wantNormal), &plain); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"key", "value", "reason", "ruleId"} {
		if got[k] != plain[k] {
			t.Fatalf("field %s differs between modes: normal=%v explain=%v", k, plain[k], got[k])
		}
	}
}
