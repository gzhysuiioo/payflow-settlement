package payflow

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 本文件回归“打开账本时的历史余额校验”在退款时点（after_seq）上的既有规则：
//
//	最终余额能对上，并不等于账本合法——重放到任一付款时点，累计净扣款
//	（含该笔付款当时的手续费）都不得超过初始余额。退款只能冲减它落账时点
//	（after_seq）之后的累计扣款，不能提前拿去抵扣尚未发生的付款。
//
// 对照场景（同一账户同一资产初始 100，两笔成功付款 70、60，全额退回
// 第一笔，费率为零，文件最终余额都写 40，且文件均带正确校验和、通过
// 结构与费用检查）：
//   - after_seq=2（付款70、付款60 之后才退款）：第二笔付款发生时
//     70+60>100，历史透支，即使末态 40 正确也必须判 corrupt_ledger；
//   - after_seq=1（付款70、退款70、再付款60）：余额合法复用，账本正常
//     打开，余额 40，两笔付款与一笔退款的编号、成功顺序与关联完整保留。
//
// 另覆盖手续费边界：付款金额连同手续费恰好用尽当时可用余额可以接受；
// 多一个最小单位则拒绝，即使后续退款让最终余额恢复正常。
//
// 所有用例都直接构造磁盘账本文件并经由 Open 的真实打开行为验证
// （进程内此前无该账本的任何句柄），不使用截断文件或校验和不符代替
// 历史余额问题；无论接受还是拒绝，读取都不得改写文件，拒绝也不得
// 返回可查询的句柄或把损坏历史当成空账本。

// signedHistoryFile 用与正式写入完全相同的序列化（ledgerState.MarshalJSON，
// 内含规范字段顺序与 sha256 校验和）把一段给定历史写到 path，返回写入的
// 原始字节。这样产出的文件校验和正确、结构合法，打开失败只能归因于
// validateAndReplay 的历史重放语义，而不是格式或校验和问题。
func signedHistoryFile(t *testing.T, path string, initial, final []BalanceView, records []Record, refunds []RefundRecord) []byte {
	t.Helper()
	st := &ledgerState{
		Version:     ledgerVersion,
		Initial:     make(map[balanceKey]int64, len(initial)),
		Balances:    make(map[balanceKey]int64, len(final)),
		Settlements: records,
		Refunds:     refunds,
	}
	for _, b := range initial {
		st.Initial[balanceKey{b.Account, b.Asset}] = b.Balance
	}
	for _, b := range final {
		st.Balances[balanceKey{b.Account, b.Asset}] = b.Balance
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal signed ledger: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return data
}

// historyPayments70And60 构造两笔成功序号连续、编号唯一、费率为零的付款：
// p1 扣款 70（seq 1）、p2 扣款 60（seq 2）。
func historyPayments70And60() []Record {
	return []Record{
		{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 70, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 70, Seq: 1},
		{ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 60, Nonce: 2, FeeBps: 0, Fee: 0, Charged: 60, Seq: 2},
	}
}

// refundOfFirstPayment 构造第一笔付款 p1 的全额退款：目标、金额（含手续费）
// 与去向（账户/资产）全部与原结算逐字一致，退款成功序号为 1；
// afterSeq 唯一可变，用于区分“退款发生在两笔付款之后”与“两笔付款之间”。
func refundOfFirstPayment(afterSeq int64) RefundRecord {
	return RefundRecord{
		ID: "r1", SettlementID: "p1", Reason: "customer cancelled",
		Account: "aa", Asset: "usdc",
		Amount: 70, Fee: 0, Charged: 70,
		AfterSeq: afterSeq, Seq: 1,
	}
}

// 非法对照：退款 after_seq=2，表示两笔付款（70、60）都成功后才退回第一笔。
// 第二笔付款发生时累计扣款 130 已超过初始余额 100，尽管退款让文件末态
// 恰好回到 40，打开也必须返回 corrupt_ledger；不得提前用后来的退款抵扣
// 第二笔付款。拒绝原因必须来自第二笔付款当时的余额不足，而不是其他
// 不相关的结构/校验和错误。
func TestOpenRejectsOverdraftBeforeLateRefundDespiteFinalBalance40(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	data := signedHistoryFile(t, path,
		[]BalanceView{{Account: "aa", Asset: "usdc", Balance: 100}},
		[]BalanceView{{Account: "aa", Asset: "usdc", Balance: 40}},
		historyPayments70And60(),
		[]RefundRecord{refundOfFirstPayment(2)},
	)

	l, err := Open(path)
	if l != nil {
		_ = l.Close()
		t.Fatalf("corrupt history must not yield a usable ledger, got %+v", l)
	}
	if KindOf(err) != ErrCorrupt {
		t.Fatalf("Open: want %s, got %v", ErrCorrupt, err)
	}
	le, ok := err.(*LedgerError)
	if !ok {
		t.Fatalf("want *LedgerError, got %T: %v", err, err)
	}
	// 失败必须发生在交错重放、且指向第二笔付款 p2 透支：这证明文件已通过
	// 校验和、结构、费用与退款引用检查，唯一问题是 p2 发生时余额不足，
	// 而不是末态不平或任何格式问题。
	if msg := le.Message; !bytes.Contains([]byte(msg), []byte(`"p2"`)) ||
		!bytes.Contains([]byte(msg), []byte("negative")) {
		t.Fatalf("rejection must attribute to p2 overdraft at its payment time, got %q", msg)
	}

	// 读取不改写被拒绝的文件，逐字节保留。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("rejected open must not rewrite the ledger file")
	}

	// 损坏结果不得被缓存为“可用”：再次打开仍然拒绝且拿不到句柄。
	if l2, err2 := Open(path); l2 != nil || KindOf(err2) != ErrCorrupt {
		if l2 != nil {
			_ = l2.Close()
		}
		t.Fatalf("reopen must stay rejected, got handle=%v err=%v", l2, err2)
	}
	// 损坏历史不能被重新初始化顶替成空账本（文件已占用，原样保留）。
	if err := CreateLedger(path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}}); KindOf(err) != ErrExists {
		t.Fatalf("init over corrupt history want %s, got %v", ErrExists, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
		t.Fatalf("failed re-init must leave the corrupt file byte-intact")
	}
}

