package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type cliResult struct {
	code int
	out  string
	err  string
}

func runCLIWith(t *testing.T, args []string, stdin string) cliResult {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runCLI(args, strings.NewReader(stdin), &out, &errBuf)
	return cliResult{code: code, out: out.String(), err: errBuf.String()}
}

func decodeOut(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	return v
}

func TestCLIVersionAndDemo(t *testing.T) {
	if r := runCLIWith(t, []string{"version"}, ""); r.code != 0 || !strings.Contains(r.out, "payflow 0.2.0") {
		t.Fatalf("version: %+v", r)
	}
	// 无参数保持历史默认行为：运行 demo。
	if r := runCLIWith(t, nil, ""); r.code != 0 || !strings.Contains(r.out, "reconciliation gaps") {
		t.Fatalf("default demo: code=%d out=%q", r.code, r.out)
	}
	if r := runCLIWith(t, []string{"demo"}, ""); r.code != 0 || !strings.Contains(r.out, "intent=pay-1") {
		t.Fatalf("demo: %+v", r)
	}
	if r := runCLIWith(t, []string{"bogus"}, ""); r.code != 2 {
		t.Fatalf("unknown command code=%d", r.code)
	}
}

func TestCLILedgerLifecycle(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"

	// init
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`)
	if r.code != 0 {
		t.Fatalf("init code=%d err=%s", r.code, r.err)
	}
	if v := decodeOut(t, r.out); v["status"] != "initialized" {
		t.Fatalf("init out=%s", r.out)
	}

	// 重复 init：退出码 1 + ledger_exists，且文件未被覆盖。
	r = runCLIWith(t, []string{"init", "-l", ledgerPath}, `{"balances":[]}`)
	if r.code != 1 {
		t.Fatalf("re-init code=%d", r.code)
	}
	if env := decodeOut(t, r.err); env["error"].(map[string]any)["kind"] != "ledger_exists" {
		t.Fatalf("re-init envelope=%s", r.err)
	}

	// 非法 init：分类为 invalid_parameter。
	badPath := t.TempDir() + "/bad.json"
	r = runCLIWith(t, []string{"init", "-l", badPath},
		`{"balances":[{"account":"","asset":"usdc","balance":1}]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("bad init: code=%d err=%s", r.code, r.err)
	}

	// submit：混合结果逐项返回，退出码 0。
	batch := `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1500,"nonce":1},
	  {"id":"p2","account":"aa-1","asset":"usdc","amount":900},
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1500,"nonce":1},
	  {"id":"p3","account":"aa-1","asset":"usdc","amount":1,"state":"failed"}
	]}`
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, batch)
	if r.code != 0 {
		t.Fatalf("submit code=%d err=%s", r.code, r.err)
	}
	var res struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(res.Results))
	for i, x := range res.Results {
		got[i] = x.Status
	}
	want := []string{"settled", "insufficient_balance", "duplicate", "state_error"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}

	// 空批次返回空结果。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":0,"intents":[]}`)
	if r.code != 0 || strings.TrimSpace(r.out) != `{
  "results": []
}` {
		t.Fatalf("empty batch: code=%d out=%q", r.code, r.out)
	}

	// 非法费率走错误信封（批次级错误，不是逐项）。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":-2,"intents":[]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("bad fee: code=%d err=%s", r.code, r.err)
	}

	// query（模拟新进程重新打开）。
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query code=%d err=%s", r.code, r.err)
	}
	var snap struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
		Settlements []struct {
			ID      string `json:"id"`
			FeeBps  int    `json:"fee_bps"`
			Charged int64  `json:"charged"`
		} `json:"settlements"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 496 {
		t.Fatalf("query balances=%+v", snap.Balances)
	}
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" || snap.Settlements[0].FeeBps != 30 || snap.Settlements[0].Charged != 1504 {
		t.Fatalf("query settlements=%+v", snap.Settlements)
	}

	// 未初始化路径。
	missing := t.TempDir() + "/nope.json"
	if r = runCLIWith(t, []string{"query", "-l", missing}, ""); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "ledger_not_initialized" {
		t.Fatalf("missing: %+v", r)
	}
}

func TestCLICorruptLedger(t *testing.T) {
	dir := t.TempDir()

	// 直接写出一个损坏账本（不经过本进程 init，绕过同进程注册表缓存，
	// 模拟用户拿一个被截断/损坏的文件启动程序）。
	path := dir + "/corrupt.json"
	if err := os.WriteFile(path, []byte(`{"version":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"query", "submit"} {
		r := runCLIWith(t, []string{cmd, "-l", path}, `{"fee_bps":0,"intents":[]}`)
		if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "corrupt_ledger" {
			t.Fatalf("%s on corrupt: code=%d out=%s err=%s", cmd, r.code, r.out, r.err)
		}
	}

	// 非法 JSON 输入分类为 invalid_parameter（与磁盘损坏区分）。
	good := dir + "/good.json"
	if r := runCLIWith(t, []string{"init", "-l", good}, `{"balances":[]}`); r.code != 0 {
		t.Fatalf("init good: %s", r.err)
	}
	if r := runCLIWith(t, []string{"submit", "-l", good}, `{not json`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("garbage input: code=%d err=%s", r.code, r.err)
	}
}

