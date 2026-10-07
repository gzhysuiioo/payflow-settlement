package payflow

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"testing"
)

// 本文件为本地账本 Ledger.Submit 锁定“金额只增加一个最小单位、手续费却刚好
// 跨过整数界限”的大额场景。费率固定 30 基点，两个相邻金额（末尾 333/334）
// 的朴素乘积 amount*30 约为 1.2e20，早已超过 int64 上界（约 9.22e18），
// 因此任何直接相乘的实现都会溢出：要么把合法付款误判成数值溢出而拒绝，
// 要么把末尾数字抹掉、把两笔相邻金额当成同一金额，或把第二笔扣款总额
// 算成只增加一个单位。手续费的唯一规则仍是 amount*feeBps/10000 向下取整
// （见 charge.go 的商/余数拆分），扣款总额 = 本金 + 手续费。
//
//	金额 4000000000000000333 -> 手续费 12000000000000000，扣款 4012000000000000333
//	金额 4000000000000000334 -> 手续费 12000000000000001，扣款 4012000000000000335
//
// 金额、手续费与扣款总额都仍可由 int64 表示；本文件只以 Ledger.Submit 的
// 逐项结果、成功记录和 Query/Balance 读数为准，不设置本次扣款上限，
// 付款状态显式使用 pending，避免其他拒绝条件掩盖金额判断。
const (
	boundaryFeeBps = 30

	boundaryAmountLo = int64(4000000000000000333)
	boundaryAmountHi = int64(4000000000000000334)

	boundaryFeeLo = int64(12000000000000000)
	boundaryFeeHi = int64(12000000000000001)

	boundaryChargedLo = int64(4012000000000000333)
	boundaryChargedHi = int64(4012000000000000335)
)

type boundaryCase struct {
	name    string
	id      string
	amount  int64
	fee     int64
	charged int64
}

func boundaryCases() []boundaryCase {
	return []boundaryCase{
		{"fee_below_boundary", "big-lo", boundaryAmountLo, boundaryFeeLo, boundaryChargedLo},
		{"fee_crossed_boundary", "big-hi", boundaryAmountHi, boundaryFeeHi, boundaryChargedHi},
	}
}

// TestSubmitLargeFeeBoundaryNumbers 先锁定数值关系本身：相邻金额只差一个
// 最小单位，手续费恰差一个单位（跨过整数界限），而扣款总额相差两个单位；
// 三者全部在 int64 范围内，朴素乘积则会溢出。
func TestSubmitLargeFeeBoundaryNumbers(t *testing.T) {
	if got := boundaryAmountHi - boundaryAmountLo; got != 1 {
		t.Fatalf("adjacent amounts must differ by 1, got %d", got)
	}
	if got := boundaryFeeHi - boundaryFeeLo; got != 1 {
		t.Fatalf("fee must step up by exactly 1 across the boundary, got %d", got)
	}
	// 手续费多增一个单位，因此扣款总额的增量（2）不等于金额增量（1）。
	if got := boundaryChargedHi - boundaryChargedLo; got != 2 {
		t.Fatalf("charged totals must differ by 2 (amount + fee steps), got %d", got)
	}
	for _, c := range boundaryCases() {
		if c.charged >= math.MaxInt64 {
			t.Fatalf("%s: charged %d must fit in int64 (max %d)", c.name, c.charged, int64(math.MaxInt64))
		}
		if c.fee < 0 || c.charged != c.amount+c.fee {
			t.Fatalf("%s: charged must equal amount plus fee", c.name)
		}
		// 与 feeFor 的商/余数拆分结果逐项对照（amount*feeBps 本身会溢出 int64）。
		if got := feeFor(c.amount, boundaryFeeBps); got != c.fee {
			t.Fatalf("%s: feeFor(%d)=%d want %d", c.name, c.amount, got, c.fee)
		}
		fee, total, ok := chargeFor(c.amount, boundaryFeeBps)
		if !ok || fee != c.fee || total != c.charged {
			t.Fatalf("%s: chargeFor=(%d,%d,%v) want (%d,%d,true)", c.name, fee, total, ok, c.fee, c.charged)
		}
	}
}

