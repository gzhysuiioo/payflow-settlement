package payflow

import (
	"os"
	"sync/atomic"
	"testing"
)

// 本文件回归“句柄关闭与付款交错”时调用者可观察到的行为：
//
//   - 关闭只结束该句柄本身：关闭后经它付款/查询一律 ledger_closed，
//     不扣款、不留成功记录、不被当成空查询；重复关闭仍成功。
//   - 同进程的其他句柄继续正常使用：余额、成功记录、幂等与冲突判定
//     不受某个句柄关闭的影响。
//   - 唯一句柄在一笔付款“已开始、保存尚未完成”时关闭并随即重开：
//     在途付款不被取消，新句柄面对的是该操作结束后的完整状态——
//     成功则余额/记录/序号/去重资格全部生效，失败则彻底回滚，
//     失败编号仍可用于之后的合法付款。
//
// 交错窗口由 gatedPersistHook 在持久化调用处确定性地制造，
// 不依赖时序调度，因此可用 -race 稳定重复运行。

// gatedPersistHook 在第一次持久化调用处制造“保存尚未完成”的窗口：
// 首次 FailNextPersist 进入后阻塞在 entered 上，直到测试 close(release)，
// 随后返回 fail（true 表示本次保存失败）；窗口之后的所有持久化一律放行，
// 使重开句柄上的后续操作正常落盘。calls 记录持久化被触发的总次数，
// 用于证明 Close 本身不产生任何写入（不生成业务历史）。
type gatedPersistHook struct {
	active  atomic.Bool
	entered chan struct{}
	release chan struct{}
	fail    bool
	calls   atomic.Int64
}

func newGatedPersistHook(fail bool) *gatedPersistHook {
	h := &gatedPersistHook{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		fail:    fail,
	}
	h.active.Store(true)
	return h
}

func (h *gatedPersistHook) FailNextPersist() bool {
	h.calls.Add(1)
	if !h.active.CompareAndSwap(true, false) {
		return false // 窗口之后的持久化全部成功
	}
	close(h.entered)
	<-h.release
	return h.fail
}

// assertClosedHandle 断言已关闭句柄上的每一类操作都返回 ledger_closed：
// 错误必须明确（余额返回 0 但同时带错，不能被当成合法的空查询；
// Query 不得返回空快照代替错误），且不产生任何业务结果。
func assertClosedHandle(t *testing.T, l *Ledger) {
	t.Helper()

	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("late", "aa", "usdc", 1),
	}}); KindOf(err) != ErrClosed {
		t.Fatalf("Submit on closed handle: want %s, got %v", ErrClosed, err)
	}
	if bal, err := l.Balance("aa", "usdc"); KindOf(err) != ErrClosed || bal != 0 {
		t.Fatalf("Balance on closed handle: want %s with zero value, got bal=%d err=%v", ErrClosed, bal, err)
	}
	if snap, err := l.Query(); KindOf(err) != ErrClosed || snap != nil {
		t.Fatalf("Query on closed handle: want %s with nil snapshot, got snap=%+v err=%v", ErrClosed, snap, err)
	}
	if _, err := l.Preview(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("late", "aa", "usdc", 1),
	}}); KindOf(err) != ErrClosed {
		t.Fatalf("Preview on closed handle: want %s, got %v", ErrClosed, err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r-late", "p1", "x"),
	}}); KindOf(err) != ErrClosed {
		t.Fatalf("Refund on closed handle: want %s, got %v", ErrClosed, err)
	}
}

// persistedBalanceAndCount 直接解码磁盘文件，返回指定组合的余额与
// 成功结算记录数，用于验证“重开读到的就是操作结束后落盘的状态”。
func persistedBalanceAndCount(t *testing.T, path, account, asset string) (int64, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	st, err := decodeState(data)
	if err != nil {
		t.Fatalf("decode persisted ledger: %v", err)
	}
	return st.Balances[balanceKey{account, asset}], len(st.Settlements)
}

// 句柄关闭后：所有操作返回 ledger_closed，不扣款、不留记录；
// 重复关闭同一已关闭句柄仍成功；账本文件可被重新打开且状态原样。
func TestClosedHandleRejectsEveryOperation(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := l.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// 重复关闭必须仍然成功（关闭是幂等的）。
	if err := l.Close(); err != nil {
		t.Fatalf("repeated close must succeed, got %v", err)
	}

	assertClosedHandle(t, l)

	// 关闭不改动磁盘：重开得到初始余额与空历史，关闭本身不留任何业务痕迹。
	l2 := openOrFail(t, path)
	if bal, _ := l2.Balance("aa", "usdc"); bal != 100 {
		t.Fatalf("balance after reopen=%d want 100", bal)
	}
	snap, _ := l2.Query()
	if len(snap.Settlements) != 0 || len(snap.Refunds) != 0 {
		t.Fatalf("close must not leave business history: %+v", snap)
	}
	if bal, n := persistedBalanceAndCount(t, path, "aa", "usdc"); bal != 100 || n != 0 {
		t.Fatalf("persisted state after close: bal=%d records=%d, want 100/0", bal, n)
	}
}

