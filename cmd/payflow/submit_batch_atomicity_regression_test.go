package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// 本组回归测试锁定 submit 的“整份 JSON 输入合法后才开始执行”语义：
// 批次级输入错误（字段类型错误、JSON 文档之后还有第二个文档）必须在打开账本、
// 执行任何意图之前整批拒绝——即使第一个意图本可以成功，也不得留下扣款或成功
// 记录。这与逐项业务失败（金额为 0 等语义非法值）严格区分：后者退出 0、
// 按输入顺序逐项给出结果，前项照常扣款落账。
//
// 两个提交入口（标准输入与 -f/--file 文件）必须遵守完全相同的规则。

// 合法的新付款 p1：费率 30 基点、金额 1500 -> 手续费 4、扣款总额 1504。
const atomicValidP1Batch = `{"fee_bps":30,"intents":[
  {"id":"p1","account":"aa","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1,"state":"pending"}
]}`

// 批次级非法输入一：第一项是能够成功的合法 p1，后一项的 amount 写成 JSON
// 字符串——encoding/json 解码整个批次时即失败，任何意图都不得开始执行。
const atomicStringAmountBatch = `{"fee_bps":30,"intents":[
  {"id":"p1","account":"aa","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1,"state":"pending"},
  {"id":"p2","account":"aa","asset":"usdc","amount":"900"}
]}`

// 批次级非法输入二：批次本身完整合法，但结尾紧接着第二个 JSON 文档。
const atomicTrailingDocBatch = atomicValidP1Batch + `{"fee_bps":0,"intents":[]}`

// setupAtomicLedger 准备一个已有成功付款与退款历史、当前余额回到 2000 的账本：
// h1（seq=1，amount 500、手续费 1、charged 501）已由 r1 全额退款。
// p1 因此是全新编号；新的成功付款序号必须紧接历史为 2，不能空缺或重复。
// 返回账本路径与提交前的账本文件字节（用于断言文件逐字节不变）。
func setupAtomicLedger(t *testing.T) (string, []byte) {
	t.Helper()
	ledgerPath := t.TempDir() + "/ledger.json"
	if r := runCLIWith(t, []string{"init", "-l", ledgerPath},
		`{"balances":[{"account":"aa","asset":"usdc","balance":2000}]}`); r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	if r := runCLIWith(t, []string{"submit", "-l", ledgerPath},
		`{"fee_bps":30,"intents":[
		  {"id":"h1","account":"aa","paymaster":"pm-0","asset":"usdc","amount":500,"nonce":7}
		]}`); r.code != 0 {
		t.Fatalf("seed payment: %s", r.err)
	}
	if r := runCLIWith(t, []string{"refund", "-l", ledgerPath},
		`{"refunds":[{"id":"r1","settlement_id":"h1","reason":"prior history"}]}`); r.code != 0 {
		t.Fatalf("seed refund: %s", r.err)
	}
	baseline, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	return ledgerPath, baseline
}

type atomicSnapshot struct {
	balance    int64
	paymentIDs []string
	paymentSeq []int64
	refundIDs  []string
}