// TestSubmitLargeFeeExactBalanceSettles：余额恰好等于各自扣款总额的账本中，
// 两种请求都必须作为新付款 settled，余额扣到 0，历史只增加对应的一条成功
// 记录，记录完整保留金额末尾数字与跨过界限的手续费；关闭重开后磁盘状态一致。
func TestSubmitLargeFeeExactBalanceSettles(t *testing.T) {
	for _, tc := range boundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: tc.charged}})
			l := openOrFail(t, path)

			res, err := l.Submit(FeeBatch{FeeBps: boundaryFeeBps, Intents: []PaymentIntent{
				intent(tc.id, "aa", "usdc", tc.amount),
			}})
			if err != nil {
				t.Fatalf("legitimate large payment must not be rejected at the batch level: %v", err)
			}
			if len(res.Results) != 1 {
				t.Fatalf("want exactly 1 per-item result, got %d: %+v", len(res.Results), res.Results)
			}
			item := res.Results[0]
			if item.ID != tc.id {
				t.Fatalf("result id=%q want %q", item.ID, tc.id)
			}
			if item.Status != StatusSettled {
				t.Fatalf("exact-balance payment must settle as a new payment, got status=%s reason=%q",
					item.Status, item.Reason)
			}
			if item.Status == StatusInvalid {
				t.Fatalf("large in-range payment must not be mistaken for numeric overflow")
			}
			if item.Reason != "" || item.Record == nil {
				t.Fatalf("settled item must carry a success record and no reason: %+v", item)
			}
			rec := item.Record
			if rec.Amount != tc.amount {
				t.Fatalf("record amount=%d want %d (trailing digits must be preserved)", rec.Amount, tc.amount)
			}
			if rec.Fee != tc.fee {
				t.Fatalf("record fee=%d want %d", rec.Fee, tc.fee)
			}
			if rec.Charged != tc.charged {
				t.Fatalf("record charged=%d want %d", rec.Charged, tc.charged)
			}
			if rec.FeeBps != boundaryFeeBps || rec.Seq != 1 || rec.ID != tc.id ||
				rec.Account != "aa" || rec.Asset != "usdc" {
				t.Fatalf("record fields off: %+v", rec)
			}

			// 查询余额为零；历史只增加对应的一条成功记录。
			if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
				t.Fatalf("balance after settle=%d want 0", bal)
			}
			snap, err := l.Query()
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Settlements) != 1 {
				t.Fatalf("want exactly 1 settlement in history, got %+v", snap.Settlements)
			}
			got := snap.Settlements[0]
			if got.ID != tc.id || got.Amount != tc.amount || got.Fee != tc.fee ||
				got.Charged != tc.charged || got.FeeBps != boundaryFeeBps || got.Seq != 1 {
				t.Fatalf("queried record does not preserve the exact amount/fee: %+v", got)
			}
			if len(snap.Balances) != 1 || snap.Balances[0].Balance != 0 {
				t.Fatalf("queried balances after settle: %+v", snap.Balances)
			}

			// 关闭重开：磁盘上的大额记录与零余额必须原样恢复（重放校验同样
			// 采用商/余数手续费规则，不能把该记录判为损坏）。
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			l2 := openOrFail(t, path)
			if bal, _ := l2.Balance("aa", "usdc"); bal != 0 {
				t.Fatalf("balance after reopen=%d want 0", bal)
			}
			snap2, _ := l2.Query()
			if len(snap2.Settlements) != 1 {
				t.Fatalf("after reopen want 1 settlement, got %+v", snap2.Settlements)
			}
			r2 := snap2.Settlements[0]
			if r2.Amount != tc.amount || r2.Fee != tc.fee || r2.Charged != tc.charged {
				t.Fatalf("after reopen record lost the exact figures: %+v", r2)
			}
		})
	}
}