// 同进程持有多个句柄时，关闭其中一个只结束该句柄：其他句柄继续看到
// 共享余额与成功记录，已成功编号仍是 duplicate（携带原记录）或 conflict，
// 旧编号不会因关闭重新获得扣款资格，后续成功付款继续正常占用序号。
func TestCloseOneHandleKeepsOtherHandleWorking(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l1.Close() })
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l2.Close() })

	first, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Results[0].Status != StatusSettled {
		t.Fatalf("setup settle: %+v", first.Results[0])
	}
	rec := *first.Results[0].Record

	// 关闭其中一个句柄：它立即不可用，但不取消任何已完成或在途的状态。
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}
	assertClosedHandle(t, l1)

	// 另一句柄照常工作：看到 100-70=30 的共享余额。
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("surviving handle balance=%d want 30", bal)
	}
	// 同编号同字段：duplicate 且逐字返回原记录，不再扣款。
	dup, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := dup.Results[0]
	if d.Status != StatusDuplicate || d.Record == nil || *d.Record != rec {
		t.Fatalf("retry on surviving handle want duplicate with original record %+v, got %+v", rec, d)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("duplicate charged again: balance=%d want 30", bal)
	}
	// 同编号改字段：conflict，旧编号不重新获得扣款资格。
	cf, _ := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 71),
	}})
	if cf.Results[0].Status != StatusConflict {
		t.Fatalf("changed retry want conflict, got %+v", cf.Results[0])
	}
	// 余额 30：另付 60 余额不足。
	funds, _ := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p2", "aa", "usdc", 60),
	}})
	if funds.Results[0].Status != StatusFunds {
		t.Fatalf("60 against balance 30 want insufficient_balance, got %+v", funds.Results[0])
	}
	// 新编号付 30 成功并占用下一个序号。
	ok, _ := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p3", "aa", "usdc", 30),
	}})
	if ok.Results[0].Status != StatusSettled || ok.Results[0].Record.Seq != 2 {
		t.Fatalf("next payment want settled with seq 2, got %+v", ok.Results[0])
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("balance=%d want 0", bal)
	}

	// 关闭后重新打开（l2 仍在）：新句柄加入同一注册表，看到完整状态。
	l3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l3.Close() })
	snap, _ := l3.Query()
	if len(snap.Settlements) != 2 ||
		snap.Settlements[0].ID != "p1" || snap.Settlements[0].Seq != 1 ||
		snap.Settlements[1].ID != "p3" || snap.Settlements[1].Seq != 2 {
		t.Fatalf("reopened handle history: %+v", snap.Settlements)
	}
	dup3, _ := l3.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}})
	if dup3.Results[0].Status != StatusDuplicate || dup3.Results[0].Record.Seq != 1 {
		t.Fatalf("duplicate after reopen: %+v", dup3.Results[0])
	}

	// 全部句柄关闭后再从磁盘恢复，结果必须一致。
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l3.Close(); err != nil {
		t.Fatal(err)
	}
	l4 := openOrFail(t, path)
	if bal, _ := l4.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("disk-reopened balance=%d want 0", bal)
	}
	snap4, _ := l4.Query()
	if len(snap4.Settlements) != 2 {
		t.Fatalf("disk-reopened history=%+v", snap4.Settlements)
	}
}

