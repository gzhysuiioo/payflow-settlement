package payflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 本文件为“打开账本时的历史余额校验”补充自动化回归保障，锁定既有规则：
// 最终余额能对上还不够，历史中任一时点都不允许透支——后来的成功退款不能
// 提前抵扣它发生之前的付款。两个用例只改退款的 after_seq，其余逐字相同：
//
//	同一账户 aa、同一资产 usdc，初始余额 100，费率均为零。
//	p1 成功扣款 70（seq=1），p2 成功扣款 60（seq=2），
//	r1 全额退回 p1（amount=fee 口径与原付款一致，charged=70，退款 seq=1），
//	文件最终余额都写 40（100-70-60+70，与退款时点无关，末态本身都对得上）。
//
//   - after_seq=2：退款发生在两笔付款之后。p2 发生时可用余额只有 30，
//     扣款 60 透支（净扣款 130 > 初始 100）；末态 40 正确也必须判
//     corrupt_ledger，且拒绝原因必须是 p2 时点余额不足，而不是截断、
//     校验和、序号或费用等任何其他问题。
//   - after_seq=1：真实顺序是“付 70 → 退 70 → 再付 60”。p2 发生时
//     退款已落账、可用余额为 100，扣款 60 合法；毛扣款 130 虽超过
//     初始 100，也必须正常打开并查到余额 40 与完整历史。
//
// 两份文件都用账本自身的序列化写出（带正确 sha256 校验和，结构与费用
// 检查均能通过），对照真正经过 Open 的文件读取行为，而不是直接调用
// 重放校验器，也不用截断文件或校验和不符来代替历史余额问题。
// 无论接受还是拒绝，读取都不得改写文件；拒绝后不得返回可查询的句柄，
// 也不得把损坏历史当成空账本重新初始化。

const (
	histAcct   = "aa"
	histAsset  = "usdc"
	histInit   = int64(100)
	histFinal  = int64(40)
	histP1Amt  = int64(70)
	histP2Amt  = int64(60)
	histReason = "cancel p1"
)

// historicalOverdraftState 构造上述对照场景：两笔零费率成功付款 p1=70、
// p2=60（成功序号 1、2，编号唯一），一笔全额退回 p1 的退款 r1
// （目标、金额、去向与 p1 完全一致，退款序号 1），最终余额写 40。
// afterSeq 唯一控制退款时点：2 表示两笔付款之后（历史透支），
// 1 表示 p1 之后、p2 之前（余额合法复用）。
func historicalOverdraftState(afterSeq int64) *ledgerState {
	k := balanceKey{histAcct, histAsset}
	return &ledgerState{
		Version:  ledgerVersion,
		Initial:  map[balanceKey]int64{k: histInit},
		Balances: map[balanceKey]int64{k: histFinal},
		Settlements: []Record{
			{ID: "p1", Account: histAcct, Paymaster: "pm", Asset: histAsset,
				Amount: histP1Amt, Nonce: 1, FeeBps: 0, Fee: 0, Charged: histP1Amt, Seq: 1},
			{ID: "p2", Account: histAcct, Paymaster: "pm", Asset: histAsset,
				Amount: histP2Amt, Nonce: 2, FeeBps: 0, Fee: 0, Charged: histP2Amt, Seq: 2},
		},
		Refunds: []RefundRecord{
			{ID: "r1", SettlementID: "p1", Reason: histReason,
				Account: histAcct, Asset: histAsset,
				Amount: histP1Amt, Fee: 0, Charged: histP1Amt,
				AfterSeq: afterSeq, Seq: 1},
		},
	}
}

// writeSignedHistoryFile 用账本自身的序列化把状态写到 path：MarshalJSON 会
// 附上与无校验和文档匹配的 sha256，因此文件完好、能通过结构与费用检查；
// 返回写入的原始字节供“读取不得改写文件”比对。
func writeSignedHistoryFile(t *testing.T, path string, st *ledgerState) []byte {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal ledger: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return data
}

