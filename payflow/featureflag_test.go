package payflow

import (
	"strings"
	"testing"
)

func mustParseConfig(t *testing.T, raw string) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig failed: %v\nconfig: %s", err, raw)
	}
	return cfg
}

func mustParseContext(t *testing.T, raw string) *Context {
	t.Helper()
	ctx, err := ParseContext([]byte(raw))
	if err != nil {
		t.Fatalf("ParseContext failed: %v\ncontext: %s", err, raw)
	}
	return ctx
}

func eval(t *testing.T, cfgRaw, key, ctxRaw string) EvalResult {
	t.Helper()
	cfg := mustParseConfig(t, cfgRaw)
	ctx := mustParseContext(t, ctxRaw)
	flag := cfg.Find(key)
	if flag == nil {
		t.Fatalf("flag %q not found", key)
	}
	return flag.Evaluate(ctx)
}

func TestEvaluateDisabled(t *testing.T) {
	// 即使 default=true 且规则命中，关闭的开关固定 false。
	cfg := `{"flags":[{"key":"f","enabled":false,"default":true,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	got := eval(t, cfg, "f", `{"plan":"pro"}`)
	want := EvalResult{Key: "f", Value: false, Reason: EvalDisabled, RuleID: nil}
	if !got.Equals(want) {
		t.Fatalf("disabled: got %+v, want %+v", got, want)
	}
}

func TestEvaluateRuleMatchOrder(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]},
		{"id":"r2","value":false,"conditions":[{"attribute":"tier","op":"in","value":["a","b"]}]}]}]}`
	// 首条全中规则获胜，r1 命中即采用其 value=true，r2 不再考虑。
	got := eval(t, cfg, "f", `{"plan":"pro","tier":"a"}`)
	id := "r1"
	want := EvalResult{Key: "f", Value: true, Reason: EvalRule, RuleID: &id}
	if !got.Equals(want) {
		t.Fatalf("first-match: got %+v, want %+v", got, want)
	}

	// r1 不成立，r2 成立。
	got = eval(t, cfg, "f", `{"plan":"free","tier":"b"}`)
	id = "r2"
	want = EvalResult{Key: "f", Value: false, Reason: EvalRule, RuleID: &id}
	if !got.Equals(want) {
		t.Fatalf("second-rule: got %+v, want %+v", got, want)
	}
}

func TestEvaluateDefault(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r1","value":false,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	got := eval(t, cfg, "f", `{"plan":"free"}`)
	want := EvalResult{Key: "f", Value: true, Reason: EvalDefault, RuleID: nil}
	if !got.Equals(want) {
		t.Fatalf("default true: got %+v, want %+v", got, want)
	}

	// 无规则时也采用 default。
	cfg = `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[]}]}`
	got = eval(t, cfg, "g", `{}`)
	want = EvalResult{Key: "g", Value: false, Reason: EvalDefault, RuleID: nil}
	if !got.Equals(want) {
		t.Fatalf("empty rules: got %+v, want %+v", got, want)
	}
}

func TestEqSemantics(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"Pro"}]}]}]}`
	cases := map[string]string{
		`{"plan":"Pro"}`:  "match",
		`{"plan":"pro"}`:  "case",       // 区分大小写
		`{"plan":" Pro"}`: "whitespace", // 不裁剪空白
		`{}`:              "missing",
	}
	for ctxRaw, label := range cases {
		got := eval(t, cfg, "f", ctxRaw)
		matched := got.Reason == EvalRule
		wantMatch := label == "match"
		if matched != wantMatch {
			t.Errorf("%s (%s): match=%v want %v", label, ctxRaw, matched, wantMatch)
		}
	}

	// 空字符串是合法属性值，可以被 eq 命中。
	cfgEmpty := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"note","op":"eq","value":""}]}]}]}`
	got := eval(t, cfgEmpty, "f", `{"note":""}`)
	if got.Reason != EvalRule || !got.Value {
		t.Fatalf("empty string eq: got %+v", got)
	}
	// 属性缺失与属性为空字符串不同。
	got = eval(t, cfgEmpty, "f", `{}`)
	if got.Reason != EvalDefault {
		t.Fatalf("missing attr vs empty: got %+v", got)
	}
}

