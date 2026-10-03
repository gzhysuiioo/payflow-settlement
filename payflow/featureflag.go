package payflow

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
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
			return nil, fmt.Errorf("config.flags[%d].key: duplicate flag key %q (first at flags[%d])", i, flag.Key, prev)
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
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("context: invalid JSON: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("context: top level must be a JSON object")
	}
	ctx := &Context{values: make(map[string]string, len(root))}
	for k, v := range root {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("context.%s: value must be a string", k)
		}
		ctx.values[k] = s
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

// --- strict JSON scanning ---------------------------------------------------
//
// 非法 Unicode 转义检查与重复字段检查都在解码前按原文顺序遍历 JSON，并把每个
// 值定位到“来源标识 + 完整字段位置”（对象层级、数组下标、字段路径）。容器栈
// 与路径推进规则只在 jsonLocation 中维护一份：两个检查各自选择合适的令牌化
// 方式（重复字段用 encoding/json 令牌流，非法转义用字节扫描），复用同一套
// 定位规则，以后调整错误定位时不必同时改两处。

// jsonContainer 是遍历过程中一层容器的定位状态。
type jsonContainer struct {
	path      string // 该容器本身的完整位置，如 config.flags[0].rules
	object    bool   // true 为对象，false 为数组
	lastKey   string // 对象内最近一个字段的解码后字段名
	expectKey bool   // 对象的下一个字符串 token 是否是字段名
	nextIndex int    // 数组下一个元素的下标
}

// jsonLocation 在一遍 JSON 遍历中维护容器栈与字段/数组位置。它只负责定位，
// 不关心令牌如何切出：两个预解码检查分别用 json.Decoder 令牌流与字节扫描
// 驱动同一组方法。
type jsonLocation struct {
	label string // 根位置前缀，即来源标识（"config" 或 "context"）
	stack []*jsonContainer
}

// openContainer 在遇到 '{' 或 '[' 时入栈，新容器路径取父容器中下一个值的位置。
func (l *jsonLocation) openContainer(object bool) {
	path := l.label
	if len(l.stack) > 0 {
		path = l.nextValuePath()
	}
	l.stack = append(l.stack, &jsonContainer{path: path, object: object, expectKey: object})
}

// closeContainer 在遇到 '}' 或 ']' 时出栈；整个容器本身也是父容器的一个值。
func (l *jsonLocation) closeContainer() {
	if len(l.stack) > 0 {
		l.stack = l.stack[:len(l.stack)-1]
	}
	l.valueDone()
}

// nextValuePath 返回栈顶容器中下一个值的完整位置：对象内为 path.key，数组内
// 为 path[index]；栈为空（根值）时返回来源标识。
func (l *jsonLocation) nextValuePath() string {
	if len(l.stack) == 0 {
		return l.label
	}
	top := l.stack[len(l.stack)-1]
	if top.object {
		return top.path + "." + top.lastKey
	}
	return top.path + "[" + strconv.Itoa(top.nextIndex) + "]"
}

// containerPath 返回栈顶容器自身的路径；字段名中的错误要指向所属对象。
func (l *jsonLocation) containerPath() string {
	if len(l.stack) == 0 {
		return l.label
	}
	return l.stack[len(l.stack)-1].path
}

// valueDone 在一个字段值或数组元素完成后推进容器状态：对象转入等待下一个
// 字段名，数组推进元素下标。
func (l *jsonLocation) valueDone() {
	if len(l.stack) == 0 {
		return
	}
	top := l.stack[len(l.stack)-1]
	if top.object {
		top.expectKey = true
	} else {
		top.nextIndex++
	}
}

// atKey 报告栈顶是否处于“等待字段名”状态。
func (l *jsonLocation) atKey() bool {
	if len(l.stack) == 0 {
		return false
	}
	top := l.stack[len(l.stack)-1]
	return top.object && top.expectKey
}

