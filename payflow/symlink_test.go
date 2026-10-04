package payflow

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// symlinkPaths 在临时目录中创建账本，并额外建立一个指向账本所在目录的软链接，
// 返回账本的真实路径与经软链接目录的路径。两者指向同一本账本。
func symlinkPaths(t *testing.T, init []BalanceInit) (realPath, linkPath string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "real")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	realPath = filepath.Join(dir, "ledger.json")
	if err := CreateLedger(realPath, init); err != nil {
		t.Fatalf("CreateLedger: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	linkPath = filepath.Join(link, "ledger.json")
	return realPath, linkPath
}

// 两个句柄经真实目录与软链接目录打开同一本账本：任一方成功付款后，另一方
// 提交完全相同的请求返回 duplicate 与原记录，不再扣款；改动字段或费率返回
// conflict，保留原记录。任一路径查询到相同的余额与成功历史。
func TestSymlinkHandlesShareIdempotencyAndBalance(t *testing.T) {
	realPath, linkPath := symlinkPaths(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})

	lReal := openOrFail(t, realPath)
	lLink := openOrFail(t, linkPath)

	base := intent("p1", "aa", "usdc", 300)
	res, err := lReal.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{base}})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("settle via real path: %v %+v", err, res.Results)
	}

	// 软链接句柄立即看到扣款。
	if bal, _ := lLink.Balance("aa", "usdc"); bal != 700 {
		t.Fatalf("link handle balance=%d want 700", bal)
	}

	// 经软链接提交完全相同请求：duplicate + 原记录，不重复扣款。
	dup, err := lLink.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{base}})
	if err != nil {
		t.Fatal(err)
	}
	d := dup.Results[0]
	if d.Status != StatusDuplicate || d.Record == nil || d.Record.Seq != 1 || d.Record.Charged != 300 {
		t.Fatalf("cross-handle duplicate: %+v", d)
	}

	// 经软链接用同一编号修改字段或费率：conflict，原记录保留。
	for i, mut := range []func(*PaymentIntent, *int){
		func(it *PaymentIntent, bps *int) { *it.Amount = 301 },
		func(it *PaymentIntent, bps *int) { *bps = 1 },
		func(it *PaymentIntent, bps *int) { it.Account = "bb" },
	} {
		it := base
		bps := 0
		mut(&it, &bps)
		cf, err := lLink.Submit(FeeBatch{FeeBps: bps, Intents: []PaymentIntent{it}})
		if err != nil {
			t.Fatal(err)
		}
		if cf.Results[0].Status != StatusConflict {
			t.Fatalf("case %d want conflict, got %+v", i, cf.Results[0])
		}
	}

	// 两条路径查询一致：同一余额、同一条成功历史。
	snapReal, _ := lReal.Query()
	snapLink, _ := lLink.Query()
	if len(snapReal.Settlements) != 1 || snapReal.Settlements[0].ID != "p1" {
		t.Fatalf("real settlements=%+v", snapReal.Settlements)
	}
	if len(snapLink.Settlements) != 1 || snapLink.Settlements[0].ID != "p1" ||
		snapLink.Settlements[0].Amount != 300 || snapLink.Settlements[0].FeeBps != 0 {
		t.Fatalf("link settlements diverged: %+v", snapLink.Settlements)
	}
	if len(snapReal.Balances) != len(snapLink.Balances) ||
		snapReal.Balances[0].Balance != 700 || snapLink.Balances[0].Balance != 700 {
		t.Fatalf("balances diverged: real=%+v link=%+v", snapReal.Balances, snapLink.Balances)
	}

	// 软链接路径先打开的句柄也共享同一注册表项（反向顺序）。
	lReal.Close()
	lLink.Close()
	a := openOrFail(t, linkPath)
	b := openOrFail(t, realPath)
	if bal, _ := b.Balance("aa", "usdc"); bal != 700 {
		t.Fatalf("reverse open order balance=%d want 700", bal)
	}
	a.Close()
	b.Close()
}

// 不同付款编号基于同一份可用余额：余额 100、费率 0，两个句柄分别提交
// 70 与 40，只能有一笔 settled，另一笔 insufficient_balance；最终余额、
// 文件记录与实际成功的那笔一致。
func TestSymlinkHandlesContendedBalance(t *testing.T) {
	for _, order := range [][2]string{{"real-first", "real"}, {"link-first", "link"}} {
		t.Run(order[0], func(t *testing.T) {
			realPath, linkPath := symlinkPaths(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
			lReal, lLink := openOrFail(t, realPath), openOrFail(t, linkPath)
			first, second := lReal, lLink
			if order[1] == "link" {
				first, second = lLink, lReal
			}

			r1, err := first.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("a", "aa", "usdc", 70)}})
			if err != nil {
				t.Fatal(err)
			}
			r2, err := second.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("b", "aa", "usdc", 40)}})
			if err != nil {
				t.Fatal(err)
			}
			if r1.Results[0].Status != StatusSettled {
				t.Fatalf("first want settled, got %+v", r1.Results[0])
			}
			if r2.Results[0].Status != StatusFunds {
				t.Fatalf("second want insufficient_balance, got %+v", r2.Results[0])
			}

			for _, l := range []*Ledger{first, second} {
				if bal, _ := l.Balance("aa", "usdc"); bal != 30 {
					t.Fatalf("balance=%d want 30", bal)
				}
				snap, _ := l.Query()
				if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "a" || snap.Settlements[0].Seq != 1 {
					t.Fatalf("settlements=%+v want only a/seq1", snap.Settlements)
				}
			}

			// 关闭全部句柄后从磁盘重开：文件中的记录与余额和成功那笔一致。
			first.Close()
			second.Close()
			reopened := openOrFail(t, linkPath)
			if bal, _ := reopened.Balance("aa", "usdc"); bal != 30 {
				t.Fatalf("reopened balance=%d want 30", bal)
			}
			snap, _ := reopened.Query()
			if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "a" {
				t.Fatalf("reopened settlements=%+v", snap.Settlements)
			}
			// 失败编号不占用成功序号：之后可成功，序号为 2。
			ok, err := reopened.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("b", "aa", "usdc", 30)}})
			if err != nil || ok.Results[0].Status != StatusSettled || ok.Results[0].Record.Seq != 2 {
				t.Fatalf("retry after insufficient: %v %+v", err, ok.Results)
			}
		})
	}
}

