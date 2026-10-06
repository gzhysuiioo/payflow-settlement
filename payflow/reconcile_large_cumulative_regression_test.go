package payflow

import (
	"encoding/json"
	"math"
	"math/big"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// 本文件为“合法历史的累计扣款/累计退款可以超过 int64 上界”补充自动化回归
// 保障。单笔付款与退款都在 int64 范围内，但全额退款释放余额后同一笔钱可以
// 再次付款：最终余额正常、净额为零都不能推出金额正确，报告必须分别精确保留
// 扣款合计与退款合计，且汇总金额始终是十进制整数字符串（不截断、不近似、
// 不出现科学计数法）。
//
// 场景（同一账户 aa-1、同一资产 usdc，账户内所有数值均为 int64 内的合法值）：
//   - 初始余额 math.MaxInt64 = 9223372036854775807；
//   - 每轮：付款 4000000000000000000（30 基点，手续费 12000000000000000，
//     扣款总额 4012000000000000000），随后 r<n> 对 p<n> 全额退款（含手续费）；
//   - 第一轮扣款后余额 5211372036854775807（仍为正），退款后回到初始值；
//     因此第二、第三轮付款同样合法，三轮结束后余额回到初始值；
//   - 3 笔付款 seqs=1..3、3 笔退款 seqs=1..3，后两轮退款落账时结算历史
//     长度分别为 2、3（after_seq 如实记录）；
//   - 累计扣款 = 累计退款 = 3 * 4012000000000000000 = 12036000000000000000，
//     该值已超过 int64 上界（9223372036854775807），只能以大整数/十进制字符串
//     精确表达。
//
// 覆盖三种对账方式：
//  1. 外部流水完整且与记录一致的全量对账：6 条逐笔 matched，两侧合计精确
//     相等，净扣款/净差额为字符串 "0"，缺失列表为空；
//  2. 同一历史只选全部退款（付款范围为空）：账本侧扣款合计 "0"、退款合计
//     仍为完整历史的精确值，净扣款 "-12036000000000000000"；补齐对应退款
//     流水后两侧净差额仍为 "0"，退款引用的原付款不顺带计入扣款；
//  3. 该退款范围收到空流水：按退款成功顺序列出 r1,r2,r3 三笔缺失退款，
//     流水侧净扣款按零参与比较，净差额为 "12036000000000000000"，方向保持
//     流水减账本。
//
// 整个过程沿用公开 Go 接口（ReconcileFlow / ReconcileFlowRanged）与既有
// 状态名、报告结构，对余额与付款/退款历史、账本文件保持只读。

const (
	// 大额历史回归用常量：30 基点下 amount=4e18 的逐项精确值与三轮累计值。
	regBigInit     = math.MaxInt64 // 9223372036854775807
	regBigAmount   = int64(4000000000000000000)
	regBigFee      = int64(12000000000000000)
	regBigCharged  = int64(4012000000000000000)
	regBigTotalS   = "12036000000000000000"
	regBigZeroS    = "0"
	regBigNegTotal = "-12036000000000000000"
)

// regBigRefundBounds 返回“只选全部退款、付款范围为空”的边界：
// 付款 (0,0] 为空；退款缺省（(0, maxRefundSeq] 覆盖全部退款）。
func regBigRefundBounds() ReconcileBounds {
	z := int64(0)
	return ReconcileBounds{PaymentAfter: &z, PaymentThrough: &z}
}

// regBigSetup 构造三轮“付款 4e18 + 全额退款”的完整历史并做基本健全性检查，
// 返回打开的账本句柄与路径。整个构造过程中每笔金额都在 int64 内。
func regBigSetup(t *testing.T) (*Ledger, string) {
	t.Helper()

	// 常量自证：feeFor 与 chargeFor 在 int64 内精确算出手续费与扣款总额；
	// 三轮累计确实越过 int64 上界（这正是汇总必须用大整数的原因）。
	if fee := feeFor(regBigAmount, 30); fee != regBigFee {
		t.Fatalf("feeFor(%d,30)=%d want %d", regBigAmount, fee, regBigFee)
	}
	fee, charged, ok := chargeFor(regBigAmount, 30)
	if !ok || fee != regBigFee || charged != regBigCharged {
		t.Fatalf("chargeFor(%d,30)=%d,%d,%v want %d,%d,true",
			regBigAmount, fee, charged, ok, regBigFee, regBigCharged)
	}
	if regBigCharged < 0 || regBigAmount < 0 || regBigFee < 0 {
		t.Fatalf("per-item values must be valid int64: amount=%d fee=%d charged=%d",
			regBigAmount, regBigFee, regBigCharged)
	}
	// 三笔累计必须确实越过 int64 上界：这正是汇总不能用 int64 累加的原因。
	triple := new(big.Int).Mul(big.NewInt(regBigCharged), big.NewInt(3))
	if triple.IsInt64() {
		t.Fatalf("3*charged=%s unexpectedly fits int64", triple.String())
	}
	if triple.String() != regBigTotalS {
		t.Fatalf("3*charged=%s want %s", triple.String(), regBigTotalS)
	}

	path := newTestLedger(t, []BalanceInit{
		{Account: "aa-1", Asset: "usdc", Balance: regBigInit},
	})
	l := openOrFail(t, path)

	// 逐轮提交，确保每轮付款成功、全额退款成功，且退款后余额完全复原——
	// 复原后的同一批 int64 余额才允许下一轮同等规模的付款。
	for round := 1; round <= 3; round++ {
		pid := regBigPaymentID(round)
		rid := regBigRefundID(round)
		res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
			intent(pid, "aa-1", "usdc", regBigAmount),
		}})
		if err != nil {
			t.Fatalf("round %d submit: %v", round, err)
		}
		if got := res.Results[0]; got.Status != StatusSettled || got.Record == nil ||
			got.Record.Seq != int64(round) || got.Record.Amount != regBigAmount ||
			got.Record.Fee != regBigFee || got.Record.Charged != regBigCharged {
			t.Fatalf("round %d payment result=%+v", round, got)
		}
		if bal, _ := l.Balance("aa-1", "usdc"); bal != regBigInit-regBigCharged {
			t.Fatalf("round %d balance after charge=%d want %d", round, bal, regBigInit-regBigCharged)
		}
		rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
			{ID: rid, SettlementID: pid, Reason: "cancel"},
		}})
		if err != nil {
			t.Fatalf("round %d refund: %v", round, err)
		}
		r0 := rr.Results[0]
		if r0.Status != StatusRefundSuccess || r0.Record == nil {
			t.Fatalf("round %d refund result=%+v", round, r0)
		}
		if r0.Record.Seq != int64(round) || r0.Record.SettlementID != pid ||
			r0.Record.Amount != regBigAmount || r0.Record.Fee != regBigFee ||
			r0.Record.Charged != regBigCharged || r0.Record.AfterSeq != int64(round) {
			t.Fatalf("round %d refund record=%+v", round, r0.Record)
		}
		if bal, _ := l.Balance("aa-1", "usdc"); bal != regBigInit {
			t.Fatalf("round %d balance after refund=%d want initial %d", round, bal, regBigInit)
		}
	}

	// 三轮结束：余额回到初始值；3 笔付款、3 笔退款按成功顺序排列，关联一一对应。
	snap := mustQuery(t, l)
	if got := snap.Balances[0]; got.Account != "aa-1" || got.Asset != "usdc" || got.Balance != regBigInit {
		t.Fatalf("final balance view=%+v want MaxInt64", got)
	}
	if len(snap.Settlements) != 3 || len(snap.Refunds) != 3 {
		t.Fatalf("history counts=%d/%d want 3/3", len(snap.Settlements), len(snap.Refunds))
	}
	for i := 0; i < 3; i++ {
		p := snap.Settlements[i]
		if p.ID != regBigPaymentID(i+1) || p.Seq != int64(i+1) || p.Charged != regBigCharged {
			t.Fatalf("payment[%d]=%+v", i, p)
		}
		r := snap.Refunds[i]
		if r.ID != regBigRefundID(i+1) || r.Seq != int64(i+1) ||
			r.SettlementID != regBigPaymentID(i+1) || r.Charged != regBigCharged {
			t.Fatalf("refund[%d]=%+v", i, r)
		}
	}
	return l, path
}

