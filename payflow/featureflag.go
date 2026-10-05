package payflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"unicode/utf8"
)

// EvalReason is the machine-readable outcome of a flag evaluation.
type EvalReason string

const (
	EvalDisabled EvalReason = "disabled"
	EvalRule     EvalReason = "rule"
	EvalDefault  EvalReason = "default"
)

// Condition is one predicate over an evaluation context.
type Condition struct {
	Attribute string
	Op        string
	// strVal 用于 eq；inVal 用于 in（至少一项）。
	strVal string
	inVal  []string
}

// Rule groups conditions that must all hold; on match the flag takes Value.
type Rule struct {
	ID         string
	Value      bool
	Conditions []Condition
}

// Flag is one feature toggle with its default and ordered rules.
type Flag struct {
	Key     string
	Enabled bool
	Default bool
	Rules   []Rule
}

// Config is a validated feature flag configuration.
type Config struct {
	Flags []Flag
}

// EvalResult is the deterministic outcome of evaluating one flag.
type EvalResult struct {
	Key    string     `json:"key"`
	Value  bool       `json:"value"`
	Reason EvalReason `json:"reason"`
	RuleID *string    `json:"ruleId"`
}

// Equals reports whether two evaluation results are identical.
func (r EvalResult) Equals(other EvalResult) bool {
	if r.Key != other.Key || r.Value != other.Value || r.Reason != other.Reason {
		return false
	}
	if (r.RuleID == nil) != (other.RuleID == nil) {
		return false
	}
	if r.RuleID != nil && *r.RuleID != *other.RuleID {
		return false
	}
	return true
}

// ConditionExplanation 记录单个条件在给定上下文上的判断：属性名、比较方式
// （eq/in）、配置中的比较值、上下文实际值以及条件是否成立。属性缺失时
// Missing 为 true 且 ActualValue 为 nil——与显式给出的空字符串
// （ActualValue 指向 ""、Missing 为 false）明确区分。
type ConditionExplanation struct {
	Attribute    string `json:"attribute"`
	Op           string `json:"op"`
	CompareValue any    `json:"compareValue"`
	// ActualValue 为上下文实际值；属性缺失时为 nil 并把 Missing 置为 true。
	ActualValue *string `json:"actualValue"`
	Missing     bool    `json:"missing"`
	Match       bool    `json:"match"`
}

// RuleExplanation 记录一条被考虑过的规则：规则编号、是否整体命中以及它的
// 全部条件逐条判断结果（即使规则在首个失败条件后已确定不命中，其余条件的
// 判断也照样给出）。
type RuleExplanation struct {
	RuleID     string                 `json:"ruleId"`
	Match      bool                   `json:"match"`
	Conditions []ConditionExplanation `json:"conditions"`
}

// EvalExplanation 是解释模式附加在结果对象上的判断过程。Outcome 用一句话
// 说明最终结果的依据；Rules 按配置顺序列出实际考虑过的规则，到第一条全部
// 条件成立的规则为止（含该规则），开关关闭时为空列表。
type EvalExplanation struct {
	Outcome string            `json:"outcome"`
	Rules   []RuleExplanation `json:"rules"`
}

// ExplainedResult 是解释模式的输出：四个既有字段与普通模式逐字一致，
// 另带 explanation 判断记录。
type ExplainedResult struct {
	EvalResult
	Explanation EvalExplanation `json:"explanation"`
}

// Context is a validated map of attribute names to string values.
type Context struct {
	values map[string]string
}

// LoadConfig reads and strictly validates a UTF-8 JSON flag config file.
// 所有开关都会被完整校验，包括不会被求值或已关闭的开关。
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %q: %w", path, err)
	}
	return ParseConfig(raw)
}

// LoadContext reads and validates a UTF-8 JSON context file.
func LoadContext(path string) (*Context, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read context file %q: %w", path, err)
	}
	return ParseContext(raw)
}

// unmarshalStrictJSON 严格解码一个 UTF-8 JSON 文档到 any，并保持与
// json.Unmarshal 相同的单文档语义：文档之后只允许空白，不允许第二个值。
// 使用 json.Decoder + UseNumber：数字按原文保留为 json.Number，而不是强制
// 转成 float64——语法合法但超出浮点范围的数字（如 1e400、由 1 后接 400 个 0
// 组成的整数）不应被当成“JSON 无效”，它们是合法的 JSON 数字，由后续的值
// 类型检查以所属字段的类型错误报告。
func unmarshalStrictJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	// Decoder 本身容忍文档之后的额外 JSON 值，需显式补回 Unmarshal 的严格性。
	offset := int(dec.InputOffset())
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, err
		}
		for offset < len(raw) && isJSONWhitespace(raw[offset]) {
			offset++
		}
		c := byte(0)
		if offset < len(raw) {
			c = raw[offset]
		}
		return nil, fmt.Errorf("invalid character %q after top-level value", c)
	}
	return doc, nil
}

