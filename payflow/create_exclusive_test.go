package payflow

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 本文件回归 init 的排他创建语义：
//
//   - 同一实际目标上相互重叠的初始化请求（同进程多 goroutine、跨多进程、
//     一方经真实目录另一方经指向该目录的符号链接）只能有一个成功，
//     其余返回 ledger_exists；胜出账本的初始余额完整、不混入败者数据；
//   - 正常账本、损坏/空普通文件、目录、账本文件位置上的符号链接
//     （含悬空链接）一律按已占用拒绝，原内容与链接去向不变，
//     悬空链接的目标不会被顺手创建；
//   - 仍允许经目录符号链接在尚未占用的文件位置创建新账本；
//   - 非法输入优先返回 invalid_parameter 且不留任何文件；
//   - 创建/写入失败返回 storage_error 且不留半成品；
//   - 胜出账本随后产生的付款与退款不会被较晚结束的初始化重置。

// ---- 跨进程辅助：把测试二进制本身当作会调用 CreateLedger 的独立进程 ----

const (
	helperEnv     = "GO_WANT_HELPER_PROCESS"
	envLedger     = "PAYFLOW_HELPER_LEDGER"
	envInitIdx    = "PAYFLOW_HELPER_INIT_IDX"
	envGate       = "PAYFLOW_HELPER_GATE"
	envResult     = "PAYFLOW_HELPER_RESULT"
	helperBalBase = 1000
)

// TestHelperCreateProcess 是“另一个独立进程”的入口：
// 等待闸门文件出现后，以属于本进程编号的唯一初始余额调用 CreateLedger，
// 把结果（ok:<编号> 或 err:<kind>）写入结果文件，并以 0/1 退出。
func TestHelperCreateProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process only")
	}
	path := os.Getenv(envLedger)
	idx, err := strconv.Atoi(os.Getenv(envInitIdx))
	if err != nil {
		os.Exit(2)
	}
	gate := os.Getenv(envGate)
	result := os.Getenv(envResult)

	init := []BalanceInit{{
		Account: fmt.Sprintf("winner-%02d", idx),
		Asset:   "usdc",
		Balance: int64(helperBalBase + idx),
	}}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	if err := CreateLedger(path, init); err != nil {
		_ = os.WriteFile(result, []byte("err:"+KindOf(err)), 0o600)
		os.Exit(1)
	}
	_ = os.WriteFile(result, []byte("ok:"+strconv.Itoa(idx)), 0o600)
	os.Exit(0)
}

type helperOutcome struct {
	idx      int
	exitCode int
	result   string
	stderr   string
}

// runConcurrentCreateHelpers 启动 n 个独立进程，让它们在闸门打开后同时
// 对 pathFor(i) 指向的目标执行初始化，返回每个进程的退出码与结果。
func runConcurrentCreateHelpers(t *testing.T, n int, pathFor func(i int) string) []helperOutcome {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	work := t.TempDir()
	gate := filepath.Join(work, "gate")
	resDir := filepath.Join(work, "results")
	if err := os.Mkdir(resDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cmds := make([]*exec.Cmd, n)
	for i := 0; i < n; i++ {
		resultFile := filepath.Join(resDir, fmt.Sprintf("r%d", i))
		cmd := exec.Command(exe, "-test.run=^TestHelperCreateProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			helperEnv+"=1",
			envLedger+"="+pathFor(i),
			envInitIdx+"="+strconv.Itoa(i),
			envGate+"="+gate,
			envResult+"="+resultFile,
		)
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", i, err)
		}
		cmds[i] = cmd
	}

	// 等所有进程都进入等待后再统一放行，最大化 init 的重叠窗口。
	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcomes := make([]helperOutcome, n)
	var wg sync.WaitGroup
	for i, cmd := range cmds {
		wg.Add(1)
		go func(i int, cmd *exec.Cmd) {
			defer wg.Done()
			err := cmd.Wait()
			code := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					code = -1
				}
			}
			res, _ := os.ReadFile(filepath.Join(work, "results", fmt.Sprintf("r%d", i)))
			outcomes[i] = helperOutcome{idx: i, exitCode: code, result: string(res)}
		}(i, cmd)
	}
	wg.Wait()
	return outcomes
}

