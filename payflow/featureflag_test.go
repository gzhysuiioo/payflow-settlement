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

func TestLoneSurrogateEscapesRejected(t *testing.T) {
	cfgWrap := func(value string) string {
		return `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":` + value + `}]}]}]}`
	}
	cases := []struct {
		name string
		raw  string
		want string // 错误信息需包含的定位片段
	}{
		{"eq value lone high surrogate", cfgWrap(`"\uD800"`), `conditions[0].value`},
		{"eq value lone low surrogate", cfgWrap(`"\uDC00"`), `conditions[0].value`},
		{"eq value reversed pair", cfgWrap(`"\uDC00\uD800"`), `conditions[0].value`},
		{"eq value high then non-surrogate escape", cfgWrap(`"\uD800A"`), `conditions[0].value`},
		{"eq value high at end of string", cfgWrap(`"a\uD800"`), `conditions[0].value`},
		{"eq value lowercase hex", cfgWrap(`"\ud800"`), `conditions[0].value`},
		{"in-list element", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":["ok","\uD800"]}]}]}]}`,
			`conditions[0].value[1]`},
		{"flag key", `{"flags":[{"key":"\uD800","enabled":true,"default":false,"rules":[]}]}`, `flags[0].key`},
		{"rule id", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"\uDC00","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `rules[0].id`},
		{"extra field value", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"meta":"\uD800"}]}`, `flags[0].meta`},
		{"extra field nested in array", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
			"tags":["ok","\uD800"]}]}`, `flags[0].tags[1]`},
		{"disabled flag not exempt", `{"flags":[{"key":"f","enabled":false,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`conditions[0].value`},
		{"unselected flag not exempt", `{"flags":[
			{"key":"good","enabled":true,"default":false,"rules":[]},
			{"key":"bad","enabled":true,"default":false,"rules":[
				{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]}]}`,
			`flags[1]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected Unicode escape error, got nil\nconfig: %s", tc.raw)
			}
			if !strings.Contains(err.Error(), "complete character") {
				t.Fatalf("error = %q, want wording about complete character", err.Error())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want location %q", err.Error(), tc.want)
			}
			if !strings.HasPrefix(err.Error(), "config") {
				t.Fatalf("error = %q, want config prefix", err.Error())
			}
		})
	}
}

func TestLoneSurrogateInContextRejected(t *testing.T) {
	// 属性值中的孤立代理项。
	_, err := ParseContext([]byte(`{"plan":"\uD800"}`))
	if err == nil || !strings.Contains(err.Error(), "context.plan") ||
		!strings.Contains(err.Error(), "complete character") {
		t.Fatalf("context value: err=%v", err)
	}
	// 字段名中的孤立代理项：指出所属对象并保留原始转义。
	_, err = ParseContext([]byte(`{"plan\uD800x":"pro"}`))
	if err == nil || !strings.Contains(err.Error(), "context") ||
		!strings.Contains(err.Error(), `\uD800`) || !strings.Contains(err.Error(), "field name") {
		t.Fatalf("context key: err=%v", err)
	}
	// 配置里附加字段的坏字段名同样报所属对象与原始转义。
	_, err = ParseConfig([]byte(`{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"meta\uD800":1}]}`))
	if err == nil || !strings.Contains(err.Error(), "config.flags[0]") ||
		!strings.Contains(err.Error(), `\uD800`) || !strings.Contains(err.Error(), "field name") {
		t.Fatalf("config key: err=%v", err)
	}
}

func TestLoneSurrogateNotReportedAsDuplicateField(t *testing.T) {
	// 两个不同的坏字段名解码后都会变成 "�"，不得误报重复字段。
	_, err := ParseContext([]byte(`{"\uD800":"a","\uDC00":"b"}`))
	if err == nil {
		t.Fatal("expected Unicode escape error")
	}
	if strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("must not report duplicate field: %v", err)
	}
	if !strings.Contains(err.Error(), "complete character") {
		t.Fatalf("want Unicode escape error: %v", err)
	}
}

func TestFirstSurrogateErrorInFileOrderWins(t *testing.T) {
	raw := `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD800"}]}]},
		{"key":"g","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uDC00"}]}]}]}`
	_, err := ParseConfig([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), `flags[0]`) || strings.Contains(err.Error(), `flags[1]`) {
		t.Fatalf("first error in file order: err=%v", err)
	}
}

func TestValidUnicodeEscapesStillAccepted(t *testing.T) {
	// 正确配对的转义与直接写出的同一字符按同一字符串比较。
	cfgEscaped := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\uD83D\uDE00"}]}]}]}`
	if got := eval(t, cfgEscaped, "f", `{"a":"😀"}`); got.Reason != EvalRule {
		t.Fatalf("escaped pair vs literal: %+v", got)
	}
	cfgLiteral := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"😀"}]}]}]}`
	if got := eval(t, cfgLiteral, "f", `{"a":"\uD83D\uDE00"}`); got.Reason != EvalRule {
		t.Fatalf("literal vs escaped pair: %+v", got)
	}
	// 真正的 "�" 与 "\uFFFD" 都是合法字符串，且按同一字符比较。
	cfgFFFD := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"�"}]}]}]}`
	if got := eval(t, cfgFFFD, "f", `{"a":"\uFFFD"}`); got.Reason != EvalRule {
		t.Fatalf("literal vs escaped FFFD: %+v", got)
	}
	// 反斜杠转义后的 uD800 只是普通文本。
	cfgText := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"\\uD800"}]}]}]}`
	if got := eval(t, cfgText, "f", `{"a":"\\uD800"}`); got.Reason != EvalRule {
		t.Fatalf("escaped backslash text: %+v", got)
	}
	// 字段名里的合法转义不受影响。
	mustParseConfig(t, `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],
		"n\u00e4me":"x"}]}`)
	mustParseContext(t, `{"\uD83D\uDE00":"emoji","\uFFFD":"replacement"}`)
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
		{`{bad`, "invalid JSON"},
	} {
		if _, err := ParseContext([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseContext(%s): err=%v want substring %q", tc.raw, err, tc.want)
		}
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
