package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// 本组回归测试锁定 refund 的“整份 JSON 输入合法后才开始执行”语义：
// 批次级输入错误（字段类型错误、JSON 文档之后还有第二个文档或无法解析的
// 非空内容）必须在打开账本、执行任何退款之前整批拒绝——即使第一项本可以
// 成功，也不得提前退回、不得留下退款记录或占用退款编号。这与逐项业务失败
// （原因为空、目标付款不存在等）严格区分：后者退出 0、按输入顺序逐项给出
// 结果，其他合法退款照常入账。
//
// 两个提交入口（标准输入与 -f/--file 文件）必须遵守完全相同的规则。

// 合法退款批次：r1 全额退回 p1（含手续费，共 1504）。
const atomicValidR1Batch = `{"refunds":[
  {"id":"r1","settlement_id":"p1","reason":"customer cancelled"}
]}`

// 批次级非法输入一：第一项是能够成功的合法 r1，后一项的 reason 写成 JSON
// 数字——encoding/json 解码整个批次时即失败，任何退款都不得开始执行。
const atomicNumericReasonBatch = `{"refunds":[
  {"id":"r1","settlement_id":"p1","reason":"customer cancelled"},
  {"id":"r2","settlement_id":"p1","reason":42}
]}`

// 批次级非法输入二：批次本身完整合法，但结尾紧接着第二个 JSON 文档。
const atomicRefundTrailingDocBatch = atomicValidR1Batch + `{"refunds":[]}`

// 批次级非法输入三：批次本身完整合法，但结尾附有无法解析的非空内容。
const atomicRefundTrailingGarbageBatch = atomicValidR1Batch + `not json`

// setupRefundBatchLedger 准备一个已有一笔未退款成功付款的账本：
// aa-1/usdc 初始余额 2000，p1 按 30 基点付款 1500（手续费 4、扣款 1504），
// 当前余额 496，无任何退款记录。返回账本路径与提交前的账本文件字节
// （用于断言文件逐字节不变）。
func setupRefundBatchLedger(t *testing.T) (string, []byte) {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath},
		`{"fee_bps":30,"intents":[
		  {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1}
		]}`); r.code != 0 {
		t.Fatalf("seed payment: %s", r.err)
	}
	baseline, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	return ledgerPath, baseline
}

// refundBatchSnapshot 是退款相关账本状态的只读快照。
type refundBatchSnapshot struct {
	balance     int64
	settlements []refundBatchSettlement
	refunds     []refundBatchRefund
}

type refundBatchSettlement struct {
	ID      string
	Account string
	Asset   string
	Amount  int64
	Fee     int64
	Charged int64
	Seq     int64
}

type refundBatchRefund struct {
	ID           string
	SettlementID string
	Reason       string
	Account      string
	Asset        string
	Amount       int64
	Fee          int64
	Charged      int64
	Seq          int64
}