func regBigPaymentID(round int) string {
	return "p" + strconv.Itoa(round)
}

func regBigRefundID(round int) string {
	return "r" + strconv.Itoa(round)
}

// regBigFullEntries 构造与三轮历史逐字段一致的完整外部流水：先三笔付款再
// 三笔退款（输入顺序刻意与落账的交错顺序不同，逐条结果必须严格保持此顺序）。
func regBigFullEntries() []FlowEntry {
	es := make([]FlowEntry, 0, 6)
	for round := 1; round <= 3; round++ {
		es = append(es, flowEntry(len(es), "payment", regBigPaymentID(round),
			"aa-1", "usdc", regBigAmount, regBigFee, regBigCharged, ""))
	}
	for round := 1; round <= 3; round++ {
		es = append(es, flowEntry(len(es), "refund", regBigRefundID(round),
			"aa-1", "usdc", regBigAmount, regBigFee, regBigCharged, regBigPaymentID(round)))
	}
	return es
}

// regBigRefundEntries 构造只含三笔退款的外部流水（退款引用各自原付款）。
func regBigRefundEntries() []FlowEntry {
	es := make([]FlowEntry, 0, 3)
	for round := 1; round <= 3; round++ {
		es = append(es, flowEntry(len(es), "refund", regBigRefundID(round),
			"aa-1", "usdc", regBigAmount, regBigFee, regBigCharged, regBigPaymentID(round)))
	}
	return es
}

