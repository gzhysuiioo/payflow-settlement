package payflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 本文件回归初始化的“只许新建、绝不覆盖”语义：
//
//   - 同一实际目标上相互重叠的初始化（同进程并发，以及经真实目录与目录
//     软链接的两个视图）恰好一个成功，其余 ledger_exists，胜出账本的初始
//     余额完整、不混入落败请求的账户/资产/金额，历史为空；
//   - 胜出账本随后的付款与退款不会被较晚的初始化重置；
//   - 正常账本、损坏/空普通文件、目录、账本文件位置上的符号链接（含悬空
//     链接）一律按占用拒绝，原内容与链接去向不变，悬空链接不会被顺着创建；
//   - 经目录符号链接在尚未占用的位置创建新账本仍然允许；
//   - 存储故障与非法输入都不留半成品。

// startBarrier 让一批 goroutine 同时开跑，尽量制造真正重叠的临界区。
func startBarrier(n int) (wait <-chan struct{}, done func()) {
	ch := make(chan struct{})
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

// contenderInit 构造第 i 个竞争者彼此可区分的初始余额：
// 不同账户、不同金额，任何两份账本混合都能被发现。
func contenderInit(i int) []BalanceInit {
	return []BalanceInit{{
		Account: fmt.Sprintf("acct%02d", i),
		Asset:   "usdc",
		Balance: int64(1000 + i),
	}}
}

// 同进程并发初始化同一尚不存在的账本：恰好一个成功，其余全部
// ledger_exists；磁盘上的账本完整保留胜出方的余额，且付款/退款历史为空。
func TestCreateConcurrentExactlyOneWins(t *testing.T) {
	const n = 24
	path := filepath.Join(t.TempDir(), "ledger.json")

	wait, release := startBarrier(n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-wait
			errs[i] = CreateLedger(path, contenderInit(i))
		}(i)
	}
	release()
	wg.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner != -1 {
				t.Fatalf("more than one init succeeded: %d and %d", winner, i)
			}
			winner = i
			continue
		}
		if KindOf(err) != ErrExists {
			t.Fatalf("contender %d: want %s, got %v", i, ErrExists, err)
		}
	}
	if winner == -1 {
		t.Fatal("no init succeeded")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantAccount := fmt.Sprintf("acct%02d", winner)
	wantBlob := fmt.Sprintf(`"account":%q`, wantAccount)
	if !strings.Contains(string(raw), wantBlob) {
		t.Fatalf("ledger file does not belong to winner %d: %s", winner, raw)
	}
	for i := 0; i < n; i++ {
		if i == winner {
			continue
		}
		other := fmt.Sprintf(`"account":%q`, fmt.Sprintf("acct%02d", i))
		if strings.Contains(string(raw), other) {
			t.Fatalf("loser %d's account leaked into winner ledger: %s", i, raw)
		}
	}

	l := openOrFail(t, path)
	defer l.Close()
	snap, err := l.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 {
		t.Fatalf("winner ledger must have exactly one balance combo, got %+v", snap.Balances)
	}
	if got := snap.Balances[0]; got.Account != wantAccount || got.Balance != int64(1000+winner) {
		t.Fatalf("winner balance=%+v, want %s=%d", got, wantAccount, 1000+winner)
	}
	if len(snap.Settlements) != 0 || len(snap.Refunds) != 0 {
		t.Fatalf("fresh ledger history must be empty: %+v %+v", snap.Settlements, snap.Refunds)
	}
}