// 两个句柄并发提交争用同一余额：绝不能两笔都报告 settled 后再由最后一次
// 保存决定结果。
func TestSymlinkHandlesConcurrentContention(t *testing.T) {
	realPath, linkPath := symlinkPaths(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	const n = 40
	var wg sync.WaitGroup
	counts := map[string]int{}
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		i := i
		path := realPath
		if i%2 == 0 {
			path = linkPath
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer l.Close()
			res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{
				intent(fmt.Sprintf("p-%d", i), "aa", "usdc", 70),
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
	l := openOrFail(t, realPath)
	if bal, _ := l.Balance("aa", "usdc"); bal != 30 {
		t.Fatalf("balance=%d want 30", bal)
	}
	snap, _ := l.Query()
	if len(snap.Settlements) != 1 {
		t.Fatalf("records=%d want 1: %+v", len(snap.Settlements), snap.Settlements)
	}
}

// 同一编号经两个句柄并发提交：恰有一笔 settled，其余 duplicate。
func TestSymlinkHandlesConcurrentSameID(t *testing.T) {
	realPath, linkPath := symlinkPaths(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	const n = 30
	var wg sync.WaitGroup
	counts := map[string]int{}
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		path := realPath
		if i%2 == 0 {
			path = linkPath
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer l.Close()
			res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("only", "aa", "usdc", 10)}})
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
	if counts[StatusSettled] != 1 || counts[StatusDuplicate] != n-1 {
		t.Fatalf("counts=%v want 1 settled / %d duplicate", counts, n-1)
	}
}

// 关闭一个句柄不影响另一个仍在使用的句柄；已完成的付款不丢失。
func TestSymlinkCloseOneHandleKeepsOther(t *testing.T) {
	realPath, linkPath := symlinkPaths(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	lReal := openOrFail(t, realPath)
	lLink := openOrFail(t, linkPath)

	if _, err := lReal.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("a", "aa", "usdc", 60)}}); err != nil {
		t.Fatal(err)
	}
	if err := lLink.Close(); err != nil {
		t.Fatal(err)
	}

	// 真实句柄仍可继续付款、查询，状态未丢。
	res, err := lReal.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("b", "aa", "usdc", 40)}})
	if err != nil || res.Results[0].Status != StatusSettled {
		t.Fatalf("submit after peer close: %v %+v", err, res.Results)
	}
	if bal, _ := lReal.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("balance=%d want 0", bal)
	}
	lReal.Close()

	// 再开（经软链接）从磁盘恢复，两笔都在。
	reopened := openOrFail(t, linkPath)
	snap, _ := reopened.Query()
	if len(snap.Settlements) != 2 || snap.Settlements[0].ID != "a" || snap.Settlements[1].ID != "b" {
		t.Fatalf("history after close/reopen: %+v", snap.Settlements)
	}
}

// 经软链接首次打开：账本不存在报 ledger_not_initialized，损坏报 corrupt_ledger，
// 均不得创建或修复账本。
func TestSymlinkOpenMissingAndCorrupt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "real")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	linkPath := filepath.Join(link, "ledger.json")

	if _, err := Open(linkPath); KindOf(err) != ErrNotInit {
		t.Fatalf("missing via symlink want ErrNotInit, got %v", err)
	}
	if _, err := os.Stat(linkPath); !os.IsNotExist(err) {
		t.Fatalf("failed open must not create file, stat=%v", err)
	}

	if err := os.WriteFile(linkPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(linkPath); KindOf(err) != ErrCorrupt {
		t.Fatalf("corrupt via symlink want ErrCorrupt, got %v", err)
	}
	// 损坏文件原封不动，未被修复或替换。
	if data, err := os.ReadFile(linkPath); err != nil || string(data) != "{not json" {
		t.Fatalf("corrupt file must remain untouched, data=%q err=%v", data, err)
	}

	// 损坏文件不能通过再次初始化“重开一本空账”（与真实路径行为一致）。
	if err := CreateLedger(linkPath, nil); KindOf(err) != ErrExists {
		t.Fatalf("init over corrupt file via symlink want ErrExists, got %v", err)
	}

	// 经软链接初始化一本新账，真实路径应立即可见，且两路径共享同一注册表项。
	newLink := filepath.Join(link, "new.json")
	newReal := filepath.Join(dir, "new.json")
	if err := CreateLedger(newLink, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 5}}); err != nil {
		t.Fatalf("CreateLedger via symlink: %v", err)
	}
	if _, err := os.Stat(newReal); err != nil {
		t.Fatalf("created file not visible at real path: %v", err)
	}
	if err := CreateLedger(newReal, nil); KindOf(err) != ErrExists {
		t.Fatalf("create at real path after symlink init want ErrExists, got %v", err)
	}
	// 经软链接打开后能读到初始化余额，证明两路径落到同一注册表项。
	l := openOrFail(t, newLink)
	if bal, _ := l.Balance("aa", "usdc"); bal != 5 {
		t.Fatalf("balance after create-via-symlink=%d want 5", bal)
	}
}
