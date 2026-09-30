// Command payflow is the 链上支付与结算编排平台 entry point.
package main

import (
	"fmt"
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
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: payflow [demo|version|help]")
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
