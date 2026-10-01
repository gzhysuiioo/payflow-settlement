package payflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func i64p(v int64) *int64 { return &v }

func intent(id, account, asset string, amount int64) PaymentIntent {
	return PaymentIntent{ID: id, Account: account, Paymaster: "pm", Asset: asset, Amount: i64p(amount), Nonce: 1, State: "pending"}
}

func newTestLedger(t *testing.T, init []BalanceInit) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := CreateLedger(path, init); err != nil {
		t.Fatalf("CreateLedger: %v", err)
	}
	return path
}

func openOrFail(t *testing.T, path string) *Ledger {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func statuses(r *BatchResult) []string {
	out := make([]string, len(r.Results))
	for i, x := range r.Results {
		out[i] = x.Status
	}
	return out
}

// ---- 初始化校验 ----

func TestCreateValidation(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		init []BalanceInit
	}{
		{"empty account", []BalanceInit{{Account: "", Asset: "usdc", Balance: 1}}},
		{"empty asset", []BalanceInit{{Account: "aa", Asset: "", Balance: 1}}},
		{"negative balance", []BalanceInit{{Account: "aa", Asset: "usdc", Balance: -1}}},
		{"duplicate entry", []BalanceInit{
			{Account: "aa", Asset: "usdc", Balance: 1},
			{Account: "aa", Asset: "usdc", Balance: 2},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			err := CreateLedger(path, tc.init)
			if err == nil || KindOf(err) != ErrInvalid {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("illegal init must not create a file, stat err=%v", statErr)
			}
		})
	}

	// 合法初始化：0 与 MaxInt64 都是合法非负余额。
	path := filepath.Join(dir, "ok.json")
	if err := CreateLedger(path, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 0},
		{Account: "aa", Asset: "eth", Balance: math.MaxInt64},
	}); err != nil {
		t.Fatalf("valid init: %v", err)
	}

	// 重复初始化报错，不覆盖。
	err := CreateLedger(path, []BalanceInit{{Account: "bb", Asset: "usdc", Balance: 1}})
	if err == nil || KindOf(err) != ErrExists {
		t.Fatalf("re-init want ErrExists, got %v", err)
	}
	l := openOrFail(t, path)
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("history overwritten: aa/usdc=%d", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 0 {
		t.Fatalf("re-init leaked new account, bb/usdc=%d", bal)
	}
}

func TestOpenUninitializedAndMissing(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope.json")); KindOf(err) != ErrNotInit {
		t.Fatalf("want ErrNotInit, got %v", err)
	}
}

// ---- 手续费 ----

func TestFeeFor(t *testing.T) {
	cases := []struct {
		amount int64
		bps    int
		want   int64
	}{
		{1, 1, 0}, // 1*1/10000 向下取整 = 0
		{10000, 1, 1},
		{1500, 30, 4}, // 45000/10000 = 4.5 -> 4
		{900, 30, 2},  // 27000/10000 = 2.7 -> 2
		{2000, 0, 0},
		{2000, 10000, 2000},
		{9999, 9999, 9998}, // 99980001/10000 = 9998.0001
		{math.MaxInt64, 1, math.MaxInt64 / 10000}, // 大数不溢出
	}
	for _, tc := range cases {
		if got := feeFor(tc.amount, tc.bps); got != tc.want {
			t.Errorf("feeFor(%d,%d)=%d want %d", tc.amount, tc.bps, got, tc.want)
		}
	}
}

// ---- 批次逐项结果与顺序语义 ----

func TestBatchOrderedIndependentResults(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 1500), // 扣 1504，成功，余 496
		intent("p2", "aa", "usdc", 900),  // 需 902，不足
		intent("p3", "aa", "usdc", 400),  // 需 401，成功，余 95
		intent("p4", "zz", "usdc", 1),    // 账户/资产不存在，按不足处理
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusFunds, StatusSettled, StatusFunds}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 95 {
		t.Fatalf("balance=%d want 95", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 2 || snap.Settlements[0].ID != "p1" || snap.Settlements[1].ID != "p3" {
		t.Fatalf("settlements not in success order: %+v", snap.Settlements)
	}
}

func TestEmptyBatch(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10}})
	l := openOrFail(t, path)
	res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 0 {
		t.Fatalf("empty batch must return empty results, got %+v", res.Results)
	}
}

func TestFeeBpsValidation(t *testing.T) {
	path := newTestLedger(t, nil)
	l := openOrFail(t, path)
	for _, bps := range []int{-1, 10001} {
		if _, err := l.Submit(FeeBatch{FeeBps: bps, Intents: []PaymentIntent{intent("x", "a", "b", 1)}}); KindOf(err) != ErrInvalid {
			t.Fatalf("bps=%d want ErrInvalid, got %v", bps, err)
		}
	}
}

