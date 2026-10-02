package payflow

import (
	"encoding/json"
	"strings"
	"testing"
)

const validConfig = `{
  "flags": [
    {
      "key": "disabled-flag",
      "enabled": false,
      "default": true,
      "rules": [
        {
          "id": "r1",
          "value": false,
          "conditions": [
            {"attribute": "country", "op": "eq", "value": "CN"}
          ]
        }
      ]
    },
    {
      "key": "eq-flag",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "cn-pro",
          "value": true,
          "conditions": [
            {"attribute": "country", "op": "eq", "value": "CN"},
            {"attribute": "plan", "op": "eq", "value": "pro"}
          ]
        }
      ]
    },
    {
      "key": "in-flag",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "sea",
          "value": true,
          "conditions": [
            {"attribute": "country", "op": "in", "value": ["CN", "SG", "JP"]}
          ]
        }
      ]
    },
    {
      "key": "empty-rules",
      "enabled": true,
      "default": true,
      "rules": []
    }
  ]
}`

func strptr(s string) *string { return &s }

func TestEvaluateFlags(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		context string
		key     string
		want    FlagResult
	}{
		{
			name:    "disabled flag always false",
			config:  validConfig,
			context: `{"country":"CN","plan":"pro"}`,
			key:     "disabled-flag",
			want:    FlagResult{Key: "disabled-flag", Value: false, Reason: "disabled", RuleID: nil},
		},
		{
			name:    "eq rule matches all conditions",
			config:  validConfig,
			context: `{"country":"CN","plan":"pro"}`,
			key:     "eq-flag",
			want:    FlagResult{Key: "eq-flag", Value: true, Reason: "rule", RuleID: strptr("cn-pro")},
		},
		{
			name:    "eq rule one condition fails falls to default",
			config:  validConfig,
			context: `{"country":"CN","plan":"free"}`,
			key:     "eq-flag",
			want:    FlagResult{Key: "eq-flag", Value: false, Reason: "default", RuleID: nil},
		},
		{
			name:    "missing attribute fails condition",
			config:  validConfig,
			context: `{"country":"CN"}`,
			key:     "eq-flag",
			want:    FlagResult{Key: "eq-flag", Value: false, Reason: "default", RuleID: nil},
		},
		{
			name:    "in rule matches",
			config:  validConfig,
			context: `{"country":"JP"}`,
			key:     "in-flag",
			want:    FlagResult{Key: "in-flag", Value: true, Reason: "rule", RuleID: strptr("sea")},
		},
		{
			name:    "in rule no match falls to default",
			config:  validConfig,
			context: `{"country":"US"}`,
			key:     "in-flag",
			want:    FlagResult{Key: "in-flag", Value: false, Reason: "default", RuleID: nil},
		},
		{
			name:    "empty rules uses default true",
			config:  validConfig,
			context: `{}`,
			key:     "empty-rules",
			want:    FlagResult{Key: "empty-rules", Value: true, Reason: "default", RuleID: nil},
		},
		{
			name:    "empty context object",
			config:  validConfig,
			context: `{}`,
			key:     "in-flag",
			want:    FlagResult{Key: "in-flag", Value: false, Reason: "default", RuleID: nil},
		},
		{
			name: "first matching rule wins",
			config: `{
				"flags": [{
					"key": "f", "enabled": true, "default": false,
					"rules": [
						{"id": "first", "value": true, "conditions": [
							{"attribute": "a", "op": "eq", "value": "x"}
						]},
						{"id": "second", "value": false, "conditions": [
							{"attribute": "a", "op": "eq", "value": "x"}
						]}
					]
				}]
			}`,
			context: `{"a":"x"}`,
			key:     "f",
			want:    FlagResult{Key: "f", Value: true, Reason: "rule", RuleID: strptr("first")},
		},
		{
			name: "case sensitive comparison",
			config: `{
				"flags": [{
					"key": "f", "enabled": true, "default": false,
					"rules": [
						{"id": "r", "value": true, "conditions": [
							{"attribute": "a", "op": "eq", "value": "X"}
						]}
					]
				}]
			}`,
			context: `{"a":"x"}`,
			key:     "f",
			want:    FlagResult{Key: "f", Value: false, Reason: "default", RuleID: nil},
		},
		{
			name: "empty string context value is valid and matches",
			config: `{
				"flags": [{
					"key": "f", "enabled": true, "default": false,
					"rules": [
						{"id": "r", "value": true, "conditions": [
							{"attribute": "a", "op": "eq", "value": ""}
						]}
					]
				}]
			}`,
			context: `{"a":""}`,
			key:     "f",
			want:    FlagResult{Key: "f", Value: true, Reason: "rule", RuleID: strptr("r")},
		},
		{
			name: "whitespace not trimmed",
			config: `{
				"flags": [{
					"key": "f", "enabled": true, "default": false,
					"rules": [
						{"id": "r", "value": true, "conditions": [
							{"attribute": "a", "op": "eq", "value": " x "}
						]}
					]
				}]
			}`,
			context: `{"a":"x"}`,
			key:     "f",
			want:    FlagResult{Key: "f", Value: false, Reason: "default", RuleID: nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvaluateFlags([]byte(tt.config), []byte(tt.context), tt.key)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Key != tt.want.Key || got.Value != tt.want.Value || got.Reason != tt.want.Reason {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if (got.RuleID == nil) != (tt.want.RuleID == nil) {
				t.Fatalf("ruleId presence mismatch: got %v, want %v", got.RuleID, tt.want.RuleID)
			}
			if got.RuleID != nil && *got.RuleID != *tt.want.RuleID {
				t.Fatalf("ruleId got %q, want %q", *got.RuleID, *tt.want.RuleID)
			}
		})
	}
}