func TestInSemantics(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"tier","op":"in","value":["a","b",""]}]}]}]}`
	hit := eval(t, cfg, "f", `{"tier":"a"}`)
	if hit.Reason != EvalRule {
		t.Fatalf("in a: got %+v", hit)
	}
	miss := eval(t, cfg, "f", `{"tier":"c"}`)
	if miss.Reason != EvalDefault {
		t.Fatalf("in c: got %+v", miss)
	}
	emptyHit := eval(t, cfg, "f", `{"tier":""}`)
	if emptyHit.Reason != EvalRule {
		t.Fatalf("in empty string: got %+v", emptyHit)
	}
	missing := eval(t, cfg, "f", `{"other":"a"}`)
	if missing.Reason != EvalDefault {
		t.Fatalf("in missing attr: got %+v", missing)
	}
}

func TestConditionsAreAND(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"},
			{"attribute":"region","op":"in","value":["cn","us"]}]}]}]}`
	if got := eval(t, cfg, "f", `{"plan":"pro","region":"cn"}`); got.Reason != EvalRule {
		t.Fatalf("both hold: got %+v", got)
	}
	if got := eval(t, cfg, "f", `{"plan":"pro"}`); got.Reason != EvalDefault {
		t.Fatalf("one missing: got %+v", got)
	}
	if got := eval(t, cfg, "f", `{"plan":"pro","region":"eu"}`); got.Reason != EvalDefault {
		t.Fatalf("one mismatch: got %+v", got)
	}
	// 其他属性不参与判断。
	if got := eval(t, cfg, "f", `{"plan":"pro","region":"us","extra":"whatever"}`); got.Reason != EvalRule {
		t.Fatalf("extra attrs: got %+v", got)
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"tier","op":"in","value":["a","b"]}]}]}]}`
	c := mustParseConfig(t, cfg)
	ctx := mustParseContext(t, `{"tier":"b","x":"1","y":"2","z":"3"}`)
	flag := c.Find("f")
	var first EvalResult
	for i := 0; i < 100; i++ {
		got := flag.Evaluate(ctx)
		if i == 0 {
			first = got
			continue
		}
		if !got.Equals(first) {
			t.Fatalf("iteration %d: %+v != %+v", i, got, first)
		}
	}
}

func TestMarshalResultShape(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-1","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`
	got := eval(t, cfg, "f", `{"a":"x"}`)
	out, err := MarshalResult(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"f","value":true,"reason":"rule","ruleId":"r-1"}`
	if string(out) != want {
		t.Fatalf("marshal rule: got %s want %s", out, want)
	}

	disabled := eval(t, `{"flags":[{"key":"f","enabled":false,"default":true,"rules":[]}]}`, "f", `{}`)
	out, _ = MarshalResult(disabled)
	want = `{"key":"f","value":false,"reason":"disabled","ruleId":null}`
	if string(out) != want {
		t.Fatalf("marshal disabled: got %s want %s", out, want)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 错误信息需包含的定位片段
	}{
		{"not object", `[1,2]`, "top level"},
		{"flags missing", `{}`, "config.flags"},
		{"flags not array", `{"flags":{}}`, "config.flags"},
		{"flags null", `{"flags":null}`, "config.flags"},
		{"flag not object", `{"flags":[1]}`, "flags[0]"},
		{"key missing", `{"flags":[{"enabled":true,"default":false,"rules":[]}]}`, `flags[0].key`},
		{"key empty", `{"flags":[{"key":"","enabled":true,"default":false,"rules":[]}]}`, `flags[0].key`},
		{"key wrong type", `{"flags":[{"key":1,"enabled":true,"default":false,"rules":[]}]}`, `flags[0].key`},
		{"enabled missing must not default to false", `{"flags":[{"key":"f","default":false,"rules":[]}]}`, `flags[0].enabled`},
		{"enabled wrong type", `{"flags":[{"key":"f","enabled":"yes","default":false,"rules":[]}]}`, `flags[0].enabled`},
		{"enabled numeric", `{"flags":[{"key":"f","enabled":1,"default":false,"rules":[]}]}`, `flags[0].enabled`},
		{"default missing", `{"flags":[{"key":"f","enabled":true,"rules":[]}]}`, `flags[0].default`},
		{"rules missing", `{"flags":[{"key":"f","enabled":true,"default":false}]}`, `flags[0].rules`},
		{"rules not array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":1}]}`, `flags[0].rules`},
		{"rule not object", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[1]}]}`, `rules[0]`},
		{"rule id missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].id`},
		{"rule id empty", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].id`},
		{"rule value missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].value`},
		{"rule value wrong type", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":"true","conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].value`},
		{"conditions missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true}]}]}`, `rules[0].conditions`},
		{"conditions empty", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[]}]}]}`, `rules[0].conditions`},
		{"condition not object", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[1]}]}]}`, `conditions[0]`},
		{"attribute missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"op":"eq","value":"x"}]}]}]}`, `conditions[0].attribute`},
		{"attribute empty", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"","op":"eq","value":"x"}]}]}]}`, `conditions[0].attribute`},
		{"op missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","value":"x"}]}]}]}`, `conditions[0].op`},
		{"op unknown", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"neq","value":"x"}]}]}]}`, `conditions[0].op`},
		{"value missing", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq"}]}]}]}`, `conditions[0].value`},
		{"eq value non-string", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":1}]}]}]}`, `conditions[0].value`},
		{"eq value bool", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":true}]}]}]}`, `conditions[0].value`},
		{"in value not array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":"x"}]}]}]}`, `conditions[0].value`},
		{"in value empty array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":[]}]}]}]}`, `conditions[0].value`},
		{"in item non-string", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":["x",1]}]}]}]}`, `conditions[0].value[1]`},
		{"duplicate flag key", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[]},
			{"key":"f","enabled":true,"default":false,"rules":[]}]}`, `flags[1].key`},
		{"duplicate rule id", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},
			{"id":"r","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`, `rules[1].id`},
		{"invalid json", `{"flags":[`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestRuleIDUniqueAcrossFlagsIsAllowed(t *testing.T) {
	cfg := `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]},
		{"key":"g","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`
	c := mustParseConfig(t, cfg)
	if len(c.Flags) != 2 {
		t.Fatalf("got %d flags", len(c.Flags))
	}
}

func TestUnselectedAndDisabledFlagsAreValidated(t *testing.T) {
	// 请求第一个开关，但第二个开关有错误：仍须整体校验失败。
	raw := `{"flags":[
		{"key":"good","enabled":true,"default":false,"rules":[]},
		{"key":"bad","enabled":true,"rules":[]}]}`
	if _, err := ParseConfig([]byte(raw)); err == nil {
		t.Fatal("expected validation of unselected flag to fail")
	}
	// 关闭的开关规则非法同样要报错。
	raw = `{"flags":[
		{"key":"off","enabled":false,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[]}]}]}`
	if _, err := ParseConfig([]byte(raw)); err == nil {
		t.Fatal("expected validation of disabled flag rules to fail")
	}
}

func TestContextValidation(t *testing.T) {
	if _, err := ParseContext([]byte(`{"a":"x"}`)); err != nil {
		t.Fatalf("valid context: %v", err)
	}
	if _, err := ParseContext([]byte(`{}`)); err != nil {
		t.Fatalf("empty context: %v", err)
	}
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`[]`, "top level"},
		{`{"a":1}`, "context.a"},
		{`{"a":true}`, "context.a"},
		{`{"a":null}`, "context.a"},
		{`{"a":{}}`, "context.a"},
		{`{"a":[]}`, "context.a"},
		{`{bad`, "invalid JSON"},
	} {
		if _, err := ParseContext([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseContext(%s): err=%v want substring %q", tc.raw, err, tc.want)
		}
	}
}