// assertHistoryFileChecksumWellFormed 独立复核文件字节带正确校验和：
// 重新计算无校验和文档的 sha256 必须与文件中的 checksum 完全一致。
// 这样一旦 Open 拒绝该文件，原因只可能来自随后的历史余额校验。
func assertHistoryFileChecksumWellFormed(t *testing.T, data []byte) {
	t.Helper()
	var doc signedDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("history file must be structurally parseable: %v\n%s", err, data)
	}
	if doc.Checksum == "" {
		t.Fatalf("history file must carry a checksum: %s", data)
	}
	payload, err := json.Marshal(doc.unsignedDoc)
	if err != nil {
		t.Fatalf("re-encode unsigned doc: %v", err)
	}
	if got := checksumHex(payload); got != doc.Checksum {
		t.Fatalf("history file checksum must be valid: file %s, want %s", doc.Checksum, got)
	}
}

// assertFileByteIntact 断言失败/只读的打开没有改写磁盘文件。
func assertFileByteIntact(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ledger file was rewritten by open:\nbefore=%s\nafter =%s", want, got)
	}
}

// TestOpenRejectsHistoricalOverdraftHiddenByFinalBalance：退款 after_seq=2，
// 即两笔付款之后才全额退回 p1。尽管文件最终余额 40 与重放末态一致，
// p2 发生时退款尚未发生、可用余额仅 30，扣款 60 在历史中透支。
// Open 必须返回 corrupt_ledger，且：
//   - 拒绝原因明确指向第二笔付款 p2 发生时的余额不足（不是后来的退款
//     被提前拿去抵扣 p2，也不是截断/校验和/序号/费用等其他错误）；
//   - 不返回任何可查询句柄，损坏历史不能被当成空账本继续使用或重建；
//   - 文件逐字节保持不变，重复打开结论一致。
func TestOpenRejectsHistoricalOverdraftHiddenByFinalBalance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	data := writeSignedHistoryFile(t, path, historicalOverdraftState(2))
	assertHistoryFileChecksumWellFormed(t, data)

	l, err := Open(path)
	if l != nil {
		_ = l.Close()
		t.Fatalf("historically overdrawn ledger must not yield a usable handle, got %+v", l)
	}
	if KindOf(err) != ErrCorrupt {
		t.Fatalf("Open after_seq=2: want %s, got %v", ErrCorrupt, err)
	}
	// 拒绝必须来自 p2 发生时点的余额不足：交错重放不会把 after_seq=2 的
	// 退款提前用于 p2。错误信息点名 p2 与“重放中透支”，排除格式类原因。
	var le *LedgerError
	if !errors.As(err, &le) {
		t.Fatalf("want *LedgerError, got %T: %v", err, err)
	}
	if !bytes.Contains([]byte(le.Message), []byte(`"p2"`)) ||
		!bytes.Contains([]byte(le.Message), []byte("negative during replay")) {
		t.Fatalf("rejection must cite p2 being insufficient at its time, got %q", le.Message)
	}

	// 读取不改写被拒绝的文件，文件仍完好（校验和仍正确）。
	assertFileByteIntact(t, path, data)
	assertHistoryFileChecksumWellFormed(t, data)

	// 损坏历史不能被当成空账本：重复打开仍被拒绝，也不允许原地重新初始化。
	if l2, err2 := Open(path); l2 != nil || KindOf(err2) != ErrCorrupt {
		if l2 != nil {
			_ = l2.Close()
		}
		t.Fatalf("reopen must keep rejecting corrupt history, got handle=%v err=%v", l2, err2)
	}
	if err := CreateLedger(path, nil); KindOf(err) != ErrExists {
		t.Fatalf("corrupt history must not be re-initialized as an empty ledger, got %v", err)
	}
	assertFileByteIntact(t, path, data)
}

