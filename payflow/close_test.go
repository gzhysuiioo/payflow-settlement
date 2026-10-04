package payflow

import (
	"sync"
	"testing"
)

// blockPersist 注入“保存进行中”状态：第一次持久化时阻塞，直到测试放行，
// 用于构造“付款已开始、保存尚未完成时句柄被关闭”的交错时序。
// 放行时按 fail 决定本次保存成败；其后的持久化调用一律正常成功。
type blockPersist struct {
	entered chan struct{} // 进入第一次持久化时关闭
	release chan struct{} // 测试关闭以放行本次保存
	fail    bool          // 放行后本次保存是否失败
	once    sync.Once
}

func newBlockPersist(fail bool) *blockPersist {
	return &blockPersist{entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
}

func (b *blockPersist) FailNextPersist() bool {
	first := false
	b.once.Do(func() {
		first = true
		close(b.entered)
	})
	if !first {
		return false
	}
	<-b.release
	return b.fail
}

// 句柄关闭后，提交付款、查询余额、查询快照、预览与退款都必须返回
// ledger_closed（而不是被当成空查询或空批次），且不扣款、不留记录；
// 重复关闭同一已关闭句柄仍应成功。
func TestClosedHandleRejectsOperations(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 40)}})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("setup settle: %v %+v", err, res.Results[0])
	}

	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 重复关闭同一已关闭句柄仍成功。
	if err := l.Close(); err != nil {
		t.Fatalf("second close must succeed: %v", err)
	}

	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p2", "aa", "usdc", 10)}}); KindOf(err) != ErrClosed {
		t.Fatalf("submit on closed handle: want %s, got %v", ErrClosed, err)
	}
	if _, err := l.Balance("aa", "usdc"); KindOf(err) != ErrClosed {
		t.Fatalf("balance on closed handle: want %s, got %v", ErrClosed, err)
	}
	// 关闭句柄的查询必须报错，不能伪装成空查询结果。
	if _, err := l.Query(); KindOf(err) != ErrClosed {
		t.Fatalf("query on closed handle: want %s, got %v", ErrClosed, err)
	}
	if _, err := l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p2", "aa", "usdc", 10)}}); KindOf(err) != ErrClosed {
		t.Fatalf("preview on closed handle: want %s, got %v", ErrClosed, err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "x")}}); KindOf(err) != ErrClosed {
		t.Fatalf("refund on closed handle: want %s, got %v", ErrClosed, err)
	}

	// 被拒绝的操作没有任何副作用：另一句柄看到的余额与历史不变。
	other := openOrFail(t, path)
	if bal, _ := other.Balance("aa", "usdc"); bal != 60 {
		t.Fatalf("closed-handle submit charged: balance=%d want 60", bal)
	}
	snap, _ := other.Query()
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" {
		t.Fatalf("closed-handle operations left records: %+v", snap.Settlements)
	}
}

// 关闭其中一个句柄不影响其他仍打开的句柄：另一句柄继续正常使用，
// 同一笔已成功付款重提仍返回 duplicate 和原记录——关闭操作不能让
// 旧编号重新获得扣款资格。
func TestCloseOneHandleLeavesOthersWorking(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l1 := openOrFail(t, path)
	l2 := openOrFail(t, path)

	res, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 70)}})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("settle via l1: %v %+v", err, res.Results[0])
	}
	orig := res.Results[0].Record

	if err := l1.Close(); err != nil {
		t.Fatalf("close l1: %v", err)
	}
	// l1 已关闭：任何操作都是 ledger_closed。
	if _, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p9", "aa", "usdc", 1)}}); KindOf(err) != ErrClosed {
		t.Fatalf("closed l1 submit: want %s, got %v", ErrClosed, err)
	}

	// l2 仍打开：看到 l1 留下的余额与成功记录。
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("l2 balance=%d want 30", bal)
	}
	// 同一笔已成功付款经 l2 重提：duplicate 并携带原记录，不再扣款。
	dup, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 70)}})
	if err != nil {
		t.Fatal(err)
	}
	d := dup.Results[0]
	if d.Status != StatusDuplicate || d.Record == nil ||
		d.Record.Seq != orig.Seq || d.Record.Charged != orig.Charged {
		t.Fatalf("resubmit after close: want duplicate with original record, got %+v", d)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("duplicate re-charged after close: balance=%d want 30", bal)
	}
	// l2 可继续新的付款，序号接续原记录。
	res2, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p2", "aa", "usdc", 30)}})
	if err != nil || res2.Results[0].Status != StatusSettled || res2.Results[0].Record.Seq != 2 {
		t.Fatalf("l2 new payment: %v %+v", err, res2.Results[0])
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("final balance=%d want 0", bal)
	}
}