func TestContextValueTypeErrorIsFirstInSourceOrder(t *testing.T) {
	// 多个非字符串属性：报告原文中最先出现的一个，且每次运行一致，
	// 不随 Go map 遍历顺序漂移。
	raw := `{"zeta":false,"plan":"pro","alpha":7}`
	var first string
	for i := 0; i < 20; i++ {
		_, err := ParseContext([]byte(raw))
		if err == nil {
			t.Fatalf("iter %d: expected error", i)
		}
		if i == 0 {
			first = err.Error()
			if !strings.Contains(first, "context.zeta") ||
				!strings.Contains(first, "value must be a string") {
				t.Fatalf("first error = %q, want context.zeta string-type error", first)
			}
			continue
		}
		if err.Error() != first {
			t.Fatalf("iter %d: %q != %q", i, err.Error(), first)
		}
	}

	// 把 alpha 放到 zeta 前面：应改为报告 context.alpha，前面的合法属性不影响选择。
	_, err := ParseContext([]byte(`{"alpha":7,"plan":"pro","zeta":false}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("reordered: err=%v want context.alpha", err)
	}

	// 用户修正第一处错误后再次求值，应看到剩余属性中最先出现的类型错误。
	_, err = ParseContext([]byte(`{"zeta":"z","plan":"pro","alpha":7}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("after fixing zeta: err=%v want context.alpha", err)
	}

	// 全部属性值合法后才返回求值结果；看起来像数字的字符串仍是合法字符串值。
	ctx, err := ParseContext([]byte(`{"zeta":"false","plan":"pro","alpha":"7"}`))
	if err != nil {
		t.Fatalf("all string values must parse: %v", err)
	}
	if v, ok := ctx.Get("alpha"); !ok || v != "7" {
		t.Fatalf("numeric-looking string should stay a string: %q %v", v, ok)
	}
}

func TestContextNonStringValueShapes(t *testing.T) {
	// 数字、布尔、null、数组、对象都不能作为顶层属性值；对象/数组值的错误
	// 指向所属的顶层属性，不指向内部成员。
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"number", `{"a":"ok","b":1}`, "context.b"},
		{"bool", `{"a":"ok","b":false}`, "context.b"},
		{"null", `{"a":"ok","b":null}`, "context.b"},
		{"array", `{"a":"ok","b":["x"]}`, "context.b"},
		{"object", `{"a":"ok","b":{"inner":1}}`, "context.b"},
		{"object inner is string too", `{"obj":{"inner":"x"},"later":1}`, "context.obj"},
		{"array with strings only", `{"arr":["x","y"]}`, "context.arr"},
		{"first of mixed kinds", `{"z":{"x":1},"a":[2],"m":true}`, "context.z"},
		{"nested bad member still points to top key", `{"a":"x","obj":{"inner":1},"b":2}`, "context.obj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseContext([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "value must be a string") {
				t.Errorf("ParseContext(%s): err=%v want location %q", tc.raw, err, tc.want)
			}
		})
	}
}