func TestCLIRequiresLedgerFlag(t *testing.T) {
	if r := runCLIWith(t, []string{"query"}, ""); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("query without -l: %+v", r)
	}
}

func TestCLIRefundLifecycle(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"

	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	// p1 成功（charged 1504），p2 余额不足失败。
	batch := `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1500,"nonce":1},
	  {"id":"p2","account":"aa-1","asset":"usdc","amount":900}
	]}`
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, batch); r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}

	// 退款批次：成功 / 重复 / 冲突 / 已退款 / 目标不存在 / 参数非法，逐项返回且退出码 0。
	refunds := `{"refunds":[
	  {"id":"r1","settlement_id":"p1","reason":"cancel"},
	  {"id":"r1","settlement_id":"p1","reason":"cancel"},
	  {"id":"r1","settlement_id":"p1","reason":"changed"},
	  {"id":"r2","settlement_id":"p1","reason":"again"},
	  {"id":"r3","settlement_id":"ghost","reason":"x"},
	  {"id":"r4","settlement_id":"p2","reason":"x"},
	  {"id":"","settlement_id":"p1","reason":"x"}
	]}`
	r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, refunds)
	if r.code != 0 {
		t.Fatalf("refund batch must exit 0 even with per-item failures: %s", r.err)
	}
	var res struct {
		Results []struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			SettlementID string `json:"settlement_id"`
			Account      string `json:"account"`
			Asset        string `json:"asset"`
			Charged      int64  `json:"charged"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	want := []string{"refunded", "duplicate", "conflict", "already_refunded", "not_found", "not_found", "invalid_parameter"}
	if len(res.Results) != len(want) {
		t.Fatalf("got %d results, want %d: %s", len(res.Results), len(want), r.out)
	}
	for i, x := range res.Results {
		if x.Status != want[i] {
			t.Fatalf("item %d status=%s want %s", i, x.Status, want[i])
		}
	}
	// 成功与重复必须回传原付款编号、账户、资产、总额。
	for _, i := range []int{0, 1} {
		x := res.Results[i]
		if x.SettlementID != "p1" || x.Account != "aa-1" || x.Asset != "usdc" || x.Charged != 1504 {
			t.Fatalf("item %d missing trace fields: %+v", i, x)
		}
	}

	// 退款后余额恢复为 2000（全额含手续费）。
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	var snap struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
		Refunds []struct {
			ID           string `json:"id"`
			SettlementID string `json:"settlement_id"`
			Reason       string `json:"reason"`
			Charged      int64  `json:"charged"`
		} `json:"refunds"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 2000 {
		t.Fatalf("balance after refund=%+v", snap.Balances)
	}
	if len(snap.Refunds) != 1 || snap.Refunds[0].SettlementID != "p1" || snap.Refunds[0].Charged != 1504 {
		t.Fatalf("query refunds=%+v", snap.Refunds)
	}

	// 空列表返回空结果。
	if r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{"refunds":[]}`); r.code != 0 ||
		strings.TrimSpace(r.out) != "{\n  \"results\": []\n}" {
		t.Fatalf("empty refund batch: code=%d out=%q err=%s", r.code, r.out, r.err)
	}

	// 非法 JSON 走批次级错误信封。
	if r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{bad`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("bad refund json: code=%d err=%s", r.code, r.err)
	}

	// 缺少 -l。
	if r := runCLIWith(t, []string{"refund"}, `{"refunds":[]}`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("refund without -l: %+v", r)
	}

	// 未初始化账本。
	missing := t.TempDir() + "/nope.json"
	if r := runCLIWith(t, []string{"refund", "-l", missing}, `{"refunds":[]}`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "ledger_not_initialized" {
		t.Fatalf("refund on missing ledger: %+v", r)
	}
}

