package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件为离线特性开关求值补充“配置 → 上下文 → 开关键查找”报错顺序的端到端
// 回归保障，普通 evaluate 与 --explain 解释模式都锁定同一现有行为：
//   - 整份配置先完整校验（包括不会被求值、已关闭、未选中的开关），配置不合法时
//     只能看到配置错误；
//   - 配置合法后才校验上下文；
//   - 两份文件都合法后，才报告开关键不存在。
//
// 测试沿用户逐步修正输入的过程推进：先补齐配置、再补齐上下文、最后请求合法的
// 关闭开关。所有输入失败都必须非零退出、stdout 完全为空（解释模式也不留结果
// 字段或半份 explanation），stderr 的位置与原因沿用既有形式；最终的成功结果
// （false/disabled）与输入失败明确区分，不能把所有 false 都当作失败。

const (
	// orderExistingKey 是配置中一个合法的关闭开关：enabled=false、default=false。
	orderExistingKey = "off-ok"
	// orderUnknownKey 不出现在任何配置中，用于触发开关键查找失败。
	orderUnknownKey = "no-such-flag"
	// orderBadAttr 是上下文中持有非字符串值的属性名。
	orderBadAttr = "plan"

	// orderConfigMissingDefault：flags[0] 是合法的关闭开关 off-ok；flags[1] 是
	// 另一个已关闭开关 broken，缺少必填的 default 字段。整份配置必须因此失败，
	// 即使请求的是 off-ok、或一个根本不存在的开关键。
	orderConfigMissingDefault = `{"flags":[
		{"key":"off-ok","enabled":false,"default":false,"rules":[]},
		{"key":"broken","enabled":false,"rules":[]}]}`
	// orderConfigFixed 仅为 broken 补上 default，其余与破损版逐字一致。
	orderConfigFixed = `{"flags":[
		{"key":"off-ok","enabled":false,"default":false,"rules":[]},
		{"key":"broken","enabled":false,"default":true,"rules":[]}]}`
	// orderContextBadType 让 plan 属性持有数字而非字符串。
	orderContextBadType = `{"plan":5}`
	// orderContextFixed 把同一属性改成合法字符串。
	orderContextFixed = `{"plan":"pro"}`
)