// isJSONWhitespace 报告 b 是否为 JSON 允许的空白字节。
func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// parseJSONDocument 执行配置与上下文两类输入共同维护的文档格式校验，使两份
// JSON 对同一组格式规则给出一致的接受结果与错误定位。检查按固定先后顺序报告
// 原文中的第一处问题：
//  1. 非法 UTF-8 字节——最先检查，不被其后任何错误遮住；
//  2. 不能组成完整字符的 \uXXXX 转义（字段名与字符串值同等检查）——先于
//     重复字段，避免 encoding/json 把孤立代理项静默替换成 U+FFFD 后误判重复；
//  3. 同一对象内的重复字段——先于业务字段问题；
//  4. JSON 语法与单文档边界：只接受一个顶层值，完整文档后只允许空白，
//     第二个值或其他尾随内容一律拒绝；
//  5. 顶层必须是一个 JSON 对象。
//
// 这一步只判定文档格式：数字一律按原文保留（json.Number），语法合法但超出
// 浮点范围的大数字不在此拒绝；具体字段的类型/必填/唯一性属于各自的业务规则，
// 由调用方在取得 root 后按配置或上下文的约定继续校验。label 是本文档在错误
// 信息中的名字（"config" 或 "context"）。
func parseJSONDocument(raw []byte, label string) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%s: file is not valid UTF-8", label)
	}
	if err := rejectInvalidUnicodeEscapes(raw, label); err != nil {
		return nil, err
	}
	if err := rejectDuplicateFields(raw, label); err != nil {
		return nil, err
	}
	doc, err := unmarshalStrictJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %w", label, err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: top level must be a JSON object", label)
	}
	return root, nil
}

// ParseConfig parses a config document from JSON bytes with strict typing.
// 文档格式校验由 parseJSONDocument 与 ParseContext 共同维护；这里只继续处理
// 配置自身的业务字段（开关、规则、条件与唯一性）。先解析到 any 再逐字段校验，
// 以区分“字段缺失”与“显式提供的 false”。
func ParseConfig(raw []byte) (*Config, error) {
	root, err := parseJSONDocument(raw, "config")
	if err != nil {
		return nil, err
	}
	flagsRaw, present := root["flags"]
	if !present {
		return nil, fmt.Errorf("config.flags: field is required")
	}
	if flagsRaw == nil {
		return nil, fmt.Errorf("config.flags: must be an array, got null")
	}
	flagsArr, ok := flagsRaw.([]any)
	if !ok {
		return nil, fmt.Errorf("config.flags: must be an array")
	}
	cfg := &Config{}
	seenKeys := map[string]int{}
	for i, fr := range flagsArr {
		flag, err := parseFlag(fr, i)
		if err != nil {
			return nil, err
		}
		if prev, dup := seenKeys[flag.Key]; dup {
			return nil, fmt.Errorf("config.flags[%d].key: duplicate flag key %q (first at flags[%d])", i, flag.Key, prev)
		}
		seenKeys[flag.Key] = i
		cfg.Flags = append(cfg.Flags, flag)
	}
	return cfg, nil
}

// ParseContext parses an attribute-name -> string context object; it may be empty.
// 文档格式校验由 parseJSONDocument 与 ParseConfig 共同维护；这里只继续检查
// 上下文自身的业务规则：每个顶层属性的值都必须是字符串。
func ParseContext(raw []byte) (*Context, error) {
	root, err := parseJSONDocument(raw, "context")
	if err != nil {
		return nil, err
	}
	// 语法校验通过后才建立可用属性：值类型检查与属性建立在同一次遍历中完成。
	return buildContext(raw, root)
}