func TestCLIRefundOnCorruptLedger(t *testing.T) {
	path := t.TempDir() + "/corrupt.json"
	if err := os.WriteFile(path, []byte(`{"version":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLIWith(t, []string{"refund", "-l", path}, `{"refunds":[]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "corrupt_ledger" {
		t.Fatalf("refund on corrupt: code=%d err=%s", r.code, r.err)
	}
}

func TestCLIHelpListsRefund(t *testing.T) {
	r := runCLIWith(t, []string{"help"}, "")
	if !strings.Contains(r.out, "refund") {
		t.Fatalf("help does not mention refund:\n%s", r.out)
	}
}

func setupReconLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100000000},{"account":"aa-2","asset":"eth","balance":100}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	// 30bps：p1 amount 1000 -> fee 3, charged 1003；p2 amount 10 -> fee 0, charged 10。
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1000,"nonce":1},
	  {"id":"p2","account":"aa-2","asset":"eth","amount":10}
	]}`); r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}
	return ledgerPath
}

func TestCLIReconcileHappyPath(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100000000}]}`)
	if r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1000,"nonce":1}
	]}`)
	if r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}
	r = runCLIWith(t, []string{"refund", "-l", ledgerPath},
		`{"refunds":[{"id":"r1","settlement_id":"p1","reason":"cancel"}]}`)
	if r.code != 0 {
		t.Fatalf("refund: %s", r.err)
	}

	body := `{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"pX","account":"aa-1","asset":"usdc","amount":9,"fee":0,"charged":9},
	  {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003,"settlement_id":"p1"}
	]}`
	r = runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, body)
	if r.code != 0 {
		t.Fatalf("reconcile exits 0 even with differences: %s", r.err)
	}
	var rep struct {
		Results []struct {
			Index     int    `json:"index"`
			Kind      string `json:"kind"`
			ID        string `json:"id"`
			Status    string `json:"status"`
			Positions []int  `json:"positions"`
			Diffs     []struct {
				Field  string `json:"field"`
				Ledger string `json:"ledger"`
				Flow   string `json:"flow"`
			} `json:"diffs"`
			Record *struct {
				SettlementID string `json:"settlement_id"`
				RefundID     string `json:"refund_id"`
			} `json:"record"`
		} `json:"results"`
		MissingPayments []map[string]any `json:"missing_payments"`
		MissingRefunds  []map[string]any `json:"missing_refunds"`
		MaxPaymentSeq   int64            `json:"max_payment_seq"`
		MaxRefundSeq    int64            `json:"max_refund_seq"`
		Totals          struct {
			Ledger []struct {
				Account       string `json:"account"`
				Asset         string `json:"asset"`
				ChargedTotal  string `json:"charged_total"`
				RefundedTotal string `json:"refunded_total"`
				NetCharged    string `json:"net_charged"`
			} `json:"ledger"`
			Flow []struct {
				NetCharged string `json:"net_charged"`
			} `json:"flow"`
		} `json:"totals"`
		NetDiff []struct {
			NetChargedDiff string `json:"net_charged_diff"`
		} `json:"net_diff"`
	}
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, r.out)
	}
	if len(rep.Results) != 3 {
		t.Fatalf("want 3 results, got %d", len(rep.Results))
	}
	want := []string{"matched", "missing_in_ledger", "matched"}
	for i, x := range rep.Results {
		if x.Status != want[i] {
			t.Fatalf("result %d = %s want %s", i, x.Status, want[i])
		}
		if x.Index != i {
			t.Fatalf("result %d index=%d", i, x.Index)
		}
	}
	// 命中账本的结果都带关联记录：p1 已退款带 refund_id；r1 带 settlement_id。
	if rep.Results[0].Record == nil || rep.Results[0].Record.RefundID != "r1" {
		t.Fatalf("p1 record link: %+v", rep.Results[0].Record)
	}
	if rep.Results[2].Record == nil || rep.Results[2].Record.SettlementID != "p1" {
		t.Fatalf("r1 record link: %+v", rep.Results[2].Record)
	}
	// 全部账本记录都被覆盖，缺失列表为空。
	if len(rep.MissingPayments) != 0 || len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing=%+v/%+v", rep.MissingPayments, rep.MissingRefunds)
	}
	if rep.MaxPaymentSeq != 1 || rep.MaxRefundSeq != 1 {
		t.Fatalf("max seqs=%d/%d", rep.MaxPaymentSeq, rep.MaxRefundSeq)
	}
	// 账本侧 usdc 扣款 1003、退款 1003、净 0；流水侧净扣款 = 1003+9-1003 = 9；差额 +9。
	if len(rep.Totals.Ledger) != 1 || rep.Totals.Ledger[0].ChargedTotal != "1003" ||
		rep.Totals.Ledger[0].RefundedTotal != "1003" || rep.Totals.Ledger[0].NetCharged != "0" {
		t.Fatalf("ledger totals=%+v", rep.Totals.Ledger)
	}
	if rep.Totals.Flow[0].NetCharged != "9" || rep.NetDiff[0].NetChargedDiff != "9" {
		t.Fatalf("flow net=%s diff=%s", rep.Totals.Flow[0].NetCharged, rep.NetDiff[0].NetChargedDiff)
	}
}

func TestCLIReconcileFieldMismatchAndDuplicate(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	// p2 出现两次（fee 都故意报错）：整组 duplicate，不判差异，账本记录不列为缺失。
	// rX 引用账本中不存在的退款编号。
	body := `{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":9,"charged":1003},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":5,"charged":99},
	  {"kind":"payment","id":"p2","account":"aa-2","asset":"eth","amount":10,"fee":5,"charged":99},
	  {"kind":"refund","id":"rX","account":"aa-1","asset":"usdc","amount":1,"fee":0,"charged":1,"settlement_id":"p1"}
	]}`
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, body)
	if r.code != 0 {
		t.Fatalf("reconcile: %s", r.err)
	}
	var rep struct {
		Results []struct {
			Status    string `json:"status"`
			Positions []int  `json:"positions"`
			Diffs     []struct {
				Field string `json:"field"`
			} `json:"diffs"`
		} `json:"results"`
		MissingPayments []struct {
			ID string `json:"id"`
		} `json:"missing_payments"`
		MissingRefunds []struct {
			ID string `json:"id"`
		} `json:"missing_refunds"`
	}
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatalf("decode: %v\n%s", err, r.out)
	}
	if rep.Results[0].Status != "field_mismatch" {
		t.Fatalf("p1 status=%s", rep.Results[0].Status)
	}
	var fields []string
	for _, d := range rep.Results[0].Diffs {
		fields = append(fields, d.Field)
	}
	if strings.Join(fields, ",") != "fee" {
		t.Fatalf("p1 diffs=%v", fields)
	}
	for _, i := range []int{1, 2} {
		if rep.Results[i].Status != "duplicate" {
			t.Fatalf("result %d status=%s", i, rep.Results[i].Status)
		}
		pos := rep.Results[i].Positions
		if len(pos) != 2 || pos[0] != 1 || pos[1] != 2 {
			t.Fatalf("result %d positions=%v", i, pos)
		}
		if len(rep.Results[i].Diffs) != 0 {
			t.Fatalf("duplicate must not list diffs: %+v", rep.Results[i].Diffs)
		}
	}
	if rep.Results[3].Status != "missing_in_ledger" {
		t.Fatalf("rX status=%s", rep.Results[3].Status)
	}
	// p2 重复组抑制缺失；p1 已覆盖（字段不符也算覆盖）。账本无退款 -> 无缺失退款。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v", rep.MissingRefunds)
	}
}

func TestCLIReconcileEmptyListsEverythingMissing(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	// 空列表：全部账本成功记录缺失，先付款（p1,p2）后退款（无）。
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, `{"entries":[]}`)
	if r.code != 0 {
		t.Fatalf("reconcile: %s", r.err)
	}
	var rep struct {
		Results         []any `json:"results"`
		MissingPayments []struct {
			ID  string `json:"id"`
			Seq int64  `json:"seq"`
		} `json:"missing_payments"`
		MissingRefunds []any `json:"missing_refunds"`
		MaxPaymentSeq  int64 `json:"max_payment_seq"`
		MaxRefundSeq   int64 `json:"max_refund_seq"`
	}
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 0 {
		t.Fatalf("results=%v", rep.Results)
	}
	if len(rep.MissingPayments) != 2 || rep.MissingPayments[0].ID != "p1" || rep.MissingPayments[1].ID != "p2" {
		t.Fatalf("missing payments=%+v", rep.MissingPayments)
	}
	if rep.MissingPayments[0].Seq != 1 || rep.MissingPayments[1].Seq != 2 {
		t.Fatalf("missing seqs=%d,%d", rep.MissingPayments[0].Seq, rep.MissingPayments[1].Seq)
	}
	if len(rep.MissingRefunds) != 0 || rep.MaxPaymentSeq != 2 || rep.MaxRefundSeq != 0 {
		t.Fatalf("refunds=%+v seqs=%d/%d", rep.MissingRefunds, rep.MaxPaymentSeq, rep.MaxRefundSeq)
	}

	// 空历史账本：最大序号为零。
	emptyPath := t.TempDir() + "/empty.json"
	if r := runCLIWith(t, []string{"init", "-l", emptyPath}, `{"balances":[]}`); r.code != 0 {
		t.Fatalf("init empty: %s", r.err)
	}
	r = runCLIWith(t, []string{"reconcile", "-l", emptyPath}, `{"entries":[]}`)
	if !strings.Contains(r.out, `"max_payment_seq": 0`) || !strings.Contains(r.out, `"max_refund_seq": 0`) {
		t.Fatalf("empty history seqs not zero: %s", r.out)
	}
}

func TestCLIReconcileInvalidInputNoPartialReport(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	bad := []string{
		`{not json`,
		`{}`,
		`{"entries":[{"kind":"wire","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":1}]}`,
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":0,"fee":0,"charged":1}]}`,
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1,"fee":-2,"charged":1}]}`,
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1,"fee":0}]}`,
		`{"entries":[{"kind":"payment","id":"x","account":"","asset":"z","amount":1,"fee":0,"charged":1}]}`,
		`{"entries":[{"kind":"refund","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":1}]}`,
		`{"entries":[{"kind":"payment","id":"x","account":"a","asset":"z","amount":1,"fee":0,"charged":99999999999999999999999}]}`,
	}
	for i, body := range bad {
		r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, body)
		if r.code != 1 {
			t.Fatalf("case %d: exit code=%d want 1", i, r.code)
		}
		if r.out != "" {
			t.Fatalf("case %d: invalid input must print no partial report: %q", i, r.out)
		}
		if env := decodeOut(t, r.err); env["error"].(map[string]any)["kind"] != "invalid_parameter" {
			t.Fatalf("case %d envelope=%s", i, r.err)
		}
	}

	// 缺 -l、账本不存在、损坏沿用现有分类。
	if r := runCLIWith(t, []string{"reconcile"}, `{"entries":[]}`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("no -l: %+v", r)
	}
	missing := t.TempDir() + "/nope.json"
	if r := runCLIWith(t, []string{"reconcile", "-l", missing}, `{"entries":[]}`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "ledger_not_initialized" {
		t.Fatalf("missing ledger: %+v", r)
	}
	corrupt := t.TempDir() + "/corrupt.json"
	if err := os.WriteFile(corrupt, []byte(`{"version":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runCLIWith(t, []string{"reconcile", "-l", corrupt}, `{"entries":[]}`); r.code != 1 ||
		decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "corrupt_ledger" {
		t.Fatalf("corrupt ledger: %+v", r)
	}
}

func TestCLIReconcileFromFileFlag(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	inputPath := t.TempDir() + "/flow.json"
	if err := os.WriteFile(inputPath, []byte(`{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// -f 读取文件，stdin 给空内容也不影响。
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath, "-f", inputPath}, "")
	if r.code != 0 {
		t.Fatalf("reconcile -f: %s", r.err)
	}
	if !strings.Contains(r.out, `"status": "matched"`) {
		t.Fatalf("report=%s", r.out)
	}
	// 读取不存在的输入文件按存储错误分类。
	r = runCLIWith(t, []string{"reconcile", "-l", ledgerPath, "-f", t.TempDir() + "/missing.json"}, "")
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "storage_error" {
		t.Fatalf("missing input file: %+v", r)
	}
}

func TestCLIReconcileDoesNotModifyLedger(t *testing.T) {
	ledgerPath := setupReconLedger(t)
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"entries":[{"kind":"payment","id":"zzz","account":"aa-1","asset":"zzz","amount":1,"fee":0,"charged":1}]}`
	if r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath}, body); r.code != 0 {
		t.Fatalf("reconcile: %s", r.err)
	}
	after, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file changed by reconcile")
	}
}

func TestCLIHelpListsReconcile(t *testing.T) {
	r := runCLIWith(t, []string{"help"}, "")
	if !strings.Contains(r.out, "reconcile") {
		t.Fatalf("help does not mention reconcile:\n%s", r.out)
	}
}

func setupRangeLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":1000000},{"account":"aa-2","asset":"eth","balance":100}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	// p1 usdc 1003, p2 usdc 2006, p3 eth 10（30bps）。
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":30,"intents":[
	  {"id":"p1","account":"aa-1","paymaster":"pm","asset":"usdc","amount":1000,"nonce":1},
	  {"id":"p2","account":"aa-1","paymaster":"pm","asset":"usdc","amount":2000,"nonce":2},
	  {"id":"p3","account":"aa-2","asset":"eth","amount":10}
	]}`); r.code != 0 {
		t.Fatalf("submit: %s", r.err)
	}
	if r := runCLIWith(t, []string{"refund", "-l", ledgerPath}, `{"refunds":[
	  {"id":"r1","settlement_id":"p1","reason":"cancel"},
	  {"id":"r2","settlement_id":"p2","reason":"cancel"}
	]}`); r.code != 0 {
		t.Fatalf("refund: %s", r.err)
	}
	return ledgerPath
}

