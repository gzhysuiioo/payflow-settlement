package payflow

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件为解释模式（EvaluateExplain / --explain）补充附加信息（负责人、备注、
// 标签等治理字段）的回归保障。既有约定是：附加字段可以出现在配置顶层以及开关、
// 规则、条件对象中，值可以是任意合法 JSON（含嵌套对象/数组与 1e400 这样超出
// 浮点范围的合法数字），但它们：
//   - 不参与求值：开关定义、规则顺序、比较值与上下文不变时，带不带附加字段
//     的解释结果与完整 explanation 必须逐字节一致；
//   - 不是上下文属性：附加对象里即使写了规则所需属性及一个能命中的值，上下文
//     缺该属性时解释仍须给出 actualValue=null、missing=true、条件不成立；
//   - 不出现在输出里：普通模式仍是四个既有字段，解释中也不携带任何附加内容；
//   - 不能豁免整份配置的格式校验：未被选中或已关闭开关的附加对象里，合法大数
//     字之后重复声明同名字段仍须拒绝整份配置（进程级契约在 cmd 层测试）。
//
// explainMetaBaseCfg 的业务形态固定为两个开关：
//   - f（启用，default=true）：r-deny 在前、返回 false，条件为 region eq "cn"
//     与 plan eq "pro"（AND）；r-allow 在后、返回 true，条件为 tier in
//     ["","a","a"]。两条规则可同时成立，用于区分“靠前规则命中 false 立即
//     定案、解释只到该规则”与“靠前规则不成立、后续规则才命中、解释保留此前
//     未命中规则及其全部条件”两种路径；
//   - off（关闭，default=false）：含一条本会命中的 r-never，用于保护关闭开关
//     不评估规则且附加字段不改变这一行为。
const explainMetaBaseCfg = `{"flags":[
	{"key":"f","enabled":true,"default":true,"rules":[
		{"id":"r-deny","value":false,"conditions":[
			{"attribute":"region","op":"eq","value":"cn"},
			{"attribute":"plan","op":"eq","value":"pro"}]},
		{"id":"r-allow","value":true,"conditions":[
			{"attribute":"tier","op":"in","value":["","a","a"]}]}]},
	{"key":"off","enabled":false,"default":false,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`