func TestInvalidItems(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: math.MaxInt64}})
	l := openOrFail(t, path)

	neg := int64(-5)
	zero := int64(0)
	batch := FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		{ID: "", Account: "aa", Asset: "usdc", Amount: i64p(1)},
		{ID: "a", Account: "", Asset: "usdc", Amount: i64p(1)},
		{ID: "b", Account: "aa", Asset: "", Amount: i64p(1)},
		{ID: "c", Account: "aa", Asset: "usdc", Amount: nil},
		{ID: "d", Account: "aa", Asset: "usdc", Amount: &zero},
		{ID: "e", Account: "aa", Asset: "usdc", Amount: &neg},
	}}
	res, err := l.Submit(batch)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res.Results {
		if r.Status != StatusInvalid {
			t.Fatalf("item %d want invalid_parameter, got %s (%s)", i, r.Status, r.Reason)
		}
	}
	// 未成功编号不占去重资格：用其中一个编号发起合法付款应成功。
	res2, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("c", "aa", "usdc", 1)}})
	if res2.Results[0].Status != StatusSettled {
		t.Fatalf("previously-invalid id should be reusable, got %s", res2.Results[0].Status)
	}
}

func TestStateErrorDistinct(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	it := intent("p", "aa", "usdc", 1)
	it.State = "failed"
	res, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{it}})
	if res.Results[0].Status != StatusState {
		t.Fatalf("want state_error, got %s", res.Results[0].Status)
	}
	// 状态错误的编号之后可重新提交成功。
	it.State = "pending"
	res, _ = l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{it}})
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("retry after state_error want settled, got %s", res.Results[0].Status)
	}
}

func TestOverflowRejectedWithoutNegativeBalance(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: math.MaxInt64}})
	l := openOrFail(t, path)
	res, _ := l.Submit(FeeBatch{FeeBps: 1, Intents: []PaymentIntent{
		intent("big", "aa", "usdc", math.MaxInt64),
	}})
	if res.Results[0].Status != StatusInvalid || !strings.Contains(res.Results[0].Reason, "overflows") {
		t.Fatalf("want overflow invalid_parameter, got %+v", res.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != math.MaxInt64 {
		t.Fatalf("overflow changed balance: %d", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 0 {
		t.Fatalf("overflow left a success record: %+v", snap.Settlements)
	}
}

func TestBpsBoundaries(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 2000}})
	l := openOrFail(t, path)
	res, _ := l.Submit(FeeBatch{FeeBps: 10000, Intents: []PaymentIntent{intent("p", "aa", "usdc", 500)}})
	r := res.Results[0]
	if r.Status != StatusSettled || r.Record.Fee != 500 || r.Record.Charged != 1000 {
		t.Fatalf("bps=10000: %+v", r)
	}
}

// ---- 幂等与冲突 ----

func TestIdempotencyAndConflict(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openOrFail(t, path)
	base := intent("p1", "aa", "usdc", 1500)
	base.Paymaster = "pm-1"
	base.Nonce = 7

	first, _ := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{base}})
	if first.Results[0].Status != StatusSettled {
		t.Fatalf("first: %+v", first.Results[0])
	}
	balAfterFirst, _ := l.Balance("aa", "usdc")

	// 完全相同（含费率）→ 幂等，返回原记录，不再扣款。
	again, _ := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{base}})
	a := again.Results[0]
	if a.Status != StatusDuplicate || a.Record == nil || a.Record.Seq != 1 {
		t.Fatalf("identical retry want duplicate with original record, got %+v", a)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfterFirst {
		t.Fatalf("duplicate charged again: %d vs %d", bal, balAfterFirst)
	}

	// 任一付款字段或费率不同 → 冲突，原记录保持不变。
	mutations := []func(*PaymentIntent, *int){
		func(it *PaymentIntent, bps *int) { it.Account = "other" },
		func(it *PaymentIntent, bps *int) { it.Paymaster = "pm-2" },
		func(it *PaymentIntent, bps *int) { it.Asset = "eth" },
		func(it *PaymentIntent, bps *int) { *it.Amount = 1501 },
		func(it *PaymentIntent, bps *int) { it.Nonce = 8 },
		func(it *PaymentIntent, bps *int) { *bps = 31 },
	}
	for i, mut := range mutations {
		it := base
		bps := 30
		mut(&it, &bps)
		res, _ := l.Submit(FeeBatch{FeeBps: bps, Intents: []PaymentIntent{it}})
		if res.Results[0].Status != StatusConflict {
			t.Fatalf("mutation %d want conflict, got %+v", i, res.Results[0])
		}
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 1 || snap.Settlements[0].Amount != 1500 || snap.Settlements[0].FeeBps != 30 {
		t.Fatalf("original record changed after conflicts: %+v", snap.Settlements)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfterFirst {
		t.Fatalf("conflict changed balance: %d vs %d", bal, balAfterFirst)
	}
}

func TestSameBatchRepeatedID(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openOrFail(t, path)
	it := intent("p", "aa", "usdc", 100)

	// 同批次内完全相同的重复编号：首项成功，第二项幂等。
	res, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{it, it}})
	if res.Results[0].Status != StatusSettled || res.Results[1].Status != StatusDuplicate {
		t.Fatalf("identical in-batch repeat: %+v", res.Results)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1_000_000-100 {
		t.Fatalf("duplicate in batch charged twice: %d", bal)
	}

	// 同批次内字段不同：冲突。
	diff := it
	*diff.Amount = 101
	res, _ = l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("q", "aa", "usdc", 100),
		diff, // q 的冲突请求
	}})
	if res.Results[1].Status != StatusConflict {
		t.Fatalf("in-batch conflicting repeat want conflict, got %+v", res.Results[1])
	}
}

