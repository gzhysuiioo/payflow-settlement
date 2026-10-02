package payflow

import (
	"encoding/json"
	"fmt"
)

// FlagResult is the outcome of evaluating one feature flag.
type FlagResult struct {
	Key    string  `json:"key"`
	Value  bool    `json:"value"`
	Reason string  `json:"reason"`
	RuleID *string `json:"ruleId"`
}

type featureFlag struct {
	Key     string
	Enabled bool
	Default bool
	Rules   []flagRule
}

type flagRule struct {
	ID         string
	Value      bool
	Conditions []flagCondition
}

type flagCondition struct {
	Attribute string
	Op        string
	StrVal    string
	ListVal   []string
}

// EvaluateFlags validates both files in full, then evaluates the named flag.
// Validation runs before lookup, so even unselected or disabled flags must be
// well-formed. On success it returns the result; on any validation or lookup
// failure it returns an error whose message identifies the offending field.
func EvaluateFlags(configData, contextData []byte, key string) (FlagResult, error) {
	flags, err := parseFlagsConfig(configData)
	if err != nil {
		return FlagResult{}, err
	}
	ctx, err := parseFlagContext(contextData)
	if err != nil {
		return FlagResult{}, err
	}
	flag, ok := flags[key]
	if !ok {
		return FlagResult{}, fmt.Errorf("找不到开关 %q", key)
	}
	return evaluateFlag(flag, ctx), nil
}

func evaluateFlag(flag featureFlag, ctx map[string]string) FlagResult {
	if !flag.Enabled {
		return FlagResult{Key: flag.Key, Value: false, Reason: "disabled", RuleID: nil}
	}
	for i := range flag.Rules {
		rule := flag.Rules[i]
		if conditionsMatch(rule.Conditions, ctx) {
			id := rule.ID
			return FlagResult{Key: flag.Key, Value: rule.Value, Reason: "rule", RuleID: &id}
		}
	}
	return FlagResult{Key: flag.Key, Value: flag.Default, Reason: "default", RuleID: nil}
}