// TestSubmitLargeFeeOneUnitShortIsInsufficient：余额比各自扣款总额少一个
// 最小单位时必须返回 insufficient_balance——付款本金本身仍小于可用余额，
// 拒绝的原因是计入手续费后恰好差一个单位。失败项不携带成功记录，余额与
// 历史保持提交前状态，账本文件逐字节不变；Submit 本身正常返回逐项结果
// （不是批次级错误）。
func TestSubmitLargeFeeOneUnitShortIsInsufficient(t *testing.T) {
	for _, tc := range boundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			start := tc.charged - 1
			path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: start}})
			l := openOrFail(t, path)

			// 明确前置条件：本金自身仍在可用余额之内，差额完全来自手续费。
			if tc.amount >= start {
				t.Fatalf("test setup invalid: principal %d must be strictly below available balance %d", tc.amount, start)
			}
			if tc.amount+tc.fee != start+1 {
				t.Fatalf("test setup invalid: amount+fee must exceed balance by exactly 1")
			}

			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			res, err := l.Submit(FeeBatch{FeeBps: boundaryFeeBps, Intents: []PaymentIntent{
				intent(tc.id, "aa", "usdc", tc.amount),
			}})
			if err != nil {
				t.Fatalf("insufficient balance is a per-item result, Submit must not error: %v", err)
			}
			if len(res.Results) != 1 {
				t.Fatalf("want 1 per-item result, got %+v", res.Results)
			}
			item := res.Results[0]
			if item.Status != StatusFunds {
				t.Fatalf("one unit short must be insufficient_balance, got status=%s reason=%q",
					item.Status, item.Reason)
			}
			if item.Reason != reasonFunds {
				t.Fatalf("reason must clearly state insufficient balance, got %q want %q",
					item.Reason, reasonFunds)
			}
			if item.Record != nil {
				t.Fatalf("rejected item must not carry a success record: %+v", item.Record)
			}

			// 余额与历史保持提交前状态。
			if bal, _ := l.Balance("aa", "usdc"); bal != start {
				t.Fatalf("balance changed after rejection: %d want %d", bal, start)
			}
			snap, _ := l.Query()
			if len(snap.Settlements) != 0 {
				t.Fatalf("rejected payment must leave no success record: %+v", snap.Settlements)
			}
			if len(snap.Balances) != 1 || snap.Balances[0].Balance != start {
				t.Fatalf("queried balances changed: %+v", snap.Balances)
			}

			// 账本文件不因这次失败发生变化（没有任何落盘）。
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("ledger file changed byte-wise after an insufficient_balance rejection")
			}

			// 重开后仍无记录、余额不变。
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			l2 := openOrFail(t, path)
			if bal, _ := l2.Balance("aa", "usdc"); bal != start {
				t.Fatalf("balance after reopen=%d want %d", bal, start)
			}
			if snap2, _ := l2.Query(); len(snap2.Settlements) != 0 {
				t.Fatalf("after reopen history must stay empty: %+v", snap2.Settlements)
			}
		})
	}
}

