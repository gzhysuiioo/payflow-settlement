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
			if err := validateAndReplay(tc.initial, tc.final, tc.records, nil); err == nil || KindOf(err) != ErrCorrupt {
				t.Fatalf("%s: want ErrCorrupt, got %v", name, err)
			}
		})
	}

	t.Run("duplicate id", func(t *testing.T) {
		err := validateAndReplay(
			map[balanceKey]int64{k: 1000},
			map[balanceKey]int64{k: 400},
			[]Record{good, func() Record { r := good; r.Seq = 2; return r }()},
			nil,
		)
		if err == nil || KindOf(err) != ErrCorrupt {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})

	// 健康状态必须通过。
	if err := validateAndReplay(initial, final, []Record{good}, nil); err != nil {
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

// TestLegacyExecuteLargeAmountNoOverflow 验证大额付款的手续费不再因中间乘积
// 溢出而算错：amount*feeBps 本身可能超过 int64，但只要最终总额可表示，
// 就按精确总额判断余额。
func TestLegacyExecuteLargeAmountNoOverflow(t *testing.T) {
	const (
		amount    = int64(4000000000000000000)
		feeBps    = 30
		wantFee   = int64(12000000000000000)
		wantTotal = amount + wantFee // 4012000000000000000
	)

	// 余额恰好等于总额：成功，扣款额精确，编号被标记。
	spent := map[string]bool{}
	s := Execute(Intent{ID: "big", State: "pending", Amount: amount}, feeBps, spent, wantTotal)
	if s.Status != "settled" {
		t.Fatalf("exact balance should settle: %+v", s)
	}
	if s.Charged != wantTotal {
		t.Fatalf("charged=%d want %d", s.Charged, wantTotal)
	}
	if s.FeeBps != feeBps || s.Intent != "big" || s.Ref != "settle:big" {
		t.Fatalf("settlement fields: %+v", s)
	}
	if !spent["big"] {
		t.Fatalf("id must be marked after success")
	}

	// 余额少一个最小单位：余额不足失败，不标记编号、不留引用、不报错扣款额。
	spent = map[string]bool{}
	s = Execute(Intent{ID: "big", State: "pending", Amount: amount}, feeBps, spent, wantTotal-1)
	if s.Status != "failed" || s.Reason != reasonFunds {
		t.Fatalf("one unit short should fail insufficient: %+v", s)
	}
	if s.Charged != 0 || s.Ref != "" {
		t.Fatalf("failure must not charge or reference: %+v", s)
	}
	if spent["big"] {
		t.Fatalf("failed id must not be marked")
	}
}

// TestLegacyExecuteTotalOverflow 验证总额超出 int64 时必须拒绝，
// 即使余额本身是 int64 最大值，也不能返回负数扣款或普通余额不足。
func TestLegacyExecuteTotalOverflow(t *testing.T) {
	spent := map[string]bool{}
	s := Execute(Intent{ID: "max", State: "pending", Amount: math.MaxInt64}, 1, spent, math.MaxInt64)
	if s.Status != "rejected" || s.Reason != reasonOverflow {
		t.Fatalf("total overflow must be rejected: %+v", s)
	}
	if s.Charged != 0 || s.Ref != "" {
		t.Fatalf("overflow rejection must not charge or reference: %+v", s)
	}
	if spent["max"] {
		t.Fatalf("rejected id must not be marked")
	}
	// 溢出拒绝后修正费率（手续费为 0），同一编号仍可作为首次付款成功。
	s = Execute(Intent{ID: "max", State: "pending", Amount: math.MaxInt64}, 0, spent, math.MaxInt64)
	if s.Status != "settled" || s.Charged != math.MaxInt64 {
		t.Fatalf("corrected request must settle as first payment: %+v", s)
	}
	if !spent["max"] {
		t.Fatalf("successful retry must mark id")
	}
}

// TestLegacyExecuteValidation 覆盖金额与费率的合法性检查及三类拒绝原因区分。
func TestLegacyExecuteValidation(t *testing.T) {
	cases := []struct {
		name   string
		amount int64
		bps    int
		reason string
	}{
		{"zero amount", 0, 0, reasonBadAmount},
		{"negative amount", -1, 0, reasonBadAmount},
		{"negative fee bps", 1, -1, "fee_bps must be within [0,10000], got -1"},
		{"fee bps above 10000", 1, 10001, "fee_bps must be within [0,10000], got 10001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spent := map[string]bool{}
			s := Execute(Intent{ID: "v", State: "pending", Amount: tc.amount}, tc.bps, spent, math.MaxInt64)
			if s.Status != "rejected" || s.Reason != tc.reason {
				t.Fatalf("got status=%s reason=%q want rejected/%q", s.Status, s.Reason, tc.reason)
			}
			if s.Charged != 0 || s.Ref != "" || spent["v"] {
				t.Fatalf("rejection must have no charge/ref/mark: %+v spent=%v", s, spent)
			}
		})
	}

	// 费率边界 0 与 10000 合法。
	for _, bps := range []int{0, 10000} {
		spent := map[string]bool{}
		if s := Execute(Intent{ID: "edge", State: "pending", Amount: 100}, bps, spent, math.MaxInt64); s.Status != "settled" {
			t.Fatalf("bps=%d must be valid: %+v", bps, s)
		}
	}
}

// TestLegacyExecutePrecedenceAndRetry 验证判断顺序与“拒绝不占用编号”。
func TestLegacyExecutePrecedenceAndRetry(t *testing.T) {
	// 非 pending 优先于金额、费率、重复检查。
	spent := map[string]bool{"a": true}
	if s := Execute(Intent{ID: "a", State: "done", Amount: 0}, 10001, spent, 0); s.Status != "rejected" || s.Reason != reasonNotPending {
		t.Fatalf("not-pending must take precedence: %+v", s)
	}
	// pending 但已结算优先于金额、费率检查。
	if s := Execute(Intent{ID: "a", State: "pending", Amount: 0}, 10001, spent, 0); s.Status != "rejected" || s.Reason != reasonDuplicate {
		t.Fatalf("duplicate must take precedence over amount/fee: %+v", s)
	}

	// 金额不合法的编号修正后作为首次付款成功（而非 duplicate）。
	fresh := map[string]bool{}
	if s := Execute(Intent{ID: "p", State: "pending", Amount: 0}, 0, fresh, 100); s.Status != "rejected" {
		t.Fatalf("zero amount rejected: %+v", s)
	}
	if s := Execute(Intent{ID: "p", State: "pending", Amount: 100}, 0, fresh, 100); s.Status != "settled" || s.Charged != 100 {
		t.Fatalf("corrected amount must settle as first payment: %+v", s)
	}

	// 余额不足失败后补足余额，同一编号仍可作为首次付款成功。
	funds := map[string]bool{}
	if s := Execute(Intent{ID: "q", State: "pending", Amount: 100}, 0, funds, 50); s.Status != "failed" {
		t.Fatalf("insufficient: %+v", s)
	}
	if s := Execute(Intent{ID: "q", State: "pending", Amount: 100}, 0, funds, 100); s.Status != "settled" || s.Charged != 100 {
		t.Fatalf("retry after top-up must settle as first payment: %+v", s)
	}
}

// ===========================================================================
// 退款（refund）测试
// ===========================================================================

func refundReq(id, sid, reason string) RefundRequest {
	return RefundRequest{ID: id, SettlementID: sid, Reason: reason}
}

// 成功全额退款：charged（含手续费）退回原账户、原资产；原结算保持不变；
// query 按退款成功顺序返回可追溯原结算的记录。
func TestRefundFullAmountIncludingFee(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1010}})
	l := openOrFail(t, path)
	res, _ := l.Submit(FeeBatch{FeeBps: 100, Intents: []PaymentIntent{
		{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: i64p(1000), Nonce: 1, State: "pending"},
	}})
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("settle: %+v", res.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("balance after settle=%d want 0", bal)
	}

	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "cancel")}})
	if err != nil {
		t.Fatal(err)
	}
	got := rr.Results[0]
	if got.Status != StatusRefundSuccess {
		t.Fatalf("status=%s want refunded: %+v", got.Status, got)
	}
	if got.SettlementID != "p1" || got.Account != "aa" || got.Asset != "usdc" || got.Charged != 1010 {
		t.Fatalf("refund result fields: %+v", got)
	}
	if got.Record == nil || got.Record.Fee != 10 || got.Record.Amount != 1000 ||
		got.Record.AfterSeq != 1 || got.Record.Seq != 1 {
		t.Fatalf("refund record: %+v", got.Record)
	}
	// 全额（含手续费）退回。
	if bal, _ := l.Balance("aa", "usdc"); bal != 1010 {
		t.Fatalf("balance after refund=%d want 1010", bal)
	}

	snap, _ := l.Query()
	// 原结算记录及其编号、顺序保持不变。
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" || snap.Settlements[0].Charged != 1010 {
		t.Fatalf("original settlement altered: %+v", snap.Settlements)
	}
	if len(snap.Refunds) != 1 {
		t.Fatalf("want 1 refund, got %+v", snap.Refunds)
	}
	rf := snap.Refunds[0]
	if rf.ID != "r1" || rf.SettlementID != "p1" || rf.Reason != "cancel" ||
		rf.Account != "aa" || rf.Asset != "usdc" || rf.Charged != 1010 {
		t.Fatalf("query refund not traceable to settlement: %+v", rf)
	}
}

