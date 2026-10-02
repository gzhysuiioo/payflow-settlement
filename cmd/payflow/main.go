// Command payflow is the 链上支付与结算编排平台 entry point.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println("payflow 0.1.0")
	case "evaluate":
		os.Exit(runEvaluate(os.Args[2:], os.Stdout, os.Stderr))
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage(os.Stderr)
		os.Exit(2)
	}
}

// runEvaluate validates both files in full and evaluates one feature flag.
// It returns the process exit code; success writes exactly one JSON object to
// stdout, while every failure keeps stdout empty and explains the cause on
// stderr.
func runEvaluate(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "用法: payflow evaluate <配置文件> <开关键> <上下文文件>")
		return 2
	}
	configPath, key, contextPath := args[0], args[1], args[2]

	configData, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "无法读取配置文件 %q: %v\n", configPath, err)
		return 1
	}
	contextData, err := os.ReadFile(contextPath)
	if err != nil {
		fmt.Fprintf(stderr, "无法读取上下文文件 %q: %v\n", contextPath, err)
		return 1
	}

	result, err := payflow.EvaluateFlags(configData, contextData, key)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	out, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(stderr, "结果编码失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `payflow — 链上支付与结算编排平台

用法:
  payflow demo
  payflow version
  payflow evaluate <配置文件> <开关键> <上下文文件>
  payflow help

evaluate:
  离线求值指定的特性开关。配置文件与上下文文件均为 UTF-8 JSON。
  成功时向标准输出写入一个 JSON 对象:
    {"key":"<开关键>","value":<布尔>,"reason":"<disabled|rule|default>","ruleId":<字符串或null>}
  失败时以非零状态退出，标准输出为空，错误原因写入标准错误。

配置文件格式 (UTF-8 JSON):
  {
    "flags": [
      {
        "key": "非空字符串",
        "enabled": true,
        "default": false,
        "rules": [
          {
            "id": "非空字符串",
            "value": true,
            "conditions": [
              {"attribute": "非空属性名", "op": "eq", "value": "字符串"},
              {"attribute": "非空属性名", "op": "in", "value": ["字符串", "..."]}
            ]
          }
        ]
      }
    ]
  }

上下文文件格式 (UTF-8 JSON):
  属性名到字符串的 JSON 对象，允许为空对象，例如 {"country": "CN", "plan": "pro"}。

求值规则:
  开关未启用 (enabled=false) 时固定返回 false，reason=disabled，不使用默认值或规则。
  启用后按顺序寻找首条全部条件成立的规则并采用其 value，reason=rule，ruleId 为该规则 id；
  没有匹配规则时采用 default，reason=default，ruleId 为 null。
  eq 比较字符串是否相等；in 判断上下文值是否出现在 value 数组中；
  属性缺失则对应条件不成立。比较区分大小写，不裁剪空白或转换类型。
`)
}

func runDemo() {
	intents := []payflow.Intent{
		{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Amount: 1500, Asset: "usdc", Nonce: 1, State: "pending"},
		{ID: "pay-2", Account: "aa-1", Paymaster: "pm-1", Amount: 900, Asset: "usdc", Nonce: 2, State: "pending"},
	}
	spent := map[string]bool{}
	var settlements []payflow.Settlement
	for _, intent := range intents {
		settlement := payflow.Execute(intent, 30, spent, 2000)
		settlements = append(settlements, settlement)
		fmt.Printf("intent=%s status=%s charged=%d reason=%s\n", settlement.Intent, settlement.Status, settlement.Charged, settlement.Reason)
	}
	duplicate := payflow.Execute(intents[0], 30, spent, 2000)
	fmt.Printf("duplicate of %s status=%s reason=%s\n", duplicate.Intent, duplicate.Status, duplicate.Reason)
	fmt.Println("reconciliation gaps:", payflow.Reconcile(intents, settlements))
}
