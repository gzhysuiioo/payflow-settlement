package payflow

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 本文件为 Query 查询结果与真实账本之间的独立性提供回归保障：
//
//  1. 时效独立：先前取得的快照是“当时的账单”，后续付款/退款改变账本后，
//     旧快照的余额、记录数量与记录内容一律保持原样；新记录只出现在新查询中。
//  2. 空间独立：调用者可以任意调整返回的余额、结算记录、退款记录供自己展示，
//     篡改只影响自己那份；另一份独立取得的结果与再次查询的真实数据都不受影响。
//  3. 判定独立：对返回数据的任何改动都不能影响后续付款按真实余额结算，
//     也不能改变原付款是否已结算/已退款的判断。
//  4. 只读：仅查询或修改返回数据时，账本文件字节与成功记录数量保持不变。
//
// 查询入口与 Snapshot 中余额、结算、退款三个列表的公开 JSON 格式同样锁定。

func mustQuery(t *testing.T, l *Ledger) *Snapshot {
	t.Helper()
	snap, err := l.Query()
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return snap
}

func mustSettle(t *testing.T, l *Ledger, bps int, it PaymentIntent) ItemResult {
	t.Helper()
	res, err := l.Submit(FeeBatch{FeeBps: bps, Intents: []PaymentIntent{it}})
	if err != nil {
		t.Fatalf("submit %q: %v", it.ID, err)
	}
	return res.Results[0]
}

func mustRefundOne(t *testing.T, l *Ledger, id, sid, reason string) RefundResult {
	t.Helper()
	res, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq(id, sid, reason)}})
	if err != nil {
		t.Fatalf("refund %q: %v", id, err)
	}
	return res.Results[0]
}

func assertSnap(t *testing.T, got, want *Snapshot, label string) {
	t.Helper()
	if !reflect.DeepEqual(got.Balances, want.Balances) {
		t.Fatalf("%s balances:\n got %#v\nwant %#v", label, got.Balances, want.Balances)
	}
	if !reflect.DeepEqual(got.Settlements, want.Settlements) {
		t.Fatalf("%s settlements:\n got %#v\nwant %#v", label, got.Settlements, want.Settlements)
	}
	if !reflect.DeepEqual(got.Refunds, want.Refunds) {
		t.Fatalf("%s refunds:\n got %#v\nwant %#v", label, got.Refunds, want.Refunds)
	}
}