// 空退款批次返回空结果，且无退款时 query 为空列表。
func TestRefundEmptyBatch(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10}})
	l := openOrFail(t, path)
	rr, err := l.Refund(RefundBatch{Refunds: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.Results) != 0 {
		t.Fatalf("empty refund batch: %+v", rr.Results)
	}
	snap, _ := l.Query()
	if len(snap.Refunds) != 0 {
		t.Fatalf("refunds must be empty list, got %+v", snap.Refunds)
	}
}

// 参数校验：三个字段缺一不可，失败不占用退款编号。
func TestRefundInvalidParameters(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 10)}}); err != nil {
		t.Fatal(err)
	}
	rr, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "", SettlementID: "p1", Reason: "x"},
		{ID: "r", SettlementID: "", Reason: "x"},
		{ID: "r", SettlementID: "p1", Reason: ""},
	}})
	for i, x := range rr.Results {
		if x.Status != StatusInvalid {
			t.Fatalf("item %d want invalid_parameter, got %s (%s)", i, x.Status, x.Reason)
		}
	}
	// 失败申请不占用编号：同名编号随后可成功。
	rr2, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r", "p1", "now valid")}})
	if rr2.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("failed id should be reusable, got %+v", rr2.Results[0])
	}
}

// 已成功退款编号优先判定：目标与原因都相同 => duplicate 并返回原退款记录；
// 目标或原因任一不同 => conflict。都不再入账。
func TestRefundDuplicateAndConflict(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1_000_000}})
	l := openOrFail(t, path)
	for _, id := range []string{"p1", "p2"} {
		if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent(id, "aa", "usdc", 100)}}); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "reason A")}})
	if first.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("first: %+v", first.Results[0])
	}
	balAfter, _ := l.Balance("aa", "usdc")

	// 完全相同：duplicate，带原退款记录，不再入账。
	dup, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "reason A")}})
	d := dup.Results[0]
	if d.Status != StatusDuplicate || d.Record == nil || d.Record.Seq != 1 ||
		d.SettlementID != "p1" || d.Charged != 100 || d.Account != "aa" || d.Asset != "usdc" {
		t.Fatalf("duplicate refund: %+v", d)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("duplicate changed balance: %d vs %d", bal, balAfter)
	}

	// 原因不同 / 目标不同：conflict。
	for i, req := range []RefundRequest{
		refundReq("r1", "p1", "reason B"),
		refundReq("r1", "p2", "reason A"),
	} {
		res, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{req}})
		if res.Results[0].Status != StatusConflict {
			t.Fatalf("case %d want conflict, got %+v", i, res.Results[0])
		}
	}
	snap, _ := l.Query()
	if len(snap.Refunds) != 1 {
		t.Fatalf("conflict must not add refund records: %+v", snap.Refunds)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("conflict changed balance: %d vs %d", bal, balAfter)
	}
}

