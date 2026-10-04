package payflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// 本文件回归“初始化后、首次打开之前”的文件权威语义：
//
// CreateLedger 成功只代表磁盘上曾写入一份完整账本，绝不代替首次 Open
// 对该路径当前内容的检查。若尚无句柄，文件被删除、清空、截断或改成
// 校验和不匹配的内容，首次 Open 必须如实返回 ledger_not_initialized 或
// corrupt_ledger，且不返回句柄、不重新生成账本、不恢复初始余额或修补文件。
//
// 同一路径重新放回合法账本后，再次 Open 读到的是该文件此刻的内容。

// writeFileAt 用给定内容覆盖 path（文件不存在时创建）。
func writeFileAt(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertOpenFails 断言 Open 返回指定错误分类、句柄为 nil，且不留下任何
// 会让后续 Open 跳过读盘的内存状态。
func assertOpenFails(t *testing.T, path, wantKind string) {
	t.Helper()
	l, err := Open(path)
	if l != nil {
		_ = l.Close()
		t.Fatalf("Open on bad ledger must not return a usable handle, got %+v", l)
	}
	if KindOf(err) != wantKind {
		t.Fatalf("Open: want %s, got %v", wantKind, err)
	}
}

// 初始化后尚无任何句柄：删除文件，首次 Open 必须报 ledger_not_initialized，
// 且不得（重新）生成账本文件。
func TestFirstOpenAfterFileDeletedIsNotInitialized(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 500},
		{Account: "aa", Asset: "eth", Balance: 3},
	})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	assertOpenFails(t, path, ErrNotInit)
	// 失败不得重新生成账本。
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed open must not recreate the ledger file, stat err=%v", err)
	}
	// 再试一次仍按磁盘现状报未初始化：失败结果不得被缓存成“已初始化”。
	assertOpenFails(t, path, ErrNotInit)
}

// 初始化后尚无任何句柄：把刚创建账本自身的文件清空、只留白字符或截断成
// 半个 JSON，首次 Open 必须报 corrupt_ledger，且原字节不得被修补或覆盖。
func TestFirstOpenOnEmptyOrTruncatedFileIsCorrupt(t *testing.T) {
	cases := map[string]func(raw []byte) []byte{
		"empty":          func(raw []byte) []byte { return nil },
		"whitespace":     func(raw []byte) []byte { return []byte("   \n\t  ") },
		"truncated json": func(raw []byte) []byte { return raw[:len(raw)/2] },
		"open brace":     func(raw []byte) []byte { return []byte("{") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			// 每个子用例用各自刚创建的账本，并篡改“同一个路径”。
			path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 500}})
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := mutate(raw)
			writeFileAt(t, path, content)

			assertOpenFails(t, path, ErrCorrupt)

			after, statErr := os.ReadFile(path)
			if statErr != nil {
				t.Fatalf("failed open must not remove the file: %v", statErr)
			}
			if !bytes.Equal(content, after) {
				t.Fatalf("failed open must not repair/rewrite the file")
			}
		})
	}
}

// 初始化后尚无任何句柄：把刚创建账本自身的文件改成校验和不匹配的内容
// （比特翻转或语义编辑），首次 Open 必须报 corrupt_ledger，且文件逐字节不变。
func TestFirstOpenOnChecksumMismatchIsCorrupt(t *testing.T) {
	cases := map[string]func(b []byte) []byte{
		"bitflip": func(b []byte) []byte {
			b[10] ^= 0x01
			return b
		},
		"semantic edit": func(b []byte) []byte {
			return bytes.ReplaceAll(b, []byte(`"balance":500`), []byte(`"balance":999`))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 500}})
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := mutate(bytes.Clone(raw))
			writeFileAt(t, path, content)

			assertOpenFails(t, path, ErrCorrupt)

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, content) {
				t.Fatalf("failed open must leave the corrupt file byte-intact")
			}
		})
	}
}

// 完好文件上的首次打开：得到初始化时的账户、资产与金额；读取本身不改文件。
func TestFirstOpenOnIntactFileReadsInitStateWithoutWriting(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 500},
		{Account: "bb", Asset: "eth", Balance: 7},
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("first open on intact file: %v", err)
	}
	// 打开后只查询，不提交任何业务操作。
	snap, err := l.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Settlements) != 0 || len(snap.Refunds) != 0 {
		t.Fatalf("fresh ledger must have empty history: %+v", snap)
	}
	if got := len(snap.Balances); got != 2 {
		t.Fatalf("balances=%+v want 2 initial combos", snap.Balances)
	}
	for _, want := range []BalanceView{
		{Account: "aa", Asset: "usdc", Balance: 500},
		{Account: "bb", Asset: "eth", Balance: 7},
	} {
		bal, err := l.Balance(want.Account, want.Asset)
		if err != nil {
			t.Fatal(err)
		}
		if bal != want.Balance {
			t.Fatalf("%s/%s balance=%d want %d", want.Account, want.Asset, bal, want.Balance)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("first open/query must not modify the ledger file")
	}
}