// buildContext 按文件原文顺序做一次流式遍历，同时完成顶层属性的值类型检查
// 与可用属性的建立：字段名顺序取自令牌流（Go map 的遍历顺序不稳定，不能
// 直接 range root，否则同一文件在不同运行中可能报告不同属性），属性值复用
// 严格解码得出的 root——同一份上下文的值只解码一次。语法、重复字段与 Unicode
// 转义已在此前校验，这里的令牌读取不会再遇到错误。键名使用解码后的文字
// （合法的 \uXXXX 转义与直接字符同义；大小写与空白按原样保留）。对象或数组
// 值整体算所属顶层属性的一个值，错误不指向其内部成员。
func buildContext(raw []byte, root map[string]any) (*Context, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// 消费顶层对象的 '{'；语法已由 unmarshalStrictJSON 校验。
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("context: invalid JSON: %w", err)
	}
	ctx := &Context{values: make(map[string]string, len(root))}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("context: invalid JSON: %w", err)
		}
		key, _ := keyTok.(string)
		// 值不再重复解码：RawMessage 只把读取位置推进到下一个字段，
		// 实际值取自已严格解码的 root。数字（含超出浮点范围的 json.Number）、
		// 布尔、null、对象、数组都在这里按所属属性报类型错误。
		var skipped json.RawMessage
		if err := dec.Decode(&skipped); err != nil {
			return nil, fmt.Errorf("context: invalid JSON: %w", err)
		}
		s, ok := root[key].(string)
		if !ok {
			return nil, fmt.Errorf("%s: value must be a string", appendFieldPath("context", key))
		}
		ctx.values[key] = s
	}
	return ctx, nil
}

// Get looks up an attribute; an empty string is a legal value.
func (c *Context) Get(attribute string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.values[attribute]
	return v, ok
}

// Find returns the flag with the given key, or nil if absent.
func (c *Config) Find(key string) *Flag {
	if c == nil {
		return nil
	}
	for i := range c.Flags {
		if c.Flags[i].Key == key {
			return &c.Flags[i]
		}
	}
	return nil
}

// Evaluate deterministically evaluates a flag against a context.
// 未启用固定 false（reason=disabled），不使用默认值或规则；启用后取首条
// 所有条件均成立的规则的 value；无匹配取 default。求值不依赖时间、随机数
// 或 map 遍历顺序，同一配置与上下文重复求值结果一致。
func (f *Flag) Evaluate(ctx *Context) EvalResult {
	if !f.Enabled {
		return EvalResult{Key: f.Key, Value: false, Reason: EvalDisabled, RuleID: nil}
	}
	for i := range f.Rules {
		rule := &f.Rules[i]
		if rule.matches(ctx) {
			id := rule.ID
			return EvalResult{Key: f.Key, Value: rule.Value, Reason: EvalRule, RuleID: &id}
		}
	}
	return EvalResult{Key: f.Key, Value: f.Default, Reason: EvalDefault, RuleID: nil}
}

func (r *Rule) matches(ctx *Context) bool {
	for i := range r.Conditions {
		if !r.Conditions[i].matches(ctx) {
			return false
		}
	}
	return true
}

// EvaluateExplain 与 Evaluate 采用完全相同的定案规则（关闭固定 false、
// 首条全部条件成立的规则决定结果、否则取 default），并额外产出按配置顺序
// 排列的判断记录：每条在定案前被考虑过的规则都给出 id、是否命中与全部条件
// 的逐条判断；第一条命中的规则（即使返回 false）也计入并立即停止，其后的
// 规则不出现。开关关闭时不评估任何规则，解释中的规则列表为空，结果依据只
// 说明关闭导致固定 false。
func (f *Flag) EvaluateExplain(ctx *Context) ExplainedResult {
	if !f.Enabled {
		return ExplainedResult{
			EvalResult: EvalResult{Key: f.Key, Value: false, Reason: EvalDisabled, RuleID: nil},
			Explanation: EvalExplanation{
				Outcome: "flag is disabled; result is fixed to false without evaluating rules or the default",
				Rules:   []RuleExplanation{},
			},
		}
	}
	considered := make([]RuleExplanation, 0, len(f.Rules))
	for i := range f.Rules {
		rule := &f.Rules[i]
		re := rule.explain(ctx)
		considered = append(considered, re)
		if re.Match {
			id := rule.ID
			return ExplainedResult{
				EvalResult: EvalResult{Key: f.Key, Value: rule.Value, Reason: EvalRule, RuleID: &id},
				Explanation: EvalExplanation{
					Outcome: fmt.Sprintf("first matching rule %q decides the result; later rules are not considered", rule.ID),
					Rules:   considered,
				},
			}
		}
	}
	return ExplainedResult{
		EvalResult: EvalResult{Key: f.Key, Value: f.Default, Reason: EvalDefault, RuleID: nil},
		Explanation: EvalExplanation{
			Outcome: "no rule matched; the flag default is used",
			Rules:   considered,
		},
	}
}