// 目标结算不存在（含原付款只曾失败）=> not_found；
// 已被其他退款编号退回 => already_refunded，不增加余额。
func TestRefundNotFoundAndAlreadyRefunded(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	// pbad 只曾余额不足失败，从未成功。
	fail, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("pbad", "aa", "usdc", 200)}})
	if fail.Results[0].Status != StatusFunds {
		t.Fatalf("setup: %+v", fail.Results[0])
	}
	// pok 成功。
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("pok", "aa", "usdc", 30)}}); err != nil {
		t.Fatal(err)
	}

	rr, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r-missing", "ghost", "x"),
		refundReq("r-failed", "pbad", "x"), // 原付款只曾失败
		refundReq("r-ok", "pok", "x"),
	}})
	if rr.Results[0].Status != StatusNotFound || rr.Results[1].Status != StatusNotFound {
		t.Fatalf("want not_found for missing/failed, got %+v", rr.Results)
	}
	if rr.Results[2].Status != StatusRefundSuccess {
		t.Fatalf("valid refund: %+v", rr.Results[2])
	}
	balAfter, _ := l.Balance("aa", "usdc") // 100-30+30 = 100

	// 新退款编号指向已退款结算：already_refunded，不加余额。
	rr2, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r-other", "pok", "again")}})
	if rr2.Results[0].Status != StatusAlreadyRefunded {
		t.Fatalf("want already_refunded, got %+v", rr2.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfter {
		t.Fatalf("already_refunded changed balance: %d vs %d", bal, balAfter)
	}
	// not_found 的编号未被占用，可以换成存在的目标（p2）再成功。
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p2", "aa", "usdc", 10)}}); err != nil {
		t.Fatal(err)
	}
	rr4, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r-failed", "p2", "reused id")}})
	if rr4.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("previously-not-found id should be reusable, got %+v", rr4.Results[0])
	}
}

// 批次逐项独立、后项可见前项退款：前项退回的余额可供同批次后项/后续付款使用；
// 前项成功后后项引用同一结算应得到 already_refunded。
func TestRefundBatchOrdering(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	// p1 扣 100，余额 0。
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)}}); err != nil {
		t.Fatal(err)
	}
	rr, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("r1", "p1", "first"),
		refundReq("r2", "p1", "second"), // 后项看到前项已退
	}})
	if rr.Results[0].Status != StatusRefundSuccess || rr.Results[1].Status != StatusAlreadyRefunded {
		t.Fatalf("batch ordering: %+v", rr.Results)
	}
	// 前项退回的余额可供后续付款使用。
	pay, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p3", "aa", "usdc", 100)}})
	if pay.Results[0].Status != StatusSettled {
		t.Fatalf("refunded balance not reusable: %+v", pay.Results[0])
	}
}

// 退款编号与付款编号分别使用：字符串相同也允许。
func TestRefundIDIndependentOfSettlementID(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("same", "aa", "usdc", 40)}}); err != nil {
		t.Fatal(err)
	}
	// 退款编号与原付款编号同名。
	rr, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("same", "same", "x")}})
	if rr.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("same-string ids must be allowed: %+v", rr.Results[0])
	}
}

// 退款后原付款的幂等资格保留：相同付款重试仍返回原结算（duplicate），
// 合法字段有变为 conflict，都不能再次扣款。
func TestSettlementIdempotencySurvivesRefund(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)
	base := PaymentIntent{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: i64p(300), Nonce: 1, State: "pending"}
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{base}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "x")}}); err != nil {
		t.Fatal(err)
	}
	balAfterRefund, _ := l.Balance("aa", "usdc") // 1000

	dup, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{base}})
	if dup.Results[0].Status != StatusDuplicate {
		t.Fatalf("original payment retry want duplicate, got %+v", dup.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfterRefund {
		t.Fatalf("duplicate re-charged after refund: %d", bal)
	}
	changed := base
	changed.Amount = i64p(301)
	cf, _ := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{changed}})
	if cf.Results[0].Status != StatusConflict {
		t.Fatalf("changed payment want conflict, got %+v", cf.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != balAfterRefund {
		t.Fatalf("conflict changed balance: %d", bal)
	}
}

// 存储失败：该项 storage_error，既不加余额也不留退款记录；前项保留，后项继续。
func TestRefundStorageFailureRollsBackItemOnly(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent(id, "aa", "usdc", 100)}}); err != nil {
			t.Fatal(err)
		}
	}
	// 三笔各扣 100，余额 700。第 2 次持久化（rb）失败。
	SetFailHook(l, &failNth{nth: 2})
	t.Cleanup(func() { SetFailHook(l, nil) })
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		refundReq("ra", "a", "x"),
		refundReq("rb", "b", "x"),
		refundReq("rc", "c", "x"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusRefundSuccess, StatusStorage, StatusRefundSuccess}
	if got := refundStatuses(rr); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	// ra、rc 各退 100，rb 回滚：700+200=900。
	if bal, _ := l.Balance("aa", "usdc"); bal != 900 {
		t.Fatalf("balance=%d want 900", bal)
	}
	snap, _ := l.Query()
	if len(snap.Refunds) != 2 || snap.Refunds[0].ID != "ra" || snap.Refunds[1].ID != "rc" {
		t.Fatalf("refund records after storage failure: %+v", snap.Refunds)
	}
	// rb 编号未占用，且结算 b 未被标记退款，可重新提交成功。
	rr2, _ := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("rb", "b", "retry")}})
	if rr2.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("rb retry: %+v", rr2.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("balance after rb retry=%d want 1000", bal)
	}

	// 关闭重开后磁盘状态与内存一致。
	l2 := openOrFail(t, path)
	snap2, _ := l2.Query()
	if len(snap2.Settlements) != 3 || len(snap2.Refunds) != 3 {
		t.Fatalf("reopen history: settlements=%d refunds=%d", len(snap2.Settlements), len(snap2.Refunds))
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("reopen balance=%d want 1000", bal)
	}
}