func TestContextTypeErrorKeyDecoding(t *testing.T) {
	// 属性名采用解码后的文字定位：合法 Unicode 转义与直接字符同义。
	escA := `"\u` + `0061lpha"` // 解码后为 "alpha"
	_, err := ParseContext([]byte(`{` + escA + `:7}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("escaped key: err=%v want context.alpha", err)
	}
	// 转义形式与直接字符匹配同一属性（Get 使用解码后的名字）。
	escP := `"\u` + `0070lan"` // 解码后为 "plan"
	ctx := mustParseContext(t, `{`+escP+`:"pro"}`)
	if v, ok := ctx.Get("plan"); !ok || v != "pro" {
		t.Fatalf("escaped key lookup: %q %v", v, ok)
	}
	// 大小写与空白按原样保留，不改变属性匹配与定位；含空格的名字走方括号定位。
	_, err = ParseContext([]byte(`{"Alpha ":7}`))
	if err == nil || !strings.Contains(err.Error(), `context["Alpha "]`) {
		t.Fatalf("case/space key: err=%v", err)
	}
	// 配对的代理项转义解码后定位到同一属性名；非 ASCII 名字用方括号 JSON 字符串。
	emojiKey := `"\u` + `D83D\u` + `DE00"` // 解码后为 "😀"
	_, err = ParseContext([]byte(`{"a":"x",` + emojiKey + `:2}`))
	if err == nil || !strings.Contains(err.Error(), `context["😀"]`) {
		t.Fatalf("surrogate pair key: err=%v", err)
	}
	// 直接写出的同一 emoji 名字必须得到相同定位（解码后文字一致）。
	_, err = ParseContext([]byte(`{"a":"x","😀":2}`))
	if err == nil || !strings.Contains(err.Error(), `context["😀"]`) {
		t.Fatalf("literal emoji key: err=%v", err)
	}
}

func TestFieldPathFormatting(t *testing.T) {
	// 字段位置统一规则：简单字段名（ASCII 字母/下划线开头，仅含 ASCII 字母数字
	// 下划线）继续用点号；其他名字在父位置后用方括号包住一个 JSON 字符串。
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"dotted name is bracketed", `{"user.name":7}`, `context["user.name"]`},
		{"empty name is bracketed", `{"":7}`, `context[""]`},
		{"newline in name is json escaped", "{\"a\\nb\":7}", `context["a\nb"]`},
		{"tab in name is json escaped", "{\"a\\tb\":7}", `context["a\tb"]`},
		{"quote in name is json escaped", `{"a\"b":7}`, `context["a\"b"]`},
		{"backslash in name is json escaped", `{"a\\b":7}`, `context["a\\b"]`},
		{"brackets are not array indices", `{"names[0]":7}`, `context["names[0]"]`},
		{"leading digit is bracketed", `{"0ab":7}`, `context["0ab"]`},
		{"dash is bracketed", `{"a-b":7}`, `context["a-b"]`},
		{"simple underscore stays dotted", `{"_":7}`, `context._`},
		{"simple alnum stays dotted", `{"plan_2":7}`, `context.plan_2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseContext([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected type error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "value must be a string") {
				t.Fatalf("err=%q want location %q", err.Error(), tc.want)
			}
			// 控制字符必须转义显示：定位文字本身不能被拆成多行。
			if strings.Contains(strings.SplitN(err.Error(), ": ", 2)[0], "\n") {
				t.Fatalf("location text must stay on one line: %q", err.Error())
			}
		})
	}

	// 合法 Unicode 转义先解码再定位：. 与直接写出的点号得到同一位置。
	_, err := ParseContext([]byte(`{"user.name":7}`))
	if err == nil || !strings.Contains(err.Error(), `context["user.name"]`) {
		t.Fatalf("escaped dot key: err=%v", err)
	}

	// 重复字段同样按统一规则定位；名字渲染为 JSON 字符串。
	_, err = ParseContext([]byte(`{"user.name":1,"user.name":2}`))
	if err == nil || !strings.Contains(err.Error(), `context["user.name"]: duplicate field "user.name"`) {
		t.Fatalf("duplicate dotted key: err=%v", err)
	}
	// 转义形式与直接字符是同名字段：重复检查与定位都按解码后的文字。
	_, err = ParseContext([]byte(`{"x.y":1,"x.y":2}`))
	if err == nil || !strings.Contains(err.Error(), `context["x.y"]: duplicate field "x.y"`) {
		t.Fatalf("duplicate escaped key: err=%v", err)
	}
	_, err = ParseContext([]byte(`{"":1,"":2}`))
	if err == nil || !strings.Contains(err.Error(), `context[""]: duplicate field ""`) {
		t.Fatalf("duplicate empty key: err=%v", err)
	}

	// 配置附加字段名为 meta.info，其中嵌套对象的 owner 重复：
	// 应显示 config.flags[0]["meta.info"].owner。
	rawConfig := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"meta.info":{"owner":"a","owner":"b"}}]}`
	_, err = ParseConfig([]byte(rawConfig))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0]["meta.info"].owner`) ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("nested duplicate under dotted extra field: err=%v", err)
	}
	// 附加字段数组元素里的点号字段名同样加方括号。
	rawConfig = `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"tags":[{"v":1},{"a.b":1,"a.b":2}]}]}`
	_, err = ParseConfig([]byte(rawConfig))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].tags[1]["a.b"]`) ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate in extra array element: err=%v", err)
	}
	// 顶层附加的点号字段名。
	_, err = ParseConfig([]byte(`{"flags":[],"x.y":1,"x.y":2}`))
	if err == nil || !strings.Contains(err.Error(), `config["x.y"]: duplicate field "x.y"`) {
		t.Fatalf("duplicate dotted top-level extra field: err=%v", err)
	}

	// 点号字段名的值含不完整 Unicode 转义：错误位置用方括号包住字段名。
	_, err = ParseContext([]byte(`{"user.name":"\uD800"}`))
	if err == nil || !strings.Contains(err.Error(), `context["user.name"]`) ||
		!strings.Contains(err.Error(), "does not form a complete character") {
		t.Fatalf("bad escape under dotted key: err=%v", err)
	}
}

func TestBadEscapeInFieldNameKeepsRawEscapeAndObjectPath(t *testing.T) {
	// 字段名自身含不完整 Unicode 转义时：仍报告它不能组成完整字符，保留可辨认的
	// 原始转义内容（\uD800 不被替换成别的字符），位置准确指向所属对象。
	_, err := ParseContext([]byte(`{"bad\uD800name":1}`))
	if err == nil {
		t.Fatal("expected invalid Unicode escape error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does not form a complete character") ||
		!strings.Contains(msg, `\uD800`) {
		t.Fatalf("context bad field name: err=%v", msg)
	}
	// 所属对象是根对象；非法名字不能被渲染进定位（不能出现替换字符定位）。
	if !strings.HasPrefix(strings.SplitN(msg, ": ", 2)[0], "context") ||
		strings.Contains(msg, "context[") || strings.Contains(msg, "context.") {
		t.Fatalf("location must point at the root object only: %q", msg)
	}

	// 配置里开关附加字段名含坏转义：所属对象位置为 config.flags[0]。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"met\uD800a":1}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0]`) ||
		!strings.Contains(err.Error(), `\uD800`) ||
		!strings.Contains(err.Error(), "does not form a complete character") {
		t.Fatalf("config bad extra field name: err=%v", err)
	}

	// 嵌套附加对象内的字段名含坏转义：位置指向该嵌套对象 config.flags[0].meta。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"meta":{"o\uD800wner":1}}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].meta`) ||
		!strings.Contains(err.Error(), `\uD800`) {
		t.Fatalf("bad field name in nested object: err=%v", err)
	}

	// 数组元素对象内的字段名含坏转义：位置指向该元素对象。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"tags":[{"k\uDC00":1}]}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].tags[0]`) ||
		!strings.Contains(err.Error(), `\uDC00`) {
		t.Fatalf("bad field name in array element: err=%v", err)
	}
}