// explain 评估一条规则的全部条件并逐条记录判断结果；Match 为所有条件都
// 成立。与 matches 不同，它不会在首个失败条件处短路，因此每个条件的属性、
// 比较值、实际值与是否成立都能展示出来。
func (r *Rule) explain(ctx *Context) RuleExplanation {
	re := RuleExplanation{RuleID: r.ID, Match: true, Conditions: make([]ConditionExplanation, 0, len(r.Conditions))}
	for i := range r.Conditions {
		ce := r.Conditions[i].explain(ctx)
		re.Conditions = append(re.Conditions, ce)
		if !ce.Match {
			re.Match = false
		}
	}
	if len(re.Conditions) == 0 {
		re.Match = false
	}
	return re
}

// explain 判断单个条件并记录属性名、比较方式、配置比较值、上下文实际值与
// 是否成立。属性缺失用 Missing=true、ActualValue=nil 表示，绝不与显式
// 空字符串（ActualValue 指向 ""）混淆。
func (c Condition) explain(ctx *Context) ConditionExplanation {
	ce := ConditionExplanation{Attribute: c.Attribute, Op: c.Op}
	switch c.Op {
	case "eq":
		ce.CompareValue = c.strVal
	case "in":
		// 复制一份，避免外部切片与解释记录共享底层数组。
		list := make([]string, len(c.inVal))
		copy(list, c.inVal)
		ce.CompareValue = list
	default:
		// 解析阶段已拒绝未知运算符，防御性处理。
		ce.CompareValue = nil
	}
	v, present := ctx.Get(c.Attribute)
	if !present {
		ce.Missing = true
		ce.Match = false
		return ce
	}
	ce.ActualValue = &v
	switch c.Op {
	case "eq":
		ce.Match = v == c.strVal
	case "in":
		for _, candidate := range c.inVal {
			if v == candidate {
				ce.Match = true
				break
			}
		}
	default:
		ce.Match = false
	}
	return ce
}

func (c Condition) matches(ctx *Context) bool {
	v, present := ctx.Get(c.Attribute)
	if !present {
		return false
	}
	switch c.Op {
	case "eq":
		return v == c.strVal
	case "in":
		for _, candidate := range c.inVal {
			if v == candidate {
				return true
			}
		}
		return false
	default:
		// 解析阶段已拒绝未知运算符，防御性处理。
		return false
	}
}

// MarshalResult renders the result as a single compact JSON object.
func MarshalResult(r EvalResult) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalExplain renders an explained result as a single compact JSON object:
// key/value/reason/ruleId 四个字段与 MarshalResult 逐字一致，其后追加
// explanation 判断记录；列表中的空字符串、重复成员与次序原样保留，不裁剪
// 空白、不合并大小写。
func MarshalExplain(r ExplainedResult) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// --- strict parsing helpers -------------------------------------------------

// isSimpleFieldName 报告字段名是否走点号连接：以 ASCII 字母或下划线开头，
// 其后只能含 ASCII 字母、数字、下划线。其他任何名字（含点号、方括号、空白、
// 空名、非 ASCII 字符）都改用方括号包住的 JSON 字符串定位，如
// context["user.name"]、context[""]。
func isSimpleFieldName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		// 非 ASCII 字节（UTF-8 续字节或首字节）一律不满足“仅 ASCII”的要求。
		if c >= 0x80 {
			return false
		}
		ok := ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c == '_'
		if i > 0 {
			ok = ok || ('0' <= c && c <= '9')
		}
		if !ok {
			return false
		}
	}
	return true
}

// appendFieldPath 在父位置后接上一个字段：简单字段名用点号连接，其余字段名
// 用方括号包住一个 JSON 字符串，引号、反斜杠与控制字符按 JSON 转义显示
// （Marshal 对 U+2028/U+2029 也会转义，但普通可见 Unicode 保持原样）。
// 名字按 JSON 解码后的文字传入，因此直接字符与合法 \uXXXX 转义得到同一位置。
func appendFieldPath(parent, name string) string {
	if isSimpleFieldName(name) {
		return parent + "." + name
	}
	return parent + "[" + jsonQuote(name) + "]"
}