// 唯一打开的句柄在一笔付款已开始、保存尚未完成时关闭，调用者随即重新
// 打开同一账本：先前的付款仍按原请求完成并落盘，新句柄使用的是这笔
// 操作结束后的完整状态——余额 30、唯一成功记录、序号连续；重提同编号
// 同字段得到 duplicate，另付 60 得到 insufficient_balance（不能因为
// 重新打开读到旧余额 100 而再次成功）。关闭本身不生成任何业务历史。
func TestCloseDuringInflightSettleThenReopen(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l1 := openOrFail(t, path)
	hook := newBlockPersist(false) // 放行后保存成功
	SetFailHook(l1, hook)

	type outcome struct {
		res *BatchResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 70)}})
		done <- outcome{res, err}
	}()
	<-hook.entered // 付款已开始、保存尚未完成

	// 此时关闭唯一句柄，并随即重新打开同一账本。
	if err := l1.Close(); err != nil {
		t.Fatalf("close during inflight submit: %v", err)
	}
	l2 := openOrFail(t, path)

	close(hook.release) // 放行保存
	out := <-done

	// 先前的付款按原请求完成：settled、序号 1、扣款 70。
	if out.err != nil {
		t.Fatalf("inflight submit: %v", out.err)
	}
	got := out.res.Results[0]
	if got.Status != StatusSettled || got.Record == nil ||
		got.Record.Seq != 1 || got.Record.Charged != 70 || got.Record.Fee != 0 {
		t.Fatalf("inflight payment must settle as requested, got %+v", got)
	}

	// 新句柄看到这笔操作结束后的完整状态。
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("reopened balance=%d want 30", bal)
	}
	snap, err := l2.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p" || snap.Settlements[0].Seq != 1 {
		t.Fatalf("reopened settlements=%+v want the single inflight record", snap.Settlements)
	}
	if len(snap.Refunds) != 0 {
		t.Fatalf("close itself must not generate history: %+v", snap.Refunds)
	}

	// 重提同编号同字段：duplicate，携带原记录（序号与原记录一致）。
	dup, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 70)}})
	if err != nil {
		t.Fatal(err)
	}
	if d := dup.Results[0]; d.Status != StatusDuplicate || d.Record == nil ||
		d.Record.Seq != 1 || d.Record.Charged != 70 {
		t.Fatalf("resubmit after reopen: want duplicate with original record, got %+v", d)
	}

	// 另付 60：余额只有 30，必须 insufficient_balance——重新打开不能
	// 读到关闭前的旧余额 100 而让同一笔再次成功。
	res2, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("q", "aa", "usdc", 60)}})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Results[0].Status != StatusFunds {
		t.Fatalf("stale balance reused: want insufficient_balance, got %+v", res2.Results[0])
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("balance=%d want 30", bal)
	}
	snap2, _ := l2.Query()
	if len(snap2.Settlements) != 1 {
		t.Fatalf("failed payment must not add records: %+v", snap2.Settlements)
	}
}

// 同样的交错时序下，若先前付款最终保存失败：它仍返回 storage_error，
// 新句柄看到的余额恢复为 100、历史中没有这笔付款；失败编号仍可用于
// 之后的合法付款——新句柄用原失败编号付款 60 首次成功、余额变为 40。
// 关闭与重新打开不能保留那笔失败付款的扣款或去重资格。
func TestCloseDuringInflightStorageFailureThenReopen(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l1 := openOrFail(t, path)
	hook := newBlockPersist(true) // 放行后本次保存失败
	SetFailHook(l1, hook)

	type outcome struct {
		res *BatchResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 70)}})
		done <- outcome{res, err}
	}()
	<-hook.entered // 付款已开始、保存尚未完成

	if err := l1.Close(); err != nil {
		t.Fatalf("close during inflight submit: %v", err)
	}
	l2 := openOrFail(t, path)

	close(hook.release) // 放行，保存失败
	out := <-done

	// 先前付款返回 storage_error（逐项业务失败，不是批次级错误）。
	if out.err != nil {
		t.Fatalf("inflight submit: %v", out.err)
	}
	if got := out.res.Results[0]; got.Status != StatusStorage {
		t.Fatalf("inflight payment must report storage_error, got %+v", got)
	}

	// 新句柄：余额恢复 100，历史中没有这笔付款。
	if bal, _ := l2.Balance("aa", "usdc"); bal != 100 {
		t.Fatalf("reopened balance=%d want 100 (failed charge rolled back)", bal)
	}
	snap, err := l2.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Settlements) != 0 {
		t.Fatalf("failed payment must not appear in history: %+v", snap.Settlements)
	}

	// 失败编号仍可用于之后的合法付款：新句柄用原失败编号付款 60
	// 首次成功（不是 duplicate），余额变为 40，序号从 1 开始。
	res2, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 60)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res2.Results[0]; got.Status != StatusSettled || got.Record == nil ||
		got.Record.Seq != 1 || got.Record.Charged != 60 {
		t.Fatalf("failed id must be reusable as a first payment, got %+v", got)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 40 {
		t.Fatalf("balance=%d want 40", bal)
	}

	// 去重资格由这次成功建立：再次重提同编号同字段才是 duplicate。
	dup, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 60)}})
	if err != nil {
		t.Fatal(err)
	}
	if d := dup.Results[0]; d.Status != StatusDuplicate || d.Record == nil || d.Record.Seq != 1 {
		t.Fatalf("resubmit want duplicate of the new record, got %+v", d)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 40 {
		t.Fatalf("duplicate re-charged: balance=%d want 40", bal)
	}
}