// TestOpenAcceptsSameFinalBalanceWhenRefundComesFirst：同一退款改为
// after_seq=1，真实顺序是“付 70 → 退 70 → 再付 60”，其余付款内容与
// 最终余额 40 完全相同。合法的余额重复使用（毛扣款 130 > 初始 100）
// 不得被误判为累计扣款超限：Open 必须成功，余额查到 40，且完整保留
// 两笔付款与一笔退款的原编号、成功顺序与关联。打开与查询都是只读，
// 不得改写文件。
func TestOpenAcceptsSameFinalBalanceWhenRefundComesFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	data := writeSignedHistoryFile(t, path, historicalOverdraftState(1))
	assertHistoryFileChecksumWellFormed(t, data)

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open after_seq=1 (pay 70, refund 70, pay 60): %v", err)
	}
	defer l.Close()

	if bal, err := l.Balance(histAcct, histAsset); err != nil || bal != histFinal {
		t.Fatalf("balance=%d (err=%v), want %d", bal, err, histFinal)
	}
	snap := mustQuery(t, l)
	if len(snap.Balances) != 1 ||
		snap.Balances[0] != (BalanceView{Account: histAcct, Asset: histAsset, Balance: histFinal}) {
		t.Fatalf("balances=%+v want single aa/usdc=40", snap.Balances)
	}

	// 两笔付款按成功顺序保留原编号、序号、金额与零费用。
	wantP1 := Record{ID: "p1", Account: histAcct, Paymaster: "pm", Asset: histAsset,
		Amount: histP1Amt, Nonce: 1, FeeBps: 0, Fee: 0, Charged: histP1Amt, Seq: 1}
	wantP2 := Record{ID: "p2", Account: histAcct, Paymaster: "pm", Asset: histAsset,
		Amount: histP2Amt, Nonce: 2, FeeBps: 0, Fee: 0, Charged: histP2Amt, Seq: 2}
	if len(snap.Settlements) != 2 || snap.Settlements[0] != wantP1 || snap.Settlements[1] != wantP2 {
		t.Fatalf("settlements not preserved with original ids/order:\n got %+v\nwant [%+v %+v]",
			snap.Settlements, wantP1, wantP2)
	}

	// 退款保留原编号、退款成功序号、after_seq=1 的时点，以及与 p1 的关联和金额去向。
	wantR1 := RefundRecord{ID: "r1", SettlementID: "p1", Reason: histReason,
		Account: histAcct, Asset: histAsset,
		Amount: histP1Amt, Fee: 0, Charged: histP1Amt, AfterSeq: 1, Seq: 1}
	if len(snap.Refunds) != 1 || snap.Refunds[0] != wantR1 {
		t.Fatalf("refund not preserved with original id/order/link:\n got %+v\nwant %+v",
			snap.Refunds, wantR1)
	}

	// 打开与查询只读：文件逐字节保持为带正确校验和的原样。
	assertFileByteIntact(t, path, data)

	// 关闭全部句柄后以磁盘权威重开（模拟新进程），结论必须一致。
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2 := openOrFail(t, path)
	defer l2.Close()
	if bal, _ := l2.Balance(histAcct, histAsset); bal != histFinal {
		t.Fatalf("reopen balance=%d, want %d", bal, histFinal)
	}
	snap2 := mustQuery(t, l2)
	if len(snap2.Settlements) != 2 || len(snap2.Refunds) != 1 ||
		snap2.Settlements[0].ID != "p1" || snap2.Settlements[1].ID != "p2" ||
		snap2.Refunds[0].ID != "r1" || snap2.Refunds[0].SettlementID != "p1" ||
		snap2.Refunds[0].AfterSeq != 1 {
		t.Fatalf("reopen history not preserved: %+v / %+v", snap2.Settlements, snap2.Refunds)
	}
	assertFileByteIntact(t, path, data)
}

