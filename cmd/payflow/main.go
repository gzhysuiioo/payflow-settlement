// Command payflow is the 链上支付与结算编排平台 entry point.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches subcommands. evaluate is the offline feature-flag evaluator.
func run(args []string, stdout, stderr io.Writer) int {
	command := "demo"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "demo":
		return runDemo(stdout)
	case "version":
		fmt.Fprintln(stdout, "payflow 0.1.0")
		return 0
	case "evaluate":
		return runEvaluate(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		printUsage(stderr)
		return 2
	}
}

// runEvaluate handles evaluate <configPath> <flagKey> <contextPath>.
// 成功时 stdout 仅有一个 JSON 对象；任何输入/校验错误都非零退出，
// stdout 保持为空，具体原因写入 stderr。
func runEvaluate(argv []string, stdout, stderr io.Writer) int {
	if len(argv) != 3 {
		fmt.Fprintf(stderr, "evaluate: expected 3 arguments <config.json> <flag-key> <context.json>, got %d\n", len(argv))
		fmt.Fprintln(stderr, "usage: payflow evaluate <config.json> <flag-key> <context.json>")
		return 2
	}
	configPath, key, contextPath := argv[0], argv[1], argv[2]
	// 两个文件先完整校验：配置全量校验（含未被选中、已关闭的开关）通过后才求值。
	cfg, err := payflow.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, err := payflow.LoadContext(contextPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	flag := cfg.Find(key)
	if flag == nil {
		fmt.Fprintf(stderr, "evaluate: flag key %q not found in config %q\n", key, configPath)
		return 1
	}
	out, err := payflow.MarshalResult(flag.Evaluate(ctx))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, string(out))
	return 0
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: payflow [demo|version|evaluate|help]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "commands:")
	fmt.Fprintln(out, "  demo                                             run the settlement demo")
	fmt.Fprintln(out, "  version                                          print the payflow version")
	fmt.Fprintln(out, "  evaluate <config.json> <flag-key> <context.json>  evaluate one offline feature flag")
	fmt.Fprintln(out, "  help                                             show this help")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "evaluate reads two UTF-8 JSON files:")
	fmt.Fprintln(out, "  config:  {\"flags\":[{\"key\":\"f\",\"enabled\":true,\"default\":false,\"rules\":["+
		"{\"id\":\"r1\",\"value\":true,\"conditions\":[{\"attribute\":\"plan\",\"op\":\"eq\",\"value\":\"pro\"}]}]}]}")
	fmt.Fprintln(out, "  context: {\"plan\":\"pro\"}  (string values only; {} is allowed; \"\" is a legal value)")
	fmt.Fprintln(out, "  condition op is \"eq\" (string value) or \"in\" (non-empty string array)")
	fmt.Fprintln(out, "  output on success: {\"key\":...,\"value\":bool,\"reason\":\"disabled|rule|default\",\"ruleId\":...}")
}

func runDemo(out io.Writer) int {
	intents := []payflow.Intent{
		{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Amount: 1500, Asset: "usdc", Nonce: 1, State: "pending"},
		{ID: "pay-2", Account: "aa-1", Paymaster: "pm-1", Amount: 900, Asset: "usdc", Nonce: 2, State: "pending"},
	}
	spent := map[string]bool{}
	var settlements []payflow.Settlement
	for _, intent := range intents {
		settlement := payflow.Execute(intent, 30, spent, 2000)
		settlements = append(settlements, settlement)
		fmt.Fprintf(out, "intent=%s status=%s charged=%d reason=%s\n", settlement.Intent, settlement.Status, settlement.Charged, settlement.Reason)
	}
	duplicate := payflow.Execute(intents[0], 30, spent, 2000)
	fmt.Fprintf(out, "duplicate of %s status=%s reason=%s\n", duplicate.Intent, duplicate.Status, duplicate.Reason)
	fmt.Fprintln(out, "reconciliation gaps:", payflow.Reconcile(intents, settlements))
	return 0
}
