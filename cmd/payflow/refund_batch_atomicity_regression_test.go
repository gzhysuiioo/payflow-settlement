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
// 成功退款，也不得增加余额或留下退款记录。这与逐项业务失败（原因为空、
// 目标付款不存在等语义非法值）严格区分：后者退出 0、按输入顺序逐项给出
// 结果，其他合法退款照常入账。
//
// 两个提交入口（标准输入与 -f/--file 文件）必须遵守完全相同的规则。

// 合法的新退款 r1：退回 p1（amount 1500、手续费 4、charged 1504）。
const atomicValidR1Batch = `{"refunds":[
  {"id":"r1","settlement_id":"p1","reason":"customer cancelled"}
]}`

// 批次级非法输入一：第一项是能够成功的合法 r1，后一项的 reason 写成 JSON
// 数字——encoding/json 解码整个批次时即失败，任何退款都不得开始执行。
const atomicNumberReasonBatch = `{"refunds":[
  {"id":"r1","settlement_id":"p1","reason":"customer cancelled"},
  {"id":"r2","settlement_id":"p1","reason":123}
]}`

// 批次级非法输入二：批次本身完整合法，但结尾紧接着第二个 JSON 文档。
const atomicRefundTrailingDocBatch = atomicValidR1Batch + `{"refunds":[]}`

// 批次级非法输入三：批次本身完整合法，但结尾附有无法解析的非空内容。
const atomicRefundTrailingGarbageBatch = atomicValidR1Batch + `not-json`

// setupRefundAtomicLedger 准备一个初始余额 2000、已按 30 基点费率支付 1500
// （手续费 4、扣款 1504）、当前余额 496 且 p1 尚未退款的账本。
// 返回账本路径与操作前的账本文件字节（用于断言文件逐字节不变）。
func setupRefundAtomicLedger(t *testing.T) (string, []byte) {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath},
		`{"fee_bps":30,"intents":[
		  {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1,"state":"pending"}
		]}`); r.code != 0 {
		t.Fatalf("seed payment: %s", r.err)
	}
	baseline, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	return ledgerPath, baseline
}

type refundAtomicSnapshot struct {
	balance        int64
	paymentIDs     []string
	paymentSeq     []int64
	paymentCharged []int64
	refundIDs      []string
}