// assertExactlyOneInitWinner 断言一组并发初始化恰好一个成功，
// 其余全部 ledger_exists（退出码 1），并返回胜出者编号。
func assertExactlyOneInitWinner(t *testing.T, outcomes []helperOutcome) int {
	t.Helper()
	winner := -1
	for _, o := range outcomes {
		switch {
		case len(o.result) >= 3 && o.result[:3] == "ok:":
			if winner != -1 {
				t.Fatalf("two inits succeeded: %d and %d: %+v", winner, o.idx, outcomes)
			}
			idx, err := strconv.Atoi(o.result[3:])
			if err != nil {
				t.Fatalf("bad winner result %q: %v", o.result, err)
			}
			winner = idx
			if o.exitCode != 0 {
				t.Fatalf("winner %d exit code=%d", o.idx, o.exitCode)
			}
		case o.result == "err:"+ErrExists:
			if o.exitCode != 1 {
				t.Fatalf("loser %d exit code=%d want 1", o.idx, o.exitCode)
			}
		default:
			t.Fatalf("helper %d unexpected outcome: code=%d result=%q", o.idx, o.exitCode, o.result)
		}
	}
	if winner == -1 {
		t.Fatalf("no init succeeded: %+v", outcomes)
	}
	return winner
}

// assertWinnerLedgerIntact 打开账本，断言其中只有胜出者的初始余额，
// 没有任何其他请求的账户/资产，付款与退款历史为空。
func assertWinnerLedgerIntact(t *testing.T, path string, winner int) {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("open winning ledger: %v", err)
	}
	defer l.Close()
	snap, err := l.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Settlements) != 0 || len(snap.Refunds) != 0 {
		t.Fatalf("fresh winning ledger must have empty history: %+v", snap)
	}
	wantAccount := fmt.Sprintf("winner-%02d", winner)
	wantBalance := int64(helperBalBase + winner)
	if len(snap.Balances) != 1 {
		t.Fatalf("winner ledger must contain exactly one combo, got %+v", snap.Balances)
	}
	b := snap.Balances[0]
	if b.Account != wantAccount || b.Asset != "usdc" || b.Balance != wantBalance {
		t.Fatalf("winner balances=%+v want %s/usdc=%d (no mixing from losers)",
			b, wantAccount, wantBalance)
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	leftover, err := filepath.Glob(filepath.Join(dir, ".ledger-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Fatalf("temporary init files left behind: %v", leftover)
	}
}

// 同进程内大量 goroutine 同时初始化同一不存在的账本：恰好一个成功。
func TestConcurrentCreateInProcessExactlyOneWinner(t *testing.T) {
	for round := 0; round < 20; round++ {
		path := filepath.Join(t.TempDir(), "ledger.json")
		const n = 16
		var wg sync.WaitGroup
		errs := make([]error, n)
		var winnerMu sync.Mutex
		winnerCount := 0
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				init := []BalanceInit{{
					Account: fmt.Sprintf("g-%02d", i),
					Asset:   "usdc",
					Balance: int64(100 + i),
				}}
				<-start
				err := CreateLedger(path, init)
				errs[i] = err
				if err == nil {
					winnerMu.Lock()
					winnerCount++
					winnerMu.Unlock()
				}
			}(i)
		}
		close(start)
		wg.Wait()

		if winnerCount != 1 {
			t.Fatalf("round %d: winners=%d want exactly 1", round, winnerCount)
		}
		for i, err := range errs {
			if err == nil {
				continue
			}
			if KindOf(err) != ErrExists {
				t.Fatalf("round %d loser %d: want %s, got %v", round, i, ErrExists, err)
			}
		}
		assertWinnerLedgerIntactDir(t, path)
	}
}

// assertWinnerLedgerIntactDir 用于 goroutine 测试：从文件中读取出胜出账户。
func assertWinnerLedgerIntactDir(t *testing.T, path string) {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer l.Close()
	snap, err := l.Query()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Balances) != 1 || len(snap.Settlements) != 0 || len(snap.Refunds) != 0 {
		t.Fatalf("want exactly one untouched combo and empty history, got %+v", snap)
	}
}

// 跨进程同时初始化同一尚不存在的账本：恰好一个进程 initialized，
// 其余全部 ledger_exists；磁盘上完整保留胜出者余额。
func TestConcurrentCreateAcrossProcessesExactlyOneWinner(t *testing.T) {
	for round := 0; round < 5; round++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "ledger.json")
		outcomes := runConcurrentCreateHelpers(t, 10, func(i int) string { return path })
		winner := assertExactlyOneInitWinner(t, outcomes)
		assertWinnerLedgerIntact(t, path, winner)
		assertNoTempFiles(t, dir)
	}
}