// queryRefundBatch 读回账本当前状态：aa-1/usdc 余额、按成功顺序的付款记录、
// 按成功顺序的退款记录。提交前的基线必须是余额 496、付款 [p1(seq1)]、无退款。
func queryRefundBatch(t *testing.T, ledgerPath string) refundBatchSnapshot {
	t.Helper()
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
			Account string `json:"account"`
			Asset   string `json:"asset"`
			Amount  int64  `json:"amount"`
			Fee     int64  `json:"fee"`
			Charged int64  `json:"charged"`
			Seq     int64  `json:"seq"`
		} `json:"settlements"`
		Refunds []struct {
			ID           string `json:"id"`
			SettlementID string `json:"settlement_id"`
			Reason       string `json:"reason"`
			Account      string `json:"account"`
			Asset        string `json:"asset"`
			Amount       int64  `json:"amount"`
			Fee          int64  `json:"fee"`
			Charged      int64  `json:"charged"`
			Seq          int64  `json:"seq"`
		} `json:"refunds"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatalf("decode query: %v\n%s", err, r.out)
	}
	var got refundBatchSnapshot
	for _, b := range snap.Balances {
		if b.Account == "aa-1" && b.Asset == "usdc" {
			got.balance = b.Balance
		}
	}
	for _, s := range snap.Settlements {
		got.settlements = append(got.settlements, refundBatchSettlement{
			ID: s.ID, Account: s.Account, Asset: s.Asset,
			Amount: s.Amount, Fee: s.Fee, Charged: s.Charged, Seq: s.Seq,
		})
	}
	for _, rf := range snap.Refunds {
		got.refunds = append(got.refunds, refundBatchRefund{
			ID: rf.ID, SettlementID: rf.SettlementID, Reason: rf.Reason,
			Account: rf.Account, Asset: rf.Asset,
			Amount: rf.Amount, Fee: rf.Fee, Charged: rf.Charged, Seq: rf.Seq,
		})
	}
	return got
}

// untouchedBaseline 断言账本保持提交前状态：余额 496、付款记录 p1 原样
// （未被改写、未被标记退款）、没有任何退款记录。
func (s refundBatchSnapshot) untouchedBaseline(t *testing.T) {
	t.Helper()
	if s.balance != 496 {
		t.Fatalf("balance changed: %d want 496", s.balance)
	}
	if len(s.settlements) != 1 {
		t.Fatalf("settlement history changed: %+v want [p1]", s.settlements)
	}
	p1 := s.settlements[0]
	if p1.ID != "p1" || p1.Account != "aa-1" || p1.Asset != "usdc" ||
		p1.Amount != 1500 || p1.Fee != 4 || p1.Charged != 1504 || p1.Seq != 1 {
		t.Fatalf("payment record p1 rewritten: %+v", p1)
	}
	if len(s.refunds) != 0 {
		t.Fatalf("refund history must stay empty, got %+v", s.refunds)
	}
}

// refundAtomic 通过指定入口提交退款批次：entry 为 "stdin" 走标准输入，
// 为 "file" 走 -f/--file 文件（标准输入给空内容，不得影响结果）。
func refundAtomic(t *testing.T, entry, ledgerPath, body string) cliResult {
	t.Helper()
	if entry == "stdin" {
		return runCLIWith(t, []string{"refund", "-l", ledgerPath}, body)
	}
	inputPath := t.TempDir() + "/refunds.json"
	if err := os.WriteFile(inputPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return runCLIWith(t, []string{"refund", "-l", ledgerPath, "-f", inputPath}, "")
}

// decodeRefundResults 解码退款批次的逐项结果。
func decodeRefundResults(t *testing.T, out string) []struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	SettlementID string `json:"settlement_id"`
	Account      string `json:"account"`
	Asset        string `json:"asset"`
	Charged      int64  `json:"charged"`
	Record       *struct {
		ID           string `json:"id"`
		SettlementID string `json:"settlement_id"`
		Reason       string `json:"reason"`
		Account      string `json:"account"`
		Asset        string `json:"asset"`
		Amount       int64  `json:"amount"`
		Fee          int64  `json:"fee"`
		Charged      int64  `json:"charged"`
		Seq          int64  `json:"seq"`
	} `json:"record"`
} {
	t.Helper()
	var res struct {
		Results []struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			Reason       string `json:"reason"`
			SettlementID string `json:"settlement_id"`
			Account      string `json:"account"`
			Asset        string `json:"asset"`
			Charged      int64  `json:"charged"`
			Record       *struct {
				ID           string `json:"id"`
				SettlementID string `json:"settlement_id"`
				Reason       string `json:"reason"`
				Account      string `json:"account"`
				Asset        string `json:"asset"`
				Amount       int64  `json:"amount"`
				Fee          int64  `json:"fee"`
				Charged      int64  `json:"charged"`
				Seq          int64  `json:"seq"`
			} `json:"record"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode refund results: %v\n%s", err, out)
	}
	return res.Results
}