// explainMetaRichCfg 在四个位置（顶层、开关、规则、条件）都携带附加字段：
// 字符串、布尔、null、嵌套对象、嵌套数组与 1e400 及 400 位整数。业务字段与
// explainMetaBaseCfg 逐字相同。
func explainMetaRichCfg() string {
	hugeInt := "1" + strings.Repeat("0", 400)
	return `{"owner":"payments-platform","schemaVersion":2,"tags":["root",1e400],"flags":[
	{"key":"f","enabled":true,"default":true,
		"owner":"@alice","labels":["checkout","gradual-rollout"],"rolloutWeight":1e400,"archived":null,"needsReview":true,
		"rules":[
		{"id":"r-deny","value":false,"note":"命中即拒绝","review":{"required":true,"approvers":["@alice","@bob"],"budget":1e400},"conditions":[
			{"attribute":"region","op":"eq","value":"cn","rationale":null,"who":{"team":"growth","nodes":[1,2,{"deep":true}]}},
			{"attribute":"plan","op":"eq","value":"pro","hint":["p",1e400,{"deep":[true,null,{}]}]}]},
		{"id":"r-allow","value":true,"note":"允许放行","conditions":[
			{"attribute":"tier","op":"in","value":["","a","a"],"weight":` + hugeInt + `}]}]},
	{"key":"off","enabled":false,"default":false,"meta":{"big":1e400,"nested":{"a":[1,2,{"b":null}]}},"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
}

// explainMetaAltCfg 相对 rich 版修改、删除并新增了一批附加字段（负责人交接、
// 删去备注、顶层新增 footer、权重改为 -1e400 等），业务字段仍完全不变。
const explainMetaAltCfg = `{"owner":"growth-platform","footer":[1,2,3],"flags":[
	{"key":"f","enabled":true,"default":true,"owner":"@carol","rolloutWeight":-1e400,"rules":[
		{"id":"r-deny","value":false,"review":{"required":false},"conditions":[
			{"attribute":"region","op":"eq","value":"cn"},
			{"attribute":"plan","op":"eq","value":"pro","weight":2}]},
		{"id":"r-allow","value":true,"since":"2026","conditions":[
			{"attribute":"tier","op":"in","value":["","a","a"]}]}]},
	{"key":"off","enabled":false,"default":false,"rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`

// explainMetaShadowCfg 的附加字段刻意与上下文属性同名：顶层、开关、规则、条件
// 各自的附加对象（含嵌套对象与数组内对象）里都写了 region/plan/tier，且值都
// 是能直接命中规则的 "cn"/"pro"/"a"。求值只能读上下文文件，这些名字一个都
// 不许被当成上下文属性补齐缺失值。业务字段仍与基准配置相同。
const explainMetaShadowCfg = `{"region":"cn","plan":"pro","tier":"a","owner":{"region":"cn"},"flags":[
	{"key":"f","enabled":true,"default":true,
		"region":"cn","plan":"pro","tier":"a",
		"review":{"region":"cn","plan":"pro","tier":"a","approvers":[{"region":"cn"}]},
		"rules":[
		{"id":"r-deny","value":false,"region":"cn","plan":"pro","conditions":[
			{"attribute":"region","op":"eq","value":"cn","region":"cn","meta":{"region":"cn"}},
			{"attribute":"plan","op":"eq","value":"pro","plan":"pro","hits":["region",{"plan":"pro"}]}]},
		{"id":"r-allow","value":true,"tier":"a","conditions":[
			{"attribute":"tier","op":"in","value":["","a","a"],"tier":"a"}]}]},
	{"key":"off","enabled":false,"default":false,"region":"cn","plan":"pro","rules":[
		{"id":"r-never","value":true,"conditions":[
			{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`

// explainMetaVariants 汇总三份与基准业务相同、仅附加信息不同的配置：丰富版、
// 增删改版，以及与上下文属性同名的影子版。
func explainMetaVariants() []namedConfig {
	return []namedConfig{
		{"extra fields added", explainMetaRichCfg()},
		{"extra fields modified and removed", explainMetaAltCfg},
		{"extra fields shadowing context attributes", explainMetaShadowCfg},
	}
}

// explainMetaCase 描述一条解释路径的既定期望：在某开关键与上下文上，最终结果
// （值、原因、获胜规则编号，nil 表示 default/disabled）以及解释中按顺序应出现
// 的规则 id 列表。
type explainMetaCase struct {
	name    string
	key     string
	ctx     string
	value   bool
	reason  EvalReason
	ruleID  *string
	ruleIDs []string
}

// 解释模式回归所用的上下文与每条路径的既定期望。
var explainMetaCases = []explainMetaCase{
	// 上下文同时满足两条规则：靠前的 r-deny（false）立即定案，解释只到它，
	// r-allow 不得出现。
	{"first rule hits and returns false", "f", `{"region":"cn","plan":"pro","tier":"a"}`,
		false, EvalRule, sp("r-deny"), []string{"r-deny"}},
	// 靠前规则两个条件都不成立：解释保留它及其全部条件，继续到 r-allow 命中。
	{"first rule misses, later rule hits", "f", `{"region":"us","plan":"free","tier":"a"}`,
		true, EvalRule, sp("r-allow"), []string{"r-deny", "r-allow"}},
	// 靠前规则仅因 region 缺失而不成立：缺失条件与成立条件都要在案。
	{"missing region falls through", "f", `{"plan":"pro","tier":"a"}`,
		true, EvalRule, sp("r-allow"), []string{"r-deny", "r-allow"}},
	// 所有属性缺失：两条规则都不命中，采用 default，全部规则进入解释。
	{"all attributes missing uses default", "f", `{}`,
		true, EvalDefault, nil, []string{"r-deny", "r-allow"}},
	// 显式空字符串：r-deny 比较失败（值在但不等），tier:"" 是 in 列表成员，
	// r-allow 命中。
	{"explicit empty strings stay present", "f", `{"region":"","plan":"","tier":""}`,
		true, EvalRule, sp("r-allow"), []string{"r-deny", "r-allow"}},
	// 无规则命中：采用 default。
	{"no rule matches uses default", "f", `{"region":"us","plan":"x","tier":"z"}`,
		true, EvalDefault, nil, []string{"r-deny", "r-allow"}},
	// 关闭开关固定 false：不评估规则，解释规则列表为空。
	{"disabled flag fixed false", "off", `{"region":"cn","plan":"pro","tier":"a"}`,
		false, EvalDisabled, nil, []string{}},
}

// sp 返回字符串指针，便于以字面量构造期望的 ruleId。
func sp(s string) *string { return &s }

// TestExplainExtraFieldsDoNotChangeExplanation 是核心回归：开关定义、规则顺序、
// 比较值与上下文不变，只在四个位置增加、修改、删除合法附加字段时，解释模式的
// 四字段结果与完整 explanation（含 outcome、规则列表、每条件的比较值、实际值、
// 缺失标记与判断）必须与无附加字段时逐字节一致；获胜规则、被考虑规则集合同样
// 不变；普通模式与解释模式的四字段结果一致。
func TestExplainExtraFieldsDoNotChangeExplanation(t *testing.T) {
	for _, tc := range explainMetaCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := mustParseContext(t, tc.ctx)
			baseFlag := mustParseConfig(t, explainMetaBaseCfg).Find(tc.key)
			if baseFlag == nil {
				t.Fatalf("base flag %q not found", tc.key)
			}
			baseExplained := baseFlag.EvaluateExplain(ctx)
			reference := mustMarshalExplain(t, baseExplained)

			assertExplainExpectation(t, "no extra fields", baseExplained, tc)

			for _, v := range explainMetaVariants() {
				t.Run(v.name, func(t *testing.T) {
					flag := mustParseConfig(t, v.raw).Find(tc.key)
					if flag == nil {
						t.Fatalf("flag %q not found in variant %q", tc.key, v.name)
					}
					got := flag.EvaluateExplain(ctx)
					assertExplainExpectation(t, v.name, got, tc)

					// 完整解释（不只是四字段）必须与基准逐字节一致：附加字段
					// 不能改获胜规则、不能增减被考虑规则、不能改任何条件记录。
					if b := mustMarshalExplain(t, got); !bytes.Equal(b, reference) {
						t.Fatalf("%s: explanation drifted from the no-extra baseline:\ngot:  %s\nwant: %s",
							v.name, b, reference)
					}
					// 普通模式与解释模式共用同一定案路径，四字段必须一致。
					if plain := flag.Evaluate(ctx); !plain.Equals(got.EvalResult) {
						t.Fatalf("%s: plain %+v != explained %+v", v.name, plain, got.EvalResult)
					}
				})
			}
		})
	}
}

// assertExplainExpectation 逐条核对一条解释路径的既定期望：四字段结果与按顺序
// 出现的规则编号（定案后不得多出后续规则）。
func assertExplainExpectation(t *testing.T, label string, got ExplainedResult, tc explainMetaCase) {
	t.Helper()
	want := EvalResult{Key: tc.key, Value: tc.value, Reason: tc.reason, RuleID: tc.ruleID}
	if !got.EvalResult.Equals(want) {
		t.Fatalf("%s: result = %+v, want %+v", label, got.EvalResult, want)
	}
	if ids := explainedIDs(got); !equalStrings(ids, tc.ruleIDs) {
		t.Fatalf("%s: considered rules = %v, want %v", label, ids, tc.ruleIDs)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestExplainExtraFieldsNeverAppearInOutput 保护输出形态：附加信息既不会进入
// 普通模式的四字段对象，也不会进入解释的任何一层（解释对象、规则、条件各保持
// 固定字段数），负责人/备注等文字与大数字都不允许被渲染出来。
func TestExplainExtraFieldsNeverAppearInOutput(t *testing.T) {
	ctx := mustParseContext(t, `{"region":"us","plan":"free","tier":"a"}`)
	for _, v := range []namedConfig{
		{"rich", explainMetaRichCfg()},
		{"alt", explainMetaAltCfg},
		{"shadow", explainMetaShadowCfg},
	} {
		t.Run(v.name, func(t *testing.T) {
			flag := mustParseConfig(t, v.raw).Find("f")

			// 普通模式输出只有四个既有字段。
			plain := mustMarshalResult(t, flag.Evaluate(ctx))
			var pm map[string]any
			if err := json.Unmarshal(plain, &pm); err != nil {
				t.Fatalf("plain output is not JSON: %v (%s)", err, plain)
			}
			if len(pm) != 4 {
				t.Fatalf("plain output must keep exactly 4 fields, got %d: %s", len(pm), plain)
			}
			for _, leak := range explainMetaLeakMarkers() {
				if bytes.Contains(plain, []byte(leak)) {
					t.Fatalf("plain output leaked extra content %q: %s", leak, plain)
				}
			}

			// 解释模式：顶层 5 个字段；explanation 2 个；规则 3 个；条件 6 个，
			// 任何附加键都无处可藏。
			out := mustMarshalExplain(t, flag.EvaluateExplain(ctx))
			var top map[string]any
			if err := json.Unmarshal(out, &top); err != nil {
				t.Fatalf("explain output is not JSON: %v (%s)", err, out)
			}
			if len(top) != 5 {
				t.Fatalf("explain top level must keep exactly 5 fields, got %d: %s", len(top), out)
			}
			exp := top["explanation"].(map[string]any)
			if len(exp) != 2 {
				t.Fatalf("explanation object must keep exactly 2 fields: %s", out)
			}
			rules := exp["rules"].([]any)
			for _, ri := range rules {
				r := ri.(map[string]any)
				if len(r) != 3 {
					t.Fatalf("rule record must keep exactly 3 fields: %s", out)
				}
				for _, ci := range r["conditions"].([]any) {
					if len(ci.(map[string]any)) != 6 {
						t.Fatalf("condition record must keep exactly 6 fields: %s", out)
					}
				}
			}
			for _, leak := range explainMetaLeakMarkers() {
				if bytes.Contains(out, []byte(leak)) {
					t.Fatalf("explain output leaked extra content %q: %s", leak, out)
				}
			}
		})
	}
}

// explainMetaLeakMarkers 是只可能来自附加信息、业务字段与解释固定文案中都不
// 会出现的文字/数字片段；大数字若被渲染会带来 "e400"（1e400 原样）或成串的
// 400 个 0。
func explainMetaLeakMarkers() []string {
	return []string{
		"@alice", "@carol", "payments-platform", "growth-platform",
		"rolloutWeight", "approvers", "rationale", "needsReview",
		"命中即拒绝", "允许放行", "e400", "E400", strings.Repeat("0", 50),
	}
}

// mustMarshalResult 渲染普通模式结果，失败即终止测试。
func mustMarshalResult(t *testing.T, r EvalResult) []byte {
	t.Helper()
	b, err := MarshalResult(r)
	if err != nil {
		t.Fatalf("MarshalResult failed: %v", err)
	}
	return b
}

// TestExplainShadowAttributesNeverFillMissingValues 专门覆盖附加信息与上下文
// 属性同名的边界：无论附加字段（含嵌套对象/数组中的同名字段）写了多少个能
// 命中的值，上下文缺失的属性在解释里必须始终是 actualValue=null、missing=true、
// 条件不成立，求值继续按既有规则处理；上下文显式给出空字符串时则保留空串、
// missing=false，绝不与缺失混淆。影子配置与无附加基准配置在每个上下文上的
// 完整解释必须逐字节一致。
// explainCondExpectation 描述解释中某条规则（rule 下标）的某个条件（cond
// 下标）应有的记录：属性名、上下文实际值（nil 表示缺失）、缺失标记与判断。
type explainCondExpectation struct {
	rule, cond int
	attribute  string
	actual     *string
	missing    bool
	match      bool
}

// explainShadowCase 描述影子附加信息场景下，一个上下文上的完整既定期望。
type explainShadowCase struct {
	name   string
	ctx    string
	conds  []explainCondExpectation
	winner *string // nil 表示落到 default
	ids    []string
}

func TestExplainShadowAttributesNeverFillMissingValues(t *testing.T) {
	cases := []explainShadowCase{
		{
			name: "all attributes missing despite shadow values",
			ctx:  `{}`,
			conds: []explainCondExpectation{
				{0, 0, "region", nil, true, false},
				{0, 1, "plan", nil, true, false},
				{1, 0, "tier", nil, true, false},
			},
			winner: nil,
			ids:    []string{"r-deny", "r-allow"},
		},
		{
			name: "only region missing",
			ctx:  `{"plan":"pro","tier":"a"}`,
			conds: []explainCondExpectation{
				{0, 0, "region", nil, true, false},
				{0, 1, "plan", sp("pro"), false, true},
				{1, 0, "tier", sp("a"), false, true},
			},
			winner: sp("r-allow"),
			ids:    []string{"r-deny", "r-allow"},
		},
		{
			name: "explicit empty strings are not missing",
			ctx:  `{"region":"","plan":"","tier":""}`,
			conds: []explainCondExpectation{
				{0, 0, "region", sp(""), false, false},
				{0, 1, "plan", sp(""), false, false},
				{1, 0, "tier", sp(""), false, true}, // "" 是 in 列表成员
			},
			winner: sp("r-allow"),
			ids:    []string{"r-deny", "r-allow"},
		},
		{
			name: "present region and plan mismatch, tier missing",
			ctx:  `{"region":"cn","plan":"free"}`,
			conds: []explainCondExpectation{
				{0, 0, "region", sp("cn"), false, true},
				{0, 1, "plan", sp("free"), false, false},
				{1, 0, "tier", nil, true, false},
			},
			winner: nil,
			ids:    []string{"r-deny", "r-allow"},
		},
	}

	baseCfg := mustParseConfig(t, explainMetaBaseCfg)
	shadowCfg := mustParseConfig(t, explainMetaShadowCfg)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := mustParseContext(t, tc.ctx)
			got := shadowCfg.Find("f").EvaluateExplain(ctx)
			base := baseCfg.Find("f").EvaluateExplain(ctx)

			// 完整解释与没有任何附加字段时逐字节一致——这直接证明影子值既没有
			// 补齐缺失，也没有改动任何比较值/实际值/判断。
			if b, ref := mustMarshalExplain(t, got), mustMarshalExplain(t, base); !bytes.Equal(b, ref) {
				t.Fatalf("shadow config explanation differs from baseline:\ngot:  %s\nwant: %s", b, ref)
			}

			if ids := explainedIDs(got); !equalStrings(ids, tc.ids) {
				t.Fatalf("considered rules = %v, want %v", ids, tc.ids)
			}
			wantResult := EvalResult{Key: "f", Reason: EvalRule, RuleID: tc.winner}
			if tc.winner == nil {
				wantResult.Reason = EvalDefault
				wantResult.Value = true
			} else {
				wantResult.Value = true
			}
			if !got.EvalResult.Equals(wantResult) {
				t.Fatalf("result = %+v, want %+v", got.EvalResult, wantResult)
			}

			for _, w := range tc.conds {
				c := got.Explanation.Rules[w.rule].Conditions[w.cond]
				if c.Attribute != w.attribute {
					t.Fatalf("condition attribute = %q, want %q", c.Attribute, w.attribute)
				}
				if c.Missing != w.missing {
					t.Fatalf("%s: missing = %v, want %v: %+v", w.attribute, c.Missing, w.missing, c)
				}
				if c.Match != w.match {
					t.Fatalf("%s: match = %v, want %v: %+v", w.attribute, c.Match, w.match, c)
				}
				switch {
				case w.actual == nil:
					if c.ActualValue != nil {
						t.Fatalf("%s: actualValue = %q, want nil (must not be filled from extra fields): %+v",
							w.attribute, *c.ActualValue, c)
					}
				default:
					if c.ActualValue == nil || *c.ActualValue != *w.actual {
						got := "<nil>"
						if c.ActualValue != nil {
							got = *c.ActualValue
						}
						t.Fatalf("%s: actualValue = %q, want %q", w.attribute, got, *w.actual)
					}
				}
			}

			// 序列化层面再守一道：缺失属性必须渲染成 null/true 相邻记录，不能
			// 出现影子值冒充实际值。
			out := mustMarshalExplain(t, got)
			if bytes.Contains(out, []byte(`"actualValue":"cn","missing":true`)) {
				t.Fatalf("shadow value must not be rendered as the actual of a missing attribute: %s", out)
			}
		})
	}
}

