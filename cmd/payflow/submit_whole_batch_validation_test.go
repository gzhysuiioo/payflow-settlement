package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// 本文件为 submit 的“整份 JSON 输入合法才开始执行”提供回归保障。
//
// 支付批次必须在完整解析（含文档后不得拼接第二个 JSON 文档）通过之后才打开
// 账本执行：即使第一项是一笔本可成功的付款，只要输入靠后的位置仍有类型错误，
// 整批都必须返回 invalid_parameter、退出码 1，第一项不得扣款、不得留下成功
// 记录、不得占用编号，账本文件逐字节保持原样。这区别于“批次结构合法、逐项
// 业务失败”（例如金额为整数 0）：后者退出码 0、结果逐项返回，前项成功保留。
//
// 基础账本带既有成功付款与退款历史：p0（100、0bps，seq=1）已由 r0 全额退回，
// aa-1/usdc 当前余额回到 2000。因此新付款 p1（1500、30bps）首次成功时
// 手续费 4、扣款 1504、余额 496，成功序号必须紧接 p0 为 2。

// atomicSeedP1Batch 是被拒批次的第一项（也是随后单独重提的合法付款）：
// p1，1500，30bps，手续费 4、扣款总额 1504。
const atomicSeedP1Batch = `{"fee_bps":30,"intents":[
  {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1}
]}`

// setupAtomicSubmitLedger 准备带既有成功付款 p0 与退款 r0 的账本：
// 余额 2000，结算历史仅 p0（seq=1、charged=100），退款历史仅 r0。
func setupAtomicSubmitLedger(t *testing.T) string {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath},
		`{"fee_bps":0,"intents":[
		  {"id":"p0","account":"aa-1","paymaster":"pm-0","asset":"usdc","amount":100}
		]}`); r.code != 0 {
		t.Fatalf("seed payment: %s", r.err)
	}
	if r := runCLIWith(t, []string{"refund", "-l", ledgerPath},
		`{"refunds":[{"id":"r0","settlement_id":"p0","reason":"seed refund"}]}`); r.code != 0 {
		t.Fatalf("seed refund: %s", r.err)
	}
	return ledgerPath
}

