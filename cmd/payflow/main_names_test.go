package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 标识名称的 CLI 回归保障：开关键与规则 id 按解码后的文字识别，原文写法
// （直接字符 / \uXXXX 转义 / 混合）不改变开关选择、唯一性判断与结果中的名称。

// cliKeySpellings 给出开关键 "结算😀" 的三种合法 JSON 写法（含普通文字与必须
// 靠完整代理项配对才能用转义表示的 😀）；cliIDSpellings 是规则编号 "r-😀" 的
// 同样三种写法（\u002D 即 '-'）。
var cliKeySpellings = []struct{ name, json string }{
	{"literal", `结算😀`},
	{"escaped", `\u7ED3\u7B97\uD83D\uDE00`},
	{"mixed", `结\u7B97😀`},
}
var cliIDSpellings = []struct{ name, json string }{
	{"literal", `r-😀`},
	{"escaped", `r-\uD83D\uDE00`},
	{"mixed", `r\u002D😀`},
}

func TestCLIEvaluateNameSpellingsEquivalent(t *testing.T) {
	var firstOut string
	for _, ks := range cliKeySpellings {
		for _, rs := range cliIDSpellings {
			t.Run(ks.name+"-key/"+rs.name+"-id", func(t *testing.T) {
				cfg := `{"flags":[{"key":"` + ks.json + `","enabled":true,"default":false,"rules":[
					{"id":"` + rs.json + `","value":true,"conditions":[
						{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
				h := newHarness(t, cfg, `{"plan":"pro"}`)
				var stdout, stderr bytes.Buffer
				// 用解码后的开关键请求求值。
				code := run(h.evalArgs("结算😀"), &stdout, &stderr)
				if code != 0 {
					t.Fatalf("exit=%d stderr=%s", code, stderr.String())
				}
				if stderr.Len() != 0 {
					t.Fatalf("stderr must be empty on success: %q", stderr.String())
				}
				out := stdout.String()
				if strings.Count(strings.TrimSpace(out), "\n") != 0 {
					t.Fatalf("stdout must contain exactly one JSON object line: %q", out)
				}
				var got map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatalf("stdout is not a single JSON object: %q (%v)", out, err)
				}
				// 结果的 key 与 ruleId 还原为解码后的同一名称。
				if got["key"] != "结算😀" || got["value"] != true ||
					got["reason"] != "rule" || got["ruleId"] != "r-😀" {
					t.Fatalf("unexpected payload: %v", got)
				}
				// 原文写法不影响输出：所有拼写变体的结果逐字节一致。
				if firstOut == "" {
					firstOut = out
				} else if out != firstOut {
					t.Fatalf("spelling changed output: %q != %q", out, firstOut)
				}
			})
		}
	}
}

func TestCLIEvaluateNameConflictFailsWholeConfig(t *testing.T) {
	// 冲突按解码后的文字判断，且位于未选中或已关闭的开关时，请求另一个合法
	// 开关也不能得到结果：命令非零退出、stdout 为空，错误区分开关键重复与
	// 规则编号重复，指出后出现与第一次出现的位置，不误报为字段重复。
	cases := []struct {
		name    string
		config  string
		wantSub []string
	}{
		{
			"duplicate key in unselected flags",
			`{"flags":[
				{"key":"good","enabled":true,"default":false,"rules":[]},
				{"key":"结算😀","enabled":true,"default":false,"rules":[]},
				{"key":"\u7ED3\u7B97\uD83D\uDE00","enabled":true,"default":false,"rules":[]}]}`,
			[]string{`config.flags[2].key: duplicate flag key "结算😀"`, "first at flags[1]"},
		},
		{
			"duplicate rule id in disabled flag",
			`{"flags":[
				{"key":"good","enabled":true,"default":false,"rules":[]},
				{"key":"off","enabled":false,"default":false,"rules":[
					{"id":"r-😀","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},
					{"id":"r-\uD83D\uDE00","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`,
			[]string{`config.flags[1].rules[1].id: duplicate rule id "r-😀"`, "first at rules[0]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.config, `{}`)
			var stdout, stderr bytes.Buffer
			code := run(h.evalArgs("good"), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("expected non-zero exit, stdout=%q", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty on error, got %q", stdout.String())
			}
			msg := stderr.String()
			for _, want := range tc.wantSub {
				if !strings.Contains(msg, want) {
					t.Fatalf("stderr=%q, want substring %q", msg, want)
				}
			}
			if strings.Contains(msg, "duplicate field") {
				t.Fatalf("name uniqueness must not read as duplicate field: %q", msg)
			}
		})
	}
}

func TestCLIEvaluateSameDecodedRuleIDAcrossFlags(t *testing.T) {
	// 不同开关各自使用解码后相同的规则编号（一个直接写、一个转义写）仍然
	// 合法；求值结果中的编号属于实际选中的开关。
	cfg := `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r-😀","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]},
		{"key":"g","enabled":true,"default":false,"rules":[
			{"id":"r-\uD83D\uDE00","value":false,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`
	h := newHarness(t, cfg, `{"a":"x"}`)
	for _, tc := range []struct {
		key   string
		value bool
	}{
		{"f", true},
		{"g", false},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(h.evalArgs(tc.key), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("%s: exit=%d stderr=%q", tc.key, code, stderr.String())
		}
		var got map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatalf("%s: stdout is not a single JSON object: %q", tc.key, stdout.String())
		}
		if got["key"] != tc.key || got["value"] != tc.value ||
			got["reason"] != "rule" || got["ruleId"] != "r-😀" {
			t.Fatalf("%s: unexpected payload: %v", tc.key, got)
		}
	}
}

func TestCLIEvaluateSimilarNameNotFound(t *testing.T) {
	// 仅在大小写、前后空白或字符序列上相似、实际不存在的开关键必须报告
	// 未找到并保持 stdout 为空，不能选择近似名称。caf\u00E9 是预组合
	// 形式，请求的 cafe\u0301 是 e + 组合尖音符的另一个序列。
	cfg := `{"flags":[
		{"key":"Feature","enabled":true,"default":false,"rules":[]},
		{"key":"caf\u00E9","enabled":true,"default":false,"rules":[]},
		{"key":"aAb","enabled":true,"default":false,"rules":[]}]}`
	cases := []struct {
		name string
		key  string
	}{
		{"different case", "feature"},
		{"extra trailing space", "Feature "},
		{"decomposed accent sequence", "cafe\u0301"},
		{"escape-looking text not decoded again", `a\u0041b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, cfg, `{}`)
			var stdout, stderr bytes.Buffer
			code := run(h.evalArgs(tc.key), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("similar key %q must not evaluate, stdout=%q", tc.key, stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must be empty on not-found, got %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "not found") {
				t.Fatalf("stderr=%q, want not found", stderr.String())
			}
		})
	}
}

func TestCLIEvaluateBackslashTextKeyNotRedecoded(t *testing.T) {
	// 配置里的 "a\\u0041b" 解码一次后是普通文本 a\u0041b：请求 "aAb"
	// 不能命中（不能二次解码），请求原文文本 a\u0041b 才命中，且结果中的
	// key 保持这段原文文本。
	cfg := `{"flags":[{"key":"a\\u0041b","enabled":true,"default":false,"rules":[
		{"id":"r\\u0041","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`
	h := newHarness(t, cfg, `{"a":"x"}`)

	var stdout, stderr bytes.Buffer
	if code := run(h.evalArgs("aAb"), &stdout, &stderr); code == 0 {
		t.Fatalf("aAb must not match the backslash-text key, stdout=%q", stdout.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must be empty on not-found, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("stderr=%q, want not found", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(h.evalArgs(`a\u0041b`), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("literal backslash text key: exit=%d stderr=%q", code, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a single JSON object: %q", stdout.String())
	}
	if got["key"] != `a\u0041b` || got["value"] != true ||
		got["reason"] != "rule" || got["ruleId"] != `r\u0041` {
		t.Fatalf("backslash text must stay verbatim in the result: %v", got)
	}
}