// 胜出账本随后产生付款与退款；之后一批并发初始化只能全部 ledger_exists，
// 余额、付款与退款历史既不被清空也不被重置成新账本的初始余额。
func TestCreateRushAfterPaymentsAndRefundsDoesNotReset(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 300)}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("submit: %+v", res.Results[0])
	}
	rr, err := l.Refund(RefundBatch{Refunds: []RefundRequest{
		{ID: "r1", SettlementID: "p1", Reason: "undo"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rr.Results[0].Status != StatusRefundSuccess {
		t.Fatalf("refund: %+v", rr.Results[0])
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("balance after refund=%d, want 1000", bal)
	}
	established, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	const m = 16
	wait, release := startBarrier(m)
	var wg sync.WaitGroup
	for i := 0; i < m; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-wait
			err := CreateLedger(path, []BalanceInit{{Account: "intruder", Asset: "usdc", Balance: 1}})
			if KindOf(err) != ErrExists {
				t.Errorf("late init: want %s, got %v", ErrExists, err)
			}
		}()
	}
	release()
	wg.Wait()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(established) != string(after) {
		t.Fatalf("late init rewrote the winner ledger\nbefore=%s\nafter=%s", established, after)
	}
	snap, err := l.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" {
		t.Fatalf("payment history reset: %+v", snap.Settlements)
	}
	if len(snap.Refunds) != 1 || snap.Refunds[0].ID != "r1" {
		t.Fatalf("refund history reset: %+v", snap.Refunds)
	}
	if bal, _ := l.Balance("intruder", "usdc"); bal != 0 {
		t.Fatalf("late init leaked its account into the winner ledger")
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1000 {
		t.Fatalf("winner balance disturbed: %d", bal)
	}
}

// 经目录符号链接在尚未占用的文件位置创建新账本：允许；
// 真实目录视图与软链接视图是同一目标，只能初始化一次。
func TestCreateThroughDirectorySymlink(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(realDir, "ledger.json")
	linkPath := filepath.Join(linkDir, "ledger.json")

	if err := CreateLedger(linkPath, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 42}}); err != nil {
		t.Fatalf("create through directory symlink: %v", err)
	}
	// 物理文件落在真实目录里，且是普通文件而非链接。
	fi, err := os.Lstat(realPath)
	if err != nil {
		t.Fatalf("physical ledger missing: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		t.Fatalf("ledger must be a regular file in the real directory, mode=%v", fi.Mode())
	}
	// 同一目标的另一个视图重复初始化：拒绝。
	if err := CreateLedger(realPath, contenderInit(9)); KindOf(err) != ErrExists {
		t.Fatalf("re-init via real view: want %s, got %v", ErrExists, err)
	}
	if err := CreateLedger(linkPath, contenderInit(9)); KindOf(err) != ErrExists {
		t.Fatalf("re-init via link view: want %s, got %v", ErrExists, err)
	}
	l := openOrFail(t, realPath)
	defer l.Close()
	if bal, _ := l.Balance("aa", "usdc"); bal != 42 {
		t.Fatalf("balance=%d, want 42", bal)
	}
}

// 一半竞争者走真实目录、一半走指向该目录的软链接：同一实际目标，
// 仍然恰好一个初始化成功。
func TestCreateConcurrentRealAndDirectorySymlinkViews(t *testing.T) {
	const n = 20
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(realDir, "ledger.json")
	linkPath := filepath.Join(linkDir, "ledger.json")

	wait, release := startBarrier(n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := realPath
			if i%2 == 1 {
				path = linkPath
			}
			<-wait
			errs[i] = CreateLedger(path, contenderInit(i))
		}(i)
	}
	release()
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		if KindOf(err) != ErrExists {
			t.Fatalf("contender %d: want %s, got %v", i, ErrExists, err)
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one init may win across real/symlink views, got %d", winners)
	}
	if _, err := os.Stat(realPath); err != nil {
		t.Fatalf("winner ledger missing in real directory: %v", err)
	}
}

// 各种已占用路径都必须返回 ledger_exists 且原样保留；
// 悬空符号链接既不被替换，也不被顺着创建目标文件。
func TestCreateRejectsOccupiedPaths(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string) string { return filepath.Join(dir, rel) }

	// 一份完好账本。
	good := mk("good.json")
	if err := CreateLedger(good, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 5}}); err != nil {
		t.Fatal(err)
	}
	goodBytes, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}

	// 文件位置符号链接指向的真实文件（含其原始字节）。
	linkTarget := mk("target.json")
	targetBytes := []byte(`{"version":1,"initial_balances":[],"balances":[],"settlements":[]}`)
	if err := os.WriteFile(linkTarget, targetBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// 悬空链接的（不存在的）去向。
	danglingDest := mk("ghost")

	cases := []struct {
		name  string
		path  string
		check func(t *testing.T)
	}{
		{
			name: "valid ledger",
			path: good,
			check: func(t *testing.T) {
				got, err := os.ReadFile(good)
				if err != nil || string(got) != string(goodBytes) {
					t.Fatalf("existing ledger modified: %v", err)
				}
			},
		},
		{
			name: "garbage regular file",
			path: mk("garbage.json"),
			check: func(t *testing.T) {
				p := mk("garbage.json")
				got, err := os.ReadFile(p)
				if err != nil || string(got) != "garbage{" {
					t.Fatalf("garbage file modified: %q %v", got, err)
				}
			},
		},
		{
			name: "empty regular file",
			path: mk("empty.json"),
			check: func(t *testing.T) {
				p := mk("empty.json")
				fi, err := os.Stat(p)
				if err != nil || fi.Size() != 0 {
					t.Fatalf("empty file must remain empty, stat=%v err=%v", fi, err)
				}
			},
		},
		{
			name: "directory",
			path: mk("adir"),
			check: func(t *testing.T) {
				p := mk("adir")
				fi, err := os.Stat(p)
				if err != nil || !fi.IsDir() {
					t.Fatalf("directory path must remain a directory, stat=%v err=%v", fi, err)
				}
				entries, err := os.ReadDir(p)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("init must not create files inside the directory: %v", entries)
				}
			},
		},
		{
			name: "symlink to existing file",
			path: mk("link-good.json"),
			check: func(t *testing.T) {
				p := mk("link-good.json")
				target, err := os.Readlink(p)
				if err != nil || target != "target.json" {
					t.Fatalf("symlink replaced: target=%q err=%v", target, err)
				}
				got, err := os.ReadFile(linkTarget)
				if err != nil || string(got) != string(targetBytes) {
					t.Fatalf("symlink target modified: %v", err)
				}
			},
		},
		{
			name: "dangling symlink",
			path: mk("link-dangling.json"),
			check: func(t *testing.T) {
				p := mk("link-dangling.json")
				target, err := os.Readlink(p)
				if err != nil || target != "ghost" {
					t.Fatalf("dangling symlink replaced: target=%q err=%v", target, err)
				}
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("link must still dangle, stat err=%v", err)
				}
				if _, err := os.Stat(danglingDest); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("init must not create the dangling link destination, stat err=%v", err)
				}
			},
		},
		{
			name: "symlink to directory",
			path: mk("link-dir.json"),
			check: func(t *testing.T) {
				p := mk("link-dir.json")
				target, err := os.Readlink(p)
				if err != nil || target != "realdir" {
					t.Fatalf("directory symlink replaced: target=%q err=%v", target, err)
				}
				entries, err := os.ReadDir(mk("realdir"))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("init must not create files through a file-position symlink: %v", entries)
				}
			},
		},
	}

	// 布置各占用形态。
	if err := os.WriteFile(mk("garbage.json"), []byte("garbage{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mk("empty.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mk("adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mk("realdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.json", mk("link-good.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ghost", mk("link-dangling.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("realdir", mk("link-dir.json")); err != nil {
		t.Fatal(err)
	}

	intruder := []BalanceInit{{Account: "zz", Asset: "usdc", Balance: 7}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CreateLedger(tc.path, intruder)
			if KindOf(err) != ErrExists {
				t.Fatalf("want %s, got %v", ErrExists, err)
			}
			tc.check(t)
		})
	}
}