// 失败的打开不污染后续使用：同一目录路径先放回一份与初始化余额不同的
// 合法账本，再次 Open 必须读到新文件内容，而不是初始化时的旧余额或
// 沿用上一次错误。
func TestFailedFirstOpenRecoversWhenValidFileRestored(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 500}})

	// 删除 → 未初始化。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertOpenFails(t, path, ErrNotInit)

	// 放回一份余额完全不同（900、带一笔付款记录）的合法账本：用库自身
	// 在另一个路径生成，再移动到原路径，保证校验和完整。
	replacement := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	rl, err := Open(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rl.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("restored-pay", "aa", "usdc", 100),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := rl.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(replacement)
	if err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, path, restored)

	l, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after restoring a valid file: %v", err)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 900 {
		t.Fatalf("reopen must read the restored file, balance=%d want 900", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "restored-pay" {
		t.Fatalf("reopen must read the restored file's history: %+v", snap.Settlements)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// 恢复后若文件再次被损坏，由于已无活动句柄，Open 必须重新以磁盘为准
	// 报损坏，而不是沿用上一次成功读到的 900 余额。
	tampered := bytes.Clone(restored)
	tampered[10] ^= 0x01
	writeFileAt(t, path, tampered)
	assertOpenFails(t, path, ErrCorrupt)
}

// 损坏与未初始化必须可区分：同一路径上“文件缺失”与“文件为空”分别
// 返回 ledger_not_initialized 与 corrupt_ledger，绝不能都表现成空余额/
// 空历史的正常账本。
func TestMissingAndCorruptAreDistinguishableOnFirstOpen(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, err := Open(missing); KindOf(err) != ErrNotInit {
		t.Fatalf("missing: want %s, got %v", ErrNotInit, err)
	}

	empty := filepath.Join(t.TempDir(), "empty.json")
	writeFileAt(t, empty, nil)
	if _, err := Open(empty); KindOf(err) != ErrCorrupt {
		t.Fatalf("empty: want %s, got %v", ErrCorrupt, err)
	}
}

// 已有句柄在使用时，外部删改磁盘文件不影响进程内共享的完整状态：
// 第二次 Open 仍加入同一状态，付款结果立即可见，重复提交返回原记录与
// duplicate，没有再次扣款的机会。全部句柄关闭后，Open 才重新以磁盘为准。
func TestOpenWhileHandleActiveSharesStateAndDiskReignsAfterClose(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	first, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Results[0].Status != StatusSettled {
		t.Fatalf("setup: %+v", first.Results[0])
	}
	rec := *first.Results[0].Record

	// 句柄存活期间外部把文件改成损坏内容：进程内状态不受影响。
	tampered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered[10] ^= 0x01
	writeFileAt(t, path, tampered)

	l2, err := Open(path)
	if err != nil {
		t.Fatalf("open while another handle is active must share state, got %v", err)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("shared balance=%d want 30", bal)
	}
	dup, err := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := dup.Results[0]
	if d.Status != StatusDuplicate || d.Record == nil || *d.Record != rec {
		t.Fatalf("retry on shared state want duplicate with original record, got %+v", d)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("duplicate must not charge again: balance=%d", bal)
	}

	// 关闭一个句柄不影响另一个。
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("closing one handle disturbed the other: balance=%d", bal)
	}

	// 最后一个句柄仍打开时再 Open 仍共享内存状态（即便磁盘是损坏的）。
	l3, err := Open(path)
	if err != nil {
		t.Fatalf("open while last handle active: %v", err)
	}
	if bal, _ := l3.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("shared balance while active=%d want 30", bal)
	}

	// 全部句柄关闭：注册表项注销，此后磁盘（此刻为损坏内容）重新成为权威。
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l3.Close(); err != nil {
		t.Fatal(err)
	}
	assertOpenFails(t, path, ErrCorrupt)

	// 放回一份完好且与 l1 提交后等价（余额 30、p1 已扣款）的文件后，
	// 立即恢复正常，读到的是该文件的内容。
	fresh := filepath.Join(t.TempDir(), "fresh.json")
	if err := CreateLedger(fresh, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}}); err != nil {
		t.Fatal(err)
	}
	fl, err := Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fl.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
		intent("p1", "aa", "usdc", 70),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := fl.Close(); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, path, valid)

	l4, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after restoring valid file: %v", err)
	}
	if bal, _ := l4.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("restored ledger balance=%d want 30", bal)
	}
	snap, _ := l4.Query()
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" {
		t.Fatalf("restored ledger history: %+v", snap.Settlements)
	}
	if err := l4.Close(); err != nil {
		t.Fatal(err)
	}
}

// 初始化后无句柄时删除文件，允许在同一路径重新初始化（文件确实不存在）；
// 文件存在时重新初始化仍报 ledger_exists，旧行为不变。
func TestCreateAfterExternalDeleteAndRejectExisting(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 500}})

	// 文件仍在：重复初始化拒绝，不覆盖。
	if err := CreateLedger(path, []BalanceInit{{Account: "zz", Asset: "usdc", Balance: 1}}); KindOf(err) != ErrExists {
		t.Fatalf("re-init existing file want %s, got %v", ErrExists, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// 文件已被外部删除且无活动句柄：允许重新初始化。
	if err := CreateLedger(path, []BalanceInit{{Account: "cc", Asset: "usdc", Balance: 9}}); err != nil {
		t.Fatalf("re-init after external delete: %v", err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if bal, _ := l.Balance("cc", "usdc"); bal != 9 {
		t.Fatalf("re-initialized ledger balance=%d want 9", bal)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("old init balance must not survive: aa/usdc=%d", bal)
	}
	l.Close()
}