func TestCLIReconcileRangeFlags(t *testing.T) {
	ledgerPath := setupRangeLedger(t)
	// 付款 (1,2]：p1 范围外、p2 入选；退款 (0,1]：r2 范围外。
	body := `{"entries":[
	  {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1000,"fee":3,"charged":1003},
	  {"kind":"payment","id":"p2","account":"aa-1","asset":"usdc","amount":2000,"fee":6,"charged":2006},
	  {"kind":"refund","id":"r2","account":"aa-1","asset":"usdc","amount":2000,"fee":6,"charged":2006,"settlement_id":"p2"}
	]}`
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath,
		"--payment-after", "1", "--payment-through", "2",
		"--refund-after", "0", "--refund-through", "1"}, body)
	if r.code != 0 {
		t.Fatalf("reconcile: %s", r.err)
	}
	var rep struct {
		Results []struct {
			Status string `json:"status"`
		} `json:"results"`
		PaymentAfter   int64 `json:"payment_after"`
		PaymentThrough int64 `json:"payment_through"`
		RefundAfter    int64 `json:"refund_after"`
		RefundThrough  int64 `json:"refund_through"`
		MaxPaymentSeq  int64 `json:"max_payment_seq"`
		MaxRefundSeq   int64 `json:"max_refund_seq"`
	}
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatal(err)
	}
	want := []string{"out_of_scope", "matched", "out_of_scope"}
	for i, x := range rep.Results {
		if x.Status != want[i] {
			t.Fatalf("result %d=%s want %s", i, x.Status, want[i])
		}
	}
	if rep.PaymentAfter != 1 || rep.PaymentThrough != 2 ||
		rep.RefundAfter != 0 || rep.RefundThrough != 1 {
		t.Fatalf("bounds=%d,%d,%d,%d", rep.PaymentAfter, rep.PaymentThrough,
			rep.RefundAfter, rep.RefundThrough)
	}
	if rep.MaxPaymentSeq != 3 || rep.MaxRefundSeq != 2 {
		t.Fatalf("max seqs=%d/%d", rep.MaxPaymentSeq, rep.MaxRefundSeq)
	}
}

