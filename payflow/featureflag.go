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

// isSimpleFieldName 报告 name 是否为“点号名字”：以 ASCII 字母或下划线开头，
// 其后仅含 ASCII 字母、数字与下划线。此类名字在位置中直接以点号连接。
func isSimpleFieldName(name string) bool {
	if name == "" {
		return false
	}
	if !isFieldNameStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isFieldNameChar(name[i]) {
			return false
		}
	}
	return true
}

func isFieldNameStart(b byte) bool {
	return b == '_' || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

func isFieldNameChar(b byte) bool {
	return isFieldNameStart(b) || ('0' <= b && b <= '9')
}

// appendFieldPath 把一个字段名按统一规则追加到父位置 parent 之后：点号名字
// （isSimpleFieldName）用 ".name" 连接；其他名字在父位置后接 "[<JSON 字符串>]"，
// 字符串采用 JSON 编码（Go 的 strconv.Quote 与 JSON 对引号、反斜杠和控制字符
// 的转义一致），如 parent + `["user.name"]`、parent + `[""]`。
func appendFieldPath(parent, name string) string {
	if isSimpleFieldName(name) {
		return parent + "." + name
	}
	return parent + "[" + strconv.Quote(name) + "]"
}

// ParseConfig parses a config document from JSON bytes with strict typing.
// 先解析到 any 再逐字段校验，以区分“字段缺失”与“显式提供的 false”。
func ParseConfig(raw []byte) (*Config, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("config: file is not valid UTF-8")
	}
	if err := rejectInvalidUnicodeEscapes(raw, "config"); err != nil {
		return nil, err
	}
	if err := rejectDuplicateFields(raw, "config"); err != nil {
		return nil, err
	}
	doc, err := unmarshalStrictJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("config: invalid JSON: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config: top level must be a JSON object")
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
			flagLoc := fmt.Sprintf("config.flags[%d]", i)
			return nil, fmt.Errorf("%s: duplicate flag key %q (first at flags[%d])", appendFieldPath(flagLoc, "key"), flag.Key, prev)
		}
		seenKeys[flag.Key] = i
		cfg.Flags = append(cfg.Flags, flag)
	}
	return cfg, nil
}

// ParseContext parses an attribute-name -> string context object; it may be empty.
func ParseContext(raw []byte) (*Context, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("context: file is not valid UTF-8")
	}
	if err := rejectInvalidUnicodeEscapes(raw, "context"); err != nil {
		return nil, err
	}
	if err := rejectDuplicateFields(raw, "context"); err != nil {
		return nil, err
	}
	doc, err := unmarshalStrictJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("context: invalid JSON: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("context: top level must be a JSON object")
	}
	// 值类型必须按原文顺序逐个判定：Go map 的遍历顺序不稳定，不能直接
	// range root，否则同一文件在不同运行中可能报告不同属性。对象或数组
	// 值整体算所属顶层属性的一个值，错误不指向其内部成员。
	if err := checkContextValueTypes(raw); err != nil {
		return nil, err
	}
	ctx := &Context{values: make(map[string]string, len(root))}
	for k, v := range root {
		// 经过 checkContextValueTypes 后所有值都是字符串；数字（含超出浮点
		// 范围的 json.Number）、布尔、null、对象、数组都已按所属属性报错。
		ctx.values[k] = v.(string)
	}
	return ctx, nil
}

// checkContextValueTypes 按文件原文顺序扫描已通过语法与重复字段检查的
// context 对象，报告第一个值不是字符串的顶层属性。键名使用解码后的文字
// （合法的 \uXXXX 转义与直接字符同义；大小写与空白按原样保留），并按统一
// 规则定位：点号名字用 context.name，其余用 context["JSON 字符串"]。嵌套
// 对象或数组整体跳过，其内部成员不单独检查。
func checkContextValueTypes(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber：数字按 json.Number 原样保留而不转 float64，使超出浮点范围
	// 的合法数字（如 1e400）也能落到“值必须是字符串”的属性类型错误，而不是
	// 在解码阶段被报成 JSON 语法问题。
	dec.UseNumber()
	// 第一个令牌必须是顶层对象的 '{'；非对象的情形在 ParseContext 中报告。
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil // 语法错误留给严格解码的报错
		}
		key, _ := keyTok.(string)
		var value any
		if err := dec.Decode(&value); err != nil {
			return nil // 语法错误留给严格解码的报错
		}
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: value must be a string", appendFieldPath("context", key))
		}
	}
	return nil
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

// --- strict parsing helpers -------------------------------------------------