// 关闭后重开：余额、两类历史、重复申请（duplicate/conflict/already_refunded）一致。
func TestRefundPersistenceAcrossReopen(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l1 := openOrFail(t, path)
	if _, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 250)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l1.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "why")}}); err != nil {
		t.Fatal(err)
	}
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}

	l2 := openOrFail(t, path)
	if bal, _ := l2.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("reopen balance=%d want 1000", bal)
	}
	snap, _ := l2.Query()
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" {
		t.Fatalf("reopen settlements: %+v", snap.Settlements)
	}
	if len(snap.Refunds) != 1 || snap.Refunds[0].SettlementID != "p1" || snap.Refunds[0].Charged != 250 {
		t.Fatalf("reopen refunds: %+v", snap.Refunds)
	}
	// 重复申请：相同 => duplicate；不同 => conflict；同结算他编号 => already_refunded。
	dup, _ := l2.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "why")}})
	if dup.Results[0].Status != StatusDuplicate {
		t.Fatalf("reopen duplicate: %+v", dup.Results[0])
	}
	cf, _ := l2.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "other")}})
	if cf.Results[0].Status != StatusConflict {
		t.Fatalf("reopen conflict: %+v", cf.Results[0])
	}
	ar, _ := l2.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r9", "p1", "x")}})
	if ar.Results[0].Status != StatusAlreadyRefunded {
		t.Fatalf("reopen already_refunded: %+v", ar.Results[0])
	}
}

// 付款 → 退款 → 再付款的账本能正常恢复，交错顺序被正确校验。
func TestRefundPayRefundPayReplay(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)
	must := func(batch FeeBatch) {
		t.Helper()
		res, err := l.Submit(batch)
		if err != nil || res.Results[0].Status != StatusSettled {
			t.Fatalf("submit: %+v err=%v", res.Results, err)
		}
	}
	must(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)}})                   // 0
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "x")}}); err != nil { // 100
		t.Fatal(err)
	}
	must(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p2", "aa", "usdc", 60)}})                    // 40
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r2", "p2", "x")}}); err != nil { // 100
		t.Fatal(err)
	}
	must(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p3", "aa", "usdc", 100)}}) // 0

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2 := openOrFail(t, path)
	if bal, _ := l2.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("final balance=%d want 0", bal)
	}
	snap, _ := l2.Query()
	if len(snap.Settlements) != 3 || len(snap.Refunds) != 2 {
		t.Fatalf("history: %d settlements, %d refunds", len(snap.Settlements), len(snap.Refunds))
	}
	if snap.Refunds[0].AfterSeq != 1 || snap.Refunds[1].AfterSeq != 2 {
		t.Fatalf("after_seq interleaving lost: %+v", snap.Refunds)
	}
}

// 并发：多个句柄并发退款，每笔原付款最多退回一次（唯一退款编号下恰一笔成功，
// 其余 already_refunded），查询看不到半更新状态。
func TestRefundConcurrentSettlesRefundOnce(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	lc := openOrFail(t, path)
	if _, err := lc.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("only", "aa", "usdc", 1000)}}); err != nil {
		t.Fatal(err)
	}

	const n = 40
	var wg sync.WaitGroup
	c2 := map[string]int{}
	var m2 sync.Mutex
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer h.Close()
			rr, err := h.Refund(RefundBatch{Refunds: []RefundRequest{refundReq(fmt.Sprintf("rid-%d", i), "only", "x")}})
			if err != nil {
				t.Errorf("Refund: %v", err)
				return
			}
			m2.Lock()
			c2[rr.Results[0].Status]++
			m2.Unlock()
		}()
	}
	wg.Wait()
	if c2[StatusRefundSuccess] != 1 {
		t.Fatalf("refunded=%d want 1 (counts=%v)", c2[StatusRefundSuccess], c2)
	}
	if c2[StatusAlreadyRefunded] != n-1 {
		t.Fatalf("already_refunded=%d want %d (counts=%v)", c2[StatusAlreadyRefunded], n-1, c2)
	}
	snap, _ := lc.Query()
	if len(snap.Refunds) != 1 {
		t.Fatalf("refund records=%d want 1", len(snap.Refunds))
	}
	if bal, _ := lc.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("balance=%d want 1000", bal)
	}
}

// 并发退款后余额立即可供并发付款使用，且总额守恒、无半更新可见。
func TestRefundedBalanceReusedConcurrently(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("seed", "aa", "usdc", 1000)}}); err != nil {
		t.Fatal(err)
	}

	// 先全额退款（唯一一笔）。
	done := make(chan struct{})
	go func() {
		h, _ := Open(path)
		defer h.Close()
		_, _ = h.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("rseed", "seed", "x")}})
		close(done)
	}()
	<-done

	// 退回的 1000 余额上并发发起 40 笔各 1000 的付款：恰有一笔成功。
	const n = 40
	var wg sync.WaitGroup
	c := map[string]int{}
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer h.Close()
			res, err := h.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
				intent(fmt.Sprintf("grab-%d", i), "aa", "usdc", 1000),
			}})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			mu.Lock()
			c[res.Results[0].Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if c[StatusSettled] != 1 || c[StatusFunds] != n-1 {
		t.Fatalf("counts=%v want exactly 1 settled", c)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("balance=%d want 0", bal)
	}
}

