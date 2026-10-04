package payflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// 本文件回归“CreateLedger 成功后、首次 Open 之前文件被改动”的行为：
//
//   - 首次打开必须以该路径此刻保存的账本为准：文件被删除返回
//     ledger_not_initialized；被清空、截断或改成校验和不匹配的内容返回
//     corrupt_ledger，且都不发放可用句柄。初始化成功这一事实不能代替
//     首次打开时的文件检查。
//   - 失败的打开不得重新生成账本、恢复初始余额、修补校验和或覆盖原文件；
//     同一路径放回合法账本后必须能再次打开并读到新文件的内容，
//     而不是继续返回初始化时的余额或沿用上一次错误。
//   - 文件保持完好时，创建后的首次打开仍正常得到原账户、资产与初始化
//     金额，且读取本身不改变文件。

// mustCreate 在指定路径创建账本（不经过 newTestLedger 的固定文件名）。
func mustCreate(t *testing.T, path string, init []BalanceInit) {
	t.Helper()
	if err := CreateLedger(path, init); err != nil {
		t.Fatalf("CreateLedger: %v", err)
	}
}

// openFails 断言 Open 以指定分类失败，且不返回可用句柄。
func openFails(t *testing.T, path, kind string) {
	t.Helper()
	l, err := Open(path)
	if KindOf(err) != kind {
		t.Fatalf("Open: want %s, got err=%v", kind, err)
	}
	if l != nil {
		t.Fatalf("failed Open must not return a handle, got %+v", l)
	}
}

// 创建后删除文件：首次打开返回 ledger_not_initialized，且不会重新生成账本。
func TestFirstOpenAfterCreateFileDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	mustCreate(t, path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	openFails(t, path, ErrNotInit)

	// 失败的打开不得重新生成账本文件。
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed open must not recreate the ledger file, stat err=%v", err)
	}
	// 再次打开仍报 ledger_not_initialized，不沿用任何缓存状态。
	openFails(t, path, ErrNotInit)
}

// 创建后文件被清空、截断或篡改校验内容：首次打开返回 corrupt_ledger，
// 原文件保持原样（不修补、不覆盖、不恢复初始余额）。
func TestFirstOpenAfterCreateFileCorrupted(t *testing.T) {
	cases := map[string]func(path string, raw []byte) []byte{
		"emptied":   func(string, []byte) []byte { return nil },
		"truncated": func(_ string, raw []byte) []byte { return raw[:len(raw)/2] },
		"checksum mismatch": func(_ string, raw []byte) []byte {
			return bytes.ReplaceAll(raw, []byte(`"balance":100`), []byte(`"balance":999`))
		},
		"garbage": func(string, []byte) []byte { return []byte("garbage{") },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.json")
			mustCreate(t, path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			bad := corrupt(path, raw)
			if err := os.WriteFile(path, bad, 0o600); err != nil {
				t.Fatal(err)
			}

			openFails(t, path, ErrCorrupt)

			// 失败的打开不得修补校验和、恢复初始余额或覆盖原文件。
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, bad) {
				t.Fatalf("failed open must leave the corrupt file untouched\n got: %q\nwant: %q", after, bad)
			}
			// 再次打开仍报 corrupt_ledger，不沿用上一次结果之外的状态。
			openFails(t, path, ErrCorrupt)
		})
	}
}

// 错误打开不阻塞后续正常使用：同一路径放回一个合法且校验完整的账本后，
// 再次打开读到的是该文件的内容，而不是初始化时的余额。
func TestFailedOpenThenValidFileOpensWithFileContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	mustCreate(t, path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})

	// 先制造一次失败的打开（文件被替换成校验和不匹配的内容）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.ReplaceAll(raw, []byte(`"balance":100`), []byte(`"balance":999`))
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	openFails(t, path, ErrCorrupt)

	// 放回一本内容不同的合法账本（另一账户、另一余额）。
	src := filepath.Join(dir, "src.json")
	mustCreate(t, src, []BalanceInit{{Account: "bb", Asset: "eth", Balance: 42}})
	valid, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}

	l := openOrFail(t, path)
	if bal, _ := l.Balance("bb", "eth"); bal != 42 {
		t.Fatalf("reopened ledger must serve the file's content: bb/eth=%d want 42", bal)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("stale initialization state leaked: aa/usdc=%d want 0", bal)
	}
	snap, _ := l.Query()
	if len(snap.Balances) != 1 || snap.Balances[0].Account != "bb" || len(snap.Settlements) != 0 {
		t.Fatalf("reopened snapshot must come from the replacement file: %+v", snap)
	}
}

// 文件保持完好时：创建后的首次打开正常得到原账户、资产与初始化金额，
// 且打开与查询本身不改变文件。
func TestFirstOpenAfterCreateIntactFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	mustCreate(t, path, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: 100},
		{Account: "bb", Asset: "eth", Balance: 7},
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	l := openOrFail(t, path)
	if bal, _ := l.Balance("aa", "usdc"); bal != 100 {
		t.Fatalf("aa/usdc=%d want 100", bal)
	}
	if bal, _ := l.Balance("bb", "eth"); bal != 7 {
		t.Fatalf("bb/eth=%d want 7", bal)
	}
	snap, _ := l.Query()
	if len(snap.Balances) != 2 || len(snap.Settlements) != 0 || len(snap.Refunds) != 0 {
		t.Fatalf("fresh ledger snapshot: %+v", snap)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("read-only open/query must not modify the file\n got: %q\nwant: %q", after, before)
	}
}

// 创建后首次打开成功、句柄仍在使用时，同进程再次打开共享当前完整状态：
// 已付款的余额与结算记录立即可见，重提同一付款返回原记录与 duplicate。
func TestFirstOpenThenSecondHandleSharesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	mustCreate(t, path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})

	l1 := openOrFail(t, path)
	res, err := l1.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 70)}})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("settle: %+v err=%v", res, err)
	}
	rec := *res.Results[0].Record

	l2 := openOrFail(t, path)
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("second handle balance=%d want 30", bal)
	}
	dup, _ := l2.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 70)}})
	d := dup.Results[0]
	if d.Status != StatusDuplicate || d.Record == nil || *d.Record != rec {
		t.Fatalf("second handle retry want duplicate with original record, got %+v", d)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("duplicate charged again: balance=%d want 30", bal)
	}
}