func TestEvaluateFlagsDeterministic(t *testing.T) {
	first, err := EvaluateFlags([]byte(validConfig), []byte(`{"country":"CN","plan":"pro"}`), "eq-flag")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		got, err := EvaluateFlags([]byte(validConfig), []byte(`{"country":"CN","plan":"pro"}`), "eq-flag")
		if err != nil {
			t.Fatal(err)
		}
		if got.Key != first.Key || got.Value != first.Value || got.Reason != first.Reason {
			t.Fatalf("run %d: got %+v, want %+v", i, got, first)
		}
		if (got.RuleID == nil) != (first.RuleID == nil) || (got.RuleID != nil && *got.RuleID != *first.RuleID) {
			t.Fatalf("run %d: ruleId got %v, want %v", i, got.RuleID, first.RuleID)
		}
	}
}

func TestFlagResultJSONShape(t *testing.T) {
	got, err := EvaluateFlags([]byte(validConfig), []byte(`{"country":"CN"}`), "disabled-flag")
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"disabled-flag","value":false,"reason":"disabled","ruleId":null}`
	if string(out) != want {
		t.Fatalf("json got %s, want %s", out, want)
	}
}

func TestEvaluateFlagsErrors(t *testing.T) {
	tests := []struct {
		name        string
		config      string
		context     string
		key         string
		wantErrSub  string
		wantContext bool // error expected even though flag lookup would succeed
	}{
		{
			name:       "invalid config json",
			config:     `{not json`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "配置文件 JSON 无效",
		},
		{
			name:       "config not an object",
			config:     `[1,2]`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "配置必须是 JSON 对象",
		},
		{
			name:       "missing flags field",
			config:     `{}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "缺少 flags 字段",
		},
		{
			name:       "flags not array",
			config:     `{"flags":{}}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags: 必须是数组",
		},
		{
			name:       "flag missing key",
			config:     `{"flags":[{"enabled":true,"default":false,"rules":[]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].key: 字段缺失",
		},
		{
			name:       "flag empty key",
			config:     `{"flags":[{"key":"","enabled":true,"default":false,"rules":[]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].key: 必须是非空字符串",
		},
		{
			name:       "flag key wrong type",
			config:     `{"flags":[{"key":1,"enabled":true,"default":false,"rules":[]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].key: 必须是字符串",
		},
		{
			name:       "missing enabled field",
			config:     `{"flags":[{"key":"f","default":false,"rules":[]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].enabled: 字段缺失",
		},
		{
			name:       "enabled wrong type",
			config:     `{"flags":[{"key":"f","enabled":"yes","default":false,"rules":[]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].enabled: 必须是布尔值",
		},
		{
			name:       "missing default field",
			config:     `{"flags":[{"key":"f","enabled":true,"rules":[]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].default: 字段缺失",
		},
		{
			name:       "missing rules field",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules: 字段缺失",
		},
		{
			name:       "rules null",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":null}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules: 必须是数组",
		},
		{
			name:       "duplicate flag keys",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[]},{"key":"f","enabled":false,"default":true,"rules":[]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: `flags[1].key: 开关键 "f" 重复`,
		},
		{
			name:       "rule missing id",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules[0].id: 字段缺失",
		},
		{
			name:       "rule empty id",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules[0].id: 必须是非空字符串",
		},
		{
			name:       "rule missing value",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules[0].value: 字段缺失",
		},
		{
			name:       "rule value wrong type",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":"true","conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules[0].value: 必须是布尔值",
		},
		{
			name:       "duplicate rule ids within flag",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},{"id":"r","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: `flags[0].rules[1].id: 规则 id "r" 重复`,
		},
		{
			name:       "empty conditions",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules[0].conditions: 条件组不能为空",
		},
		{
			name:       "missing conditions field",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules[0].conditions: 字段缺失",
		},
		{
			name:       "condition missing attribute",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"op":"eq","value":"x"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "flags[0].rules[0].conditions[0].attribute: 字段缺失",
		},
		{
			name:       "condition empty attribute",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"","op":"eq","value":"x"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "conditions[0].attribute: 必须是非空字符串",
		},
		{
			name:       "unknown operator",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"contains","value":"x"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: `conditions[0].op: 未知运算符 "contains"`,
		},
		{
			name:       "eq value not string",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":1}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "conditions[0].value: eq 时必须是字符串",
		},
		{
			name:       "in value empty array",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":[]}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "conditions[0].value: in 时必须是非空字符串数组",
		},
		{
			name:       "in value not array",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":"x"}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "conditions[0].value: in 时必须是非空字符串数组",
		},
		{
			name:       "in value element not string",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"in","value":["x",1]}]}]}]}`,
			context:    `{}`,
			key:        "f",
			wantErrSub: "conditions[0].value[1]: 必须是字符串",
		},
		{
			name:       "unselected disabled flag still validated",
			config:     `{"flags":[{"key":"other","enabled":false,"default":true,"rules":[{"id":"r","value":true,"conditions":[]}]}]}`,
			context:    `{}`,
			key:        "missing-key",
			wantErrSub: "条件组不能为空",
		},
		{
			name:       "flag not found",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[]}]}`,
			context:    `{}`,
			key:        "nope",
			wantErrSub: `找不到开关 "nope"`,
		},
		{
			name:       "invalid context json",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[]}]}`,
			context:    `{bad`,
			key:        "f",
			wantErrSub: "上下文文件 JSON 无效",
		},
		{
			name:       "context not object",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[]}]}`,
			context:    `["a"]`,
			key:        "f",
			wantErrSub: "上下文必须是 JSON 对象",
		},
		{
			name:       "context value not string",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[]}]}`,
			context:    `{"a":1}`,
			key:        "f",
			wantErrSub: `上下文属性 "a" 的值必须是字符串`,
		},
		{
			name:       "context validated even when flag missing",
			config:     `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[]}]}`,
			context:    `{"a":1}`,
			key:        "nope",
			wantErrSub: `上下文属性 "a" 的值必须是字符串`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EvaluateFlags([]byte(tt.config), []byte(tt.context), tt.key)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrSub)
			}
		})
	}
}