func TestContextValidationPrecedence(t *testing.T) {
	// 非法 UTF-8 先于属性类型错误报告。
	if _, err := ParseContext(append([]byte(`{"a":1}`), 0xff)); err == nil ||
		!strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("utf8 precedence: %v", err)
	}
	// 不能组成完整字符的 Unicode 转义不能被较早出现的属性类型错误遮住。
	// 属性类型错误在 zeta，转义错误在其后的 plan 值中：仍报转义错误。
	if _, err := ParseContext([]byte(`{"zeta":false,"plan":"\uD800"}`)); err == nil ||
		!strings.Contains(err.Error(), "does not form a complete character") ||
		!strings.Contains(err.Error(), "context.plan") {
		t.Fatalf("unicode escape precedence: %v", err)
	}
	// 重复字段先于属性类型错误。
	if _, err := ParseContext([]byte(`{"b":1,"b":2}`)); err == nil ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate field precedence: %v", err)
	}
	// JSON 语法错误先于属性类型错误。
	if _, err := ParseContext([]byte(`{"a":1,`)); err == nil ||
		!strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("syntax precedence: %v", err)
	}
}

func TestUnusedContextAttributesValidatedEvenWhenFlagDisabled(t *testing.T) {
	// 开关关闭且规则不引用任何属性：未被规则使用的属性仍必须全部校验。
	cfg := `{"flags":[{"key":"f","enabled":false,"default":true,"rules":[]}]}`
	flag := mustParseConfig(t, cfg).Find("f")
	if _, err := ParseContext([]byte(`{"anything":42}`)); err == nil ||
		!strings.Contains(err.Error(), "context.anything") {
		t.Fatalf("unused attr must still be validated even for a disabled flag: %v", err)
	}
	ctx := mustParseContext(t, `{"anything":"42"}`)
	if got := flag.Evaluate(ctx); got.Reason != EvalDisabled {
		t.Fatalf("valid unused attrs with disabled flag: %+v", got)
	}
}

func TestContextOutOfRangeNumbersAreTypeErrors(t *testing.T) {
	// 1 后接 400 个 0：远超 float64 可表示范围的合法 JSON 整数。它与 1e400、
	// -1e400 一样是语法合法的 JSON 数字——用户把应为字符串的属性写成数字时，
	// 必须报该属性的类型错误，而不是被解码阶段当成 JSON 语法错误。
	hugeInt := "1" + strings.Repeat("0", 400)
	cases := []struct {
		name         string
		raw          string
		wantAttr     string
		topLevelOnly bool // 对象/数组值的错误不得指向内部成员
	}{
		{"exponent overflow", `{"plan":1e400}`, "context.plan", false},
		{"negative exponent overflow", `{"plan":-1e400}`, "context.plan", false},
		{"400-digit integer", `{"plan":` + hugeInt + `}`, "context.plan", false},
		{"object holding big number", `{"plan":{"inner":1e400}}`, "context.plan", true},
		{"array holding big numbers", `{"plan":[1e400,-1e400]}`, "context.plan", true},
		{"nested object then later scalar", `{"a":"x","obj":{"inner":` + hugeInt + `},"b":2}`, "context.obj", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseContext([]byte(tc.raw))
			if err == nil {
				t.Fatalf("ParseContext(%s): expected error, got nil", tc.raw)
			}
			msg := err.Error()
			if strings.Contains(msg, "invalid JSON") {
				t.Fatalf("a legal big number must not be reported as a JSON syntax error: %q", msg)
			}
			if !strings.Contains(msg, tc.wantAttr) || !strings.Contains(msg, "value must be a string") {
				t.Fatalf("err=%q, want %s string-type error", msg, tc.wantAttr)
			}
			if tc.topLevelOnly && strings.Contains(msg, "inner") {
				t.Fatalf("object/array value error must stay on the top-level attribute: %q", msg)
			}
		})
	}

	// 规则不引用该属性：上下文校验独立于配置与求值，同样拒绝并按属性类型错误报告。
	_, err := ParseContext([]byte(`{"unreferenced":1e400}`))
	if err == nil || !strings.Contains(err.Error(), "context.unreferenced") ||
		!strings.Contains(err.Error(), "value must be a string") ||
		strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("unreferenced big-number attr: err=%v", err)
	}
}