// 损坏检测：篡改退款相关语义均拒绝为损坏账本。
func TestCorruptRefundLedgerRejected(t *testing.T) {
	k := balanceKey{"aa", "usdc"}
	initial := map[balanceKey]int64{k: 1000}
	rec := Record{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 300, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 300, Seq: 1}
	goodRF := RefundRecord{ID: "r1", SettlementID: "p1", Reason: "x", Account: "aa", Asset: "usdc", Amount: 300, Fee: 0, Charged: 300, AfterSeq: 1, Seq: 1}
	finalBack := map[balanceKey]int64{k: 1000} // 退完回到初始

	mkRF := func(mut func(*RefundRecord)) []RefundRecord {
		r := goodRF
		mut(&r)
		return []RefundRecord{r}
	}

	cases := map[string]struct {
		final   map[balanceKey]int64
		records []Record
		refunds []RefundRecord
	}{
		"missing settlement": {finalBack, []Record{rec}, mkRF(func(r *RefundRecord) { r.SettlementID = "ghost" })},
		"double refund": {finalBack, []Record{rec}, []RefundRecord{
			goodRF,
			func() RefundRecord { r := goodRF; r.ID = "r2"; r.Seq = 2; return r }(),
		}},
		"amount differs":      {finalBack, []Record{rec}, mkRF(func(r *RefundRecord) { r.Amount = 299; r.Charged = 299 })},
		"charged differs":     {finalBack, []Record{rec}, mkRF(func(r *RefundRecord) { r.Charged = 301 })},
		"asset differs":       {finalBack, []Record{rec}, mkRF(func(r *RefundRecord) { r.Asset = "eth" })},
		"empty reason":        {finalBack, []Record{rec}, mkRF(func(r *RefundRecord) { r.Reason = "" })},
		"seq gap":             {finalBack, []Record{rec}, mkRF(func(r *RefundRecord) { r.Seq = 2 })},
		"after_seq too large": {finalBack, []Record{rec}, mkRF(func(r *RefundRecord) { r.AfterSeq = 2 })},
		"balance mismatch":    {map[balanceKey]int64{k: 999}, []Record{rec}, mkRF(func(r *RefundRecord) {})},
	}

	// “duplicate refund id”：两笔不同结算各退一次，但两条退款记录共用编号 r1。
	rec2 := Record{ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 1, Nonce: 2, FeeBps: 0, Fee: 0, Charged: 1, Seq: 2}
	rf2 := goodRF
	rf2.Seq = 2
	rf2.SettlementID = "p2"
	rf2.Amount, rf2.Fee, rf2.Charged = 1, 0, 1
	rf2.AfterSeq = 2
	cases["duplicate refund id"] = struct {
		final   map[balanceKey]int64
		records []Record
		refunds []RefundRecord
	}{
		final:   map[balanceKey]int64{k: 1000 - 1},
		records: []Record{rec, rec2},
		refunds: []RefundRecord{goodRF, rf2},
	}

	// “after_seq 倒退”：两笔各 100 的零费率付款先后成功；退款 r1（seq 1）退第二笔，
	// 时点 after_seq=2；退款 r2（seq 2）退第一笔，时点却为 after_seq=1。
	// 两笔退款各自的金额、目标、序号与最终余额都能单独核对上，但第二次退款
	// 发生时不可能只剩一笔已完成付款——结算历史只追加，after_seq 不得倒退。
	pay1 := Record{ID: "q1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 100, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 100, Seq: 1}
	pay2 := Record{ID: "q2", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 100, Nonce: 2, FeeBps: 0, Fee: 0, Charged: 100, Seq: 2}
	back1 := RefundRecord{ID: "b1", SettlementID: "q2", Reason: "x", Account: "aa", Asset: "usdc", Amount: 100, Fee: 0, Charged: 100, AfterSeq: 2, Seq: 1}
	back2 := RefundRecord{ID: "b2", SettlementID: "q1", Reason: "x", Account: "aa", Asset: "usdc", Amount: 100, Fee: 0, Charged: 100, AfterSeq: 1, Seq: 2}
	cases["after_seq regression"] = struct {
		final   map[balanceKey]int64
		records []Record
		refunds []RefundRecord
	}{
		final:   map[balanceKey]int64{k: 1000},
		records: []Record{pay1, pay2},
		refunds: []RefundRecord{back1, back2},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateAndReplay(initial, tc.final, tc.records, tc.refunds)
			if err == nil || KindOf(err) != ErrCorrupt {
				t.Fatalf("%s: want ErrCorrupt, got %v", name, err)
			}
			if name == "after_seq regression" {
				// 错误说明要定位到发生倒退的退款，并带上相邻两笔的时点。
				msg := err.Error()
				for _, want := range []string{"b2", "b1", "after_seq"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("error %q should mention %q", msg, want)
					}
				}
			}
		})
	}

	// 健康的付款→退款账本必须通过。
	if err := validateAndReplay(finalBack, finalBack, []Record{rec}, []RefundRecord{goodRF}); err != nil {
		t.Fatalf("healthy refund ledger rejected: %v", err)
	}

	// 相邻退款 after_seq 相等是合法的：两次退款之间没有新增成功付款。
	same1 := RefundRecord{ID: "s1", SettlementID: "q1", Reason: "x", Account: "aa", Asset: "usdc", Amount: 100, Fee: 0, Charged: 100, AfterSeq: 2, Seq: 1}
	same2 := RefundRecord{ID: "s2", SettlementID: "q2", Reason: "x", Account: "aa", Asset: "usdc", Amount: 100, Fee: 0, Charged: 100, AfterSeq: 2, Seq: 2}
	if err := validateAndReplay(initial, map[balanceKey]int64{k: 1000}, []Record{pay1, pay2}, []RefundRecord{same1, same2}); err != nil {
		t.Fatalf("equal after_seq refunds rejected: %v", err)
	}

	// 正常交错历史（付款、退款、再付款、再退款）也必须通过。
	inter1 := RefundRecord{ID: "t1", SettlementID: "q1", Reason: "x", Account: "aa", Asset: "usdc", Amount: 100, Fee: 0, Charged: 100, AfterSeq: 1, Seq: 1}
	inter2 := RefundRecord{ID: "t2", SettlementID: "q2", Reason: "x", Account: "aa", Asset: "usdc", Amount: 100, Fee: 0, Charged: 100, AfterSeq: 2, Seq: 2}
	if err := validateAndReplay(initial, map[balanceKey]int64{k: 1000}, []Record{pay1, pay2}, []RefundRecord{inter1, inter2}); err != nil {
		t.Fatalf("interleaved pay/refund history rejected: %v", err)
	}
}

func refundStatuses(r *RefundBatchResult) []string {
	out := make([]string, len(r.Results))
	for i, x := range r.Results {
		out[i] = x.Status
	}
	return out
}