// 一方经真实目录、另一方经指向该目录的符号链接并发初始化同一目标：
// 同样只能有一个胜出者，账本落在真实目录里。
func TestConcurrentCreateAcrossProcessesViaDirSymlink(t *testing.T) {
	for round := 0; round < 5; round++ {
		base := t.TempDir()
		realDir := filepath.Join(base, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(base, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		realPath := filepath.Join(realDir, "ledger.json")
		linkPath := filepath.Join(linkDir, "ledger.json")

		outcomes := runConcurrentCreateHelpers(t, 10, func(i int) string {
			if i%2 == 0 {
				return realPath
			}
			return linkPath
		})
		winner := assertExactlyOneInitWinner(t, outcomes)
		// 账本必须只存在于真实目录中（符号链接本身仍是目录链接）。
		if fi, err := os.Lstat(linkDir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("directory symlink must remain a symlink: %v", err)
		}
		assertWinnerLedgerIntact(t, realPath, winner)
		assertWinnerLedgerIntact(t, linkPath, winner)
		assertNoTempFiles(t, realDir)
	}
}

// 已占用路径矩阵：正常账本、空/损坏普通文件、目录、账本位置上的各种
// 符号链接都拒绝初始化；原内容与链接保持不变。
func TestCreateRejectsOccupiedPaths(t *testing.T) {
	other := []BalanceInit{{Account: "zz", Asset: "eth", Balance: 1}}

	// 1. 已有正常账本：拒绝且逐字节不变。
	existing := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 500}})
	before, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(existing, other); KindOf(err) != ErrExists {
		t.Fatalf("existing ledger: want %s, got %v", ErrExists, err)
	}
	after, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("rejected init must not rewrite the existing ledger")
	}

	dir := t.TempDir()

	// 2. 空普通文件。
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(empty, other); KindOf(err) != ErrExists {
		t.Fatalf("empty file: want %s, got %v", ErrExists, err)
	}
	if fi, err := os.Stat(empty); err != nil || fi.Size() != 0 || fi.IsDir() {
		t.Fatalf("empty file must remain an empty regular file: %+v %v", fi, err)
	}

	// 3. 损坏的普通文件。
	garbage := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(garbage, []byte("not-a-ledger{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(garbage, other); KindOf(err) != ErrExists {
		t.Fatalf("garbage file: want %s, got %v", ErrExists, err)
	}
	got, err := os.ReadFile(garbage)
	if err != nil || string(got) != "not-a-ledger{" {
		t.Fatalf("garbage file content changed: %q %v", got, err)
	}

	// 4. 目录。
	subdir := filepath.Join(dir, "adir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(subdir, other); KindOf(err) != ErrExists {
		t.Fatalf("directory: want %s, got %v", ErrExists, err)
	}
	if fi, err := os.Stat(subdir); err != nil || !fi.IsDir() {
		t.Fatalf("directory must remain a directory: %+v %v", fi, err)
	}

	// 5. 悬空符号链接：拒绝；链接保留；目标文件不被创建。
	ghostTarget := filepath.Join(dir, "ghost-ledger.json")
	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink(ghostTarget, dangling); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(dangling, other); KindOf(err) != ErrExists {
		t.Fatalf("dangling symlink: want %s, got %v", ErrExists, err)
	}
	if fi, err := os.Lstat(dangling); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dangling symlink must remain a symlink: %+v %v", fi, err)
	}
	if dest, err := os.Readlink(dangling); err != nil || dest != ghostTarget {
		t.Fatalf("dangling symlink target changed: %q %v", dest, err)
	}
	if _, err := os.Stat(ghostTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling link target must not be created, stat err=%v", err)
	}

	// 6. 指向正常账本文件的符号链接：拒绝；链接与目标文件都不变。
	linkToFile := filepath.Join(dir, "to-file.json")
	if err := os.Symlink(existing, linkToFile); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(linkToFile, other); KindOf(err) != ErrExists {
		t.Fatalf("symlink to file: want %s, got %v", ErrExists, err)
	}
	if dest, _ := os.Readlink(linkToFile); dest != existing {
		t.Fatalf("file symlink target changed: %q", dest)
	}
	after2, _ := os.ReadFile(existing)
	if !bytes.Equal(before, after2) {
		t.Fatalf("symlinked ledger content must not change")
	}

	// 7. 指向目录的符号链接：拒绝；链接保留；目标目录内不创建账本。
	realDir2 := filepath.Join(dir, "realdir")
	if err := os.Mkdir(realDir2, 0o700); err != nil {
		t.Fatal(err)
	}
	linkToDir := filepath.Join(dir, "to-dir.json")
	if err := os.Symlink(realDir2, linkToDir); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(linkToDir, other); KindOf(err) != ErrExists {
		t.Fatalf("symlink to dir: want %s, got %v", ErrExists, err)
	}
	if dest, _ := os.Readlink(linkToDir); dest != realDir2 {
		t.Fatalf("dir symlink target changed: %q", dest)
	}
	if entries, _ := os.ReadDir(realDir2); len(entries) != 0 {
		t.Fatalf("nothing must be created behind the directory symlink: %v", entries)
	}

	// 8. 正向：经目录符号链接在尚未占用的文件位置创建成功，
	//    账本落在真实目录，经两条路径打开一致；之后任一路径再初始化都拒绝。
	base := t.TempDir()
	realD := filepath.Join(base, "real")
	if err := os.Mkdir(realD, 0o700); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(base, "dirlink")
	if err := os.Symlink(realD, dirLink); err != nil {
		t.Fatal(err)
	}
	viaReal := filepath.Join(realD, "ledger.json")
	viaLink := filepath.Join(dirLink, "ledger.json")
	if err := CreateLedger(viaLink, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 7}}); err != nil {
		t.Fatalf("create through directory symlink at unoccupied location: %v", err)
	}
	if _, err := os.Stat(viaReal); err != nil {
		t.Fatalf("ledger must land in the real directory: %v", err)
	}
	l, err := Open(viaReal)
	if err != nil {
		t.Fatalf("open via real path: %v", err)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 7 {
		t.Fatalf("balance via real path=%d want 7", bal)
	}
	l2, err := Open(viaLink)
	if err != nil {
		t.Fatalf("open via directory symlink: %v", err)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 7 {
		t.Fatalf("balance via symlink path=%d want 7", bal)
	}
	l.Close()
	l2.Close()
	if err := CreateLedger(viaReal, other); KindOf(err) != ErrExists {
		t.Fatalf("re-init via real path: want %s, got %v", ErrExists, err)
	}
	if err := CreateLedger(viaLink, other); KindOf(err) != ErrExists {
		t.Fatalf("re-init via symlink path: want %s, got %v", ErrExists, err)
	}
}

// 非法输入优先：即便路径已占用（含悬空符号链接），仍返回 invalid_parameter；
// 在空闲路径上失败时不留下任何文件，之后同一路径仍可正常初始化。
func TestCreateInvalidInputPrecedesOccupancyAndLeavesNothing(t *testing.T) {
	bad := []BalanceInit{{Account: "aa", Asset: "usdc", Balance: -1}}

	// 空闲路径：非法输入不留账本、不留临时文件。
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	if err := CreateLedger(path, bad); KindOf(err) != ErrInvalid {
		t.Fatalf("want %s, got %v", ErrInvalid, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid init must not create ledger, stat err=%v", err)
	}
	assertNoTempFiles(t, dir)
	if err := CreateLedger(path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 3}}); err != nil {
		t.Fatalf("valid init after rejected init: %v", err)
	}

	// 已占用的正常账本：校验优先，返回 invalid_parameter，文件不变。
	existing := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 500}})
	before, _ := os.ReadFile(existing)
	if err := CreateLedger(existing, bad); KindOf(err) != ErrInvalid {
		t.Fatalf("invalid input must be reported before occupancy: %v", err)
	}
	after, _ := os.ReadFile(existing)
	if !bytes.Equal(before, after) {
		t.Fatalf("invalid init over existing ledger must not touch the file")
	}

	// 悬空符号链接 + 非法输入：invalid_parameter，链接与目标状态不变。
	base := t.TempDir()
	target := filepath.Join(base, "ghost")
	link := filepath.Join(base, "dangle")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(link, bad); KindOf(err) != ErrInvalid {
		t.Fatalf("invalid input over dangling symlink: want %s, got %v", ErrInvalid, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink must remain intact after invalid init: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target must not be created, stat err=%v", err)
	}
}