// TestQuerySnapshotFreezesAcrossPaymentAndRefund 覆盖带手续费付款与全额退款
// 发生前后的查询结果：旧快照冻结在取得时刻，新查询反映最新真实账本。
func TestQuerySnapshotFreezesAcrossPaymentAndRefund(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "eth", Balance: 3},
		{Account: "aa", Asset: "usdc", Balance: 2000},
		{Account: "bb", Asset: "usdc", Balance: 500},
	})
	l := openOrFail(t, path)

	p1 := intent("p1", "aa", "usdc", 1000) // 100bps：fee=10, charged=1010
	p2 := intent("p2", "bb", "usdc", 200)  // 0bps：charged=200

	// 1. 尚无付款或退款时取得空历史。
	snap0 := mustQuery(t, l)
	want0 := &Snapshot{
		Balances: []BalanceView{
			{Account: "aa", Asset: "eth", Balance: 3},
			{Account: "aa", Asset: "usdc", Balance: 2000},
			{Account: "bb", Asset: "usdc", Balance: 500},
		},
		Settlements: []Record{},
		Refunds:     []RefundRecord{},
	}
	assertSnap(t, snap0, want0, "snap0 empty history")

	// 公开格式回归：三个列表键名固定，空列表序列化为 [] 而不是 null。
	raw0, err := json.Marshal(snap0)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"balances":[`, `"settlements":[]`, `"refunds":[]`} {
		if !bytes.Contains(raw0, []byte(frag)) {
			t.Fatalf("snapshot JSON missing %s:\n%s", frag, raw0)
		}
	}

	// 2. 带手续费付款成功：新查询显示金额与手续费一并扣除后的余额与结算记录。
	if r := mustSettle(t, l, 100, p1); r.Status != StatusSettled {
		t.Fatalf("p1: %+v", r)
	}
	want1 := &Snapshot{
		Balances: []BalanceView{
			{Account: "aa", Asset: "eth", Balance: 3},
			{Account: "aa", Asset: "usdc", Balance: 990}, // 2000-1010
			{Account: "bb", Asset: "usdc", Balance: 500},
		},
		Settlements: []Record{{
			ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
			Amount: 1000, Nonce: 1, FeeBps: 100, Fee: 10, Charged: 1010, Seq: 1,
		}},
		Refunds: []RefundRecord{},
	}
	snap1 := mustQuery(t, l)
	assertSnap(t, snap1, want1, "snap1 after p1")

	// 先前的空历史不跟随付款变化：余额、记录数量仍保持原样。
	assertSnap(t, snap0, want0, "snap0 frozen after p1")

	// 3. 第二笔付款落在另一账户/资产；随后全额退回 p1（含手续费）。
	if r := mustSettle(t, l, 0, p2); r.Status != StatusSettled {
		t.Fatalf("p2: %+v", r)
	}
	if r := mustRefundOne(t, l, "r1", "p1", "cancel order"); r.Status != StatusRefundSuccess {
		t.Fatalf("r1: %+v", r)
	}
	want2 := &Snapshot{
		Balances: []BalanceView{
			{Account: "aa", Asset: "eth", Balance: 3}, // 未发生收支的组合保持原余额
			{Account: "aa", Asset: "usdc", Balance: 2000}, // 990+1010 回到原扣款总额
			{Account: "bb", Asset: "usdc", Balance: 300},  // 500-200
		},
		Settlements: []Record{
			{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
				Amount: 1000, Nonce: 1, FeeBps: 100, Fee: 10, Charged: 1010, Seq: 1},
			{ID: "p2", Account: "bb", Paymaster: "pm", Asset: "usdc",
				Amount: 200, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 200, Seq: 2},
		},
		Refunds: []RefundRecord{{
			ID: "r1", SettlementID: "p1", Reason: "cancel order",
			Account: "aa", Asset: "usdc", Amount: 1000, Fee: 10, Charged: 1010,
			AfterSeq: 2, Seq: 1,
		}},
	}
	snap2 := mustQuery(t, l)
	assertSnap(t, snap2, want2, "snap2 after r1")
	// 原结算仍然保留、内容未被退款改写（want2 中 p1 的 charged 仍为 1010）。

	// 4. 第二笔退款：退款记录按成功先后排列；无关组合 eth 不动。
	if r := mustRefundOne(t, l, "r2", "p2", "user asked"); r.Status != StatusRefundSuccess {
		t.Fatalf("r2: %+v", r)
	}
	want3 := &Snapshot{
		Balances: []BalanceView{
			{Account: "aa", Asset: "eth", Balance: 3},
			{Account: "aa", Asset: "usdc", Balance: 2000},
			{Account: "bb", Asset: "usdc", Balance: 500}, // 300+200
		},
		Settlements: want2.Settlements, // 结算列表内容与顺序不再变化
		Refunds: []RefundRecord{
			{ID: "r1", SettlementID: "p1", Reason: "cancel order",
				Account: "aa", Asset: "usdc", Amount: 1000, Fee: 10, Charged: 1010,
				AfterSeq: 2, Seq: 1},
			{ID: "r2", SettlementID: "p2", Reason: "user asked",
				Account: "bb", Asset: "usdc", Amount: 200, Fee: 0, Charged: 200,
				AfterSeq: 2, Seq: 2},
		},
	}
	snap3 := mustQuery(t, l)
	assertSnap(t, snap3, want3, "snap3 after r2")

	// 5. 所有先前快照继续冻结在各自的取得时刻：余额、记录数量、记录内容均不变。
	assertSnap(t, snap0, want0, "snap0 frozen at end")
	assertSnap(t, snap1, want1, "snap1 frozen after refund")
	assertSnap(t, snap2, want2, "snap2 frozen after r2")

	// 公开 JSON 格式可往返，三个列表与字段名保持兼容。
	raw3, err := json.Marshal(snap3)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Snapshot
	if err := json.Unmarshal(raw3, &roundTrip); err != nil {
		t.Fatal(err)
	}
	assertSnap(t, &roundTrip, want3, "json round trip")
}

// TestQueryEmptyHistoryStaysEmpty 专门锁定：尚无任何记录时取得的空历史，
// 在后续付款、退款发生后仍必须为空；新记录只能出现在新查询里。
func TestQueryEmptyHistoryStaysEmpty(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 100},
		{Account: "bb", Asset: "usdc", Balance: 100},
	})
	l := openOrFail(t, path)

	empty := mustQuery(t, l)
	if len(empty.Settlements) != 0 || len(empty.Refunds) != 0 {
		t.Fatalf("initial history must be empty: %+v", empty)
	}
	emptyBal := append([]BalanceView(nil), empty.Balances...)

	// 账本在另一个账户上发生完整的付款 → 退款。
	if r := mustSettle(t, l, 100, intent("p1", "bb", "usdc", 50)); r.Status != StatusSettled {
		t.Fatalf("p1: %+v", r) // charged 51（50+1），bb 余 49
	}
	if r := mustRefundOne(t, l, "r1", "p1", "oops"); r.Status != StatusRefundSuccess {
		t.Fatalf("r1: %+v", r) // bb 回到 100
	}

	// 旧空快照仍为空，且余额仍是当时内容（aa 100、bb 100）。
	if len(empty.Settlements) != 0 || len(empty.Refunds) != 0 {
		t.Fatalf("old snapshot history must stay empty: settlements=%d refunds=%d",
			len(empty.Settlements), len(empty.Refunds))
	}
	if !reflect.DeepEqual(empty.Balances, emptyBal) {
		t.Fatalf("old snapshot balances changed: %#v", empty.Balances)
	}

	// 新查询包含新记录，且不影响旧快照的空列表。
	fresh := mustQuery(t, l)
	if len(fresh.Settlements) != 1 || fresh.Settlements[0].ID != "p1" {
		t.Fatalf("new query must carry the payment: %+v", fresh.Settlements)
	}
	if len(fresh.Refunds) != 1 || fresh.Refunds[0].SettlementID != "p1" {
		t.Fatalf("new query must carry the refund: %+v", fresh.Refunds)
	}
	if len(empty.Settlements) != 0 || len(empty.Refunds) != 0 {
		t.Fatalf("taking a new query must not populate the old snapshot")
	}
}

// TestQueryCallerEditsStayPrivate 验证调用者对返回结果的改写只能影响自己那份：
// 余额与三类列表的元素、追加、重排都不能污染真实账本或另一份独立结果；
// 余额和历史必须一起受到保护，再次查询仍得到真实业务数据。
func TestQueryCallerEditsStayPrivate(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 5000},
		{Account: "bb", Asset: "usdc", Balance: 1000},
	})
	l := openOrFail(t, path)

	// 真实业务：p1 带手续费成功后被 r1 全额退款；之后 p2、p3 分别在两个账户成功。
	if r := mustSettle(t, l, 100, intent("p1", "aa", "usdc", 1000)); r.Status != StatusSettled {
		t.Fatalf("p1: %+v", r) // charged 1010
	}
	if r := mustRefundOne(t, l, "r1", "p1", "bad charge"); r.Status != StatusRefundSuccess {
		t.Fatalf("r1: %+v", r) // 退回 1010，r1 落账时只有 1 笔结算（after_seq=1）
	}
	if r := mustSettle(t, l, 0, intent("p2", "aa", "usdc", 2000)); r.Status != StatusSettled {
		t.Fatalf("p2: %+v", r) // aa 余 3000
	}
	if r := mustSettle(t, l, 50, intent("p3", "bb", "usdc", 400)); r.Status != StatusSettled {
		t.Fatalf("p3: %+v", r) // fee=2, charged 402，bb 余 598
	}

	wantFrozen := &Snapshot{
		Balances: []BalanceView{
			{Account: "aa", Asset: "usdc", Balance: 3000},
			{Account: "bb", Asset: "usdc", Balance: 598},
		},
		Settlements: []Record{
			{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc",
				Amount: 1000, Nonce: 1, FeeBps: 100, Fee: 10, Charged: 1010, Seq: 1},
			{ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc",
				Amount: 2000, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 2000, Seq: 2},
			{ID: "p3", Account: "bb", Paymaster: "pm", Asset: "usdc",
				Amount: 400, Nonce: 1, FeeBps: 50, Fee: 2, Charged: 402, Seq: 3},
		},
		Refunds: []RefundRecord{{
			ID: "r1", SettlementID: "p1", Reason: "bad charge",
			Account: "aa", Asset: "usdc", Amount: 1000, Fee: 10, Charged: 1010,
			AfterSeq: 1, Seq: 1,
		}},
	}

	// 两份独立取得的结果初始一致。
	snapA := mustQuery(t, l)
	snapB := mustQuery(t, l)
	assertSnap(t, snapA, wantFrozen, "snapA before edits")
	assertSnap(t, snapB, wantFrozen, "snapB before edits")

	// 像调用者为自己展示所做的那样，任意改写 snapA：余额、结算、退款、追加与重排。
	snapA.Balances[0].Account = "zz"
	snapA.Balances[0].Asset = "btc"
	snapA.Balances[0].Balance = 999_999
	snapA.Balances = append(snapA.Balances, BalanceView{Account: "fake", Asset: "fake", Balance: 1})
	snapA.Settlements[0].ID = "HACK"
	snapA.Settlements[0].Account = "zz"
	snapA.Settlements[0].Paymaster = "fake-pm"
	snapA.Settlements[0].Asset = "btc"
	snapA.Settlements[0].Amount = 1
	snapA.Settlements[0].Nonce = 4242
	snapA.Settlements[0].FeeBps = 9999
	snapA.Settlements[0].Fee = 9
	snapA.Settlements[0].Charged = 99
	snapA.Settlements[0].Seq = 77
	snapA.Settlements[1], snapA.Settlements[2] = snapA.Settlements[2], snapA.Settlements[1]
	snapA.Settlements = append(snapA.Settlements, Record{ID: "fake-pay"})
	snapA.Refunds[0].ID = "HACK-REF"
	snapA.Refunds[0].SettlementID = "p2"
	snapA.Refunds[0].Reason = "forged"
	snapA.Refunds[0].Account = "zz"
	snapA.Refunds[0].Asset = "btc"
	snapA.Refunds[0].Amount = 1
	snapA.Refunds[0].Fee = 1
	snapA.Refunds[0].Charged = 2
	snapA.Refunds[0].AfterSeq = 99
	snapA.Refunds[0].Seq = 99
	snapA.Refunds = append(snapA.Refunds, RefundRecord{ID: "fake-ref"})

	// 此前独立取得的另一份结果不能被改写波及（余额与历史一起受保护）。
	assertSnap(t, snapB, wantFrozen, "snapB after editing snapA")

	// 再次查询仍得到真实业务产生的数据：原编号、账户、资产、金额、手续费、
	// 成功序号，以及退款原因和关联原付款全部完整。
	snapC := mustQuery(t, l)
	assertSnap(t, snapC, wantFrozen, "ledger after editing snapA")
	rp1 := snapC.Settlements[0]
	if rp1.ID != "p1" || rp1.Account != "aa" || rp1.Asset != "usdc" ||
		rp1.Amount != 1000 || rp1.Fee != 10 || rp1.FeeBps != 100 || rp1.Seq != 1 {
		t.Fatalf("real p1 record lost after caller edit: %+v", rp1)
	}
	rr1 := snapC.Refunds[0]
	if rr1.ID != "r1" || rr1.Reason != "bad charge" || rr1.SettlementID != "p1" ||
		rr1.Account != "aa" || rr1.Asset != "usdc" || rr1.Amount != 1000 ||
		rr1.Fee != 10 || rr1.Charged != 1010 || rr1.Seq != 1 {
		t.Fatalf("real r1 record lost after caller edit: %+v", rr1)
	}

	// 对返回数据的改动不能改变业务判定：
	// 已退款的原付款按原请求再提交，仍返回 duplicate 和原结算记录，不再次扣款。
	dup := mustSettle(t, l, 100, intent("p1", "aa", "usdc", 1000))
	if dup.Status != StatusDuplicate || dup.Record == nil ||
		dup.Record.Seq != 1 || dup.Record.Charged != 1010 {
		t.Fatalf("refunded payment retry want duplicate with original record: %+v", dup)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 3000 {
		t.Fatalf("duplicate after snapshot edit charged again: bal=%d", bal)
	}

	// 用新的退款编号申请退回同一付款：仍为 already_refunded，不增加余额。
	rx := mustRefundOne(t, l, "rx", "p1", "trying again")
	if rx.Status != StatusAlreadyRefunded {
		t.Fatalf("want already_refunded, got %+v", rx)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 3000 {
		t.Fatalf("already_refunded after snapshot edit changed balance: %d", bal)
	}

	// 后续付款仍按真实余额结算（篡改既没有抬高也没有压低可用余额）。
	p4 := mustSettle(t, l, 0, intent("p4", "aa", "usdc", 3000))
	if p4.Status != StatusSettled || p4.Record.Seq != 4 {
		t.Fatalf("payment must settle against the real balance: %+v", p4)
	}
	final := mustQuery(t, l)
	if final.Balances[0].Balance != 0 || final.Balances[1].Balance != 598 {
		t.Fatalf("final balances: %+v", final.Balances)
	}
	if len(final.Settlements) != 4 || len(final.Refunds) != 1 {
		t.Fatalf("final history counts: settlements=%d refunds=%d",
			len(final.Settlements), len(final.Refunds))
	}
}

// TestQueryEditCannotRaiseSpendingLimit 验证篡改返回数据（改大余额、伪造记录）
// 不能提升调用者的购买力，也不能让伪造编号在真实账本中被当成已结算。
func TestQueryEditCannotRaiseSpendingLimit(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)

	if r := mustSettle(t, l, 0, intent("p1", "aa", "usdc", 30)); r.Status != StatusSettled {
		t.Fatalf("p1: %+v", r) // 真实余额 70
	}

	snap := mustQuery(t, l)
	for i := range snap.Balances {
		if snap.Balances[i].Account == "aa" && snap.Balances[i].Asset == "usdc" {
			snap.Balances[i].Balance = 1_000_000
		}
	}
	snap.Balances = append(snap.Balances, BalanceView{Account: "ghost", Asset: "gold", Balance: 7})
	snap.Settlements[0].Charged = 0
	snap.Settlements[0].Amount = 0
	// 追加一条伪造成功记录：若查询与账本共享底层数组，这会直接污染真实历史。
	snap.Settlements = append(snap.Settlements, Record{
		ID: "fake", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 10, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 10, Seq: 99,
	})

	// 真实余额未被抬高：超出 70 的付款仍失败。
	if r := mustSettle(t, l, 0, intent("p2", "aa", "usdc", 71)); r.Status != StatusFunds {
		t.Fatalf("inflated snapshot balance must not enable overspend: %+v", r)
	}

	// 伪造编号没有进入真实历史：同编号同字段的请求按首次付款处理并成功，
	// 而不是 duplicate；扣款依据真实余额 70。
	fake := mustSettle(t, l, 0, intent("fake", "aa", "usdc", 10))
	if fake.Status != StatusSettled || fake.Record.Seq != 2 {
		t.Fatalf("forged record must not exist in the real ledger: %+v", fake)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 60 {
		t.Fatalf("real balance=%d want 60", bal)
	}
	again := mustQuery(t, l)
	if len(again.Settlements) != 2 || again.Settlements[0].Amount != 30 || again.Settlements[0].Charged != 30 {
		t.Fatalf("real p1 record must stay intact: %+v", again.Settlements)
	}
	if len(again.Balances) != 1 {
		t.Fatalf("forged balance entry leaked into the ledger: %+v", again.Balances)
	}
}

// TestQueryAndCallerEditsDoNotRewriteLedgerFile 验证仅查询或修改返回数据时，
// 账本文件字节内容与成功记录数量都保持不变。
func TestQueryAndCallerEditsDoNotRewriteLedgerFile(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)

	// 先产生一笔付款与一笔退款，使余额、结算、退款三类数据都存在。
	if r := mustSettle(t, l, 100, intent("p1", "aa", "usdc", 300)); r.Status != StatusSettled {
		t.Fatalf("p1: %+v", r) // charged 303
	}
	if r := mustRefundOne(t, l, "r1", "p1", "review"); r.Status != StatusRefundSuccess {
		t.Fatalf("r1: %+v", r)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 反复查询，每次都对自己的返回结果做破坏性改写；查询不得触发任何写盘。
	for i := 0; i < 3; i++ {
		snap := mustQuery(t, l)
		for j := range snap.Balances {
			snap.Balances[j].Balance = int64(-(i + 1))
			snap.Balances[j].Account = "mutated"
		}
		snap.Balances = append(snap.Balances, BalanceView{Account: "x", Asset: "y", Balance: 9})
		for j := range snap.Settlements {
			snap.Settlements[j].ID = "MUTATED"
			snap.Settlements[j].Charged = -1
			snap.Settlements[j].Seq = int64(100 + i)
		}
		snap.Settlements = append(snap.Settlements, Record{ID: "mutated-pay"})
		for j := range snap.Refunds {
			snap.Refunds[j].ID = "MUTATED-REF"
			snap.Refunds[j].Reason = ""
			snap.Refunds[j].SettlementID = "ghost"
		}
		snap.Refunds = append(snap.Refunds, RefundRecord{ID: "mutated-ref"})
		if _, err := json.Marshal(snap); err != nil {
			t.Fatal(err)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger file changed after queries and caller edits:\nbefore=%s\nafter =%s", before, after)
	}

	// 成功记录数量与内容保持不变。
	snap := mustQuery(t, l)
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" || snap.Settlements[0].Charged != 303 {
		t.Fatalf("settlement record changed: %+v", snap.Settlements)
	}
	if len(snap.Refunds) != 1 || snap.Refunds[0].ID != "r1" || snap.Refunds[0].SettlementID != "p1" {
		t.Fatalf("refund record changed: %+v", snap.Refunds)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("balance changed: %d", bal)
	}
}
