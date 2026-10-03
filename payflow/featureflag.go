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
	if err := validateUnicodeEscapes(raw, "config"); err != nil {
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
	if err := validateUnicodeEscapes(raw, "context"); err != nil {
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

// --- strict parsing helpers -------------------------------------------------

// validateUnicodeEscapes 按原文顺序扫描 JSON 文本里的每个字符串字面量（字段名与
// 字符串值），拒绝无法组成完整字符的 Unicode 转义：孤立的高代理项、孤立的低代理项，
// 以及不完整或顺序错误的代理项对。encoding/json 会把这类转义静默替换成 U+FFFD，
// 使 "\uD800" 与真正的 "�" 无法区分、不同的坏字段名解码后撞名被误报重复，因此
// 必须在解码与重复字段检查之前基于原文检查。正确配对的转义、直接写出的 "�" 与
// 等价的 "\uFFFD" 转义、以及 "\\uD800" 这类反斜杠转义后的普通文本都合法。
// label 是根对象的位置前缀（"config" 或 "context"）。JSON 语法错误（如
// \u 后不足 4 位十六进制）不在此报告，留给 Unmarshal。
func validateUnicodeEscapes(raw []byte, label string) error {
	type frame struct {
		path      string
		object    bool
		expectKey bool
		nextIndex int
		lastKey   string
	}
	var stack []*frame
	childPath := func() string {
		if len(stack) == 0 {
			return label
		}
		top := stack[len(stack)-1]
		if top.object {
			return top.path + "." + top.lastKey
		}
		return top.path + "[" + strconv.Itoa(top.nextIndex) + "]"
	}
	i := 0
	for i < len(raw) {
		switch c := raw[i]; c {
		case '{', '[':
			stack = append(stack, &frame{path: childPath(), object: c == '{', expectKey: true})
			i++
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			i++
		case ',':
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				if top.object {
					top.expectKey = true
				} else {
					top.nextIndex++
				}
			}
			i++
		case '"':
			end, bad := scanStringEscapes(raw, i)
			if end < 0 {
				// 字符串未闭合：语法错误留给 Unmarshal 报告。
				return nil
			}
			isKey := len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectKey
			if bad != "" {
				if isKey {
					return fmt.Errorf("%s: field name %s: invalid Unicode escape %s: escape does not form a complete character",
						stack[len(stack)-1].path, quoteRawFieldName(raw[i+1:end-1]), bad)
				}
				return fmt.Errorf("%s: invalid Unicode escape %s: escape does not form a complete character", childPath(), bad)
			}
			if isKey {
				top := stack[len(stack)-1]
				var name string
				if err := json.Unmarshal(raw[i:end], &name); err == nil {
					top.lastKey = name
				}
				top.expectKey = false
			}
			i = end
		default:
			i++
		}
	}
	return nil
}

// scanStringEscapes 从 raw[start]（必须是 '"'）扫描一个字符串字面量，返回闭引号
// 之后的位置与第一个无法组成完整字符的 Unicode 转义原文（无问题为空串）。发现坏
// 转义后仍扫描到字符串结束，保证 end 有效。字符串未闭合时 end=-1，语法问题留给
// json.Unmarshal 报告。
func scanStringEscapes(raw []byte, start int) (end int, bad string) {
	j := start + 1
	for j < len(raw) {
		switch raw[j] {
		case '"':
			return j + 1, bad
		case '\\':
			if j+1 >= len(raw) {
				return -1, bad
			}
			if raw[j+1] != 'u' {
				// 包括 \\：反斜杠转义后的 uD800 只是普通文本。
				j += 2
				continue
			}
			r, ok := hex4(raw, j+2)
			if !ok {
				// \u 后不是 4 位十六进制：JSON 语法错误，留给 Unmarshal。
				j += 2
				continue
			}
			switch {
			case r >= 0xD800 && r <= 0xDBFF:
				// 高代理项必须紧跟一个 \uDC00–\uDFFF 的低代理项转义。
				if j+12 <= len(raw) && raw[j+6] == '\\' && raw[j+7] == 'u' {
					if r2, ok2 := hex4(raw, j+8); ok2 && r2 >= 0xDC00 && r2 <= 0xDFFF {
						j += 12
						continue
					}
				}
				if bad == "" {
					bad = string(raw[j : j+6])
				}
			case r >= 0xDC00 && r <= 0xDFFF:
				if bad == "" {
					bad = string(raw[j : j+6])
				}
			}
			j += 6
		default:
			j++
		}
	}
	return -1, bad
}

// hex4 读取 4 位十六进制数字；不足 4 位或含非十六进制字符时 ok=false。
func hex4(raw []byte, i int) (r rune, ok bool) {
	if i+4 > len(raw) {
		return 0, false
	}
	var v uint32
	for k := 0; k < 4; k++ {
		c := raw[i+k]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		v = v<<4 | uint32(d)
	}
	return rune(v), true
}

// quoteRawFieldName 以原文形式引用字段名（保留 \u 转义以便辨认），过长时截断。
func quoteRawFieldName(name []byte) string {
	const max = 80
	s := string(name)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return strconv.Quote(s)
}

// rejectDuplicateFields 按文件原文顺序扫描 JSON 令牌流，拒绝任何在同一对象内
// 重复声明的字段名。字段名按解码后的字符串比较（"enabled" 与 "énabled"
// 同名），大小写不同仍是不同字段，名字中的空白不裁剪。检查覆盖所有对象，
// 包括附加字段里嵌套的对象；数组元素不属于这一检查。label 是根对象的位置
// 前缀（"config" 或 "context"）。JSON 语法错误不在此报告，留给 Unmarshal。
func rejectDuplicateFields(raw []byte, label string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	type frame struct {
		path      string
		object    bool
		keys      map[string]struct{}
		lastKey   string
		expectKey bool
		nextIndex int
	}
	var stack []*frame
	// valueDone 在栈顶容器完成一个值（对象的一个字段值或数组的一个元素）后调用。
	valueDone := func() {
		if len(stack) == 0 {
			return
		}
		top := stack[len(stack)-1]
		if top.object {
			top.expectKey = true
		} else {
			top.nextIndex++
		}
	}
	childPath := func() string {
		top := stack[len(stack)-1]
		if top.object {
			return top.path + "." + top.lastKey
		}
		return top.path + "[" + strconv.Itoa(top.nextIndex) + "]"
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF 或语法错误；语法错误由后续 Unmarshal 报告。
			break
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				path := label
				if len(stack) > 0 {
					path = childPath()
				}
				f := &frame{path: path, object: t == '{', expectKey: true}
				if f.object {
					f.keys = make(map[string]struct{})
				}
				stack = append(stack, f)
			case '}', ']':
				stack = stack[:len(stack)-1]
				valueDone()
			}
		case string:
			if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectKey {
				top := stack[len(stack)-1]
				if _, dup := top.keys[t]; dup {
					return fmt.Errorf("%s.%s: duplicate field %q", top.path, t, t)
				}
				top.keys[t] = struct{}{}
				top.lastKey = t
				top.expectKey = false
			} else {
				valueDone()
			}
		default:
			valueDone()
		}
	}
	return nil
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
