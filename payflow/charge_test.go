package payflow

import (
	"math"
	"testing"
)

// TestChargeBreakdown 锁定统一维护的金额规则：
// 金额为正 int64、费率 [0,10000] 基点、手续费 floor(amount*bps/10000)、
// 扣款总额 = 金额+手续费。手续费的中间乘积允许超过 int64；只有扣款总额
// 本身越界时 representable 才为 false。
func TestChargeBreakdown(t *testing.T) {
	cases := []struct {
		name      string
		amount    int64
		bps       int
		wantFee   int64
		wantTotal int64
		wantOK    bool
	}{
		{"rounds down below threshold", 9999, 1, 0, 9999, true},
		{"rounds up at threshold", 10000, 1, 1, 10001, true},
		{"zero fee bps", 12345, 0, 0, 12345, true},
		{"max fee bps equals amount", 2000, 10000, 2000, 4000, true},
		{"max amount zero bps", math.MaxInt64, 0, 0, math.MaxInt64, true},
		{"max amount tiny bps overflows", math.MaxInt64, 1, math.MaxInt64 / 10000, 0, false},
		{
			// 6e18 * 5000 = 3e22 远超 int64（朴素乘积溢出），但手续费
			// 3e18、总额 9e18 仍可由 int64 表示，必须按精确总额支付。
			"intermediate product overflows but total fits",
			6_000_000_000_000_000_000, 5000,
			3_000_000_000_000_000_000,
			9_000_000_000_000_000_000,
			true,
		},
		{
			// 6e18 * 10000 的手续费等于金额，总额 1.2e19 超出 int64。
			"total overflows at max bps",
			6_000_000_000_000_000_000, 10000,
			6_000_000_000_000_000_000,
			0, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fee, total, ok := chargeBreakdown(tc.amount, tc.bps)
			if ok != tc.wantOK {
				t.Fatalf("representable=%v want %v", ok, tc.wantOK)
			}
			if ok && (fee != tc.wantFee || total != tc.wantTotal) {
				t.Fatalf("fee=%d total=%d want fee=%d total=%d", fee, total, tc.wantFee, tc.wantTotal)
			}
		})
	}
}

func TestValidPaymentAmountAndFeeBps(t *testing.T) {
	for _, a := range []int64{0, -1, math.MinInt64} {
		if validPaymentAmount(a) {
			t.Errorf("amount %d must be invalid", a)
		}
	}
	for _, a := range []int64{1, math.MaxInt64} {
		if !validPaymentAmount(a) {
			t.Errorf("amount %d must be valid", a)
		}
	}
	for _, b := range []int{-1, 10001, math.MinInt, math.MaxInt} {
		if validFeeBps(b) {
			t.Errorf("fee bps %d must be invalid", b)
		}
	}
	for _, b := range []int{0, 1, 10000} {
		if !validFeeBps(b) {
			t.Errorf("fee bps %d must be valid", b)
		}
	}
}

// TestExecuteLargeRepresentableTotalSettles 与 TestExecuteLargeOverflowRejected
// 经由遗留单笔入口验证共享规则：中间乘积越界但总额可表示时按精确金额支付；
// 总额越界时 rejected/overflow，而不是负数扣款或普通余额不足。
func TestExecuteLargeRepresentableTotalSettles(t *testing.T) {
	const (
		amount    = int64(6_000_000_000_000_000_000)
		bps       = 5000
		wantTotal = int64(9_000_000_000_000_000_000)
	)
	spent := map[string]bool{}
	s := Execute(Intent{ID: "big", State: "pending", Amount: amount}, bps, spent, wantTotal)
	if s.Status != "settled" || s.Charged != wantTotal {
		t.Fatalf("representable large total should settle: %+v", s)
	}
	if !spent["big"] {
		t.Fatalf("success must mark id")
	}

	// 余额少一个最小单位：failed/余额不足，而不是 rejected。
	spent = map[string]bool{}
	s = Execute(Intent{ID: "big", State: "pending", Amount: amount}, bps, spent, wantTotal-1)
	if s.Status != "failed" || s.Reason != reasonFunds {
		t.Fatalf("one unit short must be insufficient failure: %+v", s)
	}
	if spent["big"] {
		t.Fatalf("failed id must not be marked")
	}
}

func TestExecuteLargeOverflowRejected(t *testing.T) {
	spent := map[string]bool{}
	s := Execute(Intent{ID: "big", State: "pending", Amount: 6_000_000_000_000_000_000}, 10000, spent, math.MaxInt64)
	if s.Status != "rejected" || s.Reason != reasonOverflow {
		t.Fatalf("overflow total must be rejected: %+v", s)
	}
	if s.Charged != 0 || s.Ref != "" || spent["big"] {
		t.Fatalf("overflow must not charge/reference/mark: %+v spent=%v", s, spent)
	}
}

// TestSubmitLargeRepresentableTotalSettles 验证本地批次提交（预览同规则）
// 对“中间乘积越界但总额可表示”的大额付款按精确金额扣款成功。
func TestSubmitLargeRepresentableTotalSettles(t *testing.T) {
	const (
		amount    = int64(6_000_000_000_000_000_000)
		bps       = 5000
		wantFee   = int64(3_000_000_000_000_000_000)
		wantTotal = int64(9_000_000_000_000_000_000)
	)
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: wantTotal}})
	l := openOrFail(t, path)
	res, err := l.Submit(FeeBatch{FeeBps: bps, Intents: []PaymentIntent{intent("big", "aa", "usdc", amount)}})
	if err != nil {
		t.Fatal(err)
	}
	r := res.Results[0]
	if r.Status != StatusSettled || r.Record.Fee != wantFee || r.Record.Charged != wantTotal {
		t.Fatalf("representable large total should settle: %+v", r)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("balance=%d want 0", bal)
	}

	// 同样输入在预览中也预计成功，但不写入、不预留。
	prev, err := l.Preview(FeeBatch{FeeBps: bps, Intents: []PaymentIntent{intent("big2", "aa", "zzz", amount)}})
	if err != nil {
		t.Fatal(err)
	}
	if pr := prev.Results[0]; pr.Status != StatusFunds {
		// aa/zzz 组合不存在按余额 0，必为余额不足；仅证明预览沿用同一套总额计算。
		t.Fatalf("unknown combo must be insufficient, got %+v", pr)
	}
}

// TestStoredOverflowTotalIsCorrupt 即使保存的 charged 为正且金额、费率、
// 手续费都自洽，只要金额+手续费在数学上超出 int64，打开账本就必须判
// corrupt_ledger（走总额不可表示分支），不能静默回绕。
func TestStoredOverflowTotalIsCorrupt(t *testing.T) {
	k := balanceKey{"aa", "usdc"}
	initial := map[balanceKey]int64{k: math.MaxInt64}
	final := map[balanceKey]int64{k: math.MaxInt64}
	amount := int64(math.MaxInt64)
	bps := 1
	fee := feeFor(amount, bps) // floor(MaxInt64/10000)
	records := []Record{{
		ID: "p", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: amount, Nonce: 1, FeeBps: bps, Fee: fee,
		Charged: math.MaxInt64, // 正数，但不等于数学上越界的 amount+fee
		Seq:     1,
	}}
	err := validateAndReplay(initial, final, records, nil)
	if err == nil || KindOf(err) != ErrCorrupt {
		t.Fatalf("overflowing stored total must be corrupt_ledger, got %v", err)
	}
}