// regBigAssertExactTotals 校验某单侧汇总行的三个十进制字符串精确值，并确保
// 它们是普通十进制整数：不含科学计数法、没有前导零（"0" 除外）、没有
// 小数点。这锁定“累计越过 int64 仍精确输出”这一报告契约。
func regBigAssertExactTotals(t *testing.T, row ReconTotalRow, charged, refunded, net string) {
	t.Helper()
	if row.Account != "aa-1" || row.Asset != "usdc" {
		t.Fatalf("total row identity=%+v", row)
	}
	if row.ChargedTotal != charged || row.RefundedTotal != refunded || row.NetCharged != net {
		t.Fatalf("total row=%+v want charged=%s refunded=%s net=%s", row, charged, refunded, net)
	}
	for _, v := range []string{row.ChargedTotal, row.RefundedTotal, row.NetCharged} {
		regBigAssertDecimal(t, v)
	}
}

// regBigAssertDecimal 确保汇总字符串是普通十进制整数字符串。
func regBigAssertDecimal(t *testing.T, v string) {
	t.Helper()
	if v == "" {
		t.Fatalf("decimal string must not be empty")
	}
	s := v
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" || strings.ContainsAny(v, ".eE") {
		t.Fatalf("total %q must be a plain decimal integer string (no scientific notation, no fraction)", v)
	}
	if len(s) > 1 && s[0] == '0' {
		t.Fatalf("total %q must not have leading zeros", v)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("total %q must be decimal digits only", v)
		}
	}
}