// 目录不可写时初始化返回 storage_error，路径上不留下账本或半成品；
// 恢复可写后同一路径可以正常初始化。
func TestCreateStorageFailureLeavesNoLedger(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "ledger.json")

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err := CreateLedger(path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1}})
	if KindOf(err) != ErrStorage {
		t.Fatalf("unwritable dir: want %s, got %v", ErrStorage, err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("storage failure must not leave a ledger file, stat err=%v", statErr)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CreateLedger(path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1}}); err != nil {
		t.Fatalf("init after storage failure: %v", err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatalf("open after recovered init: %v", err)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 1 {
		t.Fatalf("balance=%d want 1", bal)
	}
	l.Close()
}

// createExclusive 在路径占用时返回 ErrExists 并清理自己的临时文件，
// 先到的内容逐字节保留；悬空符号链接同样不留下临时文件。
func TestCreateExclusiveCleansTempOnOccupied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	if err := createExclusive(path, map[string]any{"hello": "world"}); err != nil {
		t.Fatalf("first createExclusive: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := createExclusive(path, map[string]any{"hello": "other"}); KindOf(err) != ErrExists {
		t.Fatalf("second createExclusive: want %s, got %v", ErrExists, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatalf("lost createExclusive must not rewrite the winner")
	}
	assertNoTempFiles(t, dir)

	ghost := filepath.Join(dir, "ghost")
	dangling := filepath.Join(dir, "dangle")
	if err := os.Symlink(ghost, dangling); err != nil {
		t.Fatal(err)
	}
	if err := createExclusive(dangling, map[string]any{"x": 1}); KindOf(err) != ErrExists {
		t.Fatalf("createExclusive on dangling symlink: want %s, got %v", ErrExists, err)
	}
	assertNoTempFiles(t, dir)
	if _, err := os.Stat(ghost); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ghost target must not appear, stat err=%v", err)
	}
}

// 胜出账本正常使用期间，另一个进程的初始化“更晚结束”也不能重置
// 已产生的付款与退款；败者的初始余额不得混入。
func TestLateInitFromOtherProcessCannotResetHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	if err := CreateLedger(path, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: 1000}}); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	submit := func(id string, amount int64) {
		t.Helper()
		res, err := l.Submit(FeeBatch{FeeBps: 0, Intents: []PaymentIntent{intent(id, "aa", "usdc", amount)}})
		if err != nil {
			t.Fatalf("submit %s: %v", id, err)
		}
		if res.Results[0].Status != StatusSettled {
			t.Fatalf("submit %s: %+v", id, res.Results[0])
		}
	}
	refund := func(id, settlement string) {
		t.Helper()
		res, err := l.Refund(RefundBatch{Refunds: []RefundRequest{{ID: id, SettlementID: settlement, Reason: "cancel"}}})
		if err != nil {
			t.Fatalf("refund %s: %v", id, err)
		}
		if res.Results[0].Status != StatusRefundSuccess {
			t.Fatalf("refund %s: %+v", id, res.Results[0])
		}
	}

	submit("p1", 100) // 900
	refund("r1", "p1")

	// 启动另一个进程的初始化，先挡在闸门前，让它“开始得早、结束得晚”。
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	gate := filepath.Join(work, "gate")
	result := filepath.Join(work, "result")
	cmd := exec.Command(exe, "-test.run=^TestHelperCreateProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		envLedger+"="+path,
		envInitIdx+"=99",
		envGate+"="+gate,
		envResult+"="+result,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 确认它已在等待

	// 胜出方继续产生付款与退款。
	submit("p2", 200) // 700
	refund("r2", "p2")
	submit("p3", 50) // 950

	// 放行较晚结束的初始化：它必须失败，不能把账本重置成新余额。
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("late init must exit non-zero")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("late init exit: %v", err)
	}
	res, _ := os.ReadFile(result)
	if string(res) != "err:"+ErrExists {
		t.Fatalf("late init result=%q want err:%s", res, ErrExists)
	}

	snap, err := l.Query()
	if err != nil {
		t.Fatal(err)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != 950 {
		t.Fatalf("balance reset by late init: %d want 950", bal)
	}
	if bal, _ := l.Balance("winner-99", "usdc"); bal != 0 {
		t.Fatalf("loser balances must not leak into winner ledger: %d", bal)
	}
	if len(snap.Settlements) != 3 || len(snap.Refunds) != 2 {
		t.Fatalf("history reset: settlements=%d refunds=%d", len(snap.Settlements), len(snap.Refunds))
	}
	if got := []string{snap.Settlements[0].ID, snap.Settlements[1].ID, snap.Settlements[2].ID}; got[0] != "p1" || got[1] != "p2" || got[2] != "p3" {
		t.Fatalf("settlements order/content: %v", got)
	}
	assertNoTempFiles(t, dir)

	// 被打扰之后付款与退款仍能正常更新余额与历史。
	submit("p4", 30)
	if bal, _ := l.Balance("aa", "usdc"); bal != 920 {
		t.Fatalf("balance after further payment=%d want 920", bal)
	}
	refund("r4", "p4")
	if bal, _ := l.Balance("aa", "usdc"); bal != 950 {
		t.Fatalf("balance after further refund=%d want 950", bal)
	}
	snap, _ = l.Query()
	if len(snap.Settlements) != 4 || len(snap.Refunds) != 3 {
		t.Fatalf("history after further ops: settlements=%d refunds=%d", len(snap.Settlements), len(snap.Refunds))
	}
}