// fieldName 登记刚扫到的字段名并转入等待字段值状态，返回所属对象的路径。
// decodedOK 为 false 时（字节扫描遇到解码失败的字段名）保留原字段名，具体
// 语法错误留给后续解码报告。
func (l *jsonLocation) fieldName(decoded string, decodedOK bool) string {
	top := l.stack[len(l.stack)-1]
	if decodedOK {
		top.lastKey = decoded
	}
	top.expectKey = false
	return top.path
}

// rejectDuplicateFields 按文件原文顺序扫描 JSON 令牌流，拒绝任何在同一对象内
// 重复声明的字段名。字段名按解码后的字符串比较（"enabled" 与 "énabled"
// 同名），大小写不同仍是不同字段，名字中的空白不裁剪。检查覆盖所有对象，
// 包括附加字段里嵌套的对象与数组中的对象；数组元素不属于这一检查。label 是
// 根位置前缀（"config" 或 "context"）。JSON 语法错误不在此报告，留给解码。
func rejectDuplicateFields(raw []byte, label string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	loc := &jsonLocation{label: label}
	// 各对象容器已见字段名集合，以容器路径为键（同一文档内对象路径唯一）。
	seen := map[string]map[string]struct{}{}
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF 或语法错误；语法错误由后续解码报告。
			break
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				loc.openContainer(t == '{')
			case '}', ']':
				loc.closeContainer()
			}
		case string:
			if loc.atKey() {
				containerPath := loc.fieldName(t, true)
				keys := seen[containerPath]
				if keys == nil {
					keys = map[string]struct{}{}
					seen[containerPath] = keys
				}
				if _, dup := keys[t]; dup {
					return fmt.Errorf("%s.%s: duplicate field %q", containerPath, t, t)
				}
				keys[t] = struct{}{}
			} else {
				loc.valueDone()
			}
		default:
			loc.valueDone()
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
// 的一处。label 是根位置前缀（"config" 或 "context"）。JSON 语法错误不在此
// 报告，留给解码。
func rejectInvalidUnicodeEscapes(raw []byte, label string) error {
	loc := &jsonLocation{label: label}
	i := 0
	for i < len(raw) {
		c := raw[i]
		switch {
		case c == '{' || c == '[':
			loc.openContainer(c == '{')
			i++
		case c == '}' || c == ']':
			loc.closeContainer()
			i++
		case c == '"':
			end := jsonStringEnd(raw, i)
			contentEnd := end
			if contentEnd > i+1 && raw[contentEnd-1] == '"' {
				contentEnd-- // 去掉右引号；未终止的字符串留给解码阶段报告
			}
			content := raw[i+1 : contentEnd]
			if bad := findBadUnicodeEscape(content); bad != nil {
				if loc.atKey() {
					return fmt.Errorf("%s: field name %q contains invalid Unicode escape %s: Unicode escape does not form a complete character (%s)",
						loc.containerPath(), string(content), bad.escape, bad.detail)
				}
				return fmt.Errorf("%s: invalid Unicode escape %s: Unicode escape does not form a complete character (%s)",
					loc.nextValuePath(), bad.escape, bad.detail)
			}
			if loc.atKey() {
				var key string
				// 转义已校验合法，解码用于定位路径；失败说明是语法错误，留给解码。
				err := json.Unmarshal(raw[i:end], &key)
				loc.fieldName(key, err == nil)
			} else {
				loc.valueDone()
			}
			i = end
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ':' || c == ',':
			i++
		default:
			// 数字与 true/false/null 等标量：跳过至下一个结构字符，按一个值计。
			for i < len(raw) && !isJSONStructural(raw[i]) {
				i++
			}
			loc.valueDone()
		}
	}
	return nil
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
		return nil, fmt.Errorf("%s.%s: field is required", loc, name)
	}
	if v == nil {
		return nil, fmt.Errorf("%s.%s: must not be null", loc, name)
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
		return nil, fmt.Errorf("%s.%s: must be an array", loc, name)
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
		return "", fmt.Errorf("%s.%s: must be a string", loc, name)
	}
	if s == "" {
		return "", fmt.Errorf("%s.%s: must not be empty", loc, name)
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
		return false, fmt.Errorf("%s.%s: must be a boolean", loc, name)
	}
	return b, nil
}