func TestFailedIDRetryThenSucceeds(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	// 首次余额不足。
	res, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 200)}})
	if res.Results[0].Status != StatusFunds {
		t.Fatalf("want funds, got %s", res.Results[0].Status)
	}
	// 重新提交更小金额成功。
	res, _ = l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 50)}})
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("retry want settled, got %s", res.Results[0].Status)
	}
}

// ---- 持久化与重开 ----

func TestPersistenceAcrossHandlesAndReopen(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})

	l1 := openOrFail(t, path)
	res, _ := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 300)}})
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("settle: %+v", res.Results[0])
	}

	// 同进程内再次 Open 同一路径，必须看到同一本账本的即时状态（共享而非重读）。
	l2 := openOrFail(t, path)
	if bal, _ := l2.Balance("aa", "usdc"); bal != 700 {
		t.Fatalf("second handle sees stale balance %d", bal)
	}

	// 关闭全部句柄，模拟程序重启后从磁盘恢复。
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	l3 := openOrFail(t, path)
	if bal, _ := l3.Balance("aa", "usdc"); bal != 700 {
		t.Fatalf("after reopen balance=%d want 700", bal)
	}
	snap, _ := l3.Query()
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p" || snap.Settlements[0].Charged != 300 {
		t.Fatalf("after reopen records=%+v", snap.Settlements)
	}
}

func TestPersistedFormatRoundTrip(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10}})
	l := openOrFail(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 50, Intents: []PaymentIntent{intent("p", "aa", "usdc", 10)}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc signedDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("on-disk JSON invalid: %v\n%s", err, raw)
	}
	if doc.Version != 1 {
		t.Fatalf("version=%d", doc.Version)
	}
	if len(doc.Initial) != 1 || doc.Initial[0].Balance != 10 {
		t.Fatalf("initial balances on disk: %+v", doc.Initial)
	}
	if len(doc.Balances) != 1 || doc.Balances[0].Balance != 0 || len(doc.Settlements) != 1 {
		t.Fatalf("unexpected disk content: %s", raw)
	}
	if doc.Checksum == "" {
		t.Fatalf("missing checksum: %s", raw)
	}
	if doc.Settlements[0].Fee != 0 { // 10*50/10000 = 0
		t.Fatalf("fee on disk=%d", doc.Settlements[0].Fee)
	}
	// 文件必须是单个完整 JSON 文档 + 换行，无残留临时文件。
	if !strings.HasSuffix(string(raw), "}\n") {
		t.Fatalf("file does not end cleanly: %q", raw[len(raw)-5:])
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".ledger-*.tmp")); len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}

	// 任一字节被篡改（即使记录与余额仍自洽）都必须被 checksum 拒绝。
	tampered := bytes.Clone(raw)
	tampered[10] ^= 0x01
	tpath := filepath.Join(filepath.Dir(path), "tampered.json")
	if err := os.WriteFile(tpath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(tpath); KindOf(err) != ErrCorrupt {
		t.Fatalf("bit-flipped file want ErrCorrupt, got %v", err)
	}
}

// ---- 损坏检测 ----