// 非法输入优先于占用判定：路径是悬空符号链接时，不合法余额仍返回
// invalid_parameter，链接保持悬空、目标不被创建。
func TestCreateInvalidInputCheckedBeforeOccupiedPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	if err := os.Symlink("ghost", path); err != nil {
		t.Fatal(err)
	}
	err := CreateLedger(path, []BalanceInit{{Account: "", Asset: "usdc", Balance: 1}})
	if KindOf(err) != ErrInvalid {
		t.Fatalf("want %s, got %v", ErrInvalid, err)
	}
	if target, rerr := os.Readlink(path); rerr != nil || target != "ghost" {
		t.Fatalf("link altered: target=%q err=%v", target, rerr)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghost")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid init must not create link destination, err=%v", err)
	}
}

// 独占创建成功后内容写入失败：返回 storage_error，半成品被删除，
// 不会留下能被查询当成新账本的空壳；随后合法初始化可以成功。
func TestCreateWriteFailureLeavesNoLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	failCreateWriteHook = func() bool { return true }
	t.Cleanup(func() { failCreateWriteHook = nil })

	err := CreateLedger(path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1}})
	if KindOf(err) != ErrStorage {
		t.Fatalf("want %s, got %v", ErrStorage, err)
	}
	if fi, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed init must remove its half-written ledger, stat=%v err=%v", fi, statErr)
	}
	if _, oerr := Open(path); KindOf(oerr) != ErrNotInit {
		t.Fatalf("no ledger may be readable after failed init: %v", oerr)
	}

	failCreateWriteHook = nil
	if err := CreateLedger(path, []BalanceInit{{Account: "bb", Asset: "usdc", Balance: 9}}); err != nil {
		t.Fatalf("retry init after storage failure: %v", err)
	}
	l := openOrFail(t, path)
	defer l.Close()
	if bal, _ := l.Balance("bb", "usdc"); bal != 9 {
		t.Fatalf("retry ledger balance=%d, want 9", bal)
	}
}

// 有活动句柄时文件即便被外部删除，也不允许重新初始化顶替在用账本。
func TestCreateRejectedWhileHandleActiveEvenIfFileDeleted(t *testing.T) {
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 100}})
	l := openOrFail(t, path)

	res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent("p1", "aa", "usdc", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != StatusSettled {
		t.Fatalf("setup submit: %+v", res.Results[0])
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(path, contenderInit(1)); KindOf(err) != ErrExists {
		t.Fatalf("init with active handle: want %s, got %v", ErrExists, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected init must not recreate the file behind the active handle, err=%v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// 句柄全部关闭、文件确实不存在后，允许重新初始化（既有语义不变）。
	if err := CreateLedger(path, contenderInit(2)); err != nil {
		t.Fatalf("init after handles closed and file externally deleted: %v", err)
	}
}