// TestCLIRefundMalformedBatchExecutesNothing 覆盖核心回归点：
// 第一项本可成功、错误出现在后面（或文档之后还有内容）的整批非法输入，
// 必须整批拒绝且不留任何痕迹。标准输入与文件入口、三种非法形态各跑一遍，
// 且每次都从相同的提交前账本开始；随后在被拒绝的同一账本上用第一项原来的
// 退款编号、付款编号和原因重新提交，验证编号未被占用、可首次成功退款。
func TestCLIRefundMalformedBatchExecutesNothing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"later_reason_is_number", atomicNumericReasonBatch},
		{"second_json_document", atomicRefundTrailingDocBatch},
		{"trailing_garbage", atomicRefundTrailingGarbageBatch},
	}
	entries := []string{"stdin", "file"}

	for _, tc := range cases {
		for _, entry := range entries {
			t.Run(tc.name+"_via_"+entry, func(t *testing.T) {
				ledgerPath, baseline := setupRefundBatchLedger(t)

				r := refundAtomic(t, entry, ledgerPath, tc.body)

				// 整批返回 invalid_parameter，退出码 1；既有错误信封只在标准错误，
				// 标准输出必须为空——不能先读到合法退款项就输出部分结果。
				if r.code != 1 {
					t.Fatalf("exit code=%d want 1", r.code)
				}
				if r.out != "" {
					t.Fatalf("stdout must be empty for a batch-level error, got: %q", r.out)
				}
				env := decodeErrEnvelope(t, r.err)
				if env["kind"] != "invalid_parameter" {
					t.Fatalf("error kind=%v want invalid_parameter: %s", env["kind"], r.err)
				}

				// 第一项不得提前退回 1504：余额仍为 496，付款记录与退款历史
				// （空）保持原样。
				queryRefundBatch(t, ledgerPath).untouchedBaseline(t)

				// 账本文件逐字节不变（没有发生任何落盘）。
				after, err := os.ReadFile(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(baseline, after) {
					t.Fatalf("ledger file changed by rejected batch")
				}

				// 被拒绝的批次不占用 r1：去掉错误内容后用第一项原来的退款编号、
				// 付款编号和原因重新提交，必须首次成功 refunded（不是 duplicate
				// 或 already_refunded）。
				r = refundAtomic(t, entry, ledgerPath, atomicValidR1Batch)
				if r.code != 0 {
					t.Fatalf("valid r1 after rejected batch: code=%d err=%s", r.code, r.err)
				}
				results := decodeRefundResults(t, r.out)
				if len(results) != 1 {
					t.Fatalf("want exactly 1 result, got %d: %s", len(results), r.out)
				}
				item := results[0]
				if item.ID != "r1" || item.Status != "refunded" || item.Record == nil {
					t.Fatalf("r1 must refund for the first time: %+v", item)
				}
				// 成功结果可追溯原付款：原账户、资产、退款总额（含手续费 1504）
				// 与原因。
				if item.SettlementID != "p1" || item.Account != "aa-1" ||
					item.Asset != "usdc" || item.Charged != 1504 ||
					item.Reason != "customer cancelled" {
					t.Fatalf("refund result missing trace fields: %+v", item)
				}

				// 余额恢复为 2000，恰好新增一条退款记录，退回金额包含原手续费；
				// 原付款记录不被改写。结果详情与新增退款记录一致。
				snap := queryRefundBatch(t, ledgerPath)
				if snap.balance != 2000 {
					t.Fatalf("balance after r1=%d want 2000", snap.balance)
				}
				if len(snap.settlements) != 1 {
					t.Fatalf("settlements=%+v want [p1]", snap.settlements)
				}
				p1 := snap.settlements[0]
				if p1.ID != "p1" || p1.Amount != 1500 || p1.Fee != 4 ||
					p1.Charged != 1504 || p1.Seq != 1 {
					t.Fatalf("payment record p1 rewritten: %+v", p1)
				}
				if len(snap.refunds) != 1 {
					t.Fatalf("want exactly 1 refund record, got %+v", snap.refunds)
				}
				rec := snap.refunds[0]
				if rec.ID != "r1" || rec.SettlementID != "p1" ||
					rec.Reason != "customer cancelled" ||
					rec.Account != "aa-1" || rec.Asset != "usdc" ||
					rec.Amount != 1500 || rec.Fee != 4 || rec.Charged != 1504 ||
					rec.Seq != 1 {
					t.Fatalf("refund record=%+v, want r1/p1 1500+4=1504 seq 1", rec)
				}
				if item.Record.ID != rec.ID || item.Record.SettlementID != rec.SettlementID ||
					item.Record.Reason != rec.Reason || item.Record.Account != rec.Account ||
					item.Record.Asset != rec.Asset || item.Record.Amount != rec.Amount ||
					item.Record.Fee != rec.Fee || item.Record.Charged != rec.Charged ||
					item.Record.Seq != rec.Seq {
					t.Fatalf("result record %+v disagrees with ledger record %+v", item.Record, rec)
				}

				// 再次提交相同 r1 现在必须是 duplicate 且不二次入账，
				// 反向证明上一笔是首次成功而非被拒绝批次的残留记录。
				r = refundAtomic(t, entry, ledgerPath, atomicValidR1Batch)
				if r.code != 0 {
					t.Fatalf("duplicate resubmit code=%d err=%s", r.code, r.err)
				}
				results = decodeRefundResults(t, r.out)
				if len(results) != 1 || results[0].Status != "duplicate" {
					t.Fatalf("second r1 must be duplicate: %s", r.out)
				}
				if snap := queryRefundBatch(t, ledgerPath); snap.balance != 2000 ||
					len(snap.refunds) != 1 {
					t.Fatalf("duplicate resubmit refunded again: %+v", snap)
				}
			})
		}
	}
}