func TestCorruptLedgerRejected(t *testing.T) {
	good := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, good)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 300)}}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(good)
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"garbage":     write("garbage.json", []byte(`garbage{`)),
		"empty":       write("empty.json", nil),
		"no checksum": write("nochecksum.json", []byte(`{"version":1,"initial_balances":[],"balances":[],"settlements":[]}`)),
		"truncated":   write("trunc.json", raw[:len(raw)/2]),
		"appended":    write("appended.json", append(bytes.Clone(raw), []byte("{}")...)),
		"bitflip": func() string {
			b := bytes.Clone(raw)
			b[12] ^= 0x01
			return write("bitflip.json", b)
		}(),
		"semantic edit": write("edited.json",
			[]byte(strings.ReplaceAll(string(raw), `"balance":700`, `"balance":999`))),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Open(p); KindOf(err) != ErrCorrupt {
				t.Fatalf("%s: want ErrCorrupt, got %v", name, err)
			}
		})
	}

	// 损坏文件不能通过再次初始化“重开一本空账”。
	if err := CreateLedger(cases["truncated"], nil); KindOf(err) != ErrExists {
		t.Fatalf("init over corrupt file must report ErrExists, got %v", err)
	}
}

// 直接对重放校验器构造状态，覆盖各种语义损坏（绕过 checksum）。
func TestValidateAndReplayRejectsSemanticCorruption(t *testing.T) {
	k := balanceKey{"aa", "usdc"}
	initial := map[balanceKey]int64{k: 1000}
	final := map[balanceKey]int64{k: 700}
	good := Record{ID: "p", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 300, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 300, Seq: 1}

	mk := func(mut func(*Record)) []Record {
		r := good
		mut(&r)
		return []Record{r}
	}

	bad := map[string]struct {
		initial map[balanceKey]int64
		final   map[balanceKey]int64
		records []Record
	}{
		"seq gap":          {initial, final, mk(func(r *Record) { r.Seq = 2 })},
		"fee mismatch":     {initial, final, mk(func(r *Record) { r.Fee = 5; r.Charged = 305 })},
		"charged mismatch": {initial, final, mk(func(r *Record) { r.Charged = 301 })},
		"bad bps":          {initial, final, mk(func(r *Record) { r.FeeBps = 10001 })},
		"empty account":    {initial, final, mk(func(r *Record) { r.Account = "" })},
		"unknown combo":    {initial, final, mk(func(r *Record) { r.Asset = "eth" })},
		"drives negative":  {initial, map[balanceKey]int64{k: 600}, mk(func(r *Record) {})}, // 扣款 300 但末态写 600
		"final too high":   {initial, map[balanceKey]int64{k: 800}, mk(func(r *Record) {})},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			if err := validateAndReplay(tc.initial, tc.final, tc.records); err == nil || KindOf(err) != ErrCorrupt {
				t.Fatalf("%s: want ErrCorrupt, got %v", name, err)
			}
		})
	}

	t.Run("duplicate id", func(t *testing.T) {
		err := validateAndReplay(
			map[balanceKey]int64{k: 1000},
			map[balanceKey]int64{k: 400},
			[]Record{good, func() Record { r := good; r.Seq = 2; return r }()},
		)
		if err == nil || KindOf(err) != ErrCorrupt {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})

	// 健康状态必须通过。
	if err := validateAndReplay(initial, final, []Record{good}); err != nil {
		t.Fatalf("healthy state rejected: %v", err)
	}
}

// ---- 存储故障：回滚该项，前项保留 ----

type failOnce struct{ remaining int }

func (f *failOnce) FailNextPersist() bool {
	if f.remaining > 0 {
		f.remaining--
		return true
	}
	return false
}

// failNth 在第 nth 次（1 起）持久化时失败，其余成功。
type failNth struct {
	nth   int
	calls int
}

func (f *failNth) FailNextPersist() bool {
	f.calls++
	return f.calls == f.nth
}

func TestStorageFailureRollsBackItemOnly(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)
	SetFailHook(l, &failNth{nth: 2}) // ok1 落盘成功，boom 落盘失败，ok2 再次成功
	t.Cleanup(func() { SetFailHook(l, nil) })

	res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("ok1", "aa", "usdc", 100),  // 成功
		intent("boom", "aa", "usdc", 100), // 保存失败 → 回滚该项
		intent("ok2", "aa", "usdc", 100),  // 注入只失败一次，后续成功
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusStorage, StatusSettled}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 800 {
		t.Fatalf("balance=%d want 800 (前项与后项保留，失败项回滚)", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 2 || snap.Settlements[0].ID != "ok1" || snap.Settlements[1].ID != "ok2" {
		t.Fatalf("records after storage failure: %+v", snap.Settlements)
	}

	// 失败编号不占去重资格，可以重新提交成功。
	res2, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("boom", "aa", "usdc", 100)}})
	if res2.Results[0].Status != StatusSettled {
		t.Fatalf("failed-on-storage id retry: %+v", res2.Results[0])
	}

	// 关闭重开：磁盘状态与内存一致，失败项不留痕。
	l2 := openOrFail(t, path)
	snap2, _ := l2.Query()
	if len(snap2.Settlements) != 3 {
		t.Fatalf("after reopen want 3 records, got %+v", snap2.Settlements)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 700 {
		t.Fatalf("after reopen balance=%d want 700", bal)
	}
}