// TestReconcileLargeCumulativeTotalsFullFlow 全量对账：外部流水完整且与记录
// 一致时逐笔 matched，两侧扣款/退款合计都保留超过 int64 的精确值
// 12036000000000000000，净扣款与净差额为十进制 "0"，缺失列表为空。
func TestReconcileLargeCumulativeTotalsFullFlow(t *testing.T) {
	l, _ := regBigSetup(t)
	entries := regBigFullEntries()

	rep, err := l.ReconcileFlow(entries)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 六条逐笔 matched，严格保持外部流水输入顺序（先付款后退款），无字段差异。
	if len(rep.Results) != 6 {
		t.Fatalf("results=%d want 6", len(rep.Results))
	}
	wantIDs := []string{"p1", "p2", "p3", "r1", "r2", "r3"}
	wantKinds := []string{"payment", "payment", "payment", "refund", "refund", "refund"}
	for i := 0; i < 6; i++ {
		r := rep.Results[i]
		if r.Index != i || r.ID != wantIDs[i] || r.Kind != wantKinds[i] {
			t.Fatalf("result %d=%+v want %s/%s", i, r, wantKinds[i], wantIDs[i])
		}
		if r.Status != ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("result %d (%s) status=%s diffs=%+v want matched with no diffs",
				i, r.ID, r.Status, r.Diffs)
		}
		if r.Record == nil || r.Record.Charged != regBigCharged {
			t.Fatalf("result %d (%s) missing/incorrect record: %+v", i, r.ID, r.Record)
		}
	}
	// 退款结果携带真正的原付款编号与自身序号；付款结果携带对应退款关联。
	for round := 1; round <= 3; round++ {
		rr := rep.Results[3+round-1]
		if rr.Record.SettlementID != regBigPaymentID(round) || rr.Record.Seq != int64(round) {
			t.Fatalf("r%d record=%+v must carry true origin %s", round, rr.Record, regBigPaymentID(round))
		}
		pr := rep.Results[round-1]
		if pr.Record.RefundID != regBigRefundID(round) || pr.Record.RefundSeq != int64(round) {
			t.Fatalf("p%d record=%+v must link refund %s", round, pr.Record, regBigRefundID(round))
		}
	}

	// 缺失列表（付款与退款）均为空：六条记录全部通过各自编号命中。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v want empty", rep.MissingRefunds)
	}

	// 两侧合计：扣款与退款分别保留精确累计值（均超过 int64），净扣款为 "0"。
	if len(rep.Totals.Ledger) != 1 || len(rep.Totals.Flow) != 1 {
		t.Fatalf("totals rows ledger=%+v flow=%+v want one row each", rep.Totals.Ledger, rep.Totals.Flow)
	}
	regBigAssertExactTotals(t, rep.Totals.Ledger[0], regBigTotalS, regBigTotalS, regBigZeroS)
	regBigAssertExactTotals(t, rep.Totals.Flow[0], regBigTotalS, regBigTotalS, regBigZeroS)

	// 净扣款差额：方向流水减账本；两侧净扣款都是 0，差额十进制 "0"。
	if len(rep.NetDiff) != 1 {
		t.Fatalf("net_diff=%+v want one row", rep.NetDiff)
	}
	d := rep.NetDiff[0]
	if d.Account != "aa-1" || d.Asset != "usdc" ||
		d.LedgerNetCharged != regBigZeroS || d.FlowNetCharged != regBigZeroS ||
		d.NetChargedDiff != regBigZeroS {
		t.Fatalf("net_diff=%+v want all decimal zero", d)
	}
	regBigAssertDecimal(t, d.NetChargedDiff)

	// 报告序列化后汇总字段逐字为完整十进制整数：不截断、不科学计数法。
	raw, merr := json.Marshal(rep)
	if merr != nil {
		t.Fatalf("marshal report: %v", merr)
	}
	rs := string(raw)
	if !strings.Contains(rs, `"charged_total":"12036000000000000000"`) {
		t.Fatalf("marshalled report missing exact charged_total: %s", rs)
	}
	if !strings.Contains(rs, `"refunded_total":"12036000000000000000"`) {
		t.Fatalf("marshalled report missing exact refunded_total: %s", rs)
	}
	if strings.Contains(rs, "e+") || strings.Contains(rs, "E+") {
		t.Fatalf("marshalled report must not use scientific notation: %s", rs)
	}
}