// 合法对照：同一笔退款 after_seq=1，表示先付款 70、退回 70、再付款 60。
// 付款与退款内容、序号、最终余额 40 都与非法对照相同，但每个付款时点
// 余额都充足（退回的余额合法复用），账本必须正常打开，查询余额 40，
// 且两笔付款与一笔退款的原编号、成功顺序与关联完整保留。
func TestOpenAcceptsRefundBetweenPaymentsAndPreservesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	data := signedHistoryFile(t, path,
		[]BalanceView{{Account: "aa", Asset: "usdc", Balance: 100}},
		[]BalanceView{{Account: "aa", Asset: "usdc", Balance: 40}},
		historyPayments70And60(),
		[]RefundRecord{refundOfFirstPayment(1)},
	)

	l, err := Open(path)
	if err != nil {
		t.Fatalf("legal pay→refund→pay history must open: %v", err)
	}
	snap, err := l.Query()
	if err != nil {
		t.Fatalf("query opened ledger: %v", err)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Balance != 40 {
		t.Fatalf("balance=%+v want single aa/usdc 40", snap.Balances)
	}
	if bal, err := l.Balance("aa", "usdc"); err != nil || bal != 40 {
		t.Fatalf("Balance aa/usdc=%d err=%v, want 40", bal, err)
	}

	// 两笔付款原编号与成功顺序完整保留，序号各自连续。
	wantRecs := []Record{
		{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 70, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 70, Seq: 1},
		{ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 60, Nonce: 2, FeeBps: 0, Fee: 0, Charged: 60, Seq: 2},
	}
	if len(snap.Settlements) != len(wantRecs) {
		t.Fatalf("settlements=%+v want 2 records", snap.Settlements)
	}
	for i, want := range wantRecs {
		if snap.Settlements[i] != want {
			t.Fatalf("settlement %d=%+v want %+v", i, snap.Settlements[i], want)
		}
	}

	// 一笔退款原编号、成功序号、时点（after_seq=1）与对原付款的关联完整保留，
	// 金额、手续费、扣款总额与去向与 p1 逐字一致。
	if len(snap.Refunds) != 1 {
		t.Fatalf("refunds=%+v want exactly 1", snap.Refunds)
	}
	rf := snap.Refunds[0]
	wantRF := refundOfFirstPayment(1)
	if rf != wantRF {
		t.Fatalf("refund=%+v want %+v", rf, wantRF)
	}
	if rf.SettlementID != snap.Settlements[0].ID {
		t.Fatalf("refund must link back to first payment p1, got %q", rf.SettlementID)
	}

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// 纯读取（打开 + 查询）不改写文件。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("opening/querying a legal ledger must not rewrite the file")
	}

	// 关闭后经文件重新打开仍得到同一合法状态。
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen legal history: %v", err)
	}
	defer l2.Close()
	if bal, _ := l2.Balance("aa", "usdc"); bal != 40 {
		t.Fatalf("reopen balance=%d want 40", bal)
	}
	snap2, _ := l2.Query()
	if len(snap2.Settlements) != 2 || len(snap2.Refunds) != 1 || snap2.Refunds[0].SettlementID != "p1" {
		t.Fatalf("reopen history not preserved: %+v / %+v", snap2.Settlements, snap2.Refunds)
	}
}