func TestContextBigNumberTypeErrorIsFirstInSourceOrder(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	// 前面的布尔值错误不能被后面的大数字盖住；且多次运行报告同一处，
	// 不随 map 遍历顺序在 zeta 与大数字属性之间漂移。
	raw := `{"zeta":false,"plan":"pro","alpha":1e400,"omega":` + hugeInt + `}`
	var first string
	for i := 0; i < 20; i++ {
		_, err := ParseContext([]byte(raw))
		if err == nil {
			t.Fatalf("iter %d: expected error", i)
		}
		if i == 0 {
			first = err.Error()
			if !strings.Contains(first, "context.zeta") ||
				!strings.Contains(first, "value must be a string") {
				t.Fatalf("first error = %q, want context.zeta type error", first)
			}
			continue
		}
		if err.Error() != first {
			t.Fatalf("iter %d: %q != %q", i, err.Error(), first)
		}
	}

	// 调整属性顺序、让大数字排到最前：应报告新的第一处错误。
	_, err := ParseContext([]byte(`{"alpha":1e400,"zeta":false}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("reordered: err=%v want context.alpha", err)
	}
	// 把第一处修正为合法字符串后：报告剩余属性中最先出现的类型错误。
	_, err = ParseContext([]byte(`{"zeta":"z","plan":"pro","alpha":1e400}`))
	if err == nil || !strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("after fixing first attr: err=%v want context.alpha", err)
	}
	// 两处都是大数字时同样取原文第一处。
	_, err = ParseContext([]byte(`{"a":1e400,"b":-1e400}`))
	if err == nil || !strings.Contains(err.Error(), "context.a") ||
		strings.Contains(err.Error(), "context.b") {
		t.Fatalf("two big numbers: err=%v want context.a only", err)
	}
}

func TestContextBigNumberValidationPrecedence(t *testing.T) {
	// 完整输入的检查优先于属性类型检查：大数字属性之后重复声明同名字段时，
	// 必须报重复字段及其所属位置，而不是较早属性的类型错误。
	_, err := ParseContext([]byte(`{"plan":1e400,"plan":"x"}`))
	if err == nil {
		t.Fatal("expected duplicate field error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "duplicate field") || !strings.Contains(msg, "context.plan") {
		t.Fatalf("err=%q, want duplicate field at context.plan", msg)
	}
	if strings.Contains(msg, "value must be a string") {
		t.Fatalf("duplicate field must win over the earlier type error: %q", msg)
	}
	// 大数字属性之后、后面对象内部的重复字段同理，位置指向内部成员。
	_, err = ParseContext([]byte(`{"a":1e400,"b":{"x":1,"x":2}}`))
	if err == nil || !strings.Contains(err.Error(), "context.b.x") ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("nested duplicate must win with its own location: err=%v", err)
	}
	// 数字写法本身无效（1e+ 缺少指数数字）：仍是 JSON 语法错误，
	// 与合法大数字的属性类型错误明确区分。
	_, err = ParseContext([]byte(`{"plan":1e+}`))
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("malformed number must be a JSON syntax error: err=%v", err)
	}
}

func TestQuotedBigNumberStaysString(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	// 同样的数字文本加上引号后就是普通字符串，按原文参与 eq 匹配，
	// 不能被数值化、改写或继续拒绝。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r-exp","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"1e400"}]},
		{"id":"r-neg","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"-1e400"}]},
		{"id":"r-int","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"` + hugeInt + `"}]}]}]}`
	for _, tc := range []struct {
		ctxRaw string
		ruleID string
		reason EvalReason
	}{
		{`{"plan":"1e400"}`, "r-exp", EvalRule},
		{`{"plan":"-1e400"}`, "r-neg", EvalRule},
		{`{"plan":"` + hugeInt + `"}`, "r-int", EvalRule},
		{`{"plan":"1e401"}`, "", EvalDefault}, // 不做数值换算："1e401" != "1e400"
	} {
		got := eval(t, cfg, "f", tc.ctxRaw)
		if got.Reason != tc.reason ||
			(tc.reason == EvalRule && (got.RuleID == nil || *got.RuleID != tc.ruleID)) {
			t.Fatalf("ctx=%s: got %+v, want reason=%s rule=%s", tc.ctxRaw, got, tc.reason, tc.ruleID)
		}
	}

	// 1e400 与 1 后接 400 个 0 在数值上相等，但字符串写法不同：不得互相命中。
	cfgInt := `{"flags":[{"key":"g","enabled":true,"default":false,"rules":[
		{"id":"r-int","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"` + hugeInt + `"}]}]}]}`
	if got := eval(t, cfgInt, "g", `{"plan":"1e400"}`); got.Reason != EvalDefault {
		t.Fatalf("numerically equal spellings must not match as strings: %+v", got)
	}
	if got := eval(t, cfgInt, "g", `{"plan":"`+hugeInt+`"}`); got.Reason != EvalRule {
		t.Fatalf("identical spelling must match verbatim: %+v", got)
	}

	// ParseContext 保留引号内的原文，不做任何转换。
	ctx := mustParseContext(t, `{"plan":"1e400"}`)
	if v, ok := ctx.Get("plan"); !ok || v != "1e400" {
		t.Fatalf("quoted big number was rewritten: %q %v", v, ok)
	}
}

func TestBigNumberRejectedEvenForDisabledFlag(t *testing.T) {
	// 请求的是关闭的开关：文件仍须先完整校验，非法上下文不能借 disabled 路径通过。
	cfg := `{"flags":[{"key":"off","enabled":false,"default":true,"rules":[]}]}`
	flag := mustParseConfig(t, cfg).Find("off")
	if _, err := ParseContext([]byte(`{"plan":1e400}`)); err == nil ||
		!strings.Contains(err.Error(), "context.plan") ||
		strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("disabled flag must not admit big-number input: err=%v", err)
	}
	// 修正为合法字符串后，关闭开关仍固定 false（reason=disabled）。
	ctx := mustParseContext(t, `{"plan":"1e400"}`)
	if got := flag.Evaluate(ctx); got.Reason != EvalDisabled || got.Value {
		t.Fatalf("disabled flag with valid quoted string: %+v", got)
	}
}

func TestInvalidUTF8(t *testing.T) {
	bad := []byte("{\"flags\":[]}\xff")
	if _, err := ParseConfig(bad); err == nil {
		t.Error("expected invalid UTF-8 config error")
	}
	if _, err := ParseContext(append([]byte("{\"a\":\"x\"}"), 0xfe)); err == nil {
		t.Error("expected invalid UTF-8 context error")
	}
}

func TestDuplicateFieldsRejected(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 错误信息需包含的定位片段
	}{
		{"flag enabled", `{"flags":[{"key":"f","enabled":false,"enabled":true,"default":false,"rules":[]}]}`,
			`config.flags[0].enabled`},
		{"second flag", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[]},
			{"key":"g","enabled":true,"enabled":false,"default":false,"rules":[]}]}`,
			`config.flags[1].enabled`},
		{"top level", `{"flags":[],"flags":[]}`, `config.flags`},
		{"rule value", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"value":false,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`,
			`config.flags[0].rules[0].value`},
		{"condition op", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","op":"in","value":"x"}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].op`},
		{"nested extra field", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"owner":"a","owner":"b"}}]}`,
			`config.flags[0].meta.owner`},
		{"deeply nested extra field", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"team":{"name":"x","name":"y"}}}]}`,
			`config.flags[0].meta.team.name`},
		{"extra field nested in array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"tags":[{"v":1,"v":2}]}]}`,
			`config.flags[0].tags[0].v`},
		{"identical values still duplicate", `{"flags":[{"key":"f","enabled":true,"default":false,"default":false,"rules":[]}]}`,
			`config.flags[0].default`},
		{"duplicate in extra top-level field", `{"flags":[],"x":1,"x":2}`, `config.x`},
		{"disabled flag not exempt", `{"flags":[{"key":"f","enabled":false,"enabled":false,"default":false,"rules":[]}]}`,
			`config.flags[0].enabled`},
		{"unselected flag not exempt", `{"flags":[
			{"key":"good","enabled":true,"default":false,"rules":[]},
			{"key":"bad","enabled":true,"default":false,"default":true,"rules":[]}]}`,
			`config.flags[1].default`},
		{"first duplicate in file order wins", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[]},
			{"key":"g","enabled":true,"enabled":true,"default":false,"default":false,"rules":[]}]}`,
			`config.flags[1].enabled`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected duplicate field error, got nil\nconfig: %s", tc.raw)
			}
			if !strings.Contains(err.Error(), "duplicate field") {
				t.Fatalf("error = %q, want %q wording", err.Error(), "duplicate field")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want location %q", err.Error(), tc.want)
			}
		})
	}
}

func TestDuplicateFieldEscapedName(t *testing.T) {
	// 字段名按解码后的字符串比较："enabled" 与 "énabled" 是同名。
	raw := `{"flags":[{"key":"f","enabled":false,"\u0065nabled":true,"default":false,"rules":[]}]}`
	_, err := ParseConfig([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0].enabled`) {
		t.Fatalf("escaped duplicate: err=%v", err)
	}
	if !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("escaped duplicate wording: err=%v", err)
	}
}