func TestCLIReconcileRangeDefaults(t *testing.T) {
	ledgerPath := setupRangeLedger(t)
	// 只给部分边界：未给的上界取最大序号，未给的下界取 0。
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath, "--payment-after", "1"},
		`{"entries":[]}`)
	if r.code != 0 {
		t.Fatalf("reconcile: %s", r.err)
	}
	var rep struct {
		PaymentAfter   int64 `json:"payment_after"`
		PaymentThrough int64 `json:"payment_through"`
		RefundAfter    int64 `json:"refund_after"`
		RefundThrough  int64 `json:"refund_through"`
	}
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.PaymentAfter != 1 || rep.PaymentThrough != 3 ||
		rep.RefundAfter != 0 || rep.RefundThrough != 2 {
		t.Fatalf("bounds=%d,%d,%d,%d", rep.PaymentAfter, rep.PaymentThrough,
			rep.RefundAfter, rep.RefundThrough)
	}
}

func TestCLIReconcileRangeInvalidBounds(t *testing.T) {
	ledgerPath := setupRangeLedger(t)
	bad := [][]string{
		{"--payment-after", "-1"},
		{"--payment-after", "2", "--payment-through", "1"},
		{"--payment-through", "4"},
		{"--refund-after", "3"},
		{"--refund-through", "3"},
		{"--payment-after", "abc"},
	}
	for i, flags := range bad {
		args := append([]string{"reconcile", "-l", ledgerPath}, flags...)
		r := runCLIWith(t, args, `{"entries":[]}`)
		if r.code != 1 {
			t.Fatalf("case %d: code=%d want 1", i, r.code)
		}
		if r.out != "" {
			t.Fatalf("case %d: invalid bounds must print no partial report: %q", i, r.out)
		}
		if env := decodeOut(t, r.err); env["error"].(map[string]any)["kind"] != "invalid_parameter" {
			t.Fatalf("case %d envelope=%s", i, r.err)
		}
	}
}

