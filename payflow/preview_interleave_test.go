package payflow

import (
	"bytes"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

// 本文件为“真实批次保存期间，另一句柄发起预览”的交错使用提供回归保障：
// 同一进程内两个句柄指向同一本账本，一个句柄正在提交付款批次，另一个句柄
// 发起预览。预览必须在 reg.mu 上等待整批处理完毕，再基于完整余额与成功
// 历史逐项判断——既不能把尚在保存的付款当成已结算（duplicate），也不能
// 拿批次中途（前项已扣、后项暂存）的余额做整份预览，更不能继承真实批次
// 某项的 storage_error。
//
// 场景（一个账户 aa、一种资产 usdc，初始余额 100，费率 0）：
//   - 句柄 A 顺序真实提交 p1=60、p2=30；
//   - 在该批次仍在保存（p1 已落账、p2 已暂存但保存结果未定）时，句柄 B
//     发起预览，依次放入与原请求完全相同的 p1、p2，以及新编号 p3=15。
//
// 两种结局各测一遍：p2 保存成功 / p2 保存失败回滚。

// interleaveGateHook 给每一次持久化调用装一道门：FailNextPersist 在门后
// 阻塞，直到测试显式放行；failNth>0 时第 failNth 次（1 起）放行后返回
// 存储失败，其余成功。用于把真实批次精确停在“某一项正在保存”的时点。
type interleaveGateHook struct {
	mu      sync.Mutex
	calls   int
	started []chan struct{} // 第 n 次持久化已进入门内
	proceed []chan struct{} // 关闭即放行第 n 次持久化
	failNth int
}

// newInterleaveGateHook 预安装 n 道门（真实批次有 n 项预计成功就会持久化 n 次）。
func newInterleaveGateHook(n, failNth int) *interleaveGateHook {
	h := &interleaveGateHook{failNth: failNth}
	for i := 0; i < n; i++ {
		h.started = append(h.started, make(chan struct{}))
		h.proceed = append(h.proceed, make(chan struct{}))
	}
	return h
}

func (h *interleaveGateHook) waitStarted(t *testing.T, n int) {
	t.Helper()
	select {
	case <-h.started[n]:
	case <-time.After(5 * time.Second):
		t.Fatalf("persist #%d never reached the gate", n+1)
	}
}

func (h *interleaveGateHook) release(t *testing.T, n int) {
	t.Helper()
	close(h.proceed[n])
}

func (h *interleaveGateHook) FailNextPersist() bool {
	h.mu.Lock()
	n := h.calls
	h.calls++
	h.mu.Unlock()

	// 预安装的门只覆盖交错窗口内的持久化；窗口之后（如失败项重试）的
	// 额外持久化不再拦截，直接成功。
	if n >= len(h.started) {
		return false
	}
	close(h.started[n])
	<-h.proceed[n] // 整批在此期间持续持有 reg.mu
	return h.failNth == n+1
}

// TestPreviewWaitsForInFlightSubmitBatch 锁定交错语义：预览必须等到在途
// 真实批次整体结束后，基于该批次自身完成后的完整状态逐项给出结果。
func TestPreviewWaitsForInFlightSubmitBatch(t *testing.T) {
	previewBatch := FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 60),
		intent("p2", "aa", "usdc", 30),
		intent("p3", "aa", "usdc", 15),
	}}
	realBatch := FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		previewBatch.Intents[0],
		previewBatch.Intents[1],
	}}

	cases := []struct {
		name   string
		failP2 bool
	}{
		{"both real payments succeed", false},
		{"second real payment fails on storage", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ---- 交错使用的账本：句柄 A 提交，句柄 B 预览 ----
			path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
			lSubmit := openOrFail(t, path)
			lPreview := openOrFail(t, path)

			hook := newInterleaveGateHook(len(realBatch.Intents), 0)
			if tc.failP2 {
				hook.failNth = 2 // 第 1 次（p1）成功，第 2 次（p2）保存失败
			}
			SetFailHook(lSubmit, hook)
			t.Cleanup(func() { SetFailHook(lSubmit, nil) })

			type batchOutcome struct {
				res *BatchResult
				err error
			}
			submitDone := make(chan batchOutcome, 1)
			go func() {
				res, err := lSubmit.Submit(realBatch)
				submitDone <- batchOutcome{res, err}
			}()

			// p1 的持久化停在门内：整批持有 reg.mu，p1 已暂存（余 40、
			// 记录已追加）但批次尚未结束。
			hook.waitStarted(t, 0)

			// 此时句柄 B 发起预览：它只能在 reg.mu 前排队等待。
			type previewOutcome struct {
				res *PreviewResult
				err error
			}
			previewDone := make(chan previewOutcome, 1)
			go func() {
				res, err := lPreview.Preview(previewBatch)
				previewDone <- previewOutcome{res, err}
			}()

			// 放行 p1：p1 落账成功；随后 p2 的持久化进入第二道门——此时
			// p1 完整生效（余 40）、p2 已暂存（余 10、记录已追加）但保存
			// 结果未定，批次仍持有锁。
			hook.release(t, 0)
			hook.waitStarted(t, 1)

			// 批次仍在保存：预览绝不能已基于中途状态跑完。
			select {
			case <-previewDone:
				t.Fatalf("preview ran against mid-batch state instead of waiting for the batch")
			default:
			}

			// 放行 p2 的保存（成功，或失败回滚）：批次到此整体结束，锁释放，
			// 排队中的预览才在完整状态上运行。
			hook.release(t, 1)

			sub := <-submitDone
			if sub.err != nil {
				t.Fatalf("real submit: %v", sub.err)
			}
			pv := <-previewDone
			if pv.err != nil {
				t.Fatalf("preview: %v", pv.err)
			}

			// 预览结果逐项对应三个输入，且恒带 dry_run:true。
			if !pv.res.DryRun {
				t.Fatalf("preview must carry dry_run=true")
			}
			if len(pv.res.Results) != 3 {
				t.Fatalf("preview results=%d want 3 (one per input)", len(pv.res.Results))
			}
			for i, wantID := range []string{"p1", "p2", "p3"} {
				if pv.res.Results[i].ID != wantID {
					t.Fatalf("preview item %d id=%q want %q", i, pv.res.Results[i].ID, wantID)
				}
			}

			// ---- 参照账本：只跑同一真实批次、不做预览，作为“批次自身完成后”
			//      的余额、历史与账本文件基准 ----
			refPath := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
			lRef := openOrFail(t, refPath)
			if tc.failP2 {
				SetFailHook(lRef, &failNth{nth: 2})
				t.Cleanup(func() { SetFailHook(lRef, nil) })
			}
			refBatch, err := lRef.Submit(realBatch)
			if err != nil {
				t.Fatalf("reference submit: %v", err)
			}

			if tc.failP2 {
				// 真实批次：p1 成功保留、p2 报告 storage_error 并回滚。
				if got := statuses(sub.res); !reflect.DeepEqual(got, []string{StatusSettled, StatusStorage}) {
					t.Fatalf("real statuses=%v want [settled storage_error]", got)
				}
				if got := statuses(refBatch); !reflect.DeepEqual(got, []string{StatusSettled, StatusStorage}) {
					t.Fatalf("reference statuses=%v want [settled storage_error]", got)
				}
				p1 := sub.res.Results[0]
				if p1.Record == nil || p1.Record.Seq != 1 || p1.Record.Amount != 60 || p1.Record.Charged != 60 {
					t.Fatalf("p1 real record: %+v", p1.Record)
				}
				if bal, _ := lSubmit.Balance("aa", "usdc"); bal != 40 {
					t.Fatalf("balance after failed p2=%d want 40", bal)
				}

				// 预览：p1 仍是 duplicate 并携带原结算记录；保存失败的真实
				// p2 不能被当成已结算（不是 duplicate），预览也不继承存储
				// 错误，而是基于完整余额 40 预计成功、预计成功序号 2；
				// p3 因模拟余额只剩 10 而不足。
				want := []string{StatusDuplicate, StatusSettled, StatusFunds}
				if got := previewStatuses(pv.res); !reflect.DeepEqual(got, want) {
					t.Fatalf("preview statuses=%v want %v", got, want)
				}
				if !reflect.DeepEqual(pv.res.Results[0].Record, p1.Record) {
					t.Fatalf("preview p1 must carry p1's original record:\n got %+v\nwant %+v",
						pv.res.Results[0].Record, p1.Record)
				}
				pr2 := pv.res.Results[1]
				if pr2.Record == nil || pr2.Record.Seq != 2 || pr2.Record.Amount != 30 || pr2.Record.Charged != 30 {
					t.Fatalf("predicted p2 record: %+v want settled seq 2 amount/charged 30", pr2.Record)
				}
				if pv.res.Results[2].Record != nil {
					t.Fatalf("insufficient p3 must not carry a record: %+v", pv.res.Results[2])
				}

				// 预览结束后的余额、付款历史、账本文件等于真实批次自身完成后的结果。
				assertStateAndFileEqual(t, path, lSubmit, refPath, lRef)

				// 预览不预留编号与序号：保存失败的 p2 仍可真实提交成功
				// （不是 duplicate，成功序号 2），余额变 10；随后真实 p3=15
				// 因余额不足失败——预览中的预计成功没有让 p2 变成 duplicate。
				retry, err := lSubmit.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
					intent("p2", "aa", "usdc", 30),
				}})
				if err != nil {
					t.Fatalf("p2 retry: %v", err)
				}
				r2 := retry.Results[0]
				if r2.Status != StatusSettled || r2.Record == nil || r2.Record.Seq != 2 {
					t.Fatalf("storage-failed p2 must really settle as seq 2: %+v", r2)
				}
				if bal, _ := lSubmit.Balance("aa", "usdc"); bal != 10 {
					t.Fatalf("balance after p2 retry=%d want 10", bal)
				}
				after, err := lSubmit.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
					intent("p3", "aa", "usdc", 15),
				}})
				if err != nil {
					t.Fatalf("p3 submit: %v", err)
				}
				if after.Results[0].Status != StatusFunds {
					t.Fatalf("p3 after real p2 want insufficient_balance, got %+v", after.Results[0])
				}
			} else {
				// 真实批次：两笔都成功，余 10。
				if got := statuses(sub.res); !reflect.DeepEqual(got, []string{StatusSettled, StatusSettled}) {
					t.Fatalf("real statuses=%v want [settled settled]", got)
				}
				p1, p2 := sub.res.Results[0], sub.res.Results[1]
				if p1.Record == nil || p1.Record.Seq != 1 || p1.Record.Amount != 60 || p1.Record.Charged != 60 {
					t.Fatalf("p1 real record: %+v", p1.Record)
				}
				if p2.Record == nil || p2.Record.Seq != 2 || p2.Record.Amount != 30 || p2.Record.Charged != 30 {
					t.Fatalf("p2 real record: %+v", p2.Record)
				}
				if bal, _ := lSubmit.Balance("aa", "usdc"); bal != 10 {
					t.Fatalf("balance after batch=%d want 10", bal)
				}

				// 预览：p1、p2 均为 duplicate 并各自携带原结算记录；
				// p3 基于完整余额 10 判断为不足，绝不携带记录。
				want := []string{StatusDuplicate, StatusDuplicate, StatusFunds}
				if got := previewStatuses(pv.res); !reflect.DeepEqual(got, want) {
					t.Fatalf("preview statuses=%v want %v", got, want)
				}
				if !reflect.DeepEqual(pv.res.Results[0].Record, p1.Record) {
					t.Fatalf("preview p1 record:\n got %+v\nwant %+v", pv.res.Results[0].Record, p1.Record)
				}
				if !reflect.DeepEqual(pv.res.Results[1].Record, p2.Record) {
					t.Fatalf("preview p2 record:\n got %+v\nwant %+v", pv.res.Results[1].Record, p2.Record)
				}
				if pv.res.Results[2].Record != nil {
					t.Fatalf("insufficient p3 must not carry a record: %+v", pv.res.Results[2])
				}

				// 预览结束后的余额、付款历史、账本文件等于真实批次自身完成后的结果。
				assertStateAndFileEqual(t, path, lSubmit, refPath, lRef)

				// 预览未预留任何编号与余额：新编号 p3=15 在真实余额 10 上仍不足。
				after, err := lSubmit.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
					intent("p3", "aa", "usdc", 15),
				}})
				if err != nil {
					t.Fatalf("p3 submit: %v", err)
				}
				if after.Results[0].Status != StatusFunds {
					t.Fatalf("p3 want insufficient_balance, got %+v", after.Results[0])
				}
				if bal, _ := lSubmit.Balance("aa", "usdc"); bal != 10 {
					t.Fatalf("preview must not reserve balance: bal=%d want 10", bal)
				}
			}

			// 两种情况下，预览都不留下预测成功的付款记录。
			snap, err := lSubmit.Query()
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range snap.Settlements {
				if r.ID == "p3" {
					t.Fatalf("predicted p3 must not be persisted: %+v", snap.Settlements)
				}
			}
		})
	}
}

// assertStateAndFileEqual 断言跑过“真实批次 + 交错预览”的账本，其余额、
// 付款历史与账本文件与只跑过同一真实批次的参照账本逐字一致：预览不扣
// 余额、不留预测记录、不预留编号和序号，也不额外改写文件。
func assertStateAndFileEqual(t *testing.T, path string, l *Ledger, refPath string, lRef *Ledger) {
	t.Helper()

	got, err := l.Query()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	want, err := lRef.Query()
	if err != nil {
		t.Fatalf("reference query: %v", err)
	}
	assertSnap(t, got, want, "interleaved ledger vs batch-only reference")

	gotBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("ledger file differs from the batch-only result:\ngot  %s\nwant %s", gotBytes, wantBytes)
	}
}