func TestDuplicateFieldsInContext(t *testing.T) {
	_, err := ParseContext([]byte(`{"plan":"free","plan":"pro"}`))
	if err == nil || !strings.Contains(err.Error(), `context.plan`) ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("context duplicate: err=%v", err)
	}
	// 转义后同名也算重复。
	_, err = ParseContext([]byte(`{"plan":"free","\u0070lan":"pro"}`))
	if err == nil || !strings.Contains(err.Error(), `context.plan`) {
		t.Fatalf("context escaped duplicate: err=%v", err)
	}
}

func TestDuplicateFieldLookalikesAllowed(t *testing.T) {
	// 大小写不同的字段名是不同字段。
	cfg := mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"Enabled":false,"default":false,"rules":[]}]}`)
	if !cfg.Flags[0].Enabled {
		t.Fatal("lowercase enabled should win as the real field")
	}
	// 名字中的空白不裁剪，"enabled " 与 "enabled" 不同名。
	mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"enabled ":false,"default":false,"rules":[]}]}`)
	// 同名字段分别出现在不同开关、不同规则里是合法的。
	mustParseConfig(t, `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]},
		{"key":"g","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`)
	// 字符串数组中的重复元素不属于重复字段。
	mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":["x","x"]}]}]}]}`)
	// 上下文中不同名的字段互不影响。
	mustParseContext(t, `{"plan":"pro","Plan":"free"}`)
}

