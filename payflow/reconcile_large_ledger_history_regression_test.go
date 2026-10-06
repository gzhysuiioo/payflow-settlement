package payflow

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

// 本文件为“大额账本历史”补充离线对账的自动化回归保障。
//
// 单笔付款与单笔退款都在 int64 范围内，但“全额退款释放同一笔钱后可再次
// 付款”的合法历史，其累计扣款与累计退款仍可能同时超过 int64 上界：
//   - 初始余额 9223372036854775807（math.MaxInt64）；
//   - 连续三轮“付款 4000000000000000000（30 基点，手续费
//     12000000000000000、扣款总额 4012000000000000000）→ 全额退款
//     （退回当时已扣手续费）→ 再付款”，每轮使用不同的付款与退款编号；
//   - 三轮后余额回到初始值，而两侧历史的累计扣款与累计退款均为
//     12036000000000000000（= 4012000000000000000*3，约为 MaxInt64 的
//     1.30 倍），已超过 int64 上界。
//
// 因此报告必须分别保留扣款合计与退款合计的精确十进制值，不能只凭最终
// 余额正常（回到 MaxInt64）或净扣款/净差额为 "0" 就算金额正确；也不能
// 截断、取近似值或变成科学计数法。以下回归同时覆盖全量对账与“只选全部
// 退款、付款范围为空”的按范围对账（含退款流水完整与退款范围收到空流水
// 两种外部流水形态），并确认对账对账本余额与付款、退款历史只读。

const (
	// regLLInitialBalance = math.MaxInt64 = 9223372036854775807。
	regLLInitialBalance = int64(math.MaxInt64)
	// regLLPayAmount 每轮付款金额：4e18。
	regLLPayAmount = int64(4_000_000_000_000_000_000)
	// regLLFee 30 基点手续费：4e18*30/10000 = 12e15。
	regLLFee = int64(12_000_000_000_000_000)
	// regLLCharged 每轮扣款总额（金额+手续费）：4.012e18，int64 内。
	regLLCharged = int64(4_012_000_000_000_000_000)
	// regLLTotal 三轮累计：12036000000000000000，超过 int64 上界。
	regLLTotal = "12036000000000000000"
)

// regLLAccount / regLLAsset 固定账户与资产。
const (
	regLLAccount = "aa-ll"
	regLLAsset   = "usdc"
)

// regLargeLedger 构造三轮“付款 → 全额退款（含手续费）→ 再付款”的大额
// 历史：p1→r1（退 p1）→ p2→r2（退 p2）→ p3→r3（退 p3）。每笔付款
// amount=4e18、fee=12e15、charged=4.012e18；每笔退款把原扣款（含当时
// 已扣手续费）全额退回。三轮后余额回到 MaxInt64，累计扣款=累计退款=
// 12.036e18（>int64 上界）。
//
// 关键在于：第 1、2 轮毛扣款已经把余额打低，只有退款把同一笔钱退回后，
// 下一轮才能在当前余额上再扣一次——单笔始终在 int64 内，累计毛额却越过
// int64 上界。账本历史校验按净额交错重放，因此这是合法历史。
func regLargeLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := newTestLedger(t, []BalanceInit{{
		Account: regLLAccount, Asset: regLLAsset, Balance: regLLInitialBalance,
	}})
	l := openOrFail(t, path)
	for i := 0; i < 3; i++ {
		pid := "p" + string(rune('1'+i))
		rid := "r" + string(rune('1'+i))
		res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{{
			ID: pid, Account: regLLAccount, Paymaster: "pm",
			Asset: regLLAsset, Amount: i64p(regLLPayAmount),
			Nonce: uint64(i + 1), State: "pending",
		}}})
		if err != nil {
			t.Fatalf("submit %s: %v", pid, err)
		}
		if s := statuses(res); len(s) != 1 || s[0] != StatusSettled {
			t.Fatalf("%s status=%v", pid, s)
		}
		regLLAssertRefund(t, l, rid, pid, int64(i+1))
	}

	// 三笔付款与三笔退款按各自成功顺序排列，余额回到初始值。
	snap, err := l.Query()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(snap.Settlements) != 3 || len(snap.Refunds) != 3 {
		t.Fatalf("history counts=%d/%d want 3/3", len(snap.Settlements), len(snap.Refunds))
	}
	if got := snap.Balances[0].Balance; got != regLLInitialBalance {
		t.Fatalf("balance after 3 rounds=%d want MaxInt64 %d", got, regLLInitialBalance)
	}
	return l, path
}

