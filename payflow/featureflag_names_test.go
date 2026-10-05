package payflow

import (
	"strings"
	"testing"
)

// 标识名称的回归保障：配置中的开关键与规则 id 都是非空字符串，同一个名字可以
// 直接写成 UTF-8 文字，也可以写成合法的 JSON Unicode 转义。名称读入后按解码
// 得到的文字识别：原文采用哪种写法不改变开关选择、唯一性判断或结果中的名称。

// decodedKeySpellings 给出开关键 "结算😀" 的三种合法 JSON 写法：直接字符、
// 完整 \uXXXX 转义（结=\u7ED3、算=\u7B97、😀=代理项对 \uD83D\uDE00）、
// 两者混合。名字同时包含普通文字与必须靠完整代理项配对才能用转义表示的字符。
var decodedKeySpellings = []struct {
	name string
	json string // 配置中 key 字段的字符串内容写法（不含引号）
}{
	{"literal", `结算😀`},
	{"escaped", `\u7ED3\u7B97\uD83D\uDE00`},
	{"mixed", `结\u7B97😀`},
}

// decodedRuleIDSpellings 给出规则编号 "r-😀" 的同样三种写法（\u002D 即 '-'）。
var decodedRuleIDSpellings = []struct {
	name string
	json string
}{
	{"literal", `r-😀`},
	{"escaped", `r-\uD83D\uDE00`},
	{"mixed", `r\u002D😀`},
}