// jsonQuote 把字符串渲染为带双引号的 JSON 字符串字面量：引号、反斜杠与控制
// 字符按 JSON 转义（U+2028/U+2029 也会转义，避免定位文字被行分隔符拆行），
// 其余可见字符（含 <>& 与非 ASCII 文字）保持原样。
func jsonQuote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// 不会发生：string 的 JSON 编码没有失败路径；保留一个确定的退路。
		return strconv.Quote(s)
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

// jsonFrame 是 JSON 原文遍历中一个对象或数组的层级状态。重复字段检查与非法
// Unicode 转义检查共用这一份层级与位置定义，调整错误定位时只需改这一处。
type jsonFrame struct {
	path   string // 容器自身的完整位置，如 config.flags[0].rules[1]
	object bool
	// keys 记录本对象已声明的字段名（按解码后的字符串），仅重复字段检查使用；
	// Unicode 遍历不登记字段，保持 nil。
	keys map[string]struct{}
	// lastKey 是对象最近一个字段名，用于拼出字段值的位置。
	lastKey string
	// expectKey 标记对象内下一个字符串是否为字段名。
	expectKey bool
	// nextIndex 是数组下一个元素的下标。
	nextIndex int
}

// jsonPathTracker 是两类 JSON 检查共用的对象层级、数组位置与字段路径状态机。
// label 是根容器的位置前缀（"config" 或 "context"）。
type jsonPathTracker struct {
	label string
	stack []*jsonFrame
}

func newJSONPathTracker(label string) *jsonPathTracker {
	return &jsonPathTracker{label: label}
}

func (t *jsonPathTracker) top() *jsonFrame {
	return t.stack[len(t.stack)-1]
}

// childPath 拼出栈顶容器内下一个子值的位置：对象的字段值按 appendFieldPath
// 规则连接（简单名用点号，其余用方括号 JSON 字符串），数组元素用 [下标]。
func (t *jsonPathTracker) childPath() string {
	top := t.top()
	if top.object {
		return appendFieldPath(top.path, top.lastKey)
	}
	return top.path + "[" + strconv.Itoa(top.nextIndex) + "]"
}

// beginContainer 在遇到 '{' 或 '[' 时压入新容器，返回新容器的完整位置。
func (t *jsonPathTracker) beginContainer(object bool) string {
	path := t.label
	if len(t.stack) > 0 {
		path = t.childPath()
	}
	t.stack = append(t.stack, &jsonFrame{path: path, object: object, expectKey: object})
	return path
}

// endContainer 在遇到 '}' 或 ']' 时弹出容器，并把整个容器计为父容器中
// 一个已完成的值。
func (t *jsonPathTracker) endContainer() {
	if len(t.stack) > 0 {
		t.stack = t.stack[:len(t.stack)-1]
	}
	t.endValue()
}

// atKey 报告下一个字符串是否应按对象字段名处理。
func (t *jsonPathTracker) atKey() bool {
	return len(t.stack) > 0 && t.top().object && t.top().expectKey
}

// currentPath 返回当前容器自身的位置（字段名报错时指向所属对象）；
// 栈为空时返回根标签。
func (t *jsonPathTracker) currentPath() string {
	if len(t.stack) == 0 {
		return t.label
	}
	return t.top().path
}

// valuePath 拼出下一个字符串值（字段值或数组元素）的位置，但不改变状态。
func (t *jsonPathTracker) valuePath() string {
	if len(t.stack) == 0 {
		return t.label
	}
	return t.childPath()
}

// endValue 在一个字段值或数组元素完成后调用：对象回到等待字段名的状态，
// 数组推进元素下标。
func (t *jsonPathTracker) endValue() {
	if len(t.stack) == 0 {
		return
	}
	top := t.top()
	if top.object {
		top.expectKey = true
	} else {
		top.nextIndex++
	}
}

// noteKey 登记一个已解码的对象字段名：更新字段路径状态，并在同一对象内第二次
// 声明同名字段时返回重复字段错误。
func (t *jsonPathTracker) noteKey(name string) error {
	top := t.top()
	if top.keys == nil {
		top.keys = make(map[string]struct{})
	}
	if _, dup := top.keys[name]; dup {
		return fmt.Errorf("%s: duplicate field %s", appendFieldPath(top.path, name), jsonQuote(name))
	}
	top.keys[name] = struct{}{}
	top.lastKey = name
	top.expectKey = false
	return nil
}

// setKey 在不做重复检查的遍历（Unicode 扫描）中记录一个成功解码的字段名，
// 仅维护字段值路径所需的状态。
func (t *jsonPathTracker) setKey(name string) {
	top := t.top()
	top.lastKey = name
	top.expectKey = false
}