// 核心回归（成功分支）：唯一打开的句柄在一笔付款已经开始、保存尚未
// 完成时关闭，调用者随即重开同一账本。在途付款必须按原请求完成，
// 新句柄面对的是这笔操作结束后的完整状态：
// 余额 100、费率 0、付款 70 成功后，新句柄看到余额 30 与唯一成功记录；
// 重提同编号同字段得到 duplicate 与原记录，另付 60 得到 insufficient_balance；
// 成功序号与原记录一致，关闭本身不触发持久化、不生成任何业务历史。
func TestReopenDuringInFlightPaymentCommitsToNewHandle(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l1.Close() })

	hook := newGatedPersistHook(false) // 窗口放行后保存成功
	SetFailHook(l1, hook)
	t.Cleanup(func() { SetFailHook(l1, nil) })

	type outcome struct {
		res *BatchResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
			intent("p1", "aa", "usdc", 70),
		}})
		done <- outcome{res, err}
	}()
	<-hook.entered // 付款已进入保存，且保存尚未完成

	// 在保存窗口内关闭唯一旧句柄，并立即重新打开同一账本。
	if err := l1.Close(); err != nil {
		t.Fatalf("close during in-flight payment: %v", err)
	}
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen while payment in flight: %v", err)
	}
	t.Cleanup(func() { l2.Close() })

	// 放行保存：在途付款不被关闭取消，按原请求成功。
	close(hook.release)
	got := <-done
	if got.err != nil {
		t.Fatalf("in-flight submit error: %v", got.err)
	}
	r := got.res.Results[0]
	if r.Status != StatusSettled || r.Record == nil {
		t.Fatalf("in-flight payment want settled, got %+v", r)
	}
	if r.Record.Seq != 1 || r.Record.Amount != 70 || r.Record.Charged != 70 || r.Record.FeeBps != 0 {
		t.Fatalf("in-flight settlement record: %+v", r.Record)
	}

	// 旧句柄在关闭后不复活：后续使用一律 ledger_closed。
	assertClosedHandle(t, l1)

	// 新句柄看到的是操作结束后的完整状态：余额 30、唯一成功记录。
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("new handle balance=%d want 30", bal)
	}
	snap, _ := l2.Query()
	if len(snap.Settlements) != 1 {
		t.Fatalf("new handle settlements=%+v want exactly one record", snap.Settlements)
	}
	if sr := snap.Settlements[0]; sr.ID != "p1" || sr.Seq != 1 || sr.Amount != 70 || sr.Charged != 70 {
		t.Fatalf("new handle record mismatch: %+v", sr)
	}
	if len(snap.Refunds) != 0 {
		t.Fatalf("close must not create refund history: %+v", snap.Refunds)
	}

	// 重提同编号同字段：duplicate 且携带原记录（序号仍是 1）。
	dup, _ := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}})
	if dup.Results[0].Status != StatusDuplicate ||
		dup.Results[0].Record == nil || dup.Results[0].Record.Seq != 1 {
		t.Fatalf("retry committed payment want duplicate with seq-1 record, got %+v", dup.Results[0])
	}
	// 同编号不同字段：conflict。
	cf, _ := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 71),
	}})
	if cf.Results[0].Status != StatusConflict {
		t.Fatalf("changed retry want conflict, got %+v", cf.Results[0])
	}
	// 另付 60：余额只有 30，必须不足——防止重开读到旧余额 100 后再次成功。
	funds, _ := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p2", "aa", "usdc", 60),
	}})
	if funds.Results[0].Status != StatusFunds {
		t.Fatalf("60 against balance 30 want insufficient_balance, got %+v", funds.Results[0])
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("balance after failed attempts=%d want 30", bal)
	}

	// 关闭本身不落盘：整个交错过程只发生过那一次在途付款的持久化。
	if n := hook.calls.Load(); n != 1 {
		t.Fatalf("persist calls=%d want 1 (close must not write)", n)
	}
	// 磁盘文件与新句柄状态一致：余额 30、唯一记录。
	if bal, n := persistedBalanceAndCount(t, path, "aa", "usdc"); bal != 30 || n != 1 {
		t.Fatalf("persisted state: bal=%d records=%d, want 30/1", bal, n)
	}

	// 全部关闭后再从磁盘重开：仍读到操作结束后的完整状态，旧余额不复活。
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	l3 := openOrFail(t, path)
	if bal, _ := l3.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("disk-reopened balance=%d want 30", bal)
	}
	snap3, _ := l3.Query()
	if len(snap3.Settlements) != 1 || snap3.Settlements[0].ID != "p1" || snap3.Settlements[0].Seq != 1 {
		t.Fatalf("disk-reopened history: %+v", snap3.Settlements)
	}
	dup3, _ := l3.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}})
	if dup3.Results[0].Status != StatusDuplicate || dup3.Results[0].Record.Seq != 1 {
		t.Fatalf("duplicate after disk reopen: %+v", dup3.Results[0])
	}
	retry60, _ := l3.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p2", "aa", "usdc", 60),
	}})
	if retry60.Results[0].Status != StatusFunds {
		t.Fatalf("60 after disk reopen want insufficient_balance, got %+v", retry60.Results[0])
	}
}