func TestCLIReconcileRangeOnlyRefunds(t *testing.T) {
	ledgerPath := setupRangeLedger(t)
	// 只选退款（付款上界 0）：账本侧扣款合计为零，退款按所选合计。
	r := runCLIWith(t, []string{"reconcile", "-l", ledgerPath, "--payment-through", "0"},
		`{"entries":[]}`)
	if r.code != 0 {
		t.Fatalf("reconcile: %s", r.err)
	}
	var rep struct {
		MissingPayments []map[string]any `json:"missing_payments"`
		MissingRefunds  []map[string]any `json:"missing_refunds"`
		Totals          struct {
			Ledger []struct {
				ChargedTotal  string `json:"charged_total"`
				RefundedTotal string `json:"refunded_total"`
			} `json:"ledger"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 2 {
		t.Fatalf("missing refunds=%+v", rep.MissingRefunds)
	}
	if len(rep.Totals.Ledger) != 1 {
		t.Fatalf("ledger rows=%+v", rep.Totals.Ledger)
	}
	row := rep.Totals.Ledger[0]
	if row.ChargedTotal != "0" || row.RefundedTotal != "3009" {
		t.Fatalf("ledger row=%+v", row)
	}
}

func TestCLISubmitChargeLimits(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000},{"account":"aa-1","asset":"eth","balance":100}]}`)
	if r.code != 0 {
		t.Fatalf("init code=%d err=%s", r.code, r.err)
	}

	// 限额内、恰好到顶、超限、未列出组合逐项对应，退出码 0。
	batch := `{"fee_bps":0,
	  "limits":[{"account":"aa-1","asset":"usdc","max_charged":1000}],
	  "intents":[
	    {"id":"p1","account":"aa-1","asset":"usdc","amount":600},
	    {"id":"p2","account":"aa-1","asset":"usdc","amount":400},
	    {"id":"p3","account":"aa-1","asset":"usdc","amount":1},
	    {"id":"p4","account":"aa-1","asset":"eth","amount":50}
	  ]}`
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, batch)
	if r.code != 0 {
		t.Fatalf("submit code=%d err=%s", r.code, r.err)
	}
	var res struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(res.Results))
	for i, x := range res.Results {
		got[i] = x.Status
	}
	want := []string{"settled", "settled", "limit_exceeded", "settled"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	for _, frag := range []string{"max_charged 1000", "used 1000", "charges 1"} {
		if !strings.Contains(res.Results[2].Reason, frag) {
			t.Fatalf("limit reason %q missing %q", res.Results[2].Reason, frag)
		}
	}

	// 上限仅对本次提交有效：下一批次从零计算，可再扣 1000。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":0,
	  "limits":[{"account":"aa-1","asset":"usdc","max_charged":1000}],
	  "intents":[{"id":"p5","account":"aa-1","asset":"usdc","amount":900}]}`)
	if r.code != 0 || !strings.Contains(r.out, `"settled"`) {
		t.Fatalf("limit must reset per submission: code=%d out=%s err=%s", r.code, r.out, r.err)
	}

	// 非法限额：整个批次拒绝，退出码 1，invalid_parameter，任何意图都不执行。
	badLimits := []string{
		`{"fee_bps":0,"limits":[{"account":"","asset":"usdc","max_charged":1}],"intents":[]}`,
		`{"fee_bps":0,"limits":[{"account":"aa-1","asset":"usdc"}],"intents":[]}`,
		`{"fee_bps":0,"limits":[{"account":"aa-1","asset":"usdc","max_charged":null}],"intents":[]}`,
		`{"fee_bps":0,"limits":[{"account":"aa-1","asset":"usdc","max_charged":-1}],"intents":[]}`,
		`{"fee_bps":0,"limits":[{"account":"aa-1","asset":"usdc","max_charged":1.5}],"intents":[]}`,
		`{"fee_bps":0,"limits":[{"account":"aa-1","asset":"usdc","max_charged":1},{"account":"aa-1","asset":"usdc","max_charged":2}],"intents":[]}`,
		// 空意图列表也要检查限额。
		`{"fee_bps":0,"limits":[{"account":"aa-1","asset":"","max_charged":1}],"intents":[]}`,
	}
	for _, bad := range badLimits {
		r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, bad)
		if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
			t.Fatalf("bad limits %s: code=%d err=%s", bad, r.code, r.err)
		}
	}
	// 带意图的非法限额批次同样整批拒绝。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath}, `{"fee_bps":0,
	  "limits":[{"account":"aa-1","asset":"usdc","max_charged":-1}],
	  "intents":[{"id":"p6","account":"aa-1","asset":"usdc","amount":1}]}`)
	if r.code != 1 {
		t.Fatalf("bad limits with intents: code=%d", r.code)
	}

	// 非法批次没有执行任何意图：余额与记录只来自前两次成功提交。
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query code=%d", r.code)
	}
	var snap struct {
		Balances []struct {
			Account string `json:"account"`
			Asset   string `json:"asset"`
			Balance int64  `json:"balance"`
		} `json:"balances"`
		Settlements []struct {
			ID string `json:"id"`
		} `json:"settlements"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Settlements) != 4 {
		t.Fatalf("settlements=%+v want 4 (p1, p2, p4, p5)", snap.Settlements)
	}
	if snap.Balances[0].Balance != 50 || snap.Balances[1].Balance != 100 {
		t.Fatalf("balances=%+v want usdc 50, eth 100", snap.Balances)
	}
}