// submitBatchFrom 从 stdin（inputFile==""）或 -f 文件提交同一内容。
func submitBatchFrom(t *testing.T, ledgerPath, inputFile, body string) cliResult {
	t.Helper()
	if inputFile == "" {
		return runCLIWith(t, []string{"submit", "-l", ledgerPath}, body)
	}
	if err := os.WriteFile(inputFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return runCLIWith(t, []string{"submit", "-l", ledgerPath, "-f", inputFile}, "")
}

// assertAtomicLedgerUnchanged 断言账本仍是“p0 已结算、r0 已退款、余额 2000”
// 的提交前状态，且账本文件与 before 逐字节相同：被拒批次没有扣款、没有新增
// 结算或退款、没有触发任何落盘。
func assertAtomicLedgerUnchanged(t *testing.T, ledgerPath string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("rejected batch rewrote the ledger file\nbefore=%s\nafter =%s", before, after)
	}
	r := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	if r.code != 0 {
		t.Fatalf("query: %s", r.err)
	}
	var snap struct {
		Balances []struct {
			Account string `json:"account"`
			Asset   string `json:"asset"`
			Balance int64  `json:"balance"`
		} `json:"balances"`
		Settlements []struct {
			ID      string `json:"id"`
			Charged int64  `json:"charged"`
			Seq     int64  `json:"seq"`
		} `json:"settlements"`
		Refunds []struct {
			ID           string `json:"id"`
			SettlementID string `json:"settlement_id"`
		} `json:"refunds"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Account != "aa-1" ||
		snap.Balances[0].Asset != "usdc" || snap.Balances[0].Balance != 2000 {
		t.Fatalf("balance changed by rejected batch: %+v", snap.Balances)
	}
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p0" ||
		snap.Settlements[0].Seq != 1 || snap.Settlements[0].Charged != 100 {
		t.Fatalf("rejected batch left a settlement (history must keep only p0): %+v", snap.Settlements)
	}
	if len(snap.Refunds) != 1 || snap.Refunds[0].ID != "r0" ||
		snap.Refunds[0].SettlementID != "p0" {
		t.Fatalf("rejected batch touched refund history: %+v", snap.Refunds)
	}
}

// TestCLISubmitMalformedBatchExecutesNothing 锁定核心回归：第一项合法可成功、
// 错误出现在输入靠后位置时，不能因为先读到了合法意图就扣款或落记录。
// 两类整批输入错误（后项 amount 写成 JSON 字符串；完整批次后再接第二个 JSON
// 文档）分别从标准输入与 -f 文件入口提交，都必须：
//   - 退出码 1，stderr 是既有 invalid_parameter 错误信封，stdout 完全为空；
//   - 不扣 p1 的款、不新增结算、账本文件逐字节不变；
//   - 不占用 p1：随后单独提交同一合法付款按首次付款 settled，手续费 4、
//     扣款 1504、余额 496、成功序号紧接 p0 为 2（非 duplicate、无空缺）。
func TestCLISubmitMalformedBatchExecutesNothing(t *testing.T) {
	badBatches := []struct {
		name  string
		input string
	}{
		{
			name: "later amount is a JSON string",
			input: `{"fee_bps":30,"intents":[
  {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1},
  {"id":"p2","account":"aa-1","asset":"usdc","amount":"1500"}
]}`},
		{
			name: "second JSON document after a complete batch",
			input: `{"fee_bps":30,"intents":[
  {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1}
]}{"fee_bps":0,"intents":[]}`,
		},
	}
	entries := []struct {
		name     string
		fromFile bool
	}{
		{"stdin", false},
		{"file", true},
	}

	for _, bad := range badBatches {
		for _, entry := range entries {
			t.Run(bad.name+"/"+entry.name, func(t *testing.T) {
				ledgerPath := setupAtomicSubmitLedger(t)
				before, err := os.ReadFile(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				inputFile := ""
				if entry.fromFile {
					inputFile = t.TempDir() + "/batch.json"
				}

				// 整批拒绝：退出码 1、stdout 为空、stderr 是既有错误信封。
				r := submitBatchFrom(t, ledgerPath, inputFile, bad.input)
				if r.code != 1 {
					t.Fatalf("reject code=%d want 1 (out=%q)", r.code, r.out)
				}
				if r.out != "" {
					t.Fatalf("rejected batch must print nothing to stdout: %q", r.out)
				}
				if env := decodeErrEnvelope(t, r.err); env["kind"] != "invalid_parameter" {
					t.Fatalf("reject envelope=%s", r.err)
				}

				// 第一项不得扣款，原有余额、付款/退款历史与账本文件保持原样。
				assertAtomicLedgerUnchanged(t, ledgerPath, before)

				// 被拒批次不能占用 p1：同一合法付款随后单独提交按首次付款结算。
				// 两种入口遵守相同规则：被拒从 -f 进入时，重提也从 -f 进入。
				r = submitBatchFrom(t, ledgerPath, inputFile, atomicSeedP1Batch)
				if r.code != 0 {
					t.Fatalf("resubmit code=%d err=%s", r.code, r.err)
				}
				var res struct {
					Results []struct {
						ID     string `json:"id"`
						Status string `json:"status"`
						Record *struct {
							Amount  int64 `json:"amount"`
							FeeBps  int   `json:"fee_bps"`
							Fee     int64 `json:"fee"`
							Charged int64 `json:"charged"`
							Seq     int64 `json:"seq"`
						} `json:"record"`
					} `json:"results"`
				}
				if err := json.Unmarshal([]byte(r.out), &res); err != nil {
					t.Fatal(err)
				}
				if len(res.Results) != 1 {
					t.Fatalf("resubmit results=%s", r.out)
				}
				got := res.Results[0]
				if got.ID != "p1" || got.Status != "settled" || got.Record == nil {
					t.Fatalf("p1 must settle as first payment, not duplicate: %+v", got)
				}
				if got.Record.Amount != 1500 || got.Record.FeeBps != 30 || got.Record.Fee != 4 ||
					got.Record.Charged != 1504 || got.Record.Seq != 2 {
					t.Fatalf("p1 settlement record=%+v want amount 1500 fee_bps 30 fee 4 charged 1504 seq 2",
						got.Record)
				}

				// 结算后余额 496，历史为 p0(seq=1)、p1(seq=2) 紧接无空缺，退款仍只有 r0。
				qr := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
				var snap struct {
					Balances []struct {
						Balance int64 `json:"balance"`
					} `json:"balances"`
					Settlements []struct {
						ID  string `json:"id"`
						Seq int64  `json:"seq"`
					} `json:"settlements"`
					Refunds []struct {
						ID string `json:"id"`
					} `json:"refunds"`
				}
				if err := json.Unmarshal([]byte(qr.out), &snap); err != nil {
					t.Fatal(err)
				}
				if len(snap.Balances) != 1 || snap.Balances[0].Balance != 496 {
					t.Fatalf("balance after p1=%+v want 496", snap.Balances)
				}
				if len(snap.Settlements) != 2 ||
					snap.Settlements[0].ID != "p0" || snap.Settlements[0].Seq != 1 ||
					snap.Settlements[1].ID != "p1" || snap.Settlements[1].Seq != 2 {
					t.Fatalf("settlements=%+v want p0 seq 1 then p1 seq 2", snap.Settlements)
				}
				if len(snap.Refunds) != 1 || snap.Refunds[0].ID != "r0" {
					t.Fatalf("refunds=%+v want only r0", snap.Refunds)
				}
			})
		}
	}
}

// TestCLISubmitTrailingWhitespaceStillExecutes 区分“尾随空白”与“第二个文档”：
// 完整 JSON 之后只有空格、制表符或换行时必须正常提交，从 stdin 与 -f 入口
// 都一样；p1 仍按首次付款 settled（seq=2、charged 1504、余额 496）。
func TestCLISubmitTrailingWhitespaceStillExecutes(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
	}{
		{"spaces newline tab", "  \n\t\n"},
		{"newlines then spaces", "\n\n  "},
		{"space tab space", " \t "},
	}
	for _, tc := range cases {
		for _, fromFile := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				ledgerPath := setupAtomicSubmitLedger(t)
				inputFile := ""
				if fromFile {
					inputFile = t.TempDir() + "/batch.json"
				}
				r := submitBatchFrom(t, ledgerPath, inputFile, atomicSeedP1Batch+tc.suffix)
				if r.code != 0 {
					t.Fatalf("trailing whitespace must submit (fromFile=%v): %s", fromFile, r.err)
				}
				var res struct {
					Results []struct {
						Status string `json:"status"`
						Record *struct {
							Charged int64 `json:"charged"`
							Seq     int64 `json:"seq"`
						} `json:"record"`
					} `json:"results"`
				}
				if err := json.Unmarshal([]byte(r.out), &res); err != nil {
					t.Fatal(err)
				}
				if len(res.Results) != 1 || res.Results[0].Status != "settled" ||
					res.Results[0].Record == nil ||
					res.Results[0].Record.Charged != 1504 || res.Results[0].Record.Seq != 2 {
					t.Fatalf("trailing whitespace result=%s", r.out)
				}
				qr := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
				var snap struct {
					Balances []struct {
						Balance int64 `json:"balance"`
					} `json:"balances"`
					Settlements []struct {
						ID string `json:"id"`
					} `json:"settlements"`
				}
				if err := json.Unmarshal([]byte(qr.out), &snap); err != nil {
					t.Fatal(err)
				}
				if snap.Balances[0].Balance != 496 || len(snap.Settlements) != 2 {
					t.Fatalf("after whitespace submit: balance=%+v settlements=%+v",
						snap.Balances, snap.Settlements)
				}
			})
		}
	}
}

// TestCLISubmitZeroAmountIsPerItemFailureNotBatchRejection 是对照场景：
// 批次 JSON 与字段类型都合法，只是后项金额明确为整数 0。整批退出 0，
// 结果按输入顺序为第一项 settled、后项 invalid_parameter；前项扣款与记录
// 保留（余额 496、p1 seq=2），后项不扣款、不留成功记录，编号也不被占用。
func TestCLISubmitZeroAmountIsPerItemFailureNotBatchRejection(t *testing.T) {
	ledgerPath := setupAtomicSubmitLedger(t)
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"fee_bps":30,"intents":[
  {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1},
  {"id":"p2","account":"aa-1","asset":"usdc","amount":0}
]}`
	r := runCLIWith(t, []string{"submit", "-l", ledgerPath}, body)
	if r.code != 0 {
		t.Fatalf("per-item failures must exit 0: code=%d err=%s", r.code, r.err)
	}
	var res struct {
		Results []struct {
			ID     string  `json:"id"`
			Status string  `json:"status"`
			Record *struct {
				Fee     int64 `json:"fee"`
				Charged int64 `json:"charged"`
				Seq     int64 `json:"seq"`
			} `json:"record"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results=%s", r.out)
	}
	if res.Results[0].ID != "p1" || res.Results[0].Status != "settled" ||
		res.Results[0].Record == nil ||
		res.Results[0].Record.Fee != 4 || res.Results[0].Record.Charged != 1504 ||
		res.Results[0].Record.Seq != 2 {
		t.Fatalf("leading item must settle: %+v", res.Results[0])
	}
	if res.Results[1].ID != "p2" || res.Results[1].Status != "invalid_parameter" ||
		res.Results[1].Record != nil {
		t.Fatalf("zero amount must be a per-item invalid_parameter without record: %+v", res.Results[1])
	}

	// 前项扣款与记录保留；后项不留成功记录，账本只有 p0、p1 两笔，余额 496。
	qr := runCLIWith(t, []string{"query", "-l", ledgerPath}, "")
	var snap struct {
		Balances []struct {
			Balance int64 `json:"balance"`
		} `json:"balances"`
		Settlements []struct {
			ID  string `json:"id"`
			Seq int64  `json:"seq"`
		} `json:"settlements"`
	}
	if err := json.Unmarshal([]byte(qr.out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 496 {
		t.Fatalf("balance=%+v want 496 (p1 charged, p2 did not)", snap.Balances)
	}
	if len(snap.Settlements) != 2 ||
		snap.Settlements[0].ID != "p0" || snap.Settlements[1].ID != "p1" {
		t.Fatalf("settlements=%+v want p0 and p1 only (zero-amount p2 leaves none)",
			snap.Settlements)
	}

	// 前项成功必然改写文件（与整批拒绝的文件不变形成对照），但后项失败不留痕：
	// p2 编号未被占用，随后合法重提按首次付款 settled，序号紧接为 3。
	if bytes.Equal(before, mustReadFile(t, ledgerPath)) {
		t.Fatalf("a settled leading item must persist, but the ledger file is unchanged")
	}
	r = runCLIWith(t, []string{"submit", "-l", ledgerPath},
		`{"fee_bps":0,"intents":[
		  {"id":"p2","account":"aa-1","asset":"usdc","amount":10}
		]}`)
	if r.code != 0 {
		t.Fatalf("p2 retry: %s", r.err)
	}
	if err := json.Unmarshal([]byte(r.out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || res.Results[0].Status != "settled" ||
		res.Results[0].Record == nil ||
		res.Results[0].Record.Charged != 10 || res.Results[0].Record.Seq != 3 {
		t.Fatalf("rejected p2 id must be reusable, result=%s", r.out)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