// noteInvalidKey 在字段名无法解码（属于 JSON 语法错误）时仅推进键值状态：
// 不覆盖已记录的字段名；语法错误留给后续严格 JSON 解码报告。
func (t *jsonPathTracker) noteInvalidKey() {
	t.top().expectKey = false
}

// rejectDuplicateFields 按文件原文顺序扫描 JSON 令牌流，拒绝任何在同一对象内
// 重复声明的字段名。字段名按解码后的字符串比较（"enabled" 与 "énabled"
// 同名），大小写不同仍是不同字段，名字中的空白不裁剪。检查覆盖所有对象，
// 包括附加字段里嵌套的对象；数组元素不属于这一检查。label 是根对象的位置
// 前缀（"config" 或 "context"）。对象层级、数组下标与字段路径由
// jsonPathTracker 统一维护。JSON 语法错误不在此报告（令牌流失效即停止），
// 留给严格 JSON 解码。
func rejectDuplicateFields(raw []byte, label string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber：数字以 json.Number 令牌返回而不转 float64，否则超出浮点范围
	// 的合法数字（如 1e400）会让令牌流提前报错并中止，遮住其后的重复字段。
	dec.UseNumber()
	tracker := newJSONPathTracker(label)
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF 或语法错误；语法错误由后续严格 JSON 解码报告。
			break
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				tracker.beginContainer(true)
			case '[':
				tracker.beginContainer(false)
			case '}', ']':
				tracker.endContainer()
			}
		case string:
			if tracker.atKey() {
				if err := tracker.noteKey(t); err != nil {
					return err
				}
			} else {
				tracker.endValue()
			}
		default:
			tracker.endValue()
		}
	}
	return nil
}

// rejectInvalidUnicodeEscapes 按文件原文顺序扫描 JSON 文本，拒绝任何不能组成
// 完整字符的 \uXXXX 转义：孤立的高代理项、孤立的低代理项，以及不完整或顺序
// 错误的代理项对。encoding/json 解码时会把这类转义静默替换为 U+FFFD，使非法
// 输入与真正的 "�" 被当作相同字符串参与求值，或让字段名在替换后被误判为重复，
// 因此必须在解码前按原文拒绝。检查覆盖字段名与所有字符串值（包括规则的比较值、
// in 列表元素以及附加字段里的字符串），一个文件有多个问题时报告原文中最先出现
// 的一处。label 是根对象的位置前缀（"config" 或 "context"）。对象层级、数组
// 下标与字段路径由 jsonPathTracker 统一维护；JSON 语法错误不在此报告，留给
// Unmarshal。
func rejectInvalidUnicodeEscapes(raw []byte, label string) error {
	tracker := newJSONPathTracker(label)
	i := 0
	for i < len(raw) {
		c := raw[i]
		switch {
		case c == '{' || c == '[':
			tracker.beginContainer(c == '{')
			i++
		case c == '}' || c == ']':
			tracker.endContainer()
			i++
		case c == '"':
			end := jsonStringEnd(raw, i)
			contentEnd := end
			if contentEnd > i+1 && raw[contentEnd-1] == '"' {
				contentEnd-- // 去掉右引号；未终止的字符串留给严格 JSON 解码报告
			}
			content := raw[i+1 : contentEnd]
			if tracker.atKey() {
				if bad := findBadUnicodeEscape(content); bad != nil {
					return fmt.Errorf("%s: field name %q contains invalid Unicode escape %s: Unicode escape does not form a complete character (%s)",
						tracker.currentPath(), string(content), bad.escape, bad.detail)
				}
				// 转义已校验合法，解码用于定位路径；失败说明是语法错误，留给严格 JSON 解码。
				if name, ok := decodeJSONString(raw[i:end]); ok {
					tracker.setKey(name)
				} else {
					tracker.noteInvalidKey()
				}
			} else {
				path := tracker.valuePath()
				if bad := findBadUnicodeEscape(content); bad != nil {
					return fmt.Errorf("%s: invalid Unicode escape %s: Unicode escape does not form a complete character (%s)",
						path, bad.escape, bad.detail)
				}
				tracker.endValue()
			}
			i = end
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ':' || c == ',':
			i++
		default:
			// 数字与 true/false/null 等标量：跳过至下一个结构字符，按一个值计。
			for i < len(raw) && !isJSONStructural(raw[i]) {
				i++
			}
			tracker.endValue()
		}
	}
	return nil
}