// jsonFrame 是 JSON 原文遍历中一个对象或数组的层级状态。重复字段检查与非法
// Unicode 转义检查共用这一份层级与位置定义，调整错误定位时只需改这一处。
type jsonFrame struct {
	path   string // 容器自身的完整位置，如 config.flags[0].rules[1]
	object bool
	// keys 记录本对象已声明的字段名（按解码后的字符串），仅重复字段检查使用；
	// Unicode 遍历不登记字段，保持 nil。
	keys map[string]struct{}
	// lastKey 是对象最近一个字段名，用于拼出字段值的位置。字段名能正常
	// JSON 解码时（lastKeyValid）保存解码后的文字；字段名含不能组成完整
	// 字符的 Unicode 转义时保存字段名原文（不含两侧引号，保留 \uXXXX
	// 等原始写法），且 lastKeyValid 为 false——这种名字无法解码，只能以
	// 原文参与后续字段的定位。
	lastKey      string
	lastKeyValid bool
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

// childPath 拼出栈顶容器内下一个子值的位置：对象的字段值按统一规则拼接
// 字段名（点号名字用 .name，其余用 ["JSON 字符串"]），数组元素用 [下标]。
func (t *jsonPathTracker) childPath() string {
	top := t.top()
	if top.object {
		if top.lastKeyValid {
			return appendFieldPath(top.path, top.lastKey)
		}
		// 字段名本身无法解码（含非法转义）：把原文（反斜杠按 JSON 再转义一次）
		// 放进方括号，不替换成 U+FFFD 之类的其他字符。
		return top.path + "[" + strconv.Quote(top.lastKey) + "]"
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
		return fmt.Errorf("%s: duplicate field %q", appendFieldPath(top.path, name), name)
	}
	top.keys[name] = struct{}{}
	top.lastKey = name
	top.lastKeyValid = true
	top.expectKey = false
	return nil
}

// setKey 在不做重复检查的遍历（Unicode 扫描）中记录一个成功解码的字段名，
// 仅维护字段值路径所需的状态。
func (t *jsonPathTracker) setKey(name string) {
	top := t.top()
	top.lastKey = name
	top.lastKeyValid = true
	top.expectKey = false
}

// setInvalidKey 在字段名含不能组成完整字符的 Unicode 转义时记录其原文
// （不含两侧引号，保留 \uXXXX 等原始写法）：不替换成 U+FFFD 之类的其他字符，
// 使同一对象内后续值的定位仍能准确指向所属对象。非法字段名不登记到重复
// 字段集合——它本身已构成错误，且无法与解码后的名字比较。
func (t *jsonPathTracker) setInvalidKey(rawName string) {
	top := t.top()
	top.lastKey = rawName
	top.lastKeyValid = false
	top.expectKey = false
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
					// 字段名原文用 strconv.Quote 包成 JSON 字符串形式：其中的引号、
					// 反斜杠与控制字符按 JSON 转义，而 \uXXXX 这类转义写法以反斜杠
					// 转义后原样可辨（不会先解码再替换成别的字符）。
					return fmt.Errorf("%s: field name %s contains invalid Unicode escape %s: Unicode escape does not form a complete character (%s)",
						tracker.currentPath(), strconv.Quote(string(content)), bad.escape, bad.detail)
				}
				// 转义已校验合法，解码用于定位路径；失败说明是语法错误，留给严格 JSON 解码。
				if name, ok := decodeJSONString(raw[i:end]); ok {
					tracker.setKey(name)
				} else {
					tracker.setInvalidKey(string(content))
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
			return Flag{}, fmt.Errorf("%s: duplicate rule id %q (first at rules[%d])",
				appendFieldPath(fmt.Sprintf("%s.rules[%d]", loc, ri), "id"), rule.ID, prev)
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
		return Rule{}, fmt.Errorf("%s: must contain at least one condition", appendFieldPath(loc, "conditions"))
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
	valueLoc := appendFieldPath(loc, "value")
	switch op {
	case "eq":
		s, ok := valueField.(string)
		if !ok {
			return Condition{}, fmt.Errorf("%s: must be a string when op is %q", valueLoc, op)
		}
		cond.strVal = s
	case "in":
		arr, ok := valueField.([]any)
		if !ok {
			return Condition{}, fmt.Errorf("%s: must be an array when op is %q", valueLoc, op)
		}
		if len(arr) == 0 {
			return Condition{}, fmt.Errorf("%s: in-list must contain at least one item", valueLoc)
		}
		for ii, item := range arr {
			s, ok := item.(string)
			if !ok {
				return Condition{}, fmt.Errorf("%s[%d]: must be a string", valueLoc, ii)
			}
			cond.inVal = append(cond.inVal, s)
		}
	default:
		return Condition{}, fmt.Errorf("%s: unknown operator %q (allowed: eq, in)", appendFieldPath(loc, "op"), op)
	}
	return cond, nil
}

func requireField(obj map[string]any, name, loc string) (any, error) {
	v, ok := obj[name]
	field := appendFieldPath(loc, name)
	if !ok {
		return nil, fmt.Errorf("%s: field is required", field)
	}
	if v == nil {
		return nil, fmt.Errorf("%s: must not be null", field)
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