// regLLAssertRefund 执行一笔全额退款并断言成功：金额为原扣款（含手续费），
// after_seq 与退款序号都等于当时已有的付款笔数。
func regLLAssertRefund(t *testing.T, l *Ledger, refundID, payID string, seq int64) {
	t.Helper()
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{{
		ID: refundID, SettlementID: payID, Reason: "cancel",
	}}})
	if err != nil {
		t.Fatalf("refund %s: %v", refundID, err)
	}
	if len(rr.Results) != 1 || rr.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("refund %s results=%+v", refundID, rr.Results)
	}
	rec := rr.Results[0].Record
	if rec == nil || rec.ID != refundID || rec.SettlementID != payID ||
		rec.Amount != regLLPayAmount || rec.Fee != regLLFee || rec.Charged != regLLCharged ||
		rec.AfterSeq != seq || rec.Seq != seq {
		t.Fatalf("refund %s record=%+v want amount=%d fee=%d charged=%d after_seq=%d seq=%d",
			refundID, rec, regLLPayAmount, regLLFee, regLLCharged, seq, seq)
	}
}

// regLLPaymentEntry 构造第 i 轮（从 0 起）付款记录逐字一致的外部流水。
func regLLPaymentEntry(idx, i int) FlowEntry {
	return FlowEntry{
		Index: idx, Kind: "payment", ID: "p" + string(rune('1'+i)),
		Account: regLLAccount, Asset: regLLAsset,
		Amount: regLLPayAmount, Fee: regLLFee, Charged: regLLCharged,
	}
}

// regLLRefundEntry 构造第 i 轮退款记录逐字一致（含原付款编号）的外部流水。
func regLLRefundEntry(idx, i int) FlowEntry {
	return FlowEntry{
		Index: idx, Kind: "refund", ID: "r" + string(rune('1'+i)),
		Account: regLLAccount, Asset: regLLAsset,
		Amount: regLLPayAmount, Fee: regLLFee, Charged: regLLCharged,
		SettlementID: "p" + string(rune('1'+i)),
	}
}

// regLLAssertMatchedIDs 断言逐条结果（按输入顺序）均为 matched 且编号/类别
// 与给定序列一致。
func regLLAssertMatchedIDs(t *testing.T, results []ReconResult, want []FlowEntry) {
	t.Helper()
	if len(results) != len(want) {
		t.Fatalf("results=%d want %d", len(results), len(want))
	}
	for i, w := range want {
		r := results[i]
		if r.Index != i || r.Kind != w.Kind || r.ID != w.ID {
			t.Fatalf("result %d=%+v want %s/%s", i, r, w.Kind, w.ID)
		}
		if r.Status != ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("result %d (%s/%s)=%s diffs=%+v want matched",
				i, r.Kind, r.ID, r.Status, r.Diffs)
		}
		if r.Record == nil {
			t.Fatalf("result %d (%s) missing record ref", i, r.ID)
		}
	}
}

// regLLAssertRow 断言某组合单侧汇总为给定的扣款/退款/净扣款十进制整数字符串。
func regLLAssertRow(t *testing.T, row ReconTotalRow, charged, refunded, net string) {
	t.Helper()
	if row.Account != regLLAccount || row.Asset != regLLAsset {
		t.Fatalf("row key=%s/%s", row.Account, row.Asset)
	}
	if row.ChargedTotal != charged || row.RefundedTotal != refunded || row.NetCharged != net {
		t.Fatalf("row=%+v want charged=%s refunded=%s net=%s", row, charged, refunded, net)
	}
}

