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
		{`{bad`, "invalid JSON"},
	} {
		if _, err := ParseContext([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseContext(%s): err=%v want substring %q", tc.raw, err, tc.want)
		}
	}
}

func TestContextNonStringErrorFollowsSourceOrder(t *testing.T) {
	// 多个非字符串属性值时，错误必须稳定指向原文中最先出现的一个，
	// 不受 map 遍历顺序或前面合法属性的影响。
	assertFirst := func(raw, want string) {
		t.Helper()
		for i := 0; i < 20; i++ {
			_, err := ParseContext([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), want) ||
				!strings.Contains(err.Error(), "value must be a string") {
				t.Fatalf("ParseContext(%s) run %d: err=%v want %q", raw, i, err, want)
			}
		}
	}
	assertFirst(`{"zeta":false,"plan":"pro","alpha":7}`, "context.zeta")
	assertFirst(`{"alpha":7,"zeta":false,"plan":"pro"}`, "context.alpha")
	// 合法字符串属性不改变选择。
	assertFirst(`{"a":"1","b":"2","mid":null,"zeta":false}`, "context.mid")
	// 对象与数组整体计为一个非字符串值，错误指向顶层属性而非内部成员。
	assertFirst(`{"ok":"1","nested":{"inner":1},"zeta":false}`, "context.nested")
	assertFirst(`{"list":[1,2],"zeta":false}`, "context.list")
	// 数字、布尔、null 各类型。
	for _, tc := range []struct{ raw, want string }{
		{`{"a":1}`, "context.a"},
		{`{"a":true}`, "context.a"},
		{`{"a":null}`, "context.a"},
	} {
		assertFirst(tc.raw, tc.want)
	}
	// 解码后的 Unicode 转义字段名与直接字符同样定位；大小写空白原样保留。
	assertFirst(`{"zeta":1}`, "context.zeta")
	assertFirst(`{"\u007aeta":1}`, "context.zeta")
	assertFirst(`{" Plan ":1}`, "context. Plan ")
	// 修正第一处后，下一次求值报告剩余属性中最先出现的类型错误。
	if _, err := ParseContext([]byte(`{"zeta":"ok","plan":"pro","alpha":7}`)); err == nil ||
		!strings.Contains(err.Error(), "context.alpha") {
		t.Fatalf("next error after fixing first: err=%v", err)
	}
	// 全部合法后才成功；空字符串与形似数字/布尔的字符串是合法值。
	mustParseContext(t, `{"zeta":"ok","plan":"pro","alpha":"7","empty":"","boolish":"false"}`)
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