// TestReconcileLargeTotalsRefundOnlyRange 同一历史只选取全部退款、付款范围
// 为空：账本侧扣款合计为 "0"，退款合计仍为 12036000000000000000，净扣款
// "-12036000000000000000"；对应退款流水完整时两侧净差额仍为 "0"，不能把
// 退款引用的原付款顺带计入扣款。
func TestReconcileLargeTotalsRefundOnlyRange(t *testing.T) {
	l, _ := regBigSetup(t)
	bounds := regBigRefundBounds()

	// 先以空流水请求同一范围：账本侧只包含入选退款，扣款为零、退款完整。
	missing, err := l.ReconcileFlowRanged(nil, bounds)
	if err != nil {
		t.Fatalf("reconcile refund-only (no flow): %v", err)
	}
	if len(missing.Totals.Ledger) != 1 {
		t.Fatalf("ledger rows=%+v want one", missing.Totals.Ledger)
	}
	regBigAssertExactTotals(t, missing.Totals.Ledger[0], regBigZeroS, regBigTotalS, regBigNegTotal)
	if missing.PaymentAfter != 0 || missing.PaymentThrough != 0 ||
		missing.RefundAfter != 0 || missing.RefundThrough != 3 ||
		missing.MaxPaymentSeq != 3 || missing.MaxRefundSeq != 3 {
		t.Fatalf("bounds/max seqs payment=%d,%d refund=%d,%d max=%d/%d",
			missing.PaymentAfter, missing.PaymentThrough, missing.RefundAfter, missing.RefundThrough,
			missing.MaxPaymentSeq, missing.MaxRefundSeq)
	}

	// 三笔退款流水完整且与记录一致：逐笔 matched（严格按 r1,r2,r3 输入顺序），
	// 每个 settlement_id 核对通过，记录详情仍指向各自原付款。
	rep, err := l.ReconcileFlowRanged(regBigRefundEntries(), bounds)
	if err != nil {
		t.Fatalf("reconcile refund-only: %v", err)
	}
	if len(rep.Results) != 3 {
		t.Fatalf("results=%d want 3", len(rep.Results))
	}
	for round := 1; round <= 3; round++ {
		r := rep.Results[round-1]
		if r.Index != round-1 || r.ID != regBigRefundID(round) || r.Kind != "refund" {
			t.Fatalf("result %d=%+v want refund %s", round-1, r, regBigRefundID(round))
		}
		if r.Status != ReconMatched || len(r.Diffs) != 0 {
			t.Fatalf("%s status=%s diffs=%+v want matched", r.ID, r.Status, r.Diffs)
		}
		if r.Record == nil || r.Record.SettlementID != regBigPaymentID(round) ||
			r.Record.Seq != int64(round) || r.Record.Charged != regBigCharged {
			t.Fatalf("%s record=%+v", r.ID, r.Record)
		}
	}

	// 缺失列表：付款范围为空所以没有入选付款；三笔退款都已覆盖。
	if len(rep.MissingPayments) != 0 {
		t.Fatalf("missing payments=%+v want empty (payment range empty)", rep.MissingPayments)
	}
	if len(rep.MissingRefunds) != 0 {
		t.Fatalf("missing refunds=%+v want empty (all covered)", rep.MissingRefunds)
	}

	// 账本侧：扣款 0、退款精确累计、净扣款负的完整累计值。
	if len(rep.Totals.Ledger) != 1 {
		t.Fatalf("ledger rows=%+v", rep.Totals.Ledger)
	}
	regBigAssertExactTotals(t, rep.Totals.Ledger[0], regBigZeroS, regBigTotalS, regBigNegTotal)
	// 流水侧：只给了退款，同样扣款 0、退款完整、净扣款为负的完整累计值。
	if len(rep.Totals.Flow) != 1 {
		t.Fatalf("flow rows=%+v", rep.Totals.Flow)
	}
	regBigAssertExactTotals(t, rep.Totals.Flow[0], regBigZeroS, regBigTotalS, regBigNegTotal)

	// 两侧净扣款相等（都是 0-退款），净差额仍为十进制 "0"（流水减账本）。
	if len(rep.NetDiff) != 1 {
		t.Fatalf("net_diff=%+v want one row", rep.NetDiff)
	}
	d := rep.NetDiff[0]
	if d.LedgerNetCharged != regBigNegTotal || d.FlowNetCharged != regBigNegTotal ||
		d.NetChargedDiff != regBigZeroS {
		t.Fatalf("net_diff=%+v want ledger/flow net %s diff 0", d, regBigNegTotal)
	}

	// 即使流水中退款引用了 p1/p2/p3，报告里也不能出现任何扣款合计：
	// 序列化文档中该组合的 charged_total 必须逐字为 "0"。
	raw, _ := json.Marshal(rep)
	if strings.Count(string(raw), `"charged_total":"0"`) != 2 {
		t.Fatalf("refund-only report must show zero charged_total on both sides, got %s", raw)
	}
}

