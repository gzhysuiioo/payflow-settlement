package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 本文件用两个真实的 payflow 进程验证初始化互斥：
// 同一尚不存在的账本上相互重叠的 init 只能有一个成功（退出码 0、
// initialized），另一个退出码 1 且 stderr 错误分类为 ledger_exists；
// 胜出账本只含胜出请求的初始余额，付款与退款历史为空。
// 经目录符号链接访问同一实际目标时规则相同。胜出账本产生付款/退款后，
// 再发起重叠 init 波次也不能重置或改写历史。

var (
	payflowBinOnce sync.Once
	payflowBinPath string
	payflowBinErr  error
)

func buildPayflowBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "payflow")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("build payflow binary: %v\n%s", err, out.String())
	}
	return bin
}

type procResult struct {
	code int
	out  string
	err  string
}

// runInit 以独立进程执行一次 init，输入为 balances JSON 片段。
func runInitProcess(t *testing.T, bin, ledgerPath, payload string) procResult {
	t.Helper()
	cmd := exec.Command(bin, "init", "-l", ledgerPath)
	cmd.Stdin = strings.NewReader(payload)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run init proc: %v", err)
	}
	return procResult{code: code, out: out.String(), err: errBuf.String()}
}

func runCLIProcess(t *testing.T, bin string, args []string, payload string) procResult {
	t.Helper()
	cmd := exec.Command(bin, args...)
	if payload != "" {
		cmd.Stdin = strings.NewReader(payload)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run proc %v: %v", args, err)
	}
	return procResult{code: code, out: out.String(), err: errBuf.String()}
}

func errorKindFromEnvelope(t *testing.T, stderr string) string {
	t.Helper()
	var env struct {
		Error struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &env); err != nil {
		t.Fatalf("stderr is not an error envelope: %v\n%s", err, stderr)
	}
	return env.Error.Kind
}

type initWinner struct {
	account string
	balance int64
}