// TestOpenHistoricalBalanceIncludesFeeAtPaymentTime：历史余额判断必须把
// 付款当时的手续费计入该时点的扣款。两份文件都只有 after_seq=2 的一笔
// 退款（退 p1 的全额 charged），最终余额分别与各自重放末态一致：
//   - "exact use"：初始 140，p1 扣 70+手续费10=80，p2 扣 60（零费用），
//     p2 发生时净扣款恰好 140，等于可用余额——必须接受，随后退款 80，
//     末态 80。
//   - "one unit over"：初始 139，记录逐字相同，p2 发生时净扣款 140，
//     比可用余额多一个最小单位——即使 after_seq=2 的退款随后让末态
//     （79）恢复正常，也必须以 p2 时点透支拒绝。
func TestOpenHistoricalBalanceIncludesFeeAtPaymentTime(t *testing.T) {
	// p1：amount=70，费率 1429 基点 -> feeFor(70,1429)=10，charged=80。
	const feeBpsP1 = 1429
	const p1Fee = int64(10)
	const p1Charged = histP1Amt + p1Fee // 80
	build := func(initial, final int64) *ledgerState {
		k := balanceKey{histAcct, histAsset}
		return &ledgerState{
			Version:     ledgerVersion,
			Initial:     map[balanceKey]int64{k: initial},
			Balances:    map[balanceKey]int64{k: final},
			Settlements: nil,
			Refunds: []RefundRecord{{
				ID: "r1", SettlementID: "p1", Reason: histReason,
				Account: histAcct, Asset: histAsset,
				Amount: histP1Amt, Fee: p1Fee, Charged: p1Charged,
				AfterSeq: 2, Seq: 1,
			}},
		}
	}
	records := []Record{
		{ID: "p1", Account: histAcct, Paymaster: "pm", Asset: histAsset,
			Amount: histP1Amt, Nonce: 1, FeeBps: feeBpsP1, Fee: p1Fee, Charged: p1Charged, Seq: 1},
		{ID: "p2", Account: histAcct, Paymaster: "pm", Asset: histAsset,
			Amount: histP2Amt, Nonce: 2, FeeBps: 0, Fee: 0, Charged: histP2Amt, Seq: 2},
	}

	t.Run("exact use accepted", func(t *testing.T) {
		// 防护：本用例依赖“p1 含 10 手续费且费用口径自洽”，先直接核对，
		// 确保接受/拒绝的分界确实只在历史余额，而非费用字段。
		if got := feeFor(histP1Amt, feeBpsP1); got != p1Fee {
			t.Fatalf("test setup: feeFor(70,%d)=%d want %d", feeBpsP1, got, p1Fee)
		}
		st := build(140, 80) // 140-80-60+80=80
		st.Settlements = records

		path := filepath.Join(t.TempDir(), "ledger.json")
		data := writeSignedHistoryFile(t, path, st)
		assertHistoryFileChecksumWellFormed(t, data)

		l, err := Open(path)
		if err != nil {
			t.Fatalf("charging exactly the available balance (fee included) must be accepted: %v", err)
		}
		defer l.Close()
		if bal, _ := l.Balance(histAcct, histAsset); bal != 80 {
			t.Fatalf("balance=%d want 80", bal)
		}
		snap := mustQuery(t, l)
		if len(snap.Settlements) != 2 || len(snap.Refunds) != 1 ||
			snap.Settlements[0].Charged != p1Charged || snap.Refunds[0].Charged != p1Charged {
			t.Fatalf("history not preserved: %+v / %+v", snap.Settlements, snap.Refunds)
		}
		assertFileByteIntact(t, path, data)
	})

	t.Run("one unit over rejected", func(t *testing.T) {
		st := build(139, 79) // 末态自洽：139-80-60+80=79
		st.Settlements = records

		path := filepath.Join(t.TempDir(), "ledger.json")
		data := writeSignedHistoryFile(t, path, st)
		assertHistoryFileChecksumWellFormed(t, data)

		l, err := Open(path)
		if l != nil {
			_ = l.Close()
			t.Fatalf("one-unit historical overdraft must not yield a handle, got %+v", l)
		}
		if KindOf(err) != ErrCorrupt {
			t.Fatalf("want %s, got %v", ErrCorrupt, err)
		}
		var le *LedgerError
		if !errors.As(err, &le) ||
			!bytes.Contains([]byte(le.Message), []byte(`"p2"`)) ||
			!bytes.Contains([]byte(le.Message), []byte("negative during replay")) {
			t.Fatalf("rejection must cite p2 overdraft at its time (fee included), got %v", err)
		}
		// 拒绝后无句柄、文件不改写，重复打开仍拒绝（不能当空账本使用）。
		assertFileByteIntact(t, path, data)
		if l2, err2 := Open(path); l2 != nil || KindOf(err2) != ErrCorrupt {
			if l2 != nil {
				_ = l2.Close()
			}
			t.Fatalf("reopen must keep rejecting, got handle=%v err=%v", l2, err2)
		}
		assertFileByteIntact(t, path, data)
	})
}