// orderRun 以普通模式或 --explain 模式运行一次 evaluate，返回退出码与
// stdout/stderr 原文。
func orderRun(t *testing.T, h *harness, key string, explain bool) (int, string, string) {
	t.Helper()
	argv := h.evalArgs(key)
	if explain {
		argv = h.evalExplainArgs(key)
	}
	var stdout, stderr bytes.Buffer
	code := run(argv, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// orderModes 让同一组断言在普通模式与解释模式下各执行一遍。
var orderModes = []struct {
	name    string
	explain bool
}{
	{"plain", false},
	{"explain", true},
}

// TestCLIEvaluateErrorOrderConfigContextThenKey 沿用户逐步修正输入的过程，锁定
// 三类错误来源的报告顺序与可辨识性：配置缺字段 → 上下文值类型错误 → 开关键
// 未找到。每个阶段分别请求合法的关闭开关 off-ok 与不存在的开关键，两种模式的
// 行为一致，且失败时 stdout 必须完全为空。
func TestCLIEvaluateErrorOrderConfigContextThenKey(t *testing.T) {
	keys := []struct {
		name string
		key  string
	}{
		{"existing disabled flag", orderExistingKey},
		{"unknown flag key", orderUnknownKey},
	}

	// 阶段一：配置缺 default、上下文也是非字符串值。无论请求哪个开关键、无论
	// 哪种模式，都只能看到配置错误，位置指向有问题的那个开关（flags[1]）。
	// 尤其不能因为 off-ok 已关闭就直接返回 disabled，也不能提前报上下文类型
	// 错误或开关键不存在。
	for _, kc := range keys {
		for _, mc := range orderModes {
			t.Run("config-broken/"+kc.name+"/"+mc.name, func(t *testing.T) {
				h := newHarness(t, orderConfigMissingDefault, orderContextBadType)
				code, stdout, stderrStr := orderRun(t, h, kc.key, mc.explain)
				if code == 0 {
					t.Fatalf("expected non-zero exit, stdout=%q", stdout)
				}
				if stdout != "" {
					t.Fatalf("stdout must be empty on config failure (no result, no half explanation): %q", stdout)
				}
				if !strings.HasPrefix(stderrStr, "config.flags[1].default: field is required") {
					t.Fatalf("stderr must point at the broken flag's missing default, got %q", stderrStr)
				}
				for _, not := range []string{
					"context." + orderBadAttr, // 不能提前反馈上下文问题
					"value must be a string",
					"not found", // 不能提前反馈开关键不存在
				} {
					if strings.Contains(stderrStr, not) {
						t.Fatalf("stderr=%q must not contain %q while config is invalid", stderrStr, not)
					}
				}
			})
		}
	}

	// 阶段二：只补齐配置中的缺失字段，保留上下文错误。两种开关键请求、两种
	// 模式都必须改报上下文属性必须为字符串，并指出对应属性；仍不能提前报开关
	// 键不存在，请求 off-ok 也不能返回 disabled。
	for _, kc := range keys {
		for _, mc := range orderModes {
			t.Run("context-broken/"+kc.name+"/"+mc.name, func(t *testing.T) {
				h := newHarness(t, orderConfigFixed, orderContextBadType)
				code, stdout, stderrStr := orderRun(t, h, kc.key, mc.explain)
				if code == 0 {
					t.Fatalf("expected non-zero exit, stdout=%q", stdout)
				}
				if stdout != "" {
					t.Fatalf("stdout must be empty on context failure: %q", stdout)
				}
				if !strings.HasPrefix(stderrStr, "context."+orderBadAttr+": value must be a string") {
					t.Fatalf("stderr must point at context.%s type error, got %q", orderBadAttr, stderrStr)
				}
				if strings.Contains(stderrStr, "not found") {
					t.Fatalf("context error must precede flag lookup: stderr=%q", stderrStr)
				}
				if strings.Contains(stderrStr, "field is required") {
					t.Fatalf("config is fixed; must not report config errors anymore: %q", stderrStr)
				}
			})
		}
	}

	// 阶段三：两份文件都合法，继续请求不存在的开关键，才报开关未找到。两种
	// 模式一致。
	for _, mc := range orderModes {
		t.Run("key-missing/"+mc.name, func(t *testing.T) {
			h := newHarness(t, orderConfigFixed, orderContextFixed)
			code, stdout, stderrStr := orderRun(t, h, orderUnknownKey, mc.explain)
			if code == 0 {
				t.Fatalf("expected non-zero exit, stdout=%q", stdout)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty on lookup failure: %q", stdout)
			}
			want := `evaluate: flag key "` + orderUnknownKey + `" not found in config`
			if !strings.HasPrefix(stderrStr, want) {
				t.Fatalf("stderr must report missing flag key, got %q", stderrStr)
			}
			for _, not := range []string{"value must be a string", "field is required"} {
				if strings.Contains(stderrStr, not) {
					t.Fatalf("both inputs are valid; stderr=%q must not contain %q", stderrStr, not)
				}
			}
		})
	}
}

// TestCLIEvaluateErrorOrderExplainUsesSameErrorsAsPlain 锁定解释模式不另设错误
// 输出格式：在三个失败阶段上，--explain 与普通模式的 stderr 逐字节一致，
// stdout 同样完全为空。
func TestCLIEvaluateErrorOrderExplainUsesSameErrorsAsPlain(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		context string
		key     string
	}{
		{"config error with existing key", orderConfigMissingDefault, orderContextBadType, orderExistingKey},
		{"config error with unknown key", orderConfigMissingDefault, orderContextBadType, orderUnknownKey},
		{"context error with existing key", orderConfigFixed, orderContextBadType, orderExistingKey},
		{"context error with unknown key", orderConfigFixed, orderContextBadType, orderUnknownKey},
		{"lookup error", orderConfigFixed, orderContextFixed, orderUnknownKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 两种模式共用同一份文件：查找失败的错误信息含配置路径，只有同一个
			// harness 才能逐字节比较 stderr。
			h := newHarness(t, tc.config, tc.context)
			pCode, pOut, pErr := orderRun(t, h, tc.key, false)
			eCode, eOut, eErr := orderRun(t, h, tc.key, true)
			if pCode == 0 || eCode == 0 {
				t.Fatalf("both modes must fail: plain=%d explain=%d", pCode, eCode)
			}
			if pCode != eCode {
				t.Fatalf("exit codes differ: plain=%d explain=%d", pCode, eCode)
			}
			if pOut != "" || eOut != "" {
				t.Fatalf("both modes must leave stdout empty: plain=%q explain=%q", pOut, eOut)
			}
			if pErr != eErr {
				t.Fatalf("stderr must use the same existing form:\nplain:   %q\nexplain: %q", pErr, eErr)
			}
		})
	}
}