// concurrentInitPair 同时启动两个 init 进程（barrier 对齐 Start 时刻），
// 返回两个进程的结果与其对应的输入。
func concurrentInitPair(t *testing.T, bin, pathA, pathB string) (procResult, initWinner, procResult, initWinner) {
	t.Helper()
	a := initWinner{account: "acct01", balance: 1001}
	b := initWinner{account: "acct02", balance: 1002}
	payload := func(w initWinner) string {
		buf, err := json.Marshal(map[string]any{"balances": []map[string]any{
			{"account": w.account, "asset": "usdc", "balance": w.balance},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return string(buf)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var ra, rb procResult
	spawn := func(path string, w initWinner, dst *procResult) {
		defer wg.Done()
		<-start
		*dst = runInitProcess(t, bin, path, payload(w))
	}
	wg.Add(2)
	go spawn(pathA, a, &ra)
	go spawn(pathB, b, &rb)
	close(start)
	wg.Wait()
	return ra, a, rb, b
}

func assertExactlyOneWinner(t *testing.T, ra procResult, wa initWinner, rb procResult, wb initWinner) initWinner {
	t.Helper()
	ok := func(r procResult, w initWinner) bool {
		if r.code != 0 {
			return false
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(r.out), &out); err != nil || out["status"] != "initialized" {
			t.Fatalf("successful init output invalid: %v\n%s", err, r.out)
		}
		return true
	}
	lost := func(r procResult) {
		if r.code != 1 {
			t.Fatalf("losing init must exit 1, got code=%d err=%s", r.code, r.err)
		}
		if kind := errorKindFromEnvelope(t, r.err); kind != "ledger_exists" {
			t.Fatalf("losing init envelope kind=%s, stderr=%s", kind, r.err)
		}
		if r.out != "" {
			t.Fatalf("losing init must not print success output: %q", r.out)
		}
	}
	switch {
	case ok(ra, wa) && !ok(rb, wb):
		lost(rb)
		return wa
	case ok(rb, wb) && !ok(ra, wa):
		lost(ra)
		return wb
	case ok(ra, wa) && ok(rb, wb):
		t.Fatal("both init processes succeeded")
	default:
		t.Fatalf("neither init succeeded:\n a: %+v\n b: %+v", ra, rb)
	}
	return initWinner{}
}

// 连续多轮：每轮两个真实进程同时初始化同一个尚不存在的账本，
// 恰一个成功；随后 query 读到的恰好是胜出方余额、历史为空。
func TestCLIConcurrentInitProcesses(t *testing.T) {
	bin := buildPayflowBinary(t)

	for round := 0; round < 8; round++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "ledger.json")
		ra, wa, rb, wb := concurrentInitPair(t, bin, path, path)
		winner := assertExactlyOneWinner(t, ra, wa, rb, wb)

		q := runCLIProcess(t, bin, []string{"query", "-l", path}, "")
		if q.code != 0 {
			t.Fatalf("round %d query: %s", round, q.err)
		}
		var snap struct {
			Balances []struct {
				Account string `json:"account"`
				Asset   string `json:"asset"`
				Balance int64  `json:"balance"`
			} `json:"balances"`
			Settlements []any `json:"settlements"`
			Refunds     []any `json:"refunds"`
		}
		if err := json.Unmarshal([]byte(q.out), &snap); err != nil {
			t.Fatalf("round %d parse query: %v\n%s", round, err, q.out)
		}
		if len(snap.Balances) != 1 ||
			snap.Balances[0].Account != winner.account ||
			snap.Balances[0].Asset != "usdc" ||
			snap.Balances[0].Balance != winner.balance {
			t.Fatalf("round %d winner ledger balances=%+v, want %s=%d",
				round, snap.Balances, winner.account, winner.balance)
		}
		if len(snap.Settlements) != 0 || len(snap.Refunds) != 0 {
			t.Fatalf("round %d fresh ledger history must be empty: %+v %+v",
				round, snap.Settlements, snap.Refunds)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		loser := wa
		if winner.account == wa.account {
			loser = wb
		}
		if strings.Contains(string(raw), loser.account) {
			t.Fatalf("round %d loser account %q leaked into winner ledger: %s",
				round, loser.account, raw)
		}
	}
}

// 两个进程分别经真实目录与指向该目录的软链接同时初始化同一目标：仍恰一个成功。
func TestCLIConcurrentInitViaDirectorySymlink(t *testing.T) {
	bin := buildPayflowBinary(t)
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

	ra, wa, rb, wb := concurrentInitPair(t, bin, realPath, linkPath)
	winner := assertExactlyOneWinner(t, ra, wa, rb, wb)

	if fi, err := os.Lstat(realPath); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("winner ledger must be a regular file in the real dir: %v %v", fi, err)
	}
	q := runCLIProcess(t, bin, []string{"query", "-l", linkPath}, "")
	if q.code != 0 {
		t.Fatalf("query via symlink view: %s", q.err)
	}
	var snap struct {
		Balances []struct {
			Account string `json:"account"`
			Balance int64  `json:"balance"`
		} `json:"balances"`
	}
	if err := json.Unmarshal([]byte(q.out), &snap); err != nil {
		t.Fatalf("parse query: %v\n%s", err, q.out)
	}
	if len(snap.Balances) != 1 || snap.Balances[0].Account != winner.account {
		t.Fatalf("balances=%+v, want winner %s", snap.Balances, winner.account)
	}
}

// 胜出账本产生付款与退款后，再同时发起两个 init：全部 ledger_exists，
// 文件逐字节不变，历史与余额不被重置。
func TestCLIInitRushAfterPaymentAndRefundKeepsHistory(t *testing.T) {
	bin := buildPayflowBinary(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	r := runInitProcess(t, bin, path, `{"balances":[{"account":"aa","asset":"usdc","balance":1000}]}`)
	if r.code != 0 {
		t.Fatalf("init: %s", r.err)
	}
	s := runCLIProcess(t, bin, []string{"submit", "-l", path},
		`{"fee_bps":0,"intents":[{"id":"p1","account":"aa","paymaster":"pm","asset":"usdc","amount":300,"nonce":1,"state":"pending"}]}`)
	if s.code != 0 {
		t.Fatalf("submit: %s", s.err)
	}
	rf := runCLIProcess(t, bin, []string{"refund", "-l", path},
		`{"refunds":[{"id":"r1","settlement_id":"p1","reason":"undo"}]}`)
	if rf.code != 0 {
		t.Fatalf("refund: %s", rf.err)
	}
	established, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]procResult, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = runInitProcess(t, bin, path,
				`{"balances":[{"account":"intruder","asset":"usdc","balance":1}]}`)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, rr := range results {
		if rr.code != 1 || errorKindFromEnvelope(t, rr.err) != "ledger_exists" {
			t.Fatalf("late init %d: %+v", i, rr)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(established, after) {
		t.Fatalf("late init wave rewrote the winner ledger\nbefore=%s\nafter=%s", established, after)
	}
	q := runCLIProcess(t, bin, []string{"query", "-l", path}, "")
	if q.code != 0 {
		t.Fatalf("query: %s", q.err)
	}
	var snap struct {
		Balances    []map[string]any `json:"balances"`
		Settlements []struct {
			ID string `json:"id"`
		} `json:"settlements"`
		Refunds []struct {
			ID string `json:"id"`
		} `json:"refunds"`
	}
	if err := json.Unmarshal([]byte(q.out), &snap); err != nil {
		t.Fatalf("parse query: %v\n%s", err, q.out)
	}
	if len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" {
		t.Fatalf("payment history reset: %+v", snap.Settlements)
	}
	if len(snap.Refunds) != 1 || snap.Refunds[0].ID != "r1" {
		t.Fatalf("refund history reset: %+v", snap.Refunds)
	}
	for _, b := range snap.Balances {
		if b["account"] == "intruder" {
			t.Fatalf("late init leaked intruder account into winner ledger: %+v", snap.Balances)
		}
		if b["account"] == "aa" && b["balance"].(float64) != 1000 {
			t.Fatalf("winner balance disturbed: %+v", b)
		}
	}
}

// 文件位置上的符号链接（含悬空）经真实进程 init 也按 ledger_exists 拒绝，
// 链接与去向原样保留。
func TestCLIInitRejectsFileSymlinks(t *testing.T) {
	bin := buildPayflowBinary(t)
	dir := t.TempDir()

	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("keep-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	goodLink := filepath.Join(dir, "good.json")
	if err := os.Symlink("target.json", goodLink); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink("ghost", dangling); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{goodLink, dangling} {
		r := runInitProcess(t, bin, p, `{"balances":[{"account":"aa","asset":"usdc","balance":1}]}`)
		if r.code != 1 || errorKindFromEnvelope(t, r.err) != "ledger_exists" {
			t.Fatalf("init on %s: %+v", p, r)
		}
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "keep-me" {
		t.Fatalf("symlink target altered: %q %v", got, err)
	}
	if dst, err := os.Readlink(dangling); err != nil || dst != "ghost" {
		t.Fatalf("dangling link replaced: %q %v", dst, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghost")); !os.IsNotExist(err) {
		t.Fatalf("init must not create dangling link destination: err=%v", err)
	}
}
