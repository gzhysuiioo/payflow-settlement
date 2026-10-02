// Command payflow is the 链上支付与结算编排平台 entry point.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const versionString = "payflow 0.2.0"

func runCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runDemo(stdout)
	}
	switch args[0] {
	case "demo":
		return runDemo(stdout)
	case "version":
		fmt.Fprintln(stdout, versionString)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "init":
		return cmdInit(args[1:], stdin, stdout, stderr)
	case "submit":
		return cmdSubmit(args[1:], stdin, stdout, stderr)
	case "refund":
		return cmdRefund(args[1:], stdin, stdout, stderr)
	case "query":
		return cmdQuery(args[1:], stdout, stderr)
	case "reconcile":
		return cmdReconcile(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: payflow <command> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  init       initialize a ledger with starting balances")
	fmt.Fprintln(w, "  submit     submit a JSON payment batch against a ledger")
	fmt.Fprintln(w, "  refund     submit a JSON full-refund batch against a ledger")
	fmt.Fprintln(w, "  query      print balances, settlement and refund records of a ledger")
	fmt.Fprintln(w, "  reconcile  reconcile an ordered JSON flow against a ledger")
	fmt.Fprintln(w, "  demo       run the in-memory settlement demo")
	fmt.Fprintln(w, "  version    print version")
	fmt.Fprintln(w, "  help       show this help")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "ledger flags (init/submit/refund/query/reconcile):")
	fmt.Fprintln(w, "  -l, --ledger <path>   ledger file path (required)")
	fmt.Fprintln(w, "  -f, --file <path>     JSON input file (init/submit/refund/reconcile; default: stdin)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "reconcile scope flags (after < seq <= through, per record kind):")
	fmt.Fprintln(w, "  --payment-after N     payments with success seq > N (default 0)")
	fmt.Fprintln(w, "  --payment-through N   payments with success seq <= N (default: max payment seq)")
	fmt.Fprintln(w, "  --refund-after N      refunds with success seq > N (default 0)")
	fmt.Fprintln(w, "  --refund-through N    refunds with success seq <= N (default: max refund seq)")
}

// initRequest 是 init 的输入：{"balances":[...]}。
type initRequest struct {
	Balances []payflow.BalanceInit `json:"balances"`
}

type errorEnvelope struct {
	Error struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
}

func failEnvelope(stderr io.Writer, kind, message string) int {
	env := errorEnvelope{}
	env.Error.Kind = kind
	env.Error.Message = message
	writeJSON(stderr, env)
	return 1
}

func failWithError(stderr io.Writer, err error) int {
	return failEnvelope(stderr, payflow.KindOf(err), err.Error())
}

func writeJSON(w io.Writer, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(w, `{"error":{"kind":"storage_error","message":%q}}`+"\n", err.Error())
		return
	}
	w.Write(data)
	w.Write([]byte("\n"))
}

// ledgerFlagSet 构造 -l/--ledger、-f/--file 标志。
func ledgerFlagSet(name string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ledgerPath, inputFile string
	fs.StringVar(&ledgerPath, "ledger", "", "ledger file path")
	fs.StringVar(&ledgerPath, "l", "", "ledger file path (shorthand)")
	fs.StringVar(&inputFile, "file", "", "JSON input file (default stdin)")
	fs.StringVar(&inputFile, "f", "", "JSON input file (shorthand)")
	return fs, &ledgerPath, &inputFile
}

func readInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}

func cmdInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, ledgerPath, inputFile := ledgerFlagSet("init")
	if err := fs.Parse(args); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}
	if *ledgerPath == "" {
		return failEnvelope(stderr, payflow.ErrInvalid, "init requires --ledger <path>")
	}
	raw, err := readInput(*inputFile, stdin)
	if err != nil {
		return failEnvelope(stderr, payflow.ErrStorage, "read input: "+err.Error())
	}
	var req initRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&req); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, "parse init JSON: "+err.Error())
	}
	if err := requireEOF(dec, "init"); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}
	if err := payflow.CreateLedger(*ledgerPath, req.Balances); err != nil {
		return failWithError(stderr, err)
	}
	writeJSON(stdout, map[string]any{
		"status":   "initialized",
		"ledger":   *ledgerPath,
		"accounts": len(req.Balances),
	})
	return 0
}

func cmdSubmit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, ledgerPath, inputFile := ledgerFlagSet("submit")
	if err := fs.Parse(args); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}
	if *ledgerPath == "" {
		return failEnvelope(stderr, payflow.ErrInvalid, "submit requires --ledger <path>")
	}
	raw, err := readInput(*inputFile, stdin)
	if err != nil {
		return failEnvelope(stderr, payflow.ErrStorage, "read input: "+err.Error())
	}
	var batch payflow.FeeBatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&batch); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, "parse batch JSON: "+err.Error())
	}
	if err := requireEOF(dec, "submit"); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}

	l, err := payflow.Open(*ledgerPath)
	if err != nil {
		return failWithError(stderr, err)
	}
	defer l.Close()

	// 即使批次内全部项都失败，也是正常的逐项结果，退出码仍为 0。
	result, err := l.Submit(batch)
	if err != nil {
		return failWithError(stderr, err)
	}
	writeJSON(stdout, result)
	return 0
}