// regLLAssertNetDiff 断言净扣款差额（方向：流水减账本）与两侧净扣款。
func regLLAssertNetDiff(t *testing.T, rep *ReconReport, ledgerNet, flowNet, diff string) {
	t.Helper()
	if len(rep.NetDiff) != 1 {
		t.Fatalf("net_diff=%+v want exactly 1 row", rep.NetDiff)
	}
	d := rep.NetDiff[0]
	if d.Account != regLLAccount || d.Asset != regLLAsset {
		t.Fatalf("net_diff key=%s/%s", d.Account, d.Asset)
	}
	if d.LedgerNetCharged != ledgerNet || d.FlowNetCharged != flowNet || d.NetChargedDiff != diff {
		t.Fatalf("net_diff=%+v want ledger=%s flow=%s diff=%s", d, ledgerNet, flowNet, diff)
	}
}

// TestReconcileLargeLedgerHistoryFullFlow 全量对账：外部流水按“付款、退款、
// 付款、退款、付款、退款”的真实业务顺序完整给出且与记录逐字一致时，逐笔
// matched；两侧扣款合计与退款合计都保留超过 int64 的精确值
// 12036000000000000000，净扣款与净差额都是字符串 "0"，付款与退款缺失列表
// 为空。汇总金额是十进制整数字符串，不截断、不取近似值、不是科学计数法。
func TestReconcileLargeLedgerHistoryFullFlow(t *testing.T) {
	l, _ := regLargeLedger(t)

	// 交错顺序：p1,r1,p2,r2,p3,r3，与“付款→退款→再付款”的成功时间顺序一致。
	entries := []FlowEntry{
		regLLPaymentEntry(0, 0),
		regLLRefundEntry(1, 0),
		regLLPaymentEntry(2, 1),
		regLLRefundEntry(3, 1),
		regLLPaymentEntry(4, 2),
		regLLRefundEntry(5, 2),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 逐笔 matched，严格保持输入顺序。
	regLLAssertMatchedIDs(t, rep.Results, entries)

	// 命中记录携带真实关联：付款 p1/p2/p3 带各自退款编号与序号；退款 r1/r2/r3
	// 带各自原付款编号与金额（含手续费）。关联不随净额为零而丢失。
	p1 := rep.Results[0].Record
	if p1.ID != "p1" || p1.Seq != 1 || p1.RefundID != "r1" || p1.RefundSeq != 1 ||
		p1.Amount != regLLPayAmount || p1.Fee != regLLFee || p1.Charged != regLLCharged {
		t.Fatalf("p1 record=%+v", p1)
	}
	r1 := rep.Results[1].Record
	if r1.ID != "r1" || r1.Seq != 1 || r1.SettlementID != "p1" ||
		r1.Amount != regLLPayAmount || r1.Fee != regLLFee || r1.Charged != regLLCharged {
		t.Fatalf("r1 record=%+v", r1)
	}
	if rep.Results[2].Record.ID != "p2" || rep.Results[2].Record.Seq != 2 ||
		rep.Results[2].Record.RefundID != "r2" || rep.Results[2].Record.RefundSeq != 2 {
		t.Fatalf("p2 record=%+v", rep.Results[2].Record)
	}
	if rep.Results[3].Record.ID != "r2" || rep.Results[3].Record.Seq != 2 ||
		rep.Results[3].Record.SettlementID != "p2" {
		t.Fatalf("r2 record=%+v", rep.Results[3].Record)
	}
	p3 := rep.Results[4].Record
	if p3.ID != "p3" || p3.Seq != 3 || p3.RefundID != "r3" || p3.RefundSeq != 3 ||
		p3.Charged != regLLCharged {
		t.Fatalf("p3 record=%+v", p3)
	}
	r3 := rep.Results[5].Record
	if r3.ID != "r3" || r3.Seq != 3 || r3.SettlementID != "p3" ||
		r3.Amount != regLLPayAmount || r3.Fee != regLLFee || r3.Charged != regLLCharged {
		t.Fatalf("r3 record=%+v", r3)
	}

	// 全部记录被覆盖：付款与退款缺失列表为空。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v want empty", rep.MissingRefunds)
	}

	// 最大成功序号与采用边界（全量）。
	if rep.MaxPaymentSeq != 3 || rep.MaxRefundSeq != 3 {
		t.Fatalf("max seqs=%d/%d want 3/3", rep.MaxPaymentSeq, rep.MaxRefundSeq)
	}
	if rep.PaymentAfter != 0 || rep.PaymentThrough != 3 ||
		rep.RefundAfter != 0 || rep.RefundThrough != 3 {
		t.Fatalf("bounds payment=%d,%d refund=%d,%d want 0,3/0,3",
			rep.PaymentAfter, rep.PaymentThrough, rep.RefundAfter, rep.RefundThrough)
	}

	// 关键回归点：累计扣款与累计退款分别保留精确值（均 > int64 上界），
	// 不因最终余额正常或净额为零而丢失两项合计。
	regLLAssertRow(t, findTotalRow(t, rep.Totals.Ledger, regLLAccount, regLLAsset),
		regLLTotal, regLLTotal, "0")
	regLLAssertRow(t, findTotalRow(t, rep.Totals.Flow, regLLAccount, regLLAsset),
		regLLTotal, regLLTotal, "0")
	if len(rep.Totals.Ledger) != 1 || len(rep.Totals.Flow) != 1 {
		t.Fatalf("totals rows ledger=%+v flow=%+v", rep.Totals.Ledger, rep.Totals.Flow)
	}
	regLLAssertNetDiff(t, rep, "0", "0", "0")
}