// TestCLIRefundAcceptsTrailingWhitespace 锁定“完整 JSON 之后只有空白”与
// “第二个 JSON 文档或垃圾内容”的区别：空格、制表符、换行（含 CRLF）是合法
// 尾随空白，批次必须正常退款，不能与尾随内容混淆。两个入口行为一致。
func TestCLIRefundAcceptsTrailingWhitespace(t *testing.T) {
	suffixes := []string{
		"   ",
		"\n",
		"\r\n",
		" \t\n  \r\n\t",
	}
	entries := []string{"stdin", "file"}
	for _, suffix := range suffixes {
		for _, entry := range entries {
			t.Run("whitespace_via_"+entry, func(t *testing.T) {
				ledgerPath, _ := setupRefundBatchLedger(t)
				r := refundAtomic(t, entry, ledgerPath, atomicValidR1Batch+suffix)
				if r.code != 0 {
					t.Fatalf("trailing whitespace must be accepted: code=%d err=%s", r.code, r.err)
				}
				results := decodeRefundResults(t, r.out)
				if len(results) != 1 || results[0].Status != "refunded" ||
					results[0].Record == nil || results[0].Record.Seq != 1 {
					t.Fatalf("whitespace-suffixed batch must refund r1 at seq 1: %s", r.out)
				}
				if snap := queryRefundBatch(t, ledgerPath); snap.balance != 2000 {
					t.Fatalf("balance=%d want 2000", snap.balance)
				}
			})
		}
	}
}