// decodeJSONString 解码一个完整的 JSON 字符串字面量（含两侧引号）；ok 为 false
// 表示字面量本身非法（未终止或转义写法错误等），属于 JSON 语法错误。
func decodeJSONString(lit []byte) (s string, ok bool) {
	if err := json.Unmarshal(lit, &s); err != nil {
		return "", false
	}
	return s, true
}

// badEscape 描述一处不能组成完整字符的 \uXXXX 转义。
type badEscape struct {
	escape string // 原文中的转义序列，如 \uD800
	detail string // 具体问题（孤立高代理项/孤立低代理项）
}

// jsonStringEnd 返回从 raw[i]（必须是 '"'）开始的字符串字面量结束位置
// （右引号之后的下标）；未终止时返回 len(raw)。
func jsonStringEnd(raw []byte, i int) int {
	j := i + 1
	for j < len(raw) {
		switch raw[j] {
		case '\\':
			j += 2
		case '"':
			return j + 1
		default:
			j++
		}
	}
	return len(raw)
}

// findBadUnicodeEscape 在字符串内容（不含引号）中查找第一处不能组成完整
// 字符的 \uXXXX 转义；没有则返回 nil。反斜杠本身的转义（\\）先于 \u 判定，
// 因此 "\\uD800" 只是普通文本。非法的 \u 写法（非 4 位十六进制）属于 JSON
// 语法错误，不在此报告。
func findBadUnicodeEscape(content []byte) *badEscape {
	j := 0
	for j < len(content) {
		if content[j] != '\\' {
			j++
			continue
		}
		if j+1 >= len(content) {
			break
		}
		if content[j+1] != 'u' {
			j += 2
			continue
		}
		if j+6 > len(content) || !isHex4(content[j+2:j+6]) {
			j += 2
			continue
		}
		cp := hexVal4(content[j+2 : j+6])
		switch {
		case cp >= 0xD800 && cp <= 0xDBFF:
			// 高代理项只有紧跟一个低代理项转义才组成完整字符。
			if j+12 <= len(content) && content[j+6] == '\\' && content[j+7] == 'u' &&
				isHex4(content[j+8:j+12]) {
				if lo := hexVal4(content[j+8 : j+12]); lo >= 0xDC00 && lo <= 0xDFFF {
					j += 12
					continue
				}
			}
			return &badEscape{
				escape: string(content[j : j+6]),
				detail: "high surrogate is not followed by a low surrogate",
			}
		case cp >= 0xDC00 && cp <= 0xDFFF:
			return &badEscape{
				escape: string(content[j : j+6]),
				detail: "low surrogate is not preceded by a high surrogate",
			}
		}
		j += 6
	}
	return nil
}

// isJSONStructural 报告 b 是否为 JSON 结构字符或空白（标量值的边界）。
func isJSONStructural(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '{', '}', '[', ']', ':', ',', '"':
		return true
	}
	return false
}