// TestReconcileLargeLedgerHistoryTotalsMarshalExact 报告整体 JSON 序列化后，
// 超过 int64 的两项合计与净扣款仍以完整十进制整数字符串出现，不截断、
// 不取近似值、不变成科学计数法。
func TestReconcileLargeLedgerHistoryTotalsMarshalExact(t *testing.T) {
	l, _ := regLargeLedger(t)
	entries := []FlowEntry{
		regLLPaymentEntry(0, 0),
		regLLRefundEntry(1, 0),
		regLLPaymentEntry(2, 1),
		regLLRefundEntry(3, 1),
		regLLPaymentEntry(4, 2),
		regLLRefundEntry(5, 2),
	}
	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		`"charged_total":"12036000000000000000"`,
		`"refunded_total":"12036000000000000000"`,
		`"net_charged":"0"`,
		`"net_charged_diff":"0"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("report JSON missing exact %s:\n%s", want, s)
		}
	}
	// 科学计数法形态（如 1.2036e19）绝不允许出现。
	if strings.Contains(s, "e19") || strings.Contains(s, "E19") {
		t.Fatalf("report JSON must not use scientific notation:\n%s", s)
	}
}

// TestReconcileLargeLedgerHistoryRefundOnlyCompleteFlow 对同一历史只选取全部
// 退款（付款范围为空），且外部退款流水完整：账本侧扣款合计为 "0"、退款
// 合计仍为精确值 12036000000000000000、净扣款为 "-12036000000000000000"；
// 不能把退款引用的原付款顺带计入扣款。流水侧与账本侧净扣款一致，两侧
// 净差额（流水减账本）仍为 "0"。
func TestReconcileLargeLedgerHistoryRefundOnlyCompleteFlow(t *testing.T) {
	l, _ := regLargeLedger(t)

	// 付款范围 (0,0] 为空：付款 p1/p2/p3 全部不入选；退款范围缺省=全量。
	z := int64(0)
	bounds := ReconcileBounds{PaymentThrough: &z}

	// 退款流水按退款成功顺序 r1,r2,r3 完整给出（即使含原付款 p1/p2/p3 也
	// 不能把原付款计入扣款侧）。
	entries := []FlowEntry{
		regLLRefundEntry(0, 0),
		regLLRefundEntry(1, 1),
		regLLRefundEntry(2, 2),
	}
	rep, err := l.ReconcileFlowRanged(entries, bounds)
	if err != nil {
		t.Fatalf("reconcile ranged: %v", err)
	}

	// 三笔退款逐笔 matched；即使只选退款，命中记录仍携带范围外原付款关联，
	// 但这绝不意味着原付款进入扣款合计。
	regLLAssertMatchedIDs(t, rep.Results, entries)
	if rep.Results[0].Record.SettlementID != "p1" ||
		rep.Results[1].Record.SettlementID != "p2" ||
		rep.Results[2].Record.SettlementID != "p3" {
		t.Fatalf("refund records must carry true origin payments: %+v / %+v / %+v",
			rep.Results[0].Record, rep.Results[1].Record, rep.Results[2].Record)
	}

	// 付款范围为空，缺失付款自然为空；三笔退款均已覆盖，缺失退款为空。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty (payment range empty)", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v want empty", rep.MissingRefunds)
	}

	// 采用边界：付款 (0,0] 空；退款 (0,3] 全量。
	if rep.PaymentAfter != 0 || rep.PaymentThrough != 0 ||
		rep.RefundAfter != 0 || rep.RefundThrough != 3 {
		t.Fatalf("bounds payment=%d,%d refund=%d,%d want payment=0,0 refund=0,3",
			rep.PaymentAfter, rep.PaymentThrough, rep.RefundAfter, rep.RefundThrough)
	}

	// 关键回归点：只选退款时账本侧扣款合计必须是 "0"，不能顺带计入退款
	// 引用的原付款；退款合计仍为 >int64 的精确值；净扣款为负的精确十进制。
	regLLAssertRow(t, findTotalRow(t, rep.Totals.Ledger, regLLAccount, regLLAsset),
		"0", regLLTotal, "-"+regLLTotal)
	// 流水侧也只有三笔退款（没有任何付款流水）：与账本侧一致。
	regLLAssertRow(t, findTotalRow(t, rep.Totals.Flow, regLLAccount, regLLAsset),
		"0", regLLTotal, "-"+regLLTotal)
	if len(rep.Totals.Ledger) != 1 || len(rep.Totals.Flow) != 1 {
		t.Fatalf("totals rows ledger=%+v flow=%+v", rep.Totals.Ledger, rep.Totals.Flow)
	}
	// 两侧净扣款都是 -12036000000000000000，差额（流水减账本）为 "0"。
	regLLAssertNetDiff(t, rep, "-"+regLLTotal, "-"+regLLTotal, "0")
}

// TestReconcileLargeLedgerHistoryRefundOnlyEmptyFlow 同一“只选全部退款、
// 付款范围为空”的范围，但退款侧收到空流水：应按退款成功顺序列出三笔
// 缺失退款 r1,r2,r3；流水侧扣款与退款都按零参与比较，净扣款为 "0"，
// 净差额方向保持流水减账本，为 "12036000000000000000"。账本侧扣款合计
// 仍为 "0"、退款合计仍为精确值，不把退款引用的原付款计入扣款。
func TestReconcileLargeLedgerHistoryRefundOnlyEmptyFlow(t *testing.T) {
	l, _ := regLargeLedger(t)
	z := int64(0)
	bounds := ReconcileBounds{PaymentThrough: &z}

	// nil 与显式空列表语义一致：没有任何外部流水。两种入参都验一遍。
	for _, entries := range [][]FlowEntry{nil, {}} {
		rep, err := l.ReconcileFlowRanged(entries, bounds)
		if err != nil {
			t.Fatalf("reconcile ranged %#v: %v", entries, err)
		}

		// 没有逐条结果（空输入），但入选退款 r1,r2,r3 按退款成功顺序列入缺失。
		if len(rep.Results) != 0 {
			t.Fatalf("results=%+v want empty", rep.Results)
		}
		if len(rep.MissingPayments) != 0 {
			t.Fatalf("missing payments=%+v want empty", rep.MissingPayments)
		}
		if got := refs(rep.MissingRefunds); got != "r1,r2,r3" {
			t.Fatalf("missing refunds=%s want r1,r2,r3 in refund success order", got)
		}
		// 缺失退款也必须带各自原付款编号与精确金额（含手续费），可追溯。
		for i, want := range []struct {
			id, settle string
			seq        int64
		}{
			{"r1", "p1", 1},
			{"r2", "p2", 2},
			{"r3", "p3", 3},
		} {
			mr := rep.MissingRefunds[i]
			if mr.ID != want.id || mr.SettlementID != want.settle || mr.Seq != want.seq ||
				mr.Amount != regLLPayAmount || mr.Fee != regLLFee || mr.Charged != regLLCharged {
				t.Fatalf("missing refund %d=%+v want id=%s settle=%s seq=%d amount=%d fee=%d charged=%d",
					i, mr, want.id, want.settle, want.seq, regLLPayAmount, regLLFee, regLLCharged)
			}
		}

		// 账本侧：扣款 0、退款精确值 T、净扣款 -T；流水侧没有任何条目，
		// 不产生汇总行（其净扣款在 net_diff 中按 "0" 参与比较）。
		regLLAssertRow(t, findTotalRow(t, rep.Totals.Ledger, regLLAccount, regLLAsset),
			"0", regLLTotal, "-"+regLLTotal)
		if len(rep.Totals.Flow) != 0 {
			t.Fatalf("flow totals=%+v want no rows for empty flow", rep.Totals.Flow)
		}
		// 净差额方向保持“流水减账本”：0 - (-T) = +T；流水侧净扣款按 "0" 给出。
		regLLAssertNetDiff(t, rep, "-"+regLLTotal, "0", regLLTotal)

		// 边界仍是付款空、退款全量，最大序号为完整历史。
		if rep.MaxPaymentSeq != 3 || rep.MaxRefundSeq != 3 ||
			rep.PaymentThrough != 0 || rep.RefundThrough != 3 {
			t.Fatalf("seqs/bounds max=%d/%d through=%d/%d",
				rep.MaxPaymentSeq, rep.MaxRefundSeq, rep.PaymentThrough, rep.RefundThrough)
		}
	}
}

// TestReconcileLargeLedgerHistoryIsReadOnly 大额历史对账（全量与只选退款两种
// 范围、含完整与空流水）前后，余额、付款与退款历史以及账本文件内容逐字节
// 保持一致。
func TestReconcileLargeLedgerHistoryIsReadOnly(t *testing.T) {
	l, path := regLargeLedger(t)

	before, err := l.Query()
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	bj, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("marshal before: %v", err)
	}
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger before: %v", err)
	}

	fullEntries := []FlowEntry{
		regLLPaymentEntry(0, 0),
		regLLRefundEntry(1, 0),
		regLLPaymentEntry(2, 1),
		regLLRefundEntry(3, 1),
		regLLPaymentEntry(4, 2),
		regLLRefundEntry(5, 2),
	}
	if _, err := l.ReconcileFlow(fullEntries); err != nil {
		t.Fatalf("full reconcile: %v", err)
	}
	z := int64(0)
	refundOnly := ReconcileBounds{PaymentThrough: &z}
	completeRefunds := []FlowEntry{
		regLLRefundEntry(0, 0),
		regLLRefundEntry(1, 1),
		regLLRefundEntry(2, 2),
	}
	if _, err := l.ReconcileFlowRanged(completeRefunds, refundOnly); err != nil {
		t.Fatalf("refund-only complete reconcile: %v", err)
	}
	if _, err := l.ReconcileFlowRanged(nil, refundOnly); err != nil {
		t.Fatalf("refund-only empty reconcile: %v", err)
	}

	after, err := l.Query()
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	aj, err := json.Marshal(after)
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("ledger state changed by reconcile:\nbefore=%s\nafter =%s", bj, aj)
	}
	diskAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger after: %v", err)
	}
	if string(diskBefore) != string(diskAfter) {
		t.Fatalf("ledger file changed by reconcile:\nbefore=%s\nafter =%s", diskBefore, diskAfter)
	}

	// 重新打开同一文件，余额与成功历史仍与对账前完全一致。
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	reopened, err := l2.Query()
	if err != nil {
		t.Fatalf("query reopened: %v", err)
	}
	if !reflect.DeepEqual(before, reopened) {
		rj, _ := json.Marshal(reopened)
		t.Fatalf("reopened ledger differs:\ndisk=%s\nmem =%s", rj, bj)
	}
}