func TestStorageFailureLeavesOldFileIntact(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	SetFailHook(l, &failOnce{remaining: 1})
	res, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p", "aa", "usdc", 50)}})
	if res.Results[0].Status != StatusStorage {
		t.Fatalf("want storage_error, got %s", res.Results[0].Status)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("failed persist must leave the old file byte-intact")
	}
}

// ---- 并发：同路径共享、同编号唯一、争用余额只成一笔 ----

func TestConcurrentSameIDSettlesOnce(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})

	const n = 40
	var wg sync.WaitGroup
	counts := map[string]int{}
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			l, err := Open(path) // 每次都重新 Open 同一路径
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer l.Close()
			res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("only", "aa", "usdc", 1000)}})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			mu.Lock()
			counts[res.Results[0].Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if counts[StatusSettled] != 1 {
		t.Fatalf("settled=%d want 1 (counts=%v)", counts[StatusSettled], counts)
	}
	if counts[StatusDuplicate] != n-1 {
		t.Fatalf("duplicate=%d want %d (counts=%v)", counts[StatusDuplicate], n-1, counts)
	}
	l := openOrFail(t, path)
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("balance=%d want 0", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 1 {
		t.Fatalf("records=%d want 1", len(snap.Settlements))
	}
}

func TestConcurrentContendedBalanceOnlyOneWins(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})

	const n = 40
	var wg sync.WaitGroup
	counts := map[string]int{}
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			l := openOrFail(t, path)
			res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
				intent(fmt.Sprintf("p-%d", i), "aa", "usdc", 1000),
			}})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			mu.Lock()
			counts[res.Results[0].Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if counts[StatusSettled] != 1 {
		t.Fatalf("settled=%d want exactly 1 (counts=%v)", counts[StatusSettled], counts)
	}
	if counts[StatusFunds] != n-1 {
		t.Fatalf("insufficient=%d want %d (counts=%v)", counts[StatusFunds], n-1, counts)
	}
}

// ---- 既有 API 仍可用 ----

func TestLegacyExecuteBranches(t *testing.T) {
	// 非 pending。
	if s := Execute(Intent{ID: "a", State: "done"}, 0, map[string]bool{}, 10); s.Status != "rejected" || s.Reason != "intent is not pending" {
		t.Fatalf("not pending: %+v", s)
	}
	// 已花费编号。
	if s := Execute(Intent{ID: "a", State: "pending"}, 0, map[string]bool{"a": true}, 10); s.Status != "rejected" || s.Reason != "duplicate settlement attempt" {
		t.Fatalf("spent: %+v", s)
	}
	// 余额不足（amount=101 > balance=100，bps=0）。
	if s := Execute(Intent{ID: "a", State: "pending", Amount: 101}, 0, map[string]bool{}, 100); s.Status != "failed" {
		t.Fatalf("insufficient: %+v", s)
	}
	// 成功时费用与扣款正确并标记 spent。
	spent := map[string]bool{}
	s := Execute(Intent{ID: "a", State: "pending", Amount: 10000}, 10, spent, 20000)
	if s.Status != "settled" || s.Charged != 10010 || s.FeeBps != 10 || s.Ref != "settle:a" || !spent["a"] {
		t.Fatalf("settle: %+v spent=%v", s, spent)
	}
}

func TestLegacyExecuteAndReconcile(t *testing.T) {
	spent := map[string]bool{}
	s := Execute(Intent{ID: "x", State: "pending", Amount: 100}, 0, spent, 100)
	if s.Status != "settled" {
		t.Fatalf("legacy execute: %+v", s)
	}
	if gaps := Reconcile(
		[]Intent{{ID: "x"}, {ID: "missing"}},
		[]Settlement{{Intent: "x", Status: "settled"}},
	); len(gaps) != 1 || gaps[0] != "missing" {
		t.Fatalf("legacy reconcile gaps=%v", gaps)
	}
}