// ---- submit --dry-run 预览 ----

func TestCLISubmitDryRun(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`)
	if r.code != 0 {
		t.Fatalf("init code=%d err=%s", r.code, r.err)
	}

	// 预览：dry_run:true，结果按输入顺序；后项看到前项预计成功后的余额。
	batch := `{"fee_bps":0,"intents":[
	  {"id":"p1","account":"aa-1","asset":"usdc","amount":70},
	  {"id":"p2","account":"aa-1","asset":"usdc","amount":40}
	]}`
	r = runCLIWith(t, []string{"submit", "--dry-run", "-l", ledgerPath}, batch)
	if r.code != 0 {
		t.Fatalf("dry-run code=%d err=%s", r.code, r.err)
	}
	var res struct {
		DryRun  bool `json:"dry_run"`
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Record *struct {
				Seq int64 `json:"seq"`
			} `json:"record"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Fatalf("dry_run must be true: %s", r.out)
	}
	want := []string{"settled", "insufficient_balance"}
	got := make([]string, len(res.Results))
	for i, x := range res.Results {
		got[i] = x.Status
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if res.Results[0].Record == nil || res.Results[0].Record.Seq != 1 {
		t.Fatalf("predicted record seq=%+v", res.Results[0].Record)
	}

	// 预览不写账本：余额与记录不变。
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query code=%d", r.code)
	}
	var snap struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
		Settlements []struct {
			ID string `json:"id"`
		} `json:"settlements"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 100 {
		t.Fatalf("balance=%+v want 100", snap.Balances)
	}
	if len(snap.Settlements) != 0 {
		t.Fatalf("settlements=%+v want empty", snap.Settlements)
	}

	// 随后实际提交仍按首次提交处理。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath},
		`{"fee_bps":0,"intents":[{"id":"p1","account":"aa-1","asset":"usdc","amount":70}]}`)
	if r.code != 0 || !strings.Contains(r.out, `"settled"`) {
		t.Fatalf("actual submit after dry-run: code=%d out=%s err=%s", r.code, r.out, r.err)
	}
	r = runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Balances[0].Balance != 30 || len(snap.Settlements) != 1 {
		t.Fatalf("after actual submit: balances=%+v settlements=%+v", snap.Balances, snap.Settlements)
	}
}

func TestCLISubmitDryRunEmptyBatch(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`)
	if r.code != 0 {
		t.Fatalf("init code=%d err=%s", r.code, r.err)
	}

	// 合法空批次：返回带预览标记的空结果列表。
	r = runCLIWith(t, []string{"submit", "--dry-run", "-l", ledgerPath}, `{"fee_bps":0,"intents":[]}`)
	if r.code != 0 {
		t.Fatalf("dry-run empty code=%d err=%s", r.code, r.err)
	}
	if strings.TrimSpace(r.out) != `{
  "dry_run": true,
  "results": []
}` {
		t.Fatalf("empty dry-run out=%q", r.out)
	}

	// 空批次也检查费率：非法费率退出码 1，不输出部分结果。
	r = runCLIWith(t, []string{"submit", "--dry-run", "-l", ledgerPath}, `{"fee_bps":-1,"intents":[]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("bad fee dry-run: code=%d err=%s", r.code, r.err)
	}
	// 空批次也检查限额。
	r = runCLIWith(t, []string{"submit", "--dry-run", "-l", ledgerPath},
		`{"fee_bps":0,"limits":[{"account":"aa-1","asset":"usdc","max_charged":-1}],"intents":[]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("bad limits dry-run: code=%d err=%s", r.code, r.err)
	}
}