// TestReconcileLargeTotalsRefundRangeEmptyFlowMissing 退款范围收到空流水：
// 按退款成功顺序列出三笔缺失退款；流水侧没有任何条目，净扣款按零参与比较，
// 净差额为 12036000000000000000（方向保持流水减账本：0 - (-累计退款)）。
func TestReconcileLargeTotalsRefundRangeEmptyFlowMissing(t *testing.T) {
	l, _ := regBigSetup(t)
	bounds := regBigRefundBounds()

	// nil 与空切片语义一致，都表示没有任何外部流水。
	for _, empty := range [][]FlowEntry{nil, {}} {
		rep, err := l.ReconcileFlowRanged(empty, bounds)
		if err != nil {
			t.Fatalf("reconcile with empty flow: %v", err)
		}
		if len(rep.Results) != 0 {
			t.Fatalf("results=%+v want none", rep.Results)
		}
		// 付款范围为空：没有入选付款，缺失付款列表为空。
		if len(rep.MissingPayments) != 0 {
			t.Fatalf("missing payments=%+v want empty", rep.MissingPayments)
		}
		// 三笔入选退款全部缺失，严格按退款成功顺序 r1,r2,r3，且每笔都携带
		// 精确金额与真正的原付款编号（关联不因付款范围为空而切断）。
		if len(rep.MissingRefunds) != 3 {
			t.Fatalf("missing refunds=%+v want 3 in success order", rep.MissingRefunds)
		}
		for round := 1; round <= 3; round++ {
			mr := rep.MissingRefunds[round-1]
			if mr.Kind != "refund" || mr.ID != regBigRefundID(round) || mr.Seq != int64(round) ||
				mr.SettlementID != regBigPaymentID(round) || mr.Account != "aa-1" || mr.Asset != "usdc" ||
				mr.Amount != regBigAmount || mr.Fee != regBigFee || mr.Charged != regBigCharged {
				t.Fatalf("missing refund %d=%+v", round, mr)
			}
		}

		// 账本侧：扣款 0、退款完整累计、净扣款 -累计退款。
		if len(rep.Totals.Ledger) != 1 {
			t.Fatalf("ledger rows=%+v want one", rep.Totals.Ledger)
		}
		regBigAssertExactTotals(t, rep.Totals.Ledger[0], regBigZeroS, regBigTotalS, regBigNegTotal)
		// 流水侧没有任何条目：不出汇总行（没有出现过的组合）。
		if len(rep.Totals.Flow) != 0 {
			t.Fatalf("flow rows=%+v want none with empty flow", rep.Totals.Flow)
		}
		// 净差额行：流水侧按零净扣款参与比较，账本侧为负累计退款，
		// 差额 = 0 - (-12036...) = +12036...，方向流水减账本。
		if len(rep.NetDiff) != 1 {
			t.Fatalf("net_diff=%+v want one row", rep.NetDiff)
		}
		d := rep.NetDiff[0]
		if d.Account != "aa-1" || d.Asset != "usdc" ||
			d.FlowNetCharged != regBigZeroS || d.LedgerNetCharged != regBigNegTotal ||
			d.NetChargedDiff != regBigTotalS {
			t.Fatalf("net_diff=%+v want flow net 0 ledger net %s diff %s",
				d, regBigNegTotal, regBigTotalS)
		}
		regBigAssertDecimal(t, d.NetChargedDiff)
	}
}