// queryAtomic 读回账本当前状态：aa/usdc 余额、按成功顺序的付款编号与序号、
// 退款编号。提交前的基线必须是余额 2000、付款 [h1(seq1)]、退款 [r1]。
func queryAtomic(t *testing.T, ledgerPath string) atomicSnapshot {
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
			ID  string `json:"id"`
			Seq int64  `json:"seq"`
		} `json:"settlements"`
		Refunds []struct {
			ID string `json:"id"`
		} `json:"refunds"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
		t.Fatalf("decode query: %v\n%s", err, r.out)
	}
	var got atomicSnapshot
	for _, b := range snap.Balances {
		if b.Account == "aa" && b.Asset == "usdc" {
			got.balance = b.Balance
		}
	}
	for _, s := range snap.Settlements {
		got.paymentIDs = append(got.paymentIDs, s.ID)
		got.paymentSeq = append(got.paymentSeq, s.Seq)
	}
	for _, rf := range snap.Refunds {
		got.refundIDs = append(got.refundIDs, rf.ID)
	}
	return got
}

func (s atomicSnapshot) untouchedBaseline(t *testing.T) {
	t.Helper()
	if s.balance != 2000 {
		t.Fatalf("balance changed: %d want 2000", s.balance)
	}
	if ids := joinComma(s.paymentIDs); ids != "h1" {
		t.Fatalf("payment history changed: %v want [h1]", s.paymentIDs)
	}
	if len(s.paymentSeq) != 1 || s.paymentSeq[0] != 1 {
		t.Fatalf("payment seqs changed: %v want [1]", s.paymentSeq)
	}
	if ids := joinComma(s.refundIDs); ids != "r1" {
		t.Fatalf("refund history changed: %v want [r1]", s.refundIDs)
	}
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// submitAtomic 通过指定入口提交 body：entry 为 "stdin" 走标准输入，
// 为 "file" 走 -f/--file 文件（标准输入给空内容，不得影响结果）。
func submitAtomic(t *testing.T, entry, ledgerPath, body string) cliResult {
	t.Helper()
	if entry == "stdin" {
		return runCLIWith(t, []string{"submit", "-l", ledgerPath}, body)
	}
	inputPath := t.TempDir() + "/batch.json"
	if err := os.WriteFile(inputPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return runCLIWith(t, []string{"submit", "-l", ledgerPath, "-f", inputPath}, "")
}

// TestCLISubmitMalformedBatchExecutesNothing 覆盖核心回归点：
// 第一项本可成功、错误出现在后面的整批非法输入，必须整批拒绝且不留任何痕迹。
// 标准输入与文件入口、两种非法形态各跑一遍，且每次都从相同的提交前账本开始；
// 随后在被拒绝的同一账本上单独提交合法 p1，验证编号与序号均未被占用。
func TestCLISubmitMalformedBatchExecutesNothing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"later_amount_is_string", atomicStringAmountBatch},
		{"second_json_document", atomicTrailingDocBatch},
	}
	entries := []string{"stdin", "file"}

	for _, tc := range cases {
		for _, entry := range entries {
			t.Run(tc.name+"_via_"+entry, func(t *testing.T) {
				ledgerPath, baseline := setupAtomicLedger(t)

				r := submitAtomic(t, entry, ledgerPath, tc.body)

				// 整批返回 invalid_parameter，退出码 1；既有错误信封只在标准错误，
				// 标准输出必须为空——不能先读到合法意图就输出部分结果。
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

				// 第一项不得扣款、不得新增结算：余额、付款与退款历史保持原样。
				queryAtomic(t, ledgerPath).untouchedBaseline(t)

				// 账本文件逐字节不变（没有发生任何落盘）。
				after, err := os.ReadFile(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(baseline, after) {
					t.Fatalf("ledger file changed by rejected batch")
				}

				// 被拒绝的批次不占用 p1：随后单独提交相同的合法付款按首次付款
				// settled（不是 duplicate/conflict），手续费 4、扣款 1504、余额
				// 496，成功序号紧接原有历史为 2，没有空缺。
				r = submitAtomic(t, entry, ledgerPath, atomicValidP1Batch)
				if r.code != 0 {
					t.Fatalf("valid p1 after rejected batch: code=%d err=%s", r.code, r.err)
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
					t.Fatalf("decode valid submit: %v\n%s", err, r.out)
				}
				if len(res.Results) != 1 {
					t.Fatalf("want exactly 1 result, got %d: %s", len(res.Results), r.out)
				}
				item := res.Results[0]
				if item.ID != "p1" || item.Status != "settled" || item.Record == nil {
					t.Fatalf("p1 must settle for the first time: %+v", item)
				}
				if item.Record.Fee != 4 || item.Record.Charged != 1504 || item.Record.Seq != 2 {
					t.Fatalf("p1 record fee=%d charged=%d seq=%d, want 4/1504/2",
						item.Record.Fee, item.Record.Charged, item.Record.Seq)
				}

				snap := queryAtomic(t, ledgerPath)
				if snap.balance != 496 {
					t.Fatalf("balance after p1=%d want 496", snap.balance)
				}
				if ids := joinComma(snap.paymentIDs); ids != "h1,p1" {
					t.Fatalf("payment ids=%v want [h1 p1]", snap.paymentIDs)
				}
				if len(snap.paymentSeq) != 2 || snap.paymentSeq[0] != 1 || snap.paymentSeq[1] != 2 {
					t.Fatalf("payment seqs=%v want [1 2]", snap.paymentSeq)
				}
				if ids := joinComma(snap.refundIDs); ids != "r1" {
					t.Fatalf("refund history must stay [r1], got %v", snap.refundIDs)
				}

				// 再次提交相同 p1 现在必须是 duplicate 且不二次扣款，
				// 反向证明上一笔是首次成功而非被拒绝批次的残留记录。
				r = submitAtomic(t, entry, ledgerPath, atomicValidP1Batch)
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
					t.Fatalf("second p1 must be duplicate: %s", r.out)
				}
				if snap := queryAtomic(t, ledgerPath); snap.balance != 496 {
					t.Fatalf("duplicate resubmit charged again: balance=%d", snap.balance)
				}
			})
		}
	}
}

// TestCLISubmitAcceptsTrailingWhitespace 锁定“完整 JSON 之后只有空白”与
// “第二个 JSON 文档”的区别：空格、制表符、换行（含 CRLF）是合法尾随空白，
// 批次必须正常提交，不能与尾随文档混淆。两个入口行为一致。
func TestCLISubmitAcceptsTrailingWhitespace(t *testing.T) {
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
				ledgerPath, _ := setupAtomicLedger(t)
				r := submitAtomic(t, entry, ledgerPath, atomicValidP1Batch+suffix)
				if r.code != 0 {
					t.Fatalf("trailing whitespace must be accepted: code=%d err=%s", r.code, r.err)
				}
				var res struct {
					Results []struct {
						Status string  `json:"status"`
						Record *struct {
							Seq int64 `json:"seq"`
						} `json:"record"`
					} `json:"results"`
				}
				if err := json.Unmarshal([]byte(r.out), &res); err != nil {
					t.Fatalf("decode: %v\n%s", err, r.out)
				}
				if len(res.Results) != 1 || res.Results[0].Status != "settled" ||
					res.Results[0].Record == nil || res.Results[0].Record.Seq != 2 {
					t.Fatalf("whitespace-suffixed batch must settle p1 at seq 2: %s", r.out)
				}
				if snap := queryAtomic(t, ledgerPath); snap.balance != 496 {
					t.Fatalf("balance=%d want 496", snap.balance)
				}
			})
		}
	}
}

// TestCLISubmitPerItemZeroAmountDoesNotAbortBatch 是对照场景：JSON 与字段类型
// 都合法、仅后项金额明确为整数 0 时属于逐项业务失败而非整批输入错误——
// 整批退出 0，结果按输入顺序为第一项 settled、后项 invalid_parameter；
// 前项扣款与记录保留（p1 seq=2、余额 496），后项不扣款、不留成功记录。
func TestCLISubmitPerItemZeroAmountDoesNotAbortBatch(t *testing.T) {
	for _, entry := range []string{"stdin", "file"} {
		t.Run("zero_amount_via_"+entry, func(t *testing.T) {
			ledgerPath, baseline := setupAtomicLedger(t)
			body := `{"fee_bps":30,"intents":[
  {"id":"p1","account":"aa","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1,"state":"pending"},
  {"id":"z1","account":"aa","asset":"usdc","amount":0}
]}`
			r := submitAtomic(t, entry, ledgerPath, body)
			if r.code != 0 {
				t.Fatalf("per-item failures are normal results, exit must be 0: %s", r.err)
			}
			var res struct {
				Results []struct {
					ID     string  `json:"id"`
					Status string  `json:"status"`
					Reason string  `json:"reason"`
					Record *struct {
						Fee     int64 `json:"fee"`
						Charged int64 `json:"charged"`
						Seq     int64 `json:"seq"`
					} `json:"record"`
				} `json:"results"`
			}
			if err := json.Unmarshal([]byte(r.out), &res); err != nil {
				t.Fatalf("decode: %v\n%s", err, r.out)
			}
			if len(res.Results) != 2 {
				t.Fatalf("want 2 per-item results, got %d: %s", len(res.Results), r.out)
			}
			first, second := res.Results[0], res.Results[1]
			if first.ID != "p1" || first.Status != "settled" || first.Record == nil {
				t.Fatalf("first item must settle: %+v", first)
			}
			if first.Record.Fee != 4 || first.Record.Charged != 1504 || first.Record.Seq != 2 {
				t.Fatalf("first item record=%+v, want fee 4 charged 1504 seq 2", first.Record)
			}
			if second.ID != "z1" || second.Status != "invalid_parameter" || second.Record != nil {
				t.Fatalf("zero amount must be a per-item invalid_parameter with no record: %+v", second)
			}
			if second.Reason != "amount must be a positive int64" {
				t.Fatalf("zero amount reason=%q", second.Reason)
			}

			// 前项扣款保留，后项不留成功记录：余额 496，历史 [h1(seq1) p1(seq2)]，
			// 没有 z1，序号无空缺；退款历史不变。账本文件相较基线确实发生了一次
			// 正常落账（与整批拒绝时的“逐字节不变”形成对照）。
			snap := queryAtomic(t, ledgerPath)
			if snap.balance != 496 {
				t.Fatalf("balance=%d want 496", snap.balance)
			}
			if ids := joinComma(snap.paymentIDs); ids != "h1,p1" {
				t.Fatalf("payment ids=%v want [h1 p1] (z1 must leave no record)", snap.paymentIDs)
			}
			if len(snap.paymentSeq) != 2 || snap.paymentSeq[1] != 2 {
				t.Fatalf("payment seqs=%v want [1 2]", snap.paymentSeq)
			}
			if ids := joinComma(snap.refundIDs); ids != "r1" {
				t.Fatalf("refund ids=%v want [r1]", snap.refundIDs)
			}
			if changed, err := os.ReadFile(ledgerPath); err != nil {
				t.Fatal(err)
			} else if bytes.Equal(baseline, changed) {
				t.Fatalf("settled first item must have persisted the ledger file")
			}
		})
	}
}