func conditionsMatch(conds []flagCondition, ctx map[string]string) bool {
	for i := range conds {
		cond := conds[i]
		actual, ok := ctx[cond.Attribute]
		if !ok {
			return false
		}
		switch cond.Op {
		case "eq":
			if actual != cond.StrVal {
				return false
			}
		case "in":
			if !containsString(cond.ListVal, actual) {
				return false
			}
		}
	}
	return true
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// parseFlagsConfig strictly validates the config JSON and indexes flags by key.
func parseFlagsConfig(data []byte) (map[string]featureFlag, error) {
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("配置文件 JSON 无效: %v", err)
	}
	if _, ok := probe.(map[string]any); !ok {
		return nil, fmt.Errorf("配置必须是 JSON 对象")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("配置文件 JSON 无效: %v", err)
	}
	rawFlags, ok := top["flags"]
	if !ok {
		return nil, fmt.Errorf("配置缺少 flags 字段")
	}
	var flagRaws *[]json.RawMessage
	if err := json.Unmarshal(rawFlags, &flagRaws); err != nil || flagRaws == nil {
		return nil, fmt.Errorf("flags: 必须是数组")
	}

	result := make(map[string]featureFlag, len(*flagRaws))
	for i, rawFlag := range *flagRaws {
		flagPath := fmt.Sprintf("flags[%d]", i)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(rawFlag, &obj); err != nil {
			return nil, fmt.Errorf("%s: 必须是 JSON 对象", flagPath)
		}
		if obj == nil {
			return nil, fmt.Errorf("%s: 必须是 JSON 对象", flagPath)
		}

		key, err := requiredNonEmptyString(obj, flagPath, "key")
		if err != nil {
			return nil, err
		}
		if _, dup := result[key]; dup {
			return nil, fmt.Errorf("%s.key: 开关键 %q 重复", flagPath, key)
		}
		enabled, err := requiredBool(obj, flagPath, "enabled")
		if err != nil {
			return nil, err
		}
		def, err := requiredBool(obj, flagPath, "default")
		if err != nil {
			return nil, err
		}
		rawRules, err := requiredRawArray(obj, flagPath, "rules")
		if err != nil {
			return nil, err
		}

		rules := make([]flagRule, 0, len(rawRules))
		seenRuleIDs := make(map[string]bool, len(rawRules))
		for j, rawRule := range rawRules {
			rulePath := fmt.Sprintf("%s.rules[%d]", flagPath, j)
			var ruleObj map[string]json.RawMessage
			if err := json.Unmarshal(rawRule, &ruleObj); err != nil {
				return nil, fmt.Errorf("%s: 必须是 JSON 对象", rulePath)
			}
			if ruleObj == nil {
				return nil, fmt.Errorf("%s: 必须是 JSON 对象", rulePath)
			}

			id, err := requiredNonEmptyString(ruleObj, rulePath, "id")
			if err != nil {
				return nil, err
			}
			if seenRuleIDs[id] {
				return nil, fmt.Errorf("%s.id: 规则 id %q 重复", rulePath, id)
			}
			seenRuleIDs[id] = true
			value, err := requiredBool(ruleObj, rulePath, "value")
			if err != nil {
				return nil, err
			}
			rawConds, err := requiredRawArray(ruleObj, rulePath, "conditions")
			if err != nil {
				return nil, err
			}
			if len(rawConds) == 0 {
				return nil, fmt.Errorf("%s.conditions: 条件组不能为空", rulePath)
			}

			conds := make([]flagCondition, 0, len(rawConds))
			for k, rawCond := range rawConds {
				condPath := fmt.Sprintf("%s.conditions[%d]", rulePath, k)
				var condObj map[string]json.RawMessage
				if err := json.Unmarshal(rawCond, &condObj); err != nil {
					return nil, fmt.Errorf("%s: 必须是 JSON 对象", condPath)
				}
				if condObj == nil {
					return nil, fmt.Errorf("%s: 必须是 JSON 对象", condPath)
				}

				attr, err := requiredNonEmptyString(condObj, condPath, "attribute")
				if err != nil {
					return nil, err
				}
				op, err := requiredNonEmptyString(condObj, condPath, "op")
				if err != nil {
					return nil, err
				}
				if op != "eq" && op != "in" {
					return nil, fmt.Errorf("%s.op: 未知运算符 %q（仅支持 eq、in）", condPath, op)
				}
				rawValue, ok := condObj["value"]
				if !ok {
					return nil, fmt.Errorf("%s.value: 字段缺失", condPath)
				}
				cond := flagCondition{Attribute: attr, Op: op}
				if op == "eq" {
					if err := json.Unmarshal(rawValue, &cond.StrVal); err != nil {
						return nil, fmt.Errorf("%s.value: eq 时必须是字符串", condPath)
					}
				} else {
					var list *[]json.RawMessage
					if err := json.Unmarshal(rawValue, &list); err != nil || list == nil {
						return nil, fmt.Errorf("%s.value: in 时必须是非空字符串数组", condPath)
					}
					if len(*list) == 0 {
						return nil, fmt.Errorf("%s.value: in 时必须是非空字符串数组", condPath)
					}
					cond.ListVal = make([]string, 0, len(*list))
					for m, rawItem := range *list {
						var item string
						if err := json.Unmarshal(rawItem, &item); err != nil {
							return nil, fmt.Errorf("%s.value[%d]: 必须是字符串", condPath, m)
						}
						cond.ListVal = append(cond.ListVal, item)
					}
				}
				conds = append(conds, cond)
			}
			rules = append(rules, flagRule{ID: id, Value: value, Conditions: conds})
		}
		result[key] = featureFlag{Key: key, Enabled: enabled, Default: def, Rules: rules}
	}
	return result, nil
}

// parseFlagContext strictly validates the context JSON object.
func parseFlagContext(data []byte) (map[string]string, error) {
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("上下文文件 JSON 无效: %v", err)
	}
	if _, ok := probe.(map[string]any); !ok {
		return nil, fmt.Errorf("上下文必须是 JSON 对象")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("上下文文件 JSON 无效: %v", err)
	}
	ctx := make(map[string]string, len(obj))
	for attr, raw := range obj {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("上下文属性 %q 的值必须是字符串", attr)
		}
		ctx[attr] = value
	}
	return ctx, nil
}

func requiredNonEmptyString(obj map[string]json.RawMessage, path, field string) (string, error) {
	raw, ok := obj[field]
	if !ok {
		return "", fmt.Errorf("%s.%s: 字段缺失", path, field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s.%s: 必须是字符串", path, field)
	}
	if s == "" {
		return "", fmt.Errorf("%s.%s: 必须是非空字符串", path, field)
	}
	return s, nil
}

func requiredBool(obj map[string]json.RawMessage, path, field string) (bool, error) {
	raw, ok := obj[field]
	if !ok {
		return false, fmt.Errorf("%s.%s: 字段缺失", path, field)
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("%s.%s: 必须是布尔值", path, field)
	}
	return b, nil
}

func requiredRawArray(obj map[string]json.RawMessage, path, field string) ([]json.RawMessage, error) {
	raw, ok := obj[field]
	if !ok {
		return nil, fmt.Errorf("%s.%s: 字段缺失", path, field)
	}
	var arr *[]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || arr == nil {
		return nil, fmt.Errorf("%s.%s: 必须是数组", path, field)
	}
	return *arr, nil
}