func cmdRefund(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, ledgerPath, inputFile := ledgerFlagSet("refund")
	if err := fs.Parse(args); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}
	if *ledgerPath == "" {
		return failEnvelope(stderr, payflow.ErrInvalid, "refund requires --ledger <path>")
	}
	raw, err := readInput(*inputFile, stdin)
	if err != nil {
		return failEnvelope(stderr, payflow.ErrStorage, "read input: "+err.Error())
	}
	var batch payflow.RefundBatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&batch); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, "parse refund batch JSON: "+err.Error())
	}
	if err := requireEOF(dec, "refund"); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}

	l, err := payflow.Open(*ledgerPath)
	if err != nil {
		return failWithError(stderr, err)
	}
	defer l.Close()

	// 即使批次内全部项都失败，也是正常的逐项结果，退出码仍为 0。
	result, err := l.Refund(batch)
	if err != nil {
		return failWithError(stderr, err)
	}
	writeJSON(stdout, result)
	return 0
}

func cmdQuery(args []string, stdout, stderr io.Writer) int {
	fs, ledgerPath, _ := ledgerFlagSet("query")
	if err := fs.Parse(args); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}
	if *ledgerPath == "" {
		return failEnvelope(stderr, payflow.ErrInvalid, "query requires --ledger <path>")
	}
	l, err := payflow.Open(*ledgerPath)
	if err != nil {
		return failWithError(stderr, err)
	}
	defer l.Close()
	snap, err := l.Query()
	if err != nil {
		return failWithError(stderr, err)
	}
	writeJSON(stdout, snap)
	return 0
}

func cmdReconcile(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, ledgerPath, inputFile := ledgerFlagSet("reconcile")
	// 序号范围边界：after < seq <= through。after 缺省 0；through 缺省
	// 取对应类别的最大成功序号（用是否显式给出区分，而非特殊值）。
	var payAfter, payThrough, refAfter, refThrough int64
	fs.Int64Var(&payAfter, "payment-after", 0, "reconcile payments with success seq greater than this")
	fs.Int64Var(&payThrough, "payment-through", 0, "reconcile payments with success seq up to this (default: max payment seq)")
	fs.Int64Var(&refAfter, "refund-after", 0, "reconcile refunds with success seq greater than this")
	fs.Int64Var(&refThrough, "refund-through", 0, "reconcile refunds with success seq up to this (default: max refund seq)")
	if err := fs.Parse(args); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}
	if *ledgerPath == "" {
		return failEnvelope(stderr, payflow.ErrInvalid, "reconcile requires --ledger <path>")
	}
	provided := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	scope := payflow.ReconScope{}
	scope.Payment.After = payAfter
	scope.Refund.After = refAfter
	if provided["payment-through"] {
		scope.Payment.Through = &payThrough
	}
	if provided["refund-through"] {
		scope.Refund.Through = &refThrough
	}
	raw, err := readInput(*inputFile, stdin)
	if err != nil {
		return failEnvelope(stderr, payflow.ErrStorage, "read input: "+err.Error())
	}
	// 先严格解析全部输入：任一条目不合法则整次拒绝，退出码 1，不输出部分报告。
	entries, err := payflow.ParseReconcileRequest(raw)
	if err != nil {
		return failWithError(stderr, err)
	}

	l, err := payflow.Open(*ledgerPath)
	if err != nil {
		return failWithError(stderr, err)
	}
	defer l.Close()

	// 对账只读：不改余额、历史或账本文件。输入有差异仍是正常结果，退出码 0。
	// 边界非法（负数、下界大于上界、上界超过最大序号）整次 invalid_parameter。
	report, err := l.ReconcileFlowScoped(entries, scope)
	if err != nil {
		return failWithError(stderr, err)
	}
	writeJSON(stdout, report)
	return 0
}

// requireEOF 拒绝 JSON 文档之后的尾随内容。
func requireEOF(dec *json.Decoder, what string) error {
	if tok, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s: unexpected trailing content after JSON document (token %v)", what, tok)
		}
		return fmt.Errorf("%s: %v", what, err)
	}
	return nil
}

func runDemo(stdout io.Writer) int {
	intents := []payflow.Intent{
		{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Amount: 1500, Asset: "usdc", Nonce: 1, State: "pending"},
		{ID: "pay-2", Account: "aa-1", Paymaster: "pm-1", Amount: 900, Asset: "usdc", Nonce: 2, State: "pending"},
	}
	spent := map[string]bool{}
	var settlements []payflow.Settlement
	for _, intent := range intents {
		settlement := payflow.Execute(intent, 30, spent, 2000)
		settlements = append(settlements, settlement)
		fmt.Fprintf(stdout, "intent=%s status=%s charged=%d reason=%s\n", settlement.Intent, settlement.Status, settlement.Charged, settlement.Reason)
	}
	duplicate := payflow.Execute(intents[0], 30, spent, 2000)
	fmt.Fprintf(stdout, "duplicate of %s status=%s reason=%s\n", duplicate.Intent, duplicate.Status, duplicate.Reason)
	fmt.Fprintln(stdout, "reconciliation gaps:", payflow.Reconcile(intents, settlements))
	return 0
}