// queryRefundAtomic 读回账本当前状态：aa-1/usdc 余额、按成功顺序的付款编号、
// 序号与扣款总额、退款编号。操作前的基线必须是余额 496、付款 [p1(seq1,
// charged 1504)]、无退款记录。
func queryRefundAtomic(t *testing.T, ledgerPath string) refundAtomicSnapshot {
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
			Seq     int64  `json:"seq"`
			Charged int64  `json:"charged"`
		} `json:"settlements"`
		Refunds []struct {
			ID string `json:"id"`
		} `json:"refunds"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatalf("decode query: %v\n%s", err, r.out)
	}
	var got refundAtomicSnapshot
	for _, b := range snap.Balances {
		if b.Account == "aa-1" && b.Asset == "usdc" {
			got.balance = b.Balance
		}
	}
	for _, s := range snap.Settlements {
		got.paymentIDs = append(got.paymentIDs, s.ID)
		got.paymentSeq = append(got.paymentSeq, s.Seq)
		got.paymentCharged = append(got.paymentCharged, s.Charged)
	}
	for _, rf := range snap.Refunds {
		got.refundIDs = append(got.refundIDs, rf.ID)
	}
	return got
}

// untouchedBaseline 断言账本保持操作前状态：余额 496、p1 未退款、无退款记录。
func (s refundAtomicSnapshot) untouchedBaseline(t *testing.T) {
	t.Helper()
	if s.balance != 496 {
		t.Fatalf("balance changed: %d want 496 (first item must not refund 1504 early)", s.balance)
	}
	if ids := joinComma(s.paymentIDs); ids != "p1" {
		t.Fatalf("payment history changed: %v want [p1]", s.paymentIDs)
	}
	if len(s.paymentSeq) != 1 || s.paymentSeq[0] != 1 {
		t.Fatalf("payment seqs changed: %v want [1]", s.paymentSeq)
	}
	if len(s.paymentCharged) != 1 || s.paymentCharged[0] != 1504 {
		t.Fatalf("payment record rewritten: charged=%v want [1504]", s.paymentCharged)
	}
	if len(s.refundIDs) != 0 {
		t.Fatalf("refund history changed: %v want []", s.refundIDs)
	}
}

// refundAtomic 通过指定入口提交退款批次：entry 为 "stdin" 走标准输入，
// 为 "file" 走 -f/--file 文件（标准输入给空内容，不得影响结果）。
func refundAtomic(t *testing.T, entry, ledgerPath, body string) cliResult {
	t.Helper()
	if entry == "stdin" {
		return runCLIWith(t, []string{"refund", "-l", ledgerPath}, body)
	}
	inputPath := t.TempDir() + "/batch.json"
	if err := os.WriteFile(inputPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return runCLIWith(t, []string{"refund", "-l", ledgerPath, "-f", inputPath}, "")
}

// checkR1Refunded 断言 r1 首次成功退款后的账本状态：余额恢复 2000，
// 恰好一条退款记录 r1，原付款记录不被改写（p1 仍 seq 1、charged 1504）。
func checkR1Refunded(t *testing.T, ledgerPath string) {
	t.Helper()
	snap := queryRefundAtomic(t, ledgerPath)
	if snap.balance != 2000 {
		t.Fatalf("balance after r1=%d want 2000 (refund must include the original fee)", snap.balance)
	}
	if ids := joinComma(snap.refundIDs); ids != "r1" {
		t.Fatalf("refund ids=%v want exactly [r1]", snap.refundIDs)
	}
	if ids := joinComma(snap.paymentIDs); ids != "p1" {
		t.Fatalf("payment ids=%v want [p1]", snap.paymentIDs)
	}
	if len(snap.paymentSeq) != 1 || snap.paymentSeq[0] != 1 ||
		len(snap.paymentCharged) != 1 || snap.paymentCharged[0] != 1504 {
		t.Fatalf("original payment record must stay seq 1 charged 1504: seq=%v charged=%v",
			snap.paymentSeq, snap.paymentCharged)
	}
}

// TestCLIRefundMalformedBatchExecutesNothing 覆盖核心回归点：
// 第一项本可成功退款、错误出现在后面（或文档之后）的整批非法输入，必须
// 整批拒绝且不留任何痕迹；随后用第一项原来的退款编号、付款编号和原因
// 重新提交，必须按首次退款成功，而非 duplicate 或 already_refunded。
// 标准输入与文件入口、三种非法形态各跑一遍，且每次都从相同的操作前账本开始。
func TestCLIRefundMalformedBatchExecutesNothing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"later_reason_is_number", atomicNumberReasonBatch},
		{"second_json_document", atomicRefundTrailingDocBatch},
		{"trailing_garbage", atomicRefundTrailingGarbageBatch},
	}
	entries := []string{"stdin", "file"}

	for _, tc := range cases {
		for _, entry := range entries {
			t.Run(tc.name+"_via_"+entry, func(t *testing.T) {
				ledgerPath, baseline := setupRefundAtomicLedger(t)

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

				// 第一项不得退款、不得新增记录：余额、付款与退款历史保持原样。
				queryRefundAtomic(t, ledgerPath).untouchedBaseline(t)

				// 账本文件逐字节不变（没有发生任何落盘）。
				after, err := os.ReadFile(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(baseline, after) {
					t.Fatalf("ledger file changed by rejected batch")
				}

				// 被拒绝的批次不占用 r1：去掉错误内容后用第一项原来的退款编号、
				// 付款编号和原因重新提交，必须首次成功退款（不是 duplicate /
				// already_refunded）。成功结果可追溯原付款：账户、资产、退款总额
				// 与原因齐全，且与新增退款记录一致。
				r = refundAtomic(t, entry, ledgerPath, atomicValidR1Batch)
				if r.code != 0 {
					t.Fatalf("valid r1 after rejected batch: code=%d err=%s", r.code, r.err)
				}
				var res struct {
					Results []struct {
						ID           string `json:"id"`
						Status       string `json:"status"`
						SettlementID string `json:"settlement_id"`
						Account      string `json:"account"`
						Asset        string `json:"asset"`
						Charged      int64  `json:"charged"`
						Reason       string `json:"reason"`
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
				if err := json.Unmarshal([]byte(r.out), &res); err != nil {
					t.Fatalf("decode valid refund: %v\n%s", err, r.out)
				}
				if len(res.Results) != 1 {
					t.Fatalf("want exactly 1 result, got %d: %s", len(res.Results), r.out)
				}
				item := res.Results[0]
				if item.ID != "r1" || item.Status != "refunded" {
					t.Fatalf("r1 must refund for the first time, got status=%q: %+v", item.Status, item)
				}
				if item.SettlementID != "p1" || item.Account != "aa-1" || item.Asset != "usdc" ||
					item.Charged != 1504 || item.Reason != "customer cancelled" {
					t.Fatalf("refund result must trace the original payment: %+v", item)
				}
				if item.Record == nil {
					t.Fatalf("refunded item must carry the new refund record: %+v", item)
				}
				rec := item.Record
				if rec.ID != "r1" || rec.SettlementID != "p1" || rec.Reason != "customer cancelled" ||
					rec.Account != "aa-1" || rec.Asset != "usdc" ||
					rec.Amount != 1500 || rec.Fee != 4 || rec.Charged != 1504 || rec.Seq != 1 {
					t.Fatalf("refund record inconsistent with result: %+v", rec)
				}

				// 余额恢复 2000（退回金额含原手续费），恰好新增一条退款记录，
				// 原付款记录不被改写。
				checkR1Refunded(t, ledgerPath)

				// 再次提交相同 r1 现在必须是 duplicate 且不二次入账，
				// 反向证明上一次是首次成功而非被拒绝批次的残留记录。
				r = refundAtomic(t, entry, ledgerPath, atomicValidR1Batch)
				if r.code != 0 {
					t.Fatalf("duplicate resubmit code=%d err=%s", r.code, r.err)
				}
				var dup struct {
					Results []struct {
						Status string `json:"status"`
					} `json:"results"`
				}
				if err := json.Unmarshal([]byte(r.out), &dup); err != nil {
					t.Fatal(err)
				}
				if len(dup.Results) != 1 || dup.Results[0].Status != "duplicate" {
					t.Fatalf("second r1 must be duplicate: %s", r.out)
				}
				checkR1Refunded(t, ledgerPath)
			})
		}
	}
}

// TestCLIRefundAcceptsTrailingWhitespace 锁定“完整 JSON 之后只有空白”与
// “第二个 JSON 文档或垃圾内容”的区别：空格、制表符、换行（含 CRLF）是合法
// 尾随空白，退款批次必须正常执行，不能与尾随内容混淆。两个入口行为一致。
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
				ledgerPath, _ := setupRefundAtomicLedger(t)
				r := refundAtomic(t, entry, ledgerPath, atomicValidR1Batch+suffix)
				if r.code != 0 {
					t.Fatalf("trailing whitespace must be accepted: code=%d err=%s", r.code, r.err)
				}
				var res struct {
					Results []struct {
						Status string `json:"status"`
					} `json:"results"`
				}
				if err := json.Unmarshal([]byte(r.out), &res); err != nil {
					t.Fatalf("decode: %v\n%s", err, r.out)
				}
				if len(res.Results) != 1 || res.Results[0].Status != "refunded" {
					t.Fatalf("whitespace-suffixed batch must refund r1: %s", r.out)
				}
				checkR1Refunded(t, ledgerPath)
			})
		}
	}
}

// TestCLIRefundPerItemFailuresDoNotAbortBatch 是对照场景：整个 JSON 可解析、
// 字段类型都合法，仅某项原因为空或目标付款不存在时属于逐项业务失败而非
// 整批输入错误——整批退出 0，结果按输入顺序分别报告 invalid_parameter /
// not_found，其他合法退款照常成功。成功项增加余额并留下退款记录，失败项
// 不增加余额、不留记录；后面的失败不能撤销前面的成功，前面的失败也不能
// 阻止后面的合法退款。
func TestCLIRefundPerItemFailuresDoNotAbortBatch(t *testing.T) {
	for _, entry := range []string{"stdin", "file"} {
		t.Run("per_item_via_"+entry, func(t *testing.T) {
			ledgerPath := t.TempDir() + "/ledger.json"
			if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
				`{"balances":[{"account":"aa-1","asset":"usdc","balance":2000}]}`); r.code != 0 {
				t.Fatalf("init: %s", r.err)
			}
			// p1 扣款 1504、p2 扣款 401（400×30bps 手续费 1），余额 95。
			if r := runCLIWith(t, []string{"submit", "-l", ledgerPath},
				`{"fee_bps":30,"intents":[
				  {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1},
				  {"id":"p2","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":400,"nonce":2}
				]}`); r.code != 0 {
				t.Fatalf("seed payments: %s", r.err)
			}

			// r1 合法（成功）→ r2 原因为空（invalid_parameter）→ r3 目标不存在
			// （not_found）→ r4 合法（成功）：前后失败都不影响另一侧的成功项。
			body := `{"refunds":[
			  {"id":"r1","settlement_id":"p1","reason":"customer cancelled"},
			  {"id":"r2","settlement_id":"p1","reason":""},
			  {"id":"r3","settlement_id":"ghost","reason":"no such payment"},
			  {"id":"r4","settlement_id":"p2","reason":"duplicate charge"}
			]}`
			r := refundAtomic(t, entry, ledgerPath, body)
			if r.code != 0 {
				t.Fatalf("per-item failures are normal results, exit must be 0: %s", r.err)
			}
			var res struct {
				Results []struct {
					ID           string `json:"id"`
					Status       string `json:"status"`
					SettlementID string `json:"settlement_id"`
					Charged      int64  `json:"charged"`
					Record       *struct {
						ID string `json:"id"`
					} `json:"record"`
				} `json:"results"`
			}
			if err := json.Unmarshal([]byte(r.out), &res); err != nil {
				t.Fatalf("decode: %v\n%s", err, r.out)
			}
			if len(res.Results) != 4 {
				t.Fatalf("want 4 per-item results, got %d: %s", len(res.Results), r.out)
			}
			wantStatus := []string{"refunded", "invalid_parameter", "not_found", "refunded"}
			wantID := []string{"r1", "r2", "r3", "r4"}
			for i, x := range res.Results {
				if x.ID != wantID[i] || x.Status != wantStatus[i] {
					t.Fatalf("item %d = %s/%s, want %s/%s (order must follow input)",
						i, x.ID, x.Status, wantID[i], wantStatus[i])
				}
			}
			// 成功项携带记录与追溯字段；失败项不带记录。
			if res.Results[0].Record == nil || res.Results[0].SettlementID != "p1" ||
				res.Results[0].Charged != 1504 {
				t.Fatalf("r1 must refund 1504 with a record: %+v", res.Results[0])
			}
			if res.Results[3].Record == nil || res.Results[3].SettlementID != "p2" ||
				res.Results[3].Charged != 401 {
				t.Fatalf("r4 must refund 401 with a record: %+v", res.Results[3])
			}
			if res.Results[1].Record != nil || res.Results[2].Record != nil {
				t.Fatalf("failed items must not carry records: %+v / %+v",
					res.Results[1].Record, res.Results[2].Record)
			}

			// 成功项增加余额并留下退款记录，失败项不增加余额、不留记录：
			// 余额 95 + 1504 + 401 = 2000，退款历史恰好 [r1, r4]，两笔付款
			// 记录保持原样。
			snap := queryRefundAtomic(t, ledgerPath)
			if snap.balance != 2000 {
				t.Fatalf("balance=%d want 2000 (1504+401 refunded onto 95)", snap.balance)
			}
			if ids := joinComma(snap.refundIDs); ids != "r1,r4" {
				t.Fatalf("refund ids=%v want exactly [r1 r4]", snap.refundIDs)
			}
			if ids := joinComma(snap.paymentIDs); ids != "p1,p2" {
				t.Fatalf("payment ids=%v want [p1 p2]", snap.paymentIDs)
			}
			if len(snap.paymentSeq) != 2 || snap.paymentSeq[0] != 1 || snap.paymentSeq[1] != 2 {
				t.Fatalf("payment seqs=%v want [1 2]", snap.paymentSeq)
			}
		})
	}
}