// 手续费必须计入付款“当时”的历史余额判断：
//   - 一笔扣款（金额+手续费）恰好用尽当时可用余额：接受；
//   - 只比可用余额多一个最小单位（多出的 1 完全来自手续费）：拒绝，
//     即使之后的全额退款让最终余额恢复正常。
func TestReplayIncludesFeeAtPaymentTimeBoundary(t *testing.T) {
	// 先扣 49（费率 0），第二笔付款发生时可用余额恰为 51。
	first := []Record{
		{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 49, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 49, Seq: 1},
	}
	// 两笔付款之后全额退回 p1（charged 49），after_seq=2。
	refundP1 := []RefundRecord{{
		ID: "r1", SettlementID: "p1", Reason: "customer cancelled",
		Account: "aa", Asset: "usdc", Amount: 49, Fee: 0, Charged: 49,
		AfterSeq: 2, Seq: 1,
	}}
	initial := []BalanceView{{Account: "aa", Asset: "usdc", Balance: 100}}

	t.Run("charged with fee exhausts available balance exactly", func(t *testing.T) {
		// 50*200/10000 = 1：charged=51 恰好等于当时可用余额 51。
		if got := feeFor(50, 200); got != 1 {
			t.Fatalf("test setup: feeFor(50,200)=%d want 1", got)
		}
		records := append(append([]Record{}, first...),
			Record{ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 50, Nonce: 2, FeeBps: 200, Fee: 1, Charged: 51, Seq: 2})
		// 重放：p1 净扣 49，p2 净扣 100（恰好用尽），退款 49，末态 49。
		final := []BalanceView{{Account: "aa", Asset: "usdc", Balance: 49}}
		path := filepath.Join(t.TempDir(), "ledger.json")
		data := signedHistoryFile(t, path, initial, final, records, refundP1)

		l, err := Open(path)
		if err != nil {
			t.Fatalf("exact exhaustion including fee must be accepted: %v", err)
		}
		if bal, _ := l.Balance("aa", "usdc"); bal != 49 {
			t.Fatalf("balance=%d want 49", bal)
		}
		snap, _ := l.Query()
		if len(snap.Settlements) != 2 || snap.Settlements[1].Fee != 1 || snap.Settlements[1].Charged != 51 {
			t.Fatalf("p2 fee accounting not preserved: %+v", snap.Settlements)
		}
		if len(snap.Refunds) != 1 || snap.Refunds[0].Charged != 49 {
			t.Fatalf("refund not preserved: %+v", snap.Refunds)
		}
		_ = l.Close()
		if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
			t.Fatalf("opening accepted ledger must not rewrite it")
		}
	})

	t.Run("one minimum unit over available balance is rejected", func(t *testing.T) {
		// 50*400/10000 = 2：charged=52 比可用余额 51 多恰好 1 个最小单位；
		// 若忽略手续费，金额 50 本身仍在可用余额之内——本用例据此锁定
		// “手续费计入当时余额”的规则。
		if got := feeFor(50, 400); got != 2 {
			t.Fatalf("test setup: feeFor(50,400)=%d want 2", got)
		}
		records := append(append([]Record{}, first...),
			Record{ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 50, Nonce: 2, FeeBps: 400, Fee: 2, Charged: 52, Seq: 2})
		// 按记录账面运算末态为 48（退款后“恢复正常”的正数余额），
		// 但 p2 发生时净扣 101 已透支，必须拒绝。
		final := []BalanceView{{Account: "aa", Asset: "usdc", Balance: 48}}
		path := filepath.Join(t.TempDir(), "ledger.json")
		data := signedHistoryFile(t, path, initial, final, records, refundP1)

		l, err := Open(path)
		if l != nil {
			_ = l.Close()
			t.Fatalf("overdraft by one unit must not yield a usable ledger")
		}
		if KindOf(err) != ErrCorrupt {
			t.Fatalf("want %s, got %v", ErrCorrupt, err)
		}
		le, ok := err.(*LedgerError)
		if !ok || !bytes.Contains([]byte(le.Message), []byte(`"p2"`)) ||
			!bytes.Contains([]byte(le.Message), []byte("negative")) {
			t.Fatalf("rejection must attribute to p2 overdraft, got %v", err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
			t.Fatalf("rejected open must not rewrite the ledger file")
		}
	})
}
