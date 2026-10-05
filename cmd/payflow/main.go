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
	fmt.Fprintln(w, "submit flags:")
	fmt.Fprintln(w, "  --dry-run             preview settlement results without writing the ledger")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "reconcile range flags (seq bounds, after < seq <= through; default: full history):")
	fmt.Fprintln(w, "  --payment-after <n>    payment seq lower bound (exclusive)")
	fmt.Fprintln(w, "  --payment-through <n>  payment seq upper bound (inclusive)")
	fmt.Fprintln(w, "  --refund-after <n>     refund seq lower bound (exclusive)")
	fmt.Fprintln(w, "  --refund-through <n>   refund seq upper bound (inclusive)")
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

// writeJSON 把 v 以缩进 JSON 加末尾换行写出。任一写入返回错误、或实际写出
// 字节少于请求字节（短写，即使未同时返回错误），都返回错误；调用方必须据此
// 判定输出失败，不能再当作成功。
func writeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(w, `{"error":{"kind":"storage_error","message":%q}}`+"\n", err.Error())
		return err
	}
	if err := writeFull(w, data); err != nil {
		return err
	}
	return writeFull(w, []byte("\n"))
}

// writeFull 要求一次写完 data：短写按 io.ErrShortWrite 处理。
func writeFull(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

// failOutput 报告结果输出阶段（写标准输出）的失败：错误信封只写标准错误，
// 不往可能已留有部分内容的标准输出再补成功响应或混入错误 JSON。
func failOutput(stderr io.Writer, err error) int {
	return failEnvelope(stderr, payflow.ErrStorage, "write result to standard output: "+err.Error())
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
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "preview settlement results without writing the ledger")
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
	// --dry-run 只预测业务结果：不写入账本，输出带 dry_run:true 标记。
	// 结果输出阶段失败（写错误或短写，含末尾换行）：退出码 1，错误信封只写
	// 标准错误；已落账的付款不回滚，预览也保持账本不变。
	if dryRun {
		result, err := l.Preview(batch)
		if err != nil {
			return failWithError(stderr, err)
		}
		if err := writeJSON(stdout, result); err != nil {
			return failOutput(stderr, err)
		}
		return 0
	}
	result, err := l.Submit(batch)
	if err != nil {
		return failWithError(stderr, err)
	}
	if err := writeJSON(stdout, result); err != nil {
		return failOutput(stderr, err)
	}
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
	// 序号边界：after 排除下界、through 包含上界（after < seq <= through）。
	// 用指针区分“未提供”与显式给出的 0：未提供时由库取缺省值（下界 0、
	// 上界为该类记录的最大成功序号）。
	var paymentAfter, paymentThrough, refundAfter, refundThrough int64
	fs.Int64Var(&paymentAfter, "payment-after", 0, "payment seq lower bound (exclusive)")
	fs.Int64Var(&paymentThrough, "payment-through", 0, "payment seq upper bound (inclusive)")
	fs.Int64Var(&refundAfter, "refund-after", 0, "refund seq lower bound (exclusive)")
	fs.Int64Var(&refundThrough, "refund-through", 0, "refund seq upper bound (inclusive)")
	if err := fs.Parse(args); err != nil {
		return failEnvelope(stderr, payflow.ErrInvalid, err.Error())
	}
	if *ledgerPath == "" {
		return failEnvelope(stderr, payflow.ErrInvalid, "reconcile requires --ledger <path>")
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

	bounds := payflow.ReconcileBounds{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "payment-after":
			v := paymentAfter
			bounds.PaymentAfter = &v
		case "payment-through":
			v := paymentThrough
			bounds.PaymentThrough = &v
		case "refund-after":
			v := refundAfter
			bounds.RefundAfter = &v
		case "refund-through":
			v := refundThrough
			bounds.RefundThrough = &v
		}
	})

	l, err := payflow.Open(*ledgerPath)
	if err != nil {
		return failWithError(stderr, err)
	}
	defer l.Close()

	// 对账只读：不改余额、历史或账本文件。边界非法同样整次拒绝、不输出部分
	// 报告；输入有差异仍是正常结果，退出码 0。
	report, err := l.ReconcileFlowRanged(entries, bounds)
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