// isHex4 报告 b 是否恰好为 4 个十六进制数字。
func isHex4(b []byte) bool {
	if len(b) != 4 {
		return false
	}
	for _, c := range b {
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

// hexVal4 把 4 个十六进制数字解析为码点值；调用前须先经 isHex4 校验。
func hexVal4(b []byte) int {
	v := 0
	for _, c := range b {
		v <<= 4
		switch {
		case '0' <= c && c <= '9':
			v |= int(c - '0')
		case 'a' <= c && c <= 'f':
			v |= int(c-'a') + 10
		default: // 'A' <= c && c <= 'F'
			v |= int(c-'A') + 10
		}
	}
	return v
}

func parseFlag(raw any, index int) (Flag, error) {
	loc := fmt.Sprintf("config.flags[%d]", index)
	obj, ok := raw.(map[string]any)
	if !ok {
		return Flag{}, fmt.Errorf("%s: flag must be a JSON object", loc)
	}
	key, err := requireNonEmptyString(obj, "key", loc)
	if err != nil {
		return Flag{}, err
	}
	enabled, err := requireBool(obj, "enabled", loc)
	if err != nil {
		return Flag{}, err
	}
	def, err := requireBool(obj, "default", loc)
	if err != nil {
		return Flag{}, err
	}
	rulesArr, err := requireArrayField(obj, "rules", loc)
	if err != nil {
		return Flag{}, err
	}
	flag := Flag{Key: key, Enabled: enabled, Default: def}
	seenRules := map[string]int{}
	for ri, rv := range rulesArr {
		rule, err := parseRule(rv, index, ri)
		if err != nil {
			return Flag{}, err
		}
		if prev, dup := seenRules[rule.ID]; dup {
			return Flag{}, fmt.Errorf("%s.rules[%d].id: duplicate rule id %q (first at rules[%d])", loc, ri, rule.ID, prev)
		}
		seenRules[rule.ID] = ri
		flag.Rules = append(flag.Rules, rule)
	}
	return flag, nil
}

func parseRule(raw any, flagIndex, ruleIndex int) (Rule, error) {
	loc := fmt.Sprintf("config.flags[%d].rules[%d]", flagIndex, ruleIndex)
	obj, ok := raw.(map[string]any)
	if !ok {
		return Rule{}, fmt.Errorf("%s: rule must be a JSON object", loc)
	}
	id, err := requireNonEmptyString(obj, "id", loc)
	if err != nil {
		return Rule{}, err
	}
	value, err := requireBool(obj, "value", loc)
	if err != nil {
		return Rule{}, err
	}
	condsArr, err := requireArrayField(obj, "conditions", loc)
	if err != nil {
		return Rule{}, err
	}
	if len(condsArr) == 0 {
		return Rule{}, fmt.Errorf("%s.conditions: must contain at least one condition", loc)
	}
	rule := Rule{ID: id, Value: value}
	for ci, cv := range condsArr {
		cond, err := parseCondition(cv, flagIndex, ruleIndex, ci)
		if err != nil {
			return Rule{}, err
		}
		rule.Conditions = append(rule.Conditions, cond)
	}
	return rule, nil
}

func parseCondition(raw any, flagIndex, ruleIndex, condIndex int) (Condition, error) {
	loc := fmt.Sprintf("config.flags[%d].rules[%d].conditions[%d]", flagIndex, ruleIndex, condIndex)
	obj, ok := raw.(map[string]any)
	if !ok {
		return Condition{}, fmt.Errorf("%s: condition must be a JSON object", loc)
	}
	attribute, err := requireNonEmptyString(obj, "attribute", loc)
	if err != nil {
		return Condition{}, err
	}
	op, err := requireNonEmptyString(obj, "op", loc)
	if err != nil {
		return Condition{}, err
	}
	valueField, err := requireField(obj, "value", loc)
	if err != nil {
		return Condition{}, err
	}
	cond := Condition{Attribute: attribute, Op: op}
	switch op {
	case "eq":
		s, ok := valueField.(string)
		if !ok {
			return Condition{}, fmt.Errorf("%s.value: must be a string when op is %q", loc, op)
		}
		cond.strVal = s
	case "in":
		arr, ok := valueField.([]any)
		if !ok {
			return Condition{}, fmt.Errorf("%s.value: must be an array when op is %q", loc, op)
		}
		if len(arr) == 0 {
			return Condition{}, fmt.Errorf("%s.value: in-list must contain at least one item", loc)
		}
		for ii, item := range arr {
			s, ok := item.(string)
			if !ok {
				return Condition{}, fmt.Errorf("%s.value[%d]: must be a string", loc, ii)
			}
			cond.inVal = append(cond.inVal, s)
		}
	default:
		return Condition{}, fmt.Errorf("%s.op: unknown operator %q (allowed: eq, in)", loc, op)
	}
	return cond, nil
}

func requireField(obj map[string]any, name, loc string) (any, error) {
	v, ok := obj[name]
	if !ok {
		return nil, fmt.Errorf("%s: field is required", appendFieldPath(loc, name))
	}
	if v == nil {
		return nil, fmt.Errorf("%s: must not be null", appendFieldPath(loc, name))
	}
	return v, nil
}

func requireArrayField(obj map[string]any, name, loc string) ([]any, error) {
	v, err := requireField(obj, name, loc)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be an array", appendFieldPath(loc, name))
	}
	return arr, nil
}

func requireNonEmptyString(obj map[string]any, name, loc string) (string, error) {
	v, err := requireField(obj, name, loc)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: must be a string", appendFieldPath(loc, name))
	}
	if s == "" {
		return "", fmt.Errorf("%s: must not be empty", appendFieldPath(loc, name))
	}
	return s, nil
}

func requireBool(obj map[string]any, name, loc string) (bool, error) {
	v, err := requireField(obj, name, loc)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%s: must be a boolean", appendFieldPath(loc, name))
	}
	return b, nil
}
