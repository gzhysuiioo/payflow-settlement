package payflow

// 本文件集中承担配置与上下文共用的 JSON 文档格式校验职责：UTF-8 编码、
// 不能组成完整字符的 Unicode 转义、重复字段、JSON 语法与单文档边界、顶层
// 必须是一个对象。两类输入走同一条校验流水线，修改任何一种格式规则时
// 只需改这一处，配置与上下文的接受结果与错误定位不会分叉。格式检查全部
// 通过后，配置中的开关定义与上下文中的属性值仍由 featureflag.go 按各自
// 的业务规则处理。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// validateJSONDocument 对一份 JSON 文档依次执行全部格式检查，顺序固定为：
// UTF-8 编码 → 非法 Unicode 转义 → 重复字段 → JSON 语法（含单文档边界）→
// 顶层必须是对象。前一项未通过时后一项不会报告，保证非法 UTF-8 不被后续
// 错误遮住、转义错误优先于重复字段、格式问题全部先于业务字段问题。
// label 是根对象的位置前缀（"config" 或 "context"），错误信息以它指明是
// 哪份输入。全部通过时返回严格解码后的顶层对象，供调用方做业务字段校验。
func validateJSONDocument(raw []byte, label string) (map[string]any, error) {
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

// --- 字段位置定位 -------------------------------------------------------------

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

// --- 原文遍历的层级与位置状态 ---------------------------------------------------

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

// --- 重复字段检查 ---------------------------------------------------------------

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

// --- Unicode 转义检查 -----------------------------------------------------------

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