// TestCLIRefundPerItemFailuresDoNotAbortBatch 是对照场景：整份 JSON 可解析、
// 字段类型都合法，仅某些项业务上失败（原因为空、目标付款不存在）时属于
// 逐项失败而非整批输入错误——整批退出 0，结果按输入顺序逐项报告
// invalid_parameter / not_found，其他合法退款照常成功。成功项增加相应余额
// 并留下退款记录，失败项不增加余额、不留记录；后面的失败不撤销前面的成功，
// 前面的失败也不阻止后面的合法退款。
func TestCLIRefundPerItemFailuresDoNotAbortBatch(t *testing.T) {
	for _, entry := range []string{"stdin", "file"} {
		t.Run("per_item_via_"+entry, func(t *testing.T) {
			// 两笔成功付款：p1 扣 1504（含手续费 4）、p2 扣 100（费率 0），
			// 余额 2000-1504-100=396。
			ledgerPath := t.TempDir() + "/ledger.json"
			if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
				`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
				t.Fatalf("init: %s", r.err)
			}
			if r := runCLIWith(t, []string{"submit", "-l", ledgerPath},
				`{"fee_bps":30,"intents":[
				  {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1}
				]}`); r.code != 0 {
				t.Fatalf("seed p1: %s", r.err)
			}
			if r := runCLIWith(t, []string{"submit", "-l", ledgerPath},
				`{"fee_bps":0,"intents":[
				  {"id":"p2","account":"aa-1","asset":"usdc","amount":100}
				]}`); r.code != 0 {
				t.Fatalf("seed p2: %s", r.err)
			}

			// 成功 → 失败（空原因）→ 失败（目标不存在）→ 成功：
			// 同时覆盖“后项失败不撤销前项成功”和“前项失败不阻止后项成功”。
			body := `{"refunds":[
			  {"id":"r1","settlement_id":"p1","reason":"customer cancelled"},
			  {"id":"r2","settlement_id":"p2","reason":""},
			  {"id":"r3","settlement_id":"ghost","reason":"x"},
			  {"id":"r4","settlement_id":"p2","reason":"duplicate charge"}
			]}`
			r := refundAtomic(t, entry, ledgerPath, body)
			if r.code != 0 {
				t.Fatalf("per-item failures are normal results, exit must be 0: %s", r.err)
			}
			results := decodeRefundResults(t, r.out)
			if len(results) != 4 {
				t.Fatalf("want 4 per-item results, got %d: %s", len(results), r.out)
			}
			wantStatus := []string{"refunded", "invalid_parameter", "not_found", "refunded"}
			wantID := []string{"r1", "r2", "r3", "r4"}
			for i, it := range results {
				if it.ID != wantID[i] || it.Status != wantStatus[i] {
					t.Fatalf("item %d = %s/%s, want %s/%s (input order must be preserved)",
						i, it.ID, it.Status, wantID[i], wantStatus[i])
				}
			}
			if results[0].Charged != 1504 || results[0].Record == nil {
				t.Fatalf("r1 must refund 1504 with a record: %+v", results[0])
			}
			if results[1].Reason != "refund reason must not be empty" || results[1].Record != nil {
				t.Fatalf("empty reason must be a per-item invalid_parameter with no record: %+v", results[1])
			}
			if results[2].Reason != `settlement "ghost" not found` || results[2].Record != nil {
				t.Fatalf("missing target must be a per-item not_found with no record: %+v", results[2])
			}
			if results[3].Charged != 100 || results[3].Record == nil {
				t.Fatalf("r4 must refund 100 with a record: %+v", results[3])
			}

			// 成功项入账、失败项不留痕：余额 396+1504+100=2000，
			// 退款历史恰好 [r1(seq1) r4(seq2)]，r2/r3 不占序号；
			// 两笔付款记录保持原样。
			snap := queryRefundBatch(t, ledgerPath)
			if snap.balance != 2000 {
				t.Fatalf("balance=%d want 2000", snap.balance)
			}
			if len(snap.settlements) != 2 || snap.settlements[0].ID != "p1" ||
				snap.settlements[0].Charged != 1504 || snap.settlements[0].Seq != 1 ||
				snap.settlements[1].ID != "p2" || snap.settlements[1].Charged != 100 ||
				snap.settlements[1].Seq != 2 {
				t.Fatalf("settlements=%+v want [p1(1504,seq1) p2(100,seq2)]", snap.settlements)
			}
			if len(snap.refunds) != 2 {
				t.Fatalf("refunds=%+v want exactly [r1 r4]", snap.refunds)
			}
			if snap.refunds[0].ID != "r1" || snap.refunds[0].Charged != 1504 ||
				snap.refunds[0].Seq != 1 || snap.refunds[1].ID != "r4" ||
				snap.refunds[1].Charged != 100 || snap.refunds[1].Seq != 2 {
				t.Fatalf("refund records=%+v want r1(1504,seq1) r4(100,seq2)", snap.refunds)
			}
		})
	}
}