// TestReconcileLargeTotalsDoesNotUseFinalBalanceOrNetZero 最终余额正常、净额
// 为零都不能替代两项合计：本用例明确区分“余额回到 MaxInt64”与“累计金额
// 超过 int64”，并确认报告在同一次对账中同时给出回初余额、精确两项合计与
// 零净额——三者必须同时成立，缺一不可。
func TestReconcileLargeTotalsDoesNotUseFinalBalanceOrNetZero(t *testing.T) {
	l, _ := regBigSetup(t)
	rep, err := l.ReconcileFlow(regBigFullEntries())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if bal, _ := l.Balance("aa-1", "usdc"); bal != regBigInit {
		t.Fatalf("balance=%d want initial %d", bal, regBigInit)
	}
	lg := findTotalRow(t, rep.Totals.Ledger, "aa-1", "usdc")
	// 即便余额恢复、净扣款为 "0"，两项合计仍是超过 int64 的精确值：
	// 不能只凭最终余额或净额判断金额正确。
	if lg.ChargedTotal != regBigTotalS || lg.RefundedTotal != regBigTotalS || lg.NetCharged != "0" {
		t.Fatalf("totals=%+v", lg)
	}
	// 每项合计都是单笔 int64 值的三倍，已不可能由 int64 表达：累计值 20 位、
	// int64 上界只有 19 位（量级而非字典序），字符串必须完整保留全部 20 位。
	if len(regBigTotalS) != 20 || len(strconv.FormatInt(math.MaxInt64, 10)) != 19 {
		t.Fatalf("cumulative %s must be a 20-digit value exceeding int64 max", regBigTotalS)
	}
	cum, parsed := new(big.Int).SetString(regBigTotalS, 10)
	if !parsed {
		t.Fatalf("cumulative %q must parse as an exact decimal integer", regBigTotalS)
	}
	if cum.IsInt64() {
		t.Fatalf("cumulative %s must exceed int64", regBigTotalS)
	}
}

// TestReconcileLargeTotalsIsReadOnly 全部对账方式（全量、只选退款、空流水
// 退款范围）前后，余额、付款/退款历史以及账本文件内容必须逐字节保持一致。
func TestReconcileLargeTotalsIsReadOnly(t *testing.T) {
	l, path := regBigSetup(t)

	before, err := l.Query()
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	bj, _ := json.Marshal(before)
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger before: %v", err)
	}

	runs := []struct {
		name    string
		entries []FlowEntry
		bounds  ReconcileBounds
	}{
		{"full", regBigFullEntries(), ReconcileBounds{}},
		{"refund only with flow", regBigRefundEntries(), regBigRefundBounds()},
		{"refund only empty flow", nil, regBigRefundBounds()},
	}
	for _, run := range runs {
		rep, err := l.ReconcileFlowRanged(run.entries, run.bounds)
		if err != nil {
			t.Fatalf("%s: reconcile: %v", run.name, err)
		}
		// 连续两次结果必须确定一致。
		rep2, err := l.ReconcileFlowRanged(run.entries, run.bounds)
		if err != nil {
			t.Fatalf("%s: re-reconcile: %v", run.name, err)
		}
		r1, _ := json.Marshal(rep)
		r2, _ := json.Marshal(rep2)
		if !reflect.DeepEqual(rep, rep2) {
			t.Fatalf("%s: reconcile not deterministic:\n%s\n%s", run.name, r1, r2)
		}

		after, err := l.Query()
		if err != nil {
			t.Fatalf("%s: query after: %v", run.name, err)
		}
		if !reflect.DeepEqual(before, after) {
			aj, _ := json.Marshal(after)
			t.Fatalf("%s: ledger state changed:\nbefore=%s\nafter =%s", run.name, bj, aj)
		}
		diskAfter, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read ledger after: %v", run.name, err)
		}
		if string(diskBefore) != string(diskAfter) {
			t.Fatalf("%s: ledger file changed:\nbefore=%s\nafter =%s", run.name, diskBefore, diskAfter)
		}
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
		t.Fatalf("reopened ledger differs:\nwant=%s\ngot =%s", bj, rj)
	}
}