func TestUniquenessErrorsDistinctFromDuplicateFields(t *testing.T) {
	// 两个开关使用相同 key 仍按唯一性规则报告，措辞与重复字段不同。
	_, err := ParseConfig([]byte(`{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[]},
		{"key":"f","enabled":true,"default":false,"rules":[]}]}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate flag key") {
		t.Fatalf("flag key uniqueness: err=%v", err)
	}
	if strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("uniqueness error must not read as duplicate field: %v", err)
	}
	// 同一开关内两条规则使用相同 id 同理。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},
		{"id":"r","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate rule id") {
		t.Fatalf("rule id uniqueness: err=%v", err)
	}
	if strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("uniqueness error must not read as duplicate field: %v", err)
	}
}

func TestInvalidUnicodeEscapesRejected(t *testing.T) {
	// encoding/json 会把孤立代理项转义静默替换为 U+FFFD；这些输入必须明确失败，
	// 且错误指出文件（config/context）、字段或数组位置，并说明转义不能组成完整字符。
	const incomplete = "does not form a complete character"
	configCases := []struct {
		name string
		raw  string
		want string
	}{
		{"eq value lone high surrogate", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].value`},
		{"eq value lone low surrogate", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uDC00"}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].value`},
		{"reversed pair", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uDC00\uD800"}]}]}]}`,
			`conditions[0].value`},
		{"high then non-surrogate escape", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800A"}]}]}]}`,
			`conditions[0].value`},
		{"high then high", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800\uD800"}]}]}]}`,
			`conditions[0].value`}, {"high at end of string", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x\uD800"}]}]}]}`,
			`conditions[0].value`},
		{"in-list element", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":["ok","\uD800"]}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].value[1]`},
		{"flag key string", `{"flags":[{"key":"\uD800","enabled":true,"default":false,"rules":[]}]}`,
			`config.flags[0].key`},
		{"extra field value", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":{"note":"\uDFFF"}}]}`,
			`config.flags[0].meta.note`},
		{"disabled flag not exempt", `{"flags":[{"key":"f","enabled":false,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`conditions[0].value`},
		{"unselected flag not exempt", `{"flags":[
			{"key":"good","enabled":true,"default":false,"rules":[]},
			{"key":"bad","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`config.flags[1].rules[0].conditions[0].value`},
		{"first error in file order wins", `{"flags":[
			{"key":"f","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]},
			{"key":"g","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uDC00"}]}]}]}`,
			`config.flags[0].rules[0].conditions[0].value`},
	}
	for _, tc := range configCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected invalid Unicode escape error, got nil\nconfig: %s", tc.raw)
			}
			if !strings.Contains(err.Error(), incomplete) {
				t.Fatalf("error = %q, want wording %q", err.Error(), incomplete)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want location %q", err.Error(), tc.want)
			}
		})
	}

	// 字段名本身非法：指出所属对象，并保留可辨认的原始转义内容。
	_, err := ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"met\uD800a":1}]}`))
	if err == nil || !strings.Contains(err.Error(), `config.flags[0]`) ||
		!strings.Contains(err.Error(), `\uD800`) || !strings.Contains(err.Error(), incomplete) {
		t.Fatalf("bad field name in config: err=%v", err)
	}

	// 上下文同样检查值与字段名。
	_, err = ParseContext([]byte(`{"plan":"\uD800"}`))
	if err == nil || !strings.Contains(err.Error(), `context.plan`) ||
		!strings.Contains(err.Error(), incomplete) {
		t.Fatalf("context value: err=%v", err)
	}
	_, err = ParseContext([]byte(`{"pl\uDC00an":"pro"}`))
	if err == nil || !strings.Contains(err.Error(), `context`) ||
		!strings.Contains(err.Error(), `\uDC00`) || !strings.Contains(err.Error(), incomplete) {
		t.Fatalf("context field name: err=%v", err)
	}
}

func TestInvalidUnicodeEscapeNotEquatedWithReplacementChar(t *testing.T) {
	// 规则要求属性值等于真正的 "�"：上下文提供 "\uD800" 不能在替换后命中规则，
	// 整个求值必须失败。
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"�"}]}]}]}`
	if _, err := ParseContext([]byte(`{"plan":"\uD800"}`)); err == nil {
		t.Fatal("context with lone surrogate must be rejected, not replaced by U+FFFD")
	}
	c := mustParseConfig(t, cfg)
	ctx := mustParseContext(t, `{"plan":"�"}`)
	if got := c.Find("f").Evaluate(ctx); got.Reason != EvalRule {
		t.Fatalf("literal U+FFFD still matches: %+v", got)
	}
	// 字段名被替换后也不能误报重复字段："\uD800x" 与 "�x" 本不应同名，
	// 但非法转义必须先于重复字段检查以自身名义报错。
	_, err := ParseContext([]byte(`{"\uD800x":"a","�x":"b"}`))
	if err == nil || strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("lone surrogate in field name must not surface as duplicate field: %v", err)
	}
}

func TestValidUnicodeEscapesUnchanged(t *testing.T) {
	// 正确配对的代理项转义与直接写出的同一字符按同一字符串比较。
	// emojiEsc 是 "😀" 的代理项对转义形式（分两段书写仅为避免源码层面的转义歧义）。
	emojiEsc := `\uD83D` + `\uDE00`
	cfgEsc := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"emoji","op":"eq","value":"` + emojiEsc + `"}]}]}]}`
	if got := eval(t, cfgEsc, "f", `{"emoji":"😀"}`); got.Reason != EvalRule {
		t.Fatalf("paired escape equals literal: %+v", got)
	}
	cfgLit := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"emoji","op":"eq","value":"😀"}]}]}]}`
	if got := eval(t, cfgLit, "f", `{"emoji":"`+emojiEsc+`"}`); got.Reason != EvalRule {
		t.Fatalf("literal equals paired escape: %+v", got)
	}
	// 真正的 "�" 与 "�" 仍是合法字符串，不能一概禁止替换字符。
	mustParseContext(t, `{"a":"�","b":"\u`+`FFFD"}`)
	if got := eval(t, cfgLit, "f", `{"emoji":"�"}`); got.Reason != EvalDefault {
		t.Fatalf("U+FFFD is just another string: %+v", got)
	}
	// 反斜杠转义后的 "\uD800" 是普通文本（内容为 \uD800 六个字符）。
	mustParseContext(t, `{"a":"\\uD800"}`)
	cfgText := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r1","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\\uD800"}]}]}]}`
	if got := eval(t, cfgText, "f", `{"a":"\\uD800"}`); got.Reason != EvalRule {
		t.Fatalf("escaped backslash text: %+v", got)
	}
}

func TestEmptyFlagListAndFind(t *testing.T) {
	cfg := mustParseConfig(t, `{"flags":[]}`)
	if cfg.Find("missing") != nil {
		t.Error("expected nil flag")
	}
	var nilCfg *Config
	if nilCfg.Find("x") != nil {
		t.Error("nil config Find should be nil")
	}
	flag := (&Flag{Key: "f", Enabled: false})
	if got := flag.Evaluate(nil); got.Reason != EvalDisabled {
		t.Errorf("nil context on disabled flag: %+v", got)
	}
}