// TestCLIEvaluateDisabledSuccessAfterFixesDistinctFromFailure 锁定修正完成后的
// 成功形态：请求合法的关闭开关 off-ok 时退出码为 0、stderr 为空，返回
// value=false、reason=disabled、ruleId=null——这是成功结果，与前面的非零退出
// 失败明确区分。普通模式仍只有既有四个结果字段；解释模式保留相同的四个字段，
// 并给出空的规则列表。
func TestCLIEvaluateDisabledSuccessAfterFixesDistinctFromFailure(t *testing.T) {
	wantPlain := `{"key":"off-ok","value":false,"reason":"disabled","ruleId":null}`

	for _, mc := range orderModes {
		t.Run(mc.name, func(t *testing.T) {
			h := newHarness(t, orderConfigFixed, orderContextFixed)
			code, stdout, stderrStr := orderRun(t, h, orderExistingKey, mc.explain)
			// 关键区分：同样是 false，这里是退出码 0 的成功，而不是输入失败。
			if code != 0 {
				t.Fatalf("fixed inputs must succeed even though the value is false: exit=%d stderr=%q", code, stderrStr)
			}
			if stderrStr != "" {
				t.Fatalf("stderr must be empty on success: %q", stderrStr)
			}
			out := strings.TrimSpace(stdout)
			if strings.Contains(out, "\n") {
				t.Fatalf("stdout must contain exactly one JSON object line: %q", out)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("stdout is not one JSON object: %v (%q)", err, out)
			}
			if got["key"] != orderExistingKey || got["value"] != false ||
				got["reason"] != "disabled" || got["ruleId"] != nil {
				t.Fatalf("disabled flag must succeed with false/disabled/null: %v", got)
			}

			if !mc.explain {
				// 普通模式：恰好既有四个字段，且输出逐字保持公开形式。
				if len(got) != 4 {
					t.Fatalf("plain mode must keep exactly the four existing fields, got %d: %s", len(got), out)
				}
				if out != wantPlain {
					t.Fatalf("plain output changed:\ngot:  %q\nwant: %q", out, wantPlain)
				}
				if strings.Contains(out, "explanation") {
					t.Fatalf("plain mode must not include explanation: %q", out)
				}
				return
			}

			// 解释模式：顶层为四字段结果加 explanation，共五个字段。
			if len(got) != 5 {
				t.Fatalf("explain mode must have exactly 5 top-level fields, got %d: %s", len(got), out)
			}
			exp, ok := got["explanation"].(map[string]any)
			if !ok || len(exp) != 2 {
				t.Fatalf("explanation must be the existing object with outcome and rules: %v", got["explanation"])
			}
			rules, ok := exp["rules"].([]any)
			if !ok {
				t.Fatalf("explanation.rules must be an array: %v", exp["rules"])
			}
			if len(rules) != 0 {
				t.Fatalf("disabled flag must keep an empty rule list, got %v", rules)
			}
			if !strings.Contains(strings.ToLower(exp["outcome"].(string)), "disabled") {
				t.Fatalf("outcome must explain the disabled fixed-false result: %v", exp["outcome"])
			}
			// 四字段与普通模式同一输入的结果逐字一致（解释模式在其后追加
			// explanation，不能按整行前缀直接比较）。
			hPlain := newHarness(t, orderConfigFixed, orderContextFixed)
			pCode, pOut, pErr := orderRun(t, hPlain, orderExistingKey, false)
			if pCode != 0 || pErr != "" {
				t.Fatalf("plain counterpart must succeed: code=%d stderr=%q", pCode, pErr)
			}
			if plainFour := strings.TrimSpace(pOut); plainFour != wantPlain {
				t.Fatalf("plain four fields changed: %q", plainFour)
			}
			var plainGot map[string]any
			if err := json.Unmarshal([]byte(pOut), &plainGot); err != nil {
				t.Fatalf("plain output is not JSON: %v (%q)", err, pOut)
			}
			for _, k := range []string{"key", "value", "reason", "ruleId"} {
				if plainGot[k] != got[k] {
					t.Fatalf("field %s differs between plain and explain: plain=%v explain=%v",
						k, plainGot[k], got[k])
				}
			}
		})
	}
}
