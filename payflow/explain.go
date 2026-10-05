package payflow

import (
	"bytes"
	"encoding/json"
)

// ConditionExplanation 是解释模式下单个条件的判断记录：属性名、比较方式、
// 配置中的比较值（eq 为字符串，in 为按配置次序保留的字符串数组）、上下文
// 实际值以及该条件是否成立。
//
// Actual 为 nil 表示上下文里没有这个属性（缺失）；属性显式给出空字符串时
// Actual 指向 ""，二者在 JSON 中分别是 null 与 ""，不会被混为一谈。
type ConditionExplanation struct {
	Attribute string  `json:"attribute"`
	Op        string  `json:"op"`
	Expected  any     `json:"expected"`
	Actual    *string `json:"actual"`
	Matched   bool    `json:"matched"`
}

// RuleExplanation 是解释模式下一条规则的判断记录：规则编号、该规则是否
// 全部条件成立，以及每个条件各自的判断结果。记录严格按配置数组次序排列。
type RuleExplanation struct {
	RuleID     string                 `json:"ruleId"`
	Matched    bool                   `json:"matched"`
	Conditions []ConditionExplanation `json:"conditions"`
}

// Explanation 承载求值过程中实际考虑过的规则。开关关闭时不考虑任何规则，
// Rules 为空数组；启用的开关按配置次序记录，直到（并包含）第一条全部条件
// 成立的规则为止——首条命中规则即使返回 false 也立即定案，其后的规则不出
// 现在记录中。
type Explanation struct {
	Rules []RuleExplanation `json:"rules"`
}

// ExplainedResult 是解释模式的输出：在四字段结果（key/value/reason/
// ruleId）之外附带 explanation。四字段与普通求值逐字段一致。
type ExplainedResult struct {
	EvalResult
	Explanation Explanation `json:"explanation"`
}

// EvaluateWithExplanation 的求值规则与 Evaluate 完全相同，但同时给出与
// 最终结果严格对应的判断记录：关闭固定 false 且规则记录为空；否则按配置
// 次序检查规则，第一条所有条件成立的规则（无论其 value 是 true 还是
// false）决定结果并停止；所有规则都不成立时记录全部规则并采用默认值。
func (f *Flag) EvaluateWithExplanation(ctx *Context) ExplainedResult {
	result, rules := f.evaluateWithRules(ctx)
	return ExplainedResult{EvalResult: result, Explanation: Explanation{Rules: rules}}
}

// evaluateWithRules 同时返回求值结果与按检查顺序累积的规则记录。
// 规则记录始终是非 nil 切片（无规则可考虑时序列化为 []，而不是 null）。
func (f *Flag) evaluateWithRules(ctx *Context) (EvalResult, []RuleExplanation) {
	records := make([]RuleExplanation, 0, len(f.Rules))
	if !f.Enabled {
		// 关闭开关固定 false：不采用默认值，也不判断任何规则。
		return EvalResult{Key: f.Key, Value: false, Reason: EvalDisabled, RuleID: nil}, records
	}
	for i := range f.Rules {
		rule := &f.Rules[i]
		record := RuleExplanation{
			RuleID:     rule.ID,
			Conditions: make([]ConditionExplanation, 0, len(rule.Conditions)),
		}
		allMatch := true
		for j := range rule.Conditions {
			cond := rule.Conditions[j].explain(ctx)
			if !cond.Matched {
				allMatch = false
			}
			record.Conditions = append(record.Conditions, cond)
		}
		record.Matched = allMatch
		records = append(records, record)
		if allMatch {
			// 第一条全部条件成立的规则立即定案——即使 value=false 也停止，
			// 后续规则既不决定结果也不进入解释。
			id := rule.ID
			return EvalResult{Key: f.Key, Value: rule.Value, Reason: EvalRule, RuleID: &id}, records
		}
	}
	return EvalResult{Key: f.Key, Value: f.Default, Reason: EvalDefault, RuleID: nil}, records
}

// explain 给出单个条件在给定上下文上的完整判断：Expected 取配置中解码后的
// 比较值（eq 是字符串；in 是保留空串、重复成员与次序的列表副本），Actual
// 取上下文解码后的实际值，缺失时保持 nil。字符串不裁剪空白、不合并大小写。
func (c Condition) explain(ctx *Context) ConditionExplanation {
	ce := ConditionExplanation{Attribute: c.Attribute, Op: c.Op}
	switch c.Op {
	case "eq":
		ce.Expected = c.strVal
	case "in":
		// 复制一份，保证解释记录与规则内部数据互不影响；空串与重复成员按原样保留。
		list := make([]string, len(c.inVal))
		copy(list, c.inVal)
		ce.Expected = list
	}
	v, present := ctx.Get(c.Attribute)
	if !present {
		// 属性缺失：Actual 留 nil（JSON null），与显式空字符串明确区分。
		return ce
	}
	ce.Actual = &v
	ce.Matched = c.matches(ctx)
	return ce
}

// MarshalExplained renders an explained result as a single compact JSON object,
// 与 MarshalResult 同样不转义 HTML 字符、不在末尾保留换行。
func MarshalExplained(r ExplainedResult) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