// TestExplainExtraFieldDuplicateRejectedWholeConfig 保留“整份配置先校验”的
// 边界在解释相关代码路径上的行为：即使请求的开关完全合法、能立刻得到解释，
// 未被选中或已关闭开关的附加对象在合法大数字之后重复声明同名字段时，
// ParseConfig 仍必须拒绝整份配置，错误点名重复字段与其具体位置，既不能误报
// 为数字过大/JSON 语法错误，也不受开关是否被选中、是否关闭影响。进程级契约
// （非零退出、stdout 为空、无半份解释）在 cmd/payflow 层端到端验证。
func TestExplainExtraFieldDuplicateRejectedWholeConfig(t *testing.T) {
	hugeInt := "1" + strings.Repeat("0", 400)
	cases := []struct {
		name    string
		key     string
		raw     string
		wantLoc string
	}{
		{
			"duplicate after 1e400 in unselected enabled flag",
			"good",
			`{"flags":[
				{"key":"good","enabled":true,"default":false,"rules":[
					{"id":"r","value":true,"conditions":[
						{"attribute":"plan","op":"eq","value":"pro"}]}]},
				{"key":"bad","enabled":true,"default":false,"rules":[],
					"meta":{"n":1e400,"n":2}}]}`,
			`config.flags[1].meta.n`,
		},
		{
			"duplicate after 400-digit integer in selected disabled flag",
			"off",
			`{"flags":[
				{"key":"off","enabled":false,"default":false,"rules":[],
					"meta":{"n":` + hugeInt + `,"n":2}}]}`,
			`config.flags[0].meta.n`,
		},
		{
			"duplicate after 1e400 in unselected disabled flag",
			"good",
			`{"flags":[
				{"key":"good","enabled":true,"default":true,"rules":[]},
				{"key":"off","enabled":false,"default":false,"rules":[],
					"meta":{"n":1e400,"n":2}}]}`,
			`config.flags[1].meta.n`,
		},
		{
			"duplicate after 1e400 deep in disabled flag condition extra",
			"good",
			`{"flags":[
				{"key":"good","enabled":true,"default":false,"rules":[
					{"id":"r","value":true,"conditions":[
						{"attribute":"plan","op":"eq","value":"pro"}]}]},
				{"key":"off","enabled":false,"default":false,"rules":[
					{"id":"r-never","value":true,"conditions":[
						{"attribute":"plan","op":"eq","value":"pro",
							"hint":{"deep":{"n":1e400,"n":2}}}]}]}]}`,
			`config.flags[1].rules[0].conditions[0].hint.deep.n`,
		},
		{
			"duplicate after 1e400 in top-level extra object",
			"good",
			`{"meta":{"n":1e400,"n":2},"flags":[
				{"key":"good","enabled":true,"default":true,"rules":[]}]}`,
			`config.meta.n`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected whole-config rejection, config parsed: %+v", cfg)
			}
			msg := err.Error()
			if !strings.Contains(msg, "duplicate field") {
				t.Fatalf("error must name a duplicate field, got %q", msg)
			}
			if !strings.Contains(msg, tc.wantLoc) {
				t.Fatalf("error must point at %s, got %q", tc.wantLoc, msg)
			}
			// 合法大数字绝不能被误报成数字过大或 JSON 语法错误。
			if strings.Contains(msg, "invalid JSON") {
				t.Fatalf("legal 1e400 must not be reported as invalid JSON: %q", msg)
			}
			if l := strings.ToLower(msg); strings.Contains(l, "too large") ||
				strings.Contains(l, "overflow") || strings.Contains(l, "out of range") {
				t.Fatalf("legal 1e400 must not be reported as a number range error: %q", msg)
			}
		})
	}
}