// 旧版账本（没有 refunds 字段）无需转换即可查询、付款和退款；
// 读取与仅付款都不改写文件（保持无 refunds 键），首次退款才引入该字段。
func TestLegacyFileWithoutRefundsField(t *testing.T) {
	// 按磁盘文档规范字段顺序构造一个带合法 checksum 的旧版文档。
	legacy := func(balance int64, records ...Record) []byte {
		type doc struct {
			Version     int           `json:"version"`
			Initial     []BalanceView `json:"initial_balances"`
			Balances    []BalanceView `json:"balances"`
			Settlements []Record      `json:"settlements"`
		}
		d := doc{
			Version:     1,
			Initial:     []BalanceView{{Account: "aa", Asset: "usdc", Balance: 1000}},
			Balances:    []BalanceView{{Account: "aa", Asset: "usdc", Balance: balance}},
			Settlements: records,
		}
		payload, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		sum := checksumHex(payload)
		return append(append(payload[:len(payload)-1], []byte(`,"checksum":"`+sum+`"}`)...), '\n')
	}
	rec := Record{ID: "p1", Account: "aa", Paymaster: "pm", Asset: "usdc", Amount: 300, Nonce: 1, FeeBps: 0, Fee: 0, Charged: 300, Seq: 1}
	path := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(path, legacy(700, rec), 0o600); err != nil {
		t.Fatal(err)
	}

	l := openOrFail(t, path)
	snap, err := l.Query()
	if err != nil {
		t.Fatalf("query legacy: %v", err)
	}
	if len(snap.Refunds) != 0 || snap.Balances[0].Balance != 700 {
		t.Fatalf("legacy query: %+v", snap)
	}

	// 再付款一笔：无退款，文件必须保持不含 refunds 键。
	if _, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p2", "aa", "usdc", 100)}}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); bytes.Contains(data, []byte(`"refunds"`)) {
		t.Fatalf("submit on refund-less ledger must not add refunds key:\n%s", data)
	}

	// 首次退款引入 refunds 键并全额回到 900（700-100+300）。
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "legacy")}})
	if err != nil {
		t.Fatal(err)
	}
	if rr.Results[0].Status != StatusRefundSuccess || rr.Results[0].Charged != 300 {
		t.Fatalf("legacy refund: %+v", rr.Results[0])
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte(`"refunds"`)) {
		t.Fatalf("refund must persist refunds key:\n%s", data)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 900 {
		t.Fatalf("balance after legacy refund=%d want 900", bal)
	}

	// 关闭重开（新进程语义）应正常恢复付款→退款→付款账本。
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2 := openOrFail(t, path)
	snap2, _ := l2.Query()
	if len(snap2.Settlements) != 2 || len(snap2.Refunds) != 1 {
		t.Fatalf("reopen legacy: %+v", snap2)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 900 {
		t.Fatalf("reopen balance=%d want 900", bal)
	}
}

// ---- 本次批次扣款上限（limits） ----

func limit(account, asset string, max int64) ChargeLimit {
	return ChargeLimit{Account: account, Asset: asset, MaxCharged: i64p(max)}
}

func TestChargeLimitBasics(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 10000},
		{Account: "aa", Asset: "eth", Balance: 100},
	})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{
		FeeBps: 0,
		Limits: []ChargeLimit{limit("aa", "usdc", 1000)},
		Intents: []PaymentIntent{
			intent("p1", "aa", "usdc", 600), // 成功，已用 600
			intent("p2", "aa", "usdc", 400), // 恰好达到上限，成功
			intent("p3", "aa", "usdc", 1),   // 超限
			intent("p4", "aa", "eth", 5),    // 未列出的组合不受限
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusSettled, StatusLimitExceeded, StatusSettled}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	r := res.Results[2]
	for _, frag := range []string{"aa/usdc", "max_charged 1000", "used 1000", "charges 1"} {
		if !strings.Contains(r.Reason, frag) {
			t.Fatalf("limit reason %q missing %q", r.Reason, frag)
		}
	}
	if r.Record != nil {
		t.Fatalf("limit_exceeded must not carry a record: %+v", r.Record)
	}
	// 超限项不扣余额、不留记录；未列出组合正常扣款。
	if bal, _ := l.Balance("aa", "usdc"); bal != 9000 {
		t.Fatalf("usdc balance=%d want 9000", bal)
	}
	if bal, _ := l.Balance("aa", "eth"); bal != 95 {
		t.Fatalf("eth balance=%d want 95", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 3 {
		t.Fatalf("settlements=%+v want 3 records", snap.Settlements)
	}
}

func TestChargeLimitRetrySmallerSucceeds(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	big := intent("p2", "aa", "usdc", 200)
	small := intent("p2", "aa", "usdc", 100)
	res, err := l.Submit(FeeBatch{
		FeeBps:  0,
		Limits:  []ChargeLimit{limit("aa", "usdc", 500)},
		Intents: []PaymentIntent{intent("p1", "aa", "usdc", 400), big, small},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 前项超限失败后，同一编号改为较小金额仍可在本批次内成功。
	want := []string{StatusSettled, StatusLimitExceeded, StatusSettled}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 9500 {
		t.Fatalf("balance=%d want 9500", bal)
	}
}

func TestChargeLimitIncludesFee(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	// 上限计入手续费：100 + 1(费) = 101 恰好达到上限。
	res, err := l.Submit(FeeBatch{
		FeeBps:  100,
		Limits:  []ChargeLimit{limit("aa", "usdc", 101)},
		Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100), intent("p2", "aa", "usdc", 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusLimitExceeded}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if got := res.Results[0].Record.Charged; got != 101 {
		t.Fatalf("charged=%d want 101", got)
	}
}

func TestChargeLimitPerComboIndependent(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 10000},
		{Account: "aa", Asset: "eth", Balance: 10000},
		{Account: "bb", Asset: "usdc", Balance: 10000},
	})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{
		FeeBps: 0,
		Limits: []ChargeLimit{limit("aa", "usdc", 100), limit("aa", "eth", 50)},
		Intents: []PaymentIntent{
			intent("u1", "aa", "usdc", 100), // 用尽 aa/usdc 额度
			intent("u2", "aa", "usdc", 1),   // aa/usdc 超限
			intent("e1", "aa", "eth", 50),   // 同账户不同资产：额度各自独立
			intent("b1", "bb", "usdc", 100), // 同资产不同账户：未列出，不受限
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusLimitExceeded, StatusSettled, StatusSettled}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
}

func TestChargeLimitZeroBlocksCombo(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{
		FeeBps:  0,
		Limits:  []ChargeLimit{limit("aa", "usdc", 0)},
		Intents: []PaymentIntent{intent("p1", "aa", "usdc", 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusLimitExceeded {
		t.Fatalf("zero limit must block any charge: %+v", res.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("balance=%d want 1000", bal)
	}
}

func TestChargeLimitCheckedBeforeBalance(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 5}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{
		FeeBps: 0,
		Limits: []ChargeLimit{limit("aa", "usdc", 6)},
		Intents: []PaymentIntent{
			intent("p1", "aa", "usdc", 8), // 超限且余额不足：报告超限
			intent("p2", "aa", "usdc", 6), // 限额内但余额不足：报告余额
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusLimitExceeded, StatusFunds}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
}

func TestChargeLimitFailuresDoNotConsumeQuota(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 150}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{
		FeeBps: 0,
		Limits: []ChargeLimit{limit("aa", "usdc", 200)},
		Intents: []PaymentIntent{
			intent("p1", "aa", "usdc", 100), // 成功，已用 100，余额 50
			intent("p2", "aa", "usdc", 100), // 限额内但余额不足，不占额度
			intent("p3", "aa", "usdc", 100), // 若 p2 占了额度此处会报超限
			intent("p4", "aa", "usdc", 50),  // 成功
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusFunds, StatusFunds, StatusSettled}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
}

func TestChargeLimitPerSubmissionAndIdempotency(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)

	// 第一次提交：上限 100，p1 用满。
	res, err := l.Submit(FeeBatch{
		FeeBps:  0,
		Limits:  []ChargeLimit{limit("aa", "usdc", 100)},
		Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)},
	})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("first submit: %v %+v", err, res.Results[0])
	}

	// 重复提交已成功付款：即使不带限额（或额度已用完）仍返回 duplicate 及原记录。
	res, err = l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusDuplicate || res.Results[0].Record == nil || res.Results[0].Record.Charged != 100 {
		t.Fatalf("duplicate after limit batch: %+v", res.Results[0])
	}
	// 限额变化不属于付款字段变化：带不同限额重复提交仍是 duplicate。
	res, err = l.Submit(FeeBatch{
		FeeBps:  0,
		Limits:  []ChargeLimit{limit("aa", "usdc", 1)},
		Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusDuplicate {
		t.Fatalf("limits must not affect idempotency: %+v", res.Results[0])
	}

	// 额度按提交从零计算：新批次同样的上限可再扣 100，历史扣款不占额度。
	res, err = l.Submit(FeeBatch{
		FeeBps:  0,
		Limits:  []ChargeLimit{limit("aa", "usdc", 100)},
		Intents: []PaymentIntent{intent("p2", "aa", "usdc", 100)},
	})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("limit must reset per submission: %v %+v", err, res.Results[0])
	}

	// 退款不回补本次额度：第三次提交内 p3 用满后，即使此前 p1 已退款，
	// 本批次额度仍只有 100。
	if _, err := l.Refund(RefundBatch{Refunds: []RefundRequest{refundReq("r1", "p1", "cancel")}}); err != nil {
		t.Fatal(err)
	}
	res, err = l.Submit(FeeBatch{
		FeeBps:  0,
		Limits:  []ChargeLimit{limit("aa", "usdc", 100)},
		Intents: []PaymentIntent{intent("p3", "aa", "usdc", 100), intent("p4", "aa", "usdc", 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusLimitExceeded}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("refund must not restore quota: %v want %v", got, want)
	}
}

func TestChargeLimitOverflowSafety(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: math.MaxInt64}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{
		FeeBps: 0,
		Limits: []ChargeLimit{limit("aa", "usdc", math.MaxInt64)},
		Intents: []PaymentIntent{
			intent("p1", "aa", "usdc", math.MaxInt64-10), // 成功，余 10
			intent("p2", "aa", "usdc", 100),              // 累计会溢出 int64：必须拒绝
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusLimitExceeded}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 10 {
		t.Fatalf("balance=%d want 10", bal)
	}
}

func TestChargeLimitStorageFailureRestoresQuota(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 10000}})
	l := openOrFail(t, path)
	SetFailHook(l, &failNth{nth: 2})
	t.Cleanup(func() { SetFailHook(l, nil) })

	res, err := l.Submit(FeeBatch{
		FeeBps: 0,
		Limits: []ChargeLimit{limit("aa", "usdc", 200)},
		Intents: []PaymentIntent{
			intent("ok1", "aa", "usdc", 100), // 成功，已用 100
			intent("boom", "aa", "usdc", 50), // 保存失败：回滚且不占额度
			intent("ok2", "aa", "usdc", 100), // 恰好用满 200
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StatusSettled, StatusStorage, StatusSettled}
	if got := statuses(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses=%v want %v", got, want)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 9800 {
		t.Fatalf("balance=%d want 9800", bal)
	}
}

func TestChargeLimitInvalidRejectsWholeBatch(t *testing.T) {
	cases := []struct {
		name   string
		limits []ChargeLimit
	}{
		{"empty account", []ChargeLimit{limit("", "usdc", 1)}},
		{"empty asset", []ChargeLimit{limit("aa", "", 1)}},
		{"missing max_charged", []ChargeLimit{{Account: "aa", Asset: "usdc"}}},
		{"negative max_charged", []ChargeLimit{limit("aa", "usdc", -1)}},
		{"duplicate combo", []ChargeLimit{limit("aa", "usdc", 1), limit("aa", "usdc", 2)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
			l := openOrFail(t, path)

			// 空意图列表也要检查限额。
			if _, err := l.Submit(FeeBatch{FeeBps: 0, Limits: tc.limits}); KindOf(err) != ErrInvalid {
				t.Fatalf("empty intents: want ErrInvalid, got %v", err)
			}
			// 任一限额项不合法：整个批次拒绝，任何意图都不执行。
			_, err := l.Submit(FeeBatch{
				FeeBps:  0,
				Limits:  tc.limits,
				Intents: []PaymentIntent{intent("p1", "aa", "usdc", 100)},
			})
			if KindOf(err) != ErrInvalid {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if bal, _ := l.Balance("aa", "usdc"); bal != 1000 {
				t.Fatalf("invalid limits must not execute any intent, balance=%d", bal)
			}
			snap, _ := l.Query()
			if len(snap.Settlements) != 0 {
				t.Fatalf("invalid limits must not settle: %+v", snap.Settlements)
			}
		})
	}
}

func TestChargeLimitAbsentOrEmptyMeansUnlimited(t *testing.T) {
	for _, limits := range [][]ChargeLimit{nil, {}} {
		path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
		l := openOrFail(t, path)
		res, err := l.Submit(FeeBatch{
			FeeBps:  0,
			Limits:  limits,
			Intents: []PaymentIntent{intent("p1", "aa", "usdc", 1000)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Results[0].Status != StatusSettled {
			t.Fatalf("limits=%v must not restrict: %+v", limits, res.Results[0])
		}
		l.Close()
	}
}

// ---- 目录软链接：同一账本的两个路径共享同一份结算状态 ----

// symlinkedLedger 在真实目录初始化账本，再建立指向该目录的软链接，
// 返回真实路径与经软链接的路径。
func symlinkedLedger(t *testing.T, init []BalanceInit) (realPath, linkPath string) {
	t.Helper()
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	realPath = filepath.Join(realDir, "ledger.json")
	if err := CreateLedger(realPath, init); err != nil {
		t.Fatalf("CreateLedger: %v", err)
	}
	return realPath, filepath.Join(linkDir, "ledger.json")
}

func TestSymlinkHandlesShareState(t *testing.T) {
	realPath, linkPath := symlinkedLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	lReal := openOrFail(t, realPath)
	lLink := openOrFail(t, linkPath)

	// 经真实路径提交付款。
	res, err := lReal.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 60)}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("first submit: %+v", res.Results[0])
	}

	// 经软链接路径提交完全相同的请求：幂等返回原记录，不再扣款。
	dup, err := lLink.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 60)}})
	if err != nil {
		t.Fatal(err)
	}
	if dup.Results[0].Status != StatusDuplicate || dup.Results[0].Record == nil ||
		dup.Results[0].Record.Seq != res.Results[0].Record.Seq {
		t.Fatalf("want duplicate with original record, got %+v", dup.Results[0])
	}

	// 同一编号修改金额或费率：冲突，保留原记录。
	modified := intent("p1", "aa", "usdc", 61)
	conf, err := lLink.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{modified}})
	if err != nil {
		t.Fatal(err)
	}
	if conf.Results[0].Status != StatusConflict {
		t.Fatalf("want conflict, got %+v", conf.Results[0])
	}
	confFee, err := lReal.Submit(FeeBatch{FeeBps: 10, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 60)}})
	if err != nil {
		t.Fatal(err)
	}
	if confFee.Results[0].Status != StatusConflict {
		t.Fatalf("fee change: want conflict, got %+v", confFee.Results[0])
	}

	// 两个路径查询到相同的余额与历史。
	for name, l := range map[string]*Ledger{"real": lReal, "link": lLink} {
		bal, err := l.Balance("aa", "usdc")
		if err != nil {
			t.Fatal(err)
		}
		if bal != 40 {
			t.Fatalf("%s handle: balance=%d, want 40", name, bal)
		}
		snap, err := l.Query()
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" {
			t.Fatalf("%s handle: settlements=%+v", name, snap.Settlements)
		}
	}
}