func TestFlagKeyAndRuleIDRecognizedByDecodedText(t *testing.T) {
	const wantKey, wantRuleID = "结算😀", "r-😀"
	wantID := wantRuleID
	want := EvalResult{Key: wantKey, Value: true, Reason: EvalRule, RuleID: &wantID}
	wantJSON := `{"key":"结算😀","value":true,"reason":"rule","ruleId":"r-😀"}`
	for _, ks := range decodedKeySpellings {
		for _, rs := range decodedRuleIDSpellings {
			t.Run(ks.name+" key / "+rs.name+" id", func(t *testing.T) {
				cfg := `{"flags":[{"key":"` + ks.json + `","enabled":true,"default":false,"rules":[
					{"id":"` + rs.json + `","value":true,"conditions":[
						{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
				c := mustParseConfig(t, cfg)
				// 用解码后的开关键请求：无论原文采用哪种写法都找到同一个开关。
				flag := c.Find(wantKey)
				if flag == nil {
					t.Fatalf("decoded key %q must find flag spelled %q", wantKey, ks.json)
				}
				// 原文的转义写法本身（未解码的反斜杠文本）不是这个开关的名字。
				if ks.name != "literal" && c.Find(ks.json) != nil {
					t.Fatalf("raw spelling %q must not be a second name", ks.json)
				}
				// 同一上下文命中同一条规则，布尔结果与原因一致。
				got := flag.Evaluate(mustParseContext(t, `{"plan":"pro"}`))
				if !got.Equals(want) {
					t.Fatalf("got %+v, want %+v", got, want)
				}
				// 结果中的 key 与 ruleId 还原为解码后的同一名称，与原文写法无关。
				out, err := MarshalResult(got)
				if err != nil {
					t.Fatal(err)
				}
				if string(out) != wantJSON {
					t.Fatalf("marshaled result = %s, want %s", out, wantJSON)
				}
			})
		}
	}
}

func TestDuplicateFlagKeyAcrossSpellingForms(t *testing.T) {
	// 两个开关分别用不同写法声明同一个解码后的 key：整份配置必须被拒绝，
	// 错误按开关键唯一性报告（区别于同一对象内的字段重复），并指出后出现
	// 的位置与第一次出现的位置。
	cases := []struct {
		name   string
		first  string
		second string
	}{
		{"literal then escaped", `结算😀`, `\u7ED3\u7B97\uD83D\uDE00`},
		{"escaped then literal", `\u7ED3\u7B97\uD83D\uDE00`, `结算😀`},
		{"mixed then literal", `结\u7B97😀`, `结算😀`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"flags":[
				{"key":"` + tc.first + `","enabled":true,"default":false,"rules":[]},
				{"key":"` + tc.second + `","enabled":true,"default":false,"rules":[]},
				{"key":"other","enabled":true,"default":false,"rules":[]}]}`
			_, err := ParseConfig([]byte(raw))
			if err == nil {
				t.Fatal("expected duplicate flag key error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, `config.flags[1].key: duplicate flag key "结算😀"`) {
				t.Fatalf("err=%q, want duplicate flag key at flags[1] with decoded name", msg)
			}
			if !strings.Contains(msg, "first at flags[0]") {
				t.Fatalf("err=%q, want first occurrence position", msg)
			}
			if strings.Contains(msg, "duplicate field") {
				t.Fatalf("key uniqueness must not read as duplicate field: %q", msg)
			}
		})
	}
}

func TestDuplicateRuleIDAcrossSpellingForms(t *testing.T) {
	// 同一开关里的两条规则用不同写法声明同一个解码后的 id：整份配置必须被
	// 拒绝，错误按规则编号唯一性报告，指出后出现的位置与第一次出现的位置。
	cases := []struct {
		name   string
		first  string
		second string
	}{
		{"literal then escaped", `r-😀`, `r-\uD83D\uDE00`},
		{"escaped then literal", `r-\uD83D\uDE00`, `r-😀`},
		{"mixed then literal", `r\u002D😀`, `r-😀`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"` + tc.first + `","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},
				{"id":"` + tc.second + `","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`
			_, err := ParseConfig([]byte(raw))
			if err == nil {
				t.Fatal("expected duplicate rule id error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, `config.flags[0].rules[1].id: duplicate rule id "r-😀"`) {
				t.Fatalf("err=%q, want duplicate rule id at rules[1] with decoded name", msg)
			}
			if !strings.Contains(msg, "first at rules[0]") {
				t.Fatalf("err=%q, want first occurrence position", msg)
			}
			if strings.Contains(msg, "duplicate field") {
				t.Fatalf("rule id uniqueness must not read as duplicate field: %q", msg)
			}
		})
	}
}

func TestSameDecodedRuleIDInDifferentFlagsAllowed(t *testing.T) {
	// 规则编号的唯一性以开关为界：不同开关各自使用解码后相同的编号（一个直接
	// 写、一个转义写）仍然合法；求值结果中的编号属于实际选中的开关。
	cfg := `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r-😀","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]},
		{"key":"g","enabled":true,"default":false,"rules":[
			{"id":"r-\uD83D\uDE00","value":false,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`
	c := mustParseConfig(t, cfg)
	ctx := mustParseContext(t, `{"a":"x"}`)
	sharedID := "r-😀"
	gotF := c.Find("f").Evaluate(ctx)
	if want := (EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &sharedID}); !gotF.Equals(want) {
		t.Fatalf("flag f: got %+v, want %+v", gotF, want)
	}
	gotG := c.Find("g").Evaluate(ctx)
	if want := (EvalResult{Key: "g", Value: false, Reason: EvalRule, RuleID: &sharedID}); !gotG.Equals(want) {
		t.Fatalf("flag g: got %+v, want %+v", gotG, want)
	}
}

func TestSimilarNamesStayDistinct(t *testing.T) {
	// 大小写不同、前后空白不同、外观相似但字符序列不同的名称互不合并、互不
	// 算重复：caf\u00E9（é 为预组合单码点）与 cafe\u0301（e + 组合尖音符
	// 两个码点）序列不同；fl\u0430g 中的 а 是西里尔字母，与拉丁 a 不同。
	cfg := `{"flags":[
		{"key":"Feature","enabled":true,"default":false,"rules":[]},
		{"key":"feature","enabled":true,"default":true,"rules":[]},
		{"key":"feature ","enabled":false,"default":false,"rules":[]},
		{"key":"caf\u00E9","enabled":true,"default":false,"rules":[]},
		{"key":"cafe\u0301","enabled":true,"default":true,"rules":[]},
		{"key":"fl\u0430g","enabled":true,"default":false,"rules":[]}]}`
	c := mustParseConfig(t, cfg)
	if len(c.Flags) != 6 {
		t.Fatalf("similar names must not be merged or rejected as duplicates: got %d flags", len(c.Flags))
	}
	// 每个名字仍按各自的精确序列找到各自的开关。
	if got := c.Find("Feature"); got == nil || got.Default {
		t.Fatalf("Feature: %+v", got)
	}
	if got := c.Find("feature"); got == nil || !got.Default {
		t.Fatalf("feature: %+v", got)
	}
	if got := c.Find("feature "); got == nil || got.Enabled {
		t.Fatalf("trailing-space key: %+v", got)
	}
	if got := c.Find("caf\u00E9"); got == nil || got.Default {
		t.Fatalf("precomposed café: %+v", got)
	}
	if got := c.Find("cafe\u0301"); got == nil || !got.Default {
		t.Fatalf("decomposed café: %+v", got)
	}
	if got := c.Find("fl\u0430g"); got == nil {
		t.Fatalf("cyrillic-a flag: %+v", got)
	}
	// 仅在大小写、空白或字符序列上相似、实际不存在的名字不能命中任何开关。
	for _, absent := range []string{
		"FEATURE",   // 大小写不同
		" feature",  // 前导空白不同
		"feature  ", // 空白数量不同
		"cafe",      // 缺少尖音符
		"flag",      // 配置里只有西里尔 а 的版本
	} {
		if c.Find(absent) != nil {
			t.Fatalf("similar-but-absent key %q must not match any flag", absent)
		}
	}
}

func TestBackslashTextNameNotDecodedAgain(t *testing.T) {
	// 配置里的 "a\\u0041b" 经 JSON 解码一次后是普通文本 a\u0041b
	// （含反斜杠的 8 个字符），不能再被当成转义二次解码成 "aAb"。规则编号同理。
	cfg := `{"flags":[{"key":"a\\u0041b","enabled":true,"default":false,"rules":[
		{"id":"r\\u0041","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`
	c := mustParseConfig(t, cfg)
	if c.Find("aAb") != nil {
		t.Fatal("backslash text must not be decoded a second time into aAb")
	}
	flag := c.Find(`a\u0041b`)
	if flag == nil {
		t.Fatal("literal backslash text key must be found by its decoded text")
	}
	got := flag.Evaluate(mustParseContext(t, `{"a":"x"}`))
	id := `r\u0041`
	want := EvalResult{Key: `a\u0041b`, Value: true, Reason: EvalRule, RuleID: &id}
	if !got.Equals(want) {
		t.Fatalf("backslash text names: got %+v, want %+v", got, want)
	}
	// 反向同样成立：配置直接写 "aAb"，请求文本 a\u0041b 不能命中。
	cfg2 := `{"flags":[{"key":"aAb","enabled":true,"default":true,"rules":[]}]}`
	if mustParseConfig(t, cfg2).Find(`a\u0041b`) != nil {
		t.Fatal("raw escape-looking text must not match the directly written name")
	}
}