// 核心回归（保存失败分支）：唯一打开的句柄在一笔付款已经开始、保存
// 尚未完成时关闭并随即重开，而该笔付款最终保存失败。它仍必须按逐项
// 结果返回 storage_error（批次本身不报错），新句柄看到余额恢复为 100、
// 历史中没有这笔付款；失败编号不占去重资格，用原编号付 60 首次成功、
// 余额变为 40。关闭与重开不能保留失败付款的扣款或去重资格。
func TestReopenDuringFailingPaymentRollsBackForNewHandle(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l1.Close() })

	hook := newGatedPersistHook(true) // 窗口放行后本次保存失败
	SetFailHook(l1, hook)
	t.Cleanup(func() { SetFailHook(l1, nil) })

	type outcome struct {
		res *BatchResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
			intent("p1", "aa", "usdc", 70),
		}})
		done <- outcome{res, err}
	}()
	<-hook.entered

	if err := l1.Close(); err != nil {
		t.Fatalf("close during in-flight payment: %v", err)
	}
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen while payment in flight: %v", err)
	}
	t.Cleanup(func() { l2.Close() })

	close(hook.release)
	got := <-done
	// storage_error 是逐项业务结果（与成功、参数/状态/余额等业务失败、
	// ledger_closed 账本错误都不同），而不是批次级错误。
	if got.err != nil {
		t.Fatalf("storage failure must be reported as an item result, got batch err=%v", got.err)
	}
	if got.res == nil || got.res.Results[0].Status != StatusStorage {
		t.Fatalf("in-flight payment want storage_error item result, got %+v err=%v", got.res, got.err)
	}

	// 旧句柄已关闭；新句柄看到回滚后的完整状态。
	assertClosedHandle(t, l1)
	if bal, _ := l2.Balance("aa", "usdc"); bal != 100 {
		t.Fatalf("new handle balance after failed save=%d want 100", bal)
	}
	snap, _ := l2.Query()
	if len(snap.Settlements) != 0 {
		t.Fatalf("failed payment must leave no success record: %+v", snap.Settlements)
	}
	if len(snap.Refunds) != 0 {
		t.Fatalf("failed payment must leave no refunds: %+v", snap.Refunds)
	}
	// 磁盘同样保持旧状态：余额 100、无记录。
	if bal, n := persistedBalanceAndCount(t, path, "aa", "usdc"); bal != 100 || n != 0 {
		t.Fatalf("persisted state after failed save: bal=%d records=%d, want 100/0", bal, n)
	}

	// 原失败编号仍可用于合法付款：付 60 首次成功（不是 duplicate），
	// 序号从 1 开始，余额变为 40。
	retry, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 60),
	}})
	if err != nil {
		t.Fatal(err)
	}
	rr := retry.Results[0]
	if rr.Status != StatusSettled || rr.Record == nil {
		t.Fatalf("reuse of failed id want first-time settled, got %+v", rr)
	}
	if rr.Record.Seq != 1 || rr.Record.Amount != 60 || rr.Record.Charged != 60 {
		t.Fatalf("retry settlement record: %+v", rr.Record)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 40 {
		t.Fatalf("balance after retry=%d want 40", bal)
	}
	// 持久化恰发生两次：失败的 70 一次、成功的 60 一次；关闭从不写盘。
	if n := hook.calls.Load(); n != 2 {
		t.Fatalf("persist calls=%d want 2", n)
	}

	// 全部关闭后从磁盘重开：不残留失败付款的扣款或去重资格。
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	l3 := openOrFail(t, path)
	if bal, _ := l3.Balance("aa", "usdc"); bal != 40 {
		t.Fatalf("disk-reopened balance=%d want 40", bal)
	}
	snap3, _ := l3.Query()
	if len(snap3.Settlements) != 1 {
		t.Fatalf("disk-reopened history=%+v want exactly one record", snap3.Settlements)
	}
	if sr := snap3.Settlements[0]; sr.ID != "p1" || sr.Amount != 60 || sr.Seq != 1 {
		t.Fatalf("disk-reopened record: %+v", sr)
	}
	// 已落盘的 60 付款：重提 duplicate；改成 70 conflict；编号资格不复活。
	dup, _ := l3.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 60),
	}})
	if dup.Results[0].Status != StatusDuplicate || dup.Results[0].Record.Seq != 1 {
		t.Fatalf("duplicate after disk reopen: %+v", dup.Results[0])
	}
	cf, _ := l3.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}})
	if cf.Results[0].Status != StatusConflict {
		t.Fatalf("changed retry after disk reopen want conflict, got %+v", cf.Results[0])
	}
	if bal, _ := l3.Balance("aa", "usdc"); bal != 40 {
		t.Fatalf("balance after duplicate/conflict=%d want 40", bal)
	}
}
