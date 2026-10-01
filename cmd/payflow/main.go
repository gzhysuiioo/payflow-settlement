// Command payflow is the 链上支付与结算编排平台 entry point.
package main

import (
	"encoding/json"
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
	case "init":
		runInit()
	case "submit":
		runSubmit()
	case "balance":
		runBalance()
	case "records":
		runRecords()
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: payflow [demo|version|init|submit|balance|records|help]")
}

func runInit() {
	if len(os.Args) < 3 {
		fatal("usage: payflow init <ledger-path>")
	}
	path := os.Args[2]
	var balances []payflow.Balance
	if err := json.NewDecoder(os.Stdin).Decode(&balances); err != nil {
		fatal("invalid init data: %v", err)
	}
	if err := payflow.Init(path, balances); err != nil {
		fatal("init failed: %v", err)
	}
	fmt.Printf("ledger initialized at %s\n", path)
}

func runSubmit() {
	if len(os.Args) < 3 {
		fatal("usage: payflow submit <ledger-path>")
	}
	path := os.Args[2]
	var batch payflow.Batch
	if err := json.NewDecoder(os.Stdin).Decode(&batch); err != nil {
		fatal("invalid batch: %v", err)
	}
	ledger, err := payflow.Open(path)
	if err != nil {
		fatal("cannot open ledger: %v", err)
	}
	defer ledger.Close()
	results, err := ledger.Submit(batch)
	if err != nil {
		fatal("submit failed: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		fatal("cannot write results: %v", err)
	}
}

func runBalance() {
	if len(os.Args) < 3 {
		fatal("usage: payflow balance <ledger-path>")
	}
	path := os.Args[2]
	ledger, err := payflow.Open(path)
	if err != nil {
		fatal("cannot open ledger: %v", err)
	}
	defer ledger.Close()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ledger.Balances()); err != nil {
		fatal("cannot write balances: %v", err)
	}
}

func runRecords() {
	if len(os.Args) < 3 {
		fatal("usage: payflow records <ledger-path>")
	}
	path := os.Args[2]
	ledger, err := payflow.Open(path)
	if err != nil {
		fatal("cannot open ledger: %v", err)
	}
	defer ledger.Close()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ledger.Records()); err != nil {
		fatal("cannot write records: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
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