func TestSymlinkHandlesShareBalance(t *testing.T) {
	realPath, linkPath := symlinkedLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	lReal := openOrFail(t, realPath)
	lLink := openOrFail(t, linkPath)

	// 初始余额 100、费率 0：先成功的 70 立即影响另一路径上的 40。
	res70, err := lReal.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p70", "aa", "usdc", 70)}})
	if err != nil {
		t.Fatal(err)
	}
	if res70.Results[0].Status != StatusSettled {
		t.Fatalf("p70: %+v", res70.Results[0])
	}
	res40, err := lLink.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p40", "aa", "usdc", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	if res40.Results[0].Status != StatusFunds {
		t.Fatalf("p40: want insufficient_balance, got %+v", res40.Results[0])
	}

	// 失败项不留成功记录、不占序号；余额与文件内容与成功的那笔一致。
	if bal, _ := lLink.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("balance=%d, want 30", bal)
	}
	snap, err := lReal.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p70" || snap.Settlements[0].Seq != 1 {
		t.Fatalf("settlements=%+v", snap.Settlements)
	}
	raw, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"p70"`) || strings.Contains(string(raw), `"p40"`) {
		t.Fatalf("ledger file must contain only the settled record: %s", raw)
	}

	// 关闭其中一个句柄不影响另一个继续使用。
	if err := lReal.Close(); err != nil {
		t.Fatal(err)
	}
	res30, err := lLink.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p30", "aa", "usdc", 30)}})
	if err != nil {
		t.Fatal(err)
	}
	if res30.Results[0].Status != StatusSettled || res30.Results[0].Record.Seq != 2 {
		t.Fatalf("p30 after close: %+v", res30.Results[0])
	}
	if bal, _ := lLink.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("balance=%d, want 0", bal)
	}
}

func TestSymlinkHandlesConcurrentSubmit(t *testing.T) {
	realPath, linkPath := symlinkedLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	lReal := openOrFail(t, realPath)
	lLink := openOrFail(t, linkPath)

	// 两个句柄同时提交 70 与 40：只能有一笔 settled，另一笔 insufficient_balance。
	var wg sync.WaitGroup
	statuses := make([]string, 2)
	submit := func(idx int, l *Ledger, id string, amount int64) {
		defer wg.Done()
		res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent(id, "aa", "usdc", amount)}})
		if err != nil {
			t.Error(err)
			return
		}
		statuses[idx] = res.Results[0].Status
	}
	wg.Add(2)
	go submit(0, lReal, "c70", 70)
	go submit(1, lLink, "c40", 40)
	wg.Wait()

	settled := 0
	for _, s := range statuses {
		switch s {
		case StatusSettled:
			settled++
		case StatusFunds:
		default:
			t.Fatalf("unexpected status %q", s)
		}
	}
	if settled != 1 {
		t.Fatalf("exactly one payment may settle, statuses=%v", statuses)
	}
	snap, err := lReal.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Settlements) != 1 {
		t.Fatalf("settlements=%+v", snap.Settlements)
	}
	var wantBal int64
	switch snap.Settlements[0].Charged {
	case 70:
		wantBal = 30
	case 40:
		wantBal = 60
	default:
		t.Fatalf("unexpected charged %d", snap.Settlements[0].Charged)
	}
	if bal, _ := lLink.Balance("aa", "usdc"); bal != wantBal {
		t.Fatalf("balance=%d, want %d", bal, wantBal)
	}
}

func TestSymlinkOpenErrors(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}

	// 账本不存在：经软链接打开仍报 ledger_not_initialized，且不创建文件。
	missing := filepath.Join(linkDir, "nope.json")
	if _, err := Open(missing); KindOf(err) != ErrNotInit {
		t.Fatalf("want ErrNotInit, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(realDir, "nope.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed open must not create a file, stat err=%v", err)
	}

	// 账本损坏：经软链接打开仍报 corrupt_ledger，且不被修复。
	bad := filepath.Join(realDir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"version":1,"checksum":"00"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(linkDir, "bad.json")); KindOf(err) != ErrCorrupt {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	after, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("corrupt ledger must not be repaired on open")
	}
}