func TestCLISubmitDryRunInvalidJSON(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`)
	if r.code != 0 {
		t.Fatalf("init code=%d err=%s", r.code, r.err)
	}

	// 非法 JSON：批次级错误，退出码 1，不输出部分结果。
	r = runCLIWith(t, []string{"submit", "--dry-run", "-l", ledgerPath}, `{not json`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "invalid_parameter" {
		t.Fatalf("invalid JSON dry-run: code=%d err=%s", r.code, r.err)
	}
	if r.out != "" {
		t.Fatalf("invalid JSON must not produce partial output: %q", r.out)
	}
}

func TestCLISubmitDryRunMissingLedger(t *testing.T) {
	missing := t.TempDir() + "/nope.json"
	r := runCLIWith(t, []string{"submit", "--dry-run", "-l", missing}, `{"fee_bps":0,"intents":[]}`)
	if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "ledger_not_initialized" {
		t.Fatalf("missing ledger dry-run: code=%d err=%s", r.code, r.err)
	}
	// 预览不创建账本。
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create ledger: stat err=%v", err)
	}
}

func TestCLISubmitWithoutDryRunUnchanged(t *testing.T) {
	ledgerPath := t.TempDir() + "/ledger.json"
	r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":100}]}`)
	if r.code != 0 {
		t.Fatalf("init code=%d err=%s", r.code, r.err)
	}

	// 未启用预览：输出不含 dry_run 标记，格式与原来一致。
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath},
		`{"fee_bps":0,"intents":[{"id":"p1","account":"aa-1","asset":"usdc","amount":70}]}`)
	if r.code != 0 {
		t.Fatalf("submit code=%d err=%s", r.code, r.err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(r.out), &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["dry_run"]; present {
		t.Fatalf("normal submit must not carry dry_run: %s", r.out)
	}
	if _, present := raw["results"]; !present {
		t.Fatalf("normal submit must carry results: %s", r.out)
	}
}

func TestCLIHelpListsDryRun(t *testing.T) {
	r := runCLIWith(t, []string{"help"}, "")
	if r.code != 0 {
		t.Fatalf("help code=%d err=%s", r.code, r.err)
	}
	if !strings.Contains(r.out, "--dry-run") {
		t.Fatalf("help must list --dry-run: %s", r.out)
	}
}