// TestSubmitLargeFeeBoundaryPerItemFailure 证明余额不足在多项目批次中仍只是
// 逐项业务失败：合法批次正常返回逐项结果，失败项不扣余额、不留记录、不占
// 编号，相邻金额不会被当成同一笔付款。
func TestSubmitLargeFeeBoundaryPerItemFailure(t *testing.T) {
	// 场景一：先提交手续费跨界限的金额（差两个单位，失败），再提交较小的
	// 相邻金额（余额恰好够）。初始余额 = chargedHi-1 = chargedLo+1。
	t.Run("short_hi_then_exact_lo", func(t *testing.T) {
		start := boundaryChargedHi - 1 // 比 high 扣款少 1，比 low 扣款多 1
		path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: start}})
		l := openOrFail(t, path)

		res, err := l.Submit(FeeBatch{FeeBps: boundaryFeeBps, Intents: []PaymentIntent{
			intent("big-hi", "aa", "usdc", boundaryAmountHi), // 计入手续费后差 1，失败
			intent("big-lo", "aa", "usdc", boundaryAmountLo), // 前项失败不扣款，本项成功
		}})
		if err != nil {
			t.Fatalf("a batch containing a failing item must still return results: %v", err)
		}
		want := []string{StatusFunds, StatusSettled}
		if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("statuses=%v want %v", got, want)
		}
		hi, lo := res.Results[0], res.Results[1]
		if hi.Reason != reasonFunds || hi.Record != nil {
			t.Fatalf("hi item must fail insufficient without a record: %+v", hi)
		}
		if lo.Record == nil || lo.Record.Amount != boundaryAmountLo ||
			lo.Record.Fee != boundaryFeeLo || lo.Record.Charged != boundaryChargedLo || lo.Record.Seq != 1 {
			t.Fatalf("lo item must settle with its exact figures: %+v", lo)
		}

		// 余额只被成功项扣减：start - chargedLo = 1。
		if bal, _ := l.Balance("aa", "usdc"); bal != 1 {
			t.Fatalf("balance=%d want 1", bal)
		}
		snap, _ := l.Query()
		if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "big-lo" {
			t.Fatalf("history must contain only the settled lo item: %+v", snap.Settlements)
		}

		// 失败的 hi 编号未被占用：再次提交仍走到余额判断（insufficient_balance），
		// 而不是 duplicate/conflict——两项自始至终都是不同的付款请求。
		retry, err := l.Submit(FeeBatch{FeeBps: boundaryFeeBps, Intents: []PaymentIntent{
			intent("big-hi", "aa", "usdc", boundaryAmountHi),
		}})
		if err != nil {
			t.Fatal(err)
		}
		r := retry.Results[0]
		if r.Status != StatusFunds || r.Record != nil {
			t.Fatalf("failed hi id must remain unconsumed and fail on funds again: %+v", r)
		}
		if bal, _ := l.Balance("aa", "usdc"); bal != 1 {
			t.Fatalf("retry must not change balance: %d", bal)
		}
		if snap, _ := l.Query(); len(snap.Settlements) != 1 {
			t.Fatalf("retry must not add history: %+v", snap.Settlements)
		}

		// 账本文件中只留有成功的 lo 记录，失败的 hi 从未落盘。
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(`"big-hi"`)) || !bytes.Contains(raw, []byte(`"big-lo"`)) {
			t.Fatalf("ledger file must persist only big-lo:\n%s", raw)
		}
	})

	// 场景二：较小金额先成功把余额扣到 0，随后提交较大的相邻金额。若实现把
	// 334 截断/抹零成 333，第二项会被识别成同一笔已成功付款而返回 duplicate；
	// 正确行为是把它视为另一笔新付款，因余额不足返回 insufficient_balance。
	t.Run("exact_lo_then_short_hi", func(t *testing.T) {
		path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: boundaryChargedLo}})
		l := openOrFail(t, path)

		res, err := l.Submit(FeeBatch{FeeBps: boundaryFeeBps, Intents: []PaymentIntent{
			intent("big-lo", "aa", "usdc", boundaryAmountLo), // 恰好扣尽，成功
			intent("big-hi", "aa", "usdc", boundaryAmountHi), // 相邻但不同的新付款：余额不足
		}})
		if err != nil {
			t.Fatalf("per-item failure must still return a normal batch result: %v", err)
		}
		want := []string{StatusSettled, StatusFunds}
		if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("statuses=%v want %v", got, want)
		}
		hi := res.Results[1]
		if hi.Reason != reasonFunds || hi.Record != nil {
			t.Fatalf("hi item must be a distinct payment failing on funds, not a duplicate: %+v", hi)
		}
		if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
			t.Fatalf("balance=%d want 0", bal)
		}
		snap, _ := l.Query()
		if len(snap.Settlements) != 1 {
			t.Fatalf("only the lo payment may enter history: %+v", snap.Settlements)
		}
		rec := snap.Settlements[0]
		if rec.ID != "big-lo" || rec.Amount != boundaryAmountLo ||
			rec.Fee != boundaryFeeLo || rec.Charged != boundaryChargedLo {
			t.Fatalf("lo record must keep its exact trailing digits and fee: %+v", rec)
		}
	})
}
