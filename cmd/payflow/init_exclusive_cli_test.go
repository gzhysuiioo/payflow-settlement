package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// 本文件用真实编译出的 payflow 二进制回归 CLI 层 init 的排他语义：
// 跨独立进程同时初始化、经真实目录与目录符号链接访问同一目标、
// 账本位置上的悬空符号链接、以及经目录符号链接的正常创建。

// buildPayflowBinary 把当前模块编译到本测试专属临时目录并返回其路径。
func buildPayflowBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "payflow")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/payflow")
	// go test 运行时工作目录即 cmd/payflow，模块根在其上两级。
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = filepath.Join(wd, "..", "..")
	var buildErr bytes.Buffer
	cmd.Stderr = &buildErr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build payflow: %v\n%s", err, buildErr.String())
	}
	return bin
}

type procResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// runConcurrentInitProcesses 启动 n 个真实 payflow init 进程，各自持有
// 一份互不相同的初始余额（账户 proc-i）。所有进程先在读取 stdin 处等待，
// 再同时放行，最大化 init 在文件系统上的重叠窗口。
func runConcurrentInitProcesses(t *testing.T, bin string, n int, pathFor func(i int) string) []procResult {
	t.Helper()
	type proc struct {
		cmd *exec.Cmd
		w   *os.File
		out bytes.Buffer
		err bytes.Buffer
	}
	procs := make([]*proc, n)
	for i := 0; i < n; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		p := &proc{}
		p.cmd = exec.Command(bin, "init", "-l", pathFor(i))
		p.cmd.Stdin = r
		p.cmd.Stdout = &p.out
		p.cmd.Stderr = &p.err
		p.w = w
		if err := p.cmd.Start(); err != nil {
			t.Fatalf("start init process %d: %v", i, err)
		}
		_ = r.Close() // 父进程只需写端
		procs[i] = p
	}

	// 全部进程已启动并阻塞在读 stdin：同时写入各自输入并关闭管道，
	// 让它们在同一时刻解析 JSON 并调用 CreateLedger。
	var startWG sync.WaitGroup
	for i, p := range procs {
		startWG.Add(1)
		go func(i int, p *proc) {
			defer startWG.Done()
			body := fmt.Sprintf(`{"balances":[{"account":"proc-%02d","asset":"usdc","balance":%d}]}`, i, 2000+i)
			_, _ = p.w.Write([]byte(body))
			_ = p.w.Close()
		}(i, p)
	}
	startWG.Wait()

	results := make([]procResult, n)
	var wg sync.WaitGroup
	for i, p := range procs {
		wg.Add(1)
		go func(i int, p *proc) {
			defer wg.Done()
			err := p.cmd.Wait()
			code := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					t.Errorf("wait init process %d: %v", i, err)
					code = -1
				}
			}
			results[i] = procResult{exitCode: code, stdout: p.out.String(), stderr: p.err.String()}
		}(i, p)
	}
	wg.Wait()
	return results
}

// assertCLIExactlyOneInitWinner 断言并发 init 恰好一个退出码 0 且输出
// initialized，其余退出码 1、stderr 为 ledger_exists、stdout 为空；
// 返回胜出者编号。
func assertCLIExactlyOneInitWinner(t *testing.T, results []procResult) int {
	t.Helper()
	winner := -1
	for i, r := range results {
		switch r.exitCode {
		case 0:
			if winner != -1 {
				t.Fatalf("two init processes succeeded: %d and %d", winner, i)
			}
			winner = i
			var out map[string]any
			if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out["status"] != "initialized" {
				t.Fatalf("winner %d stdout=%q", i, r.stdout)
			}
			if r.stderr != "" {
				t.Fatalf("winner %d must not write stderr: %q", i, r.stderr)
			}
		case 1:
			if r.stdout != "" {
				t.Fatalf("loser %d must not write success output: %q", i, r.stdout)
			}
			var env map[string]any
			if err := json.Unmarshal([]byte(r.stderr), &env); err != nil {
				t.Fatalf("loser %d stderr not JSON: %q", i, r.stderr)
			}
			if kind := env["error"].(map[string]any)["kind"]; kind != "ledger_exists" {
				t.Fatalf("loser %d kind=%v want ledger_exists", i, kind)
			}
		default:
			t.Fatalf("process %d unexpected exit code %d: %s", i, r.exitCode, r.stderr)
		}
	}
	if winner == -1 {
		t.Fatalf("no init process succeeded")
	}
	return winner
}

// 多个独立进程同时 init 同一尚不存在的账本：恰好一个 initialized，
// 其余 ledger_exists；query 只读到胜出者余额，付款/退款历史为空。
func TestCLIConcurrentInitExactlyOneWinner(t *testing.T) {
	bin := buildPayflowBinary(t)
	for round := 0; round < 10; round++ {
		path := filepath.Join(t.TempDir(), "ledger.json")
		const n = 12
		pathFor := func(i int) string { return path }
		results := runConcurrentInitProcesses(t, bin, n, pathFor)
		winner := assertCLIExactlyOneInitWinner(t, results)

		r := runCLIWith(t, []string{"query", "-l", path}, "")
		if r.code != 0 {
			t.Fatalf("round %d query: %s", round, r.err)
		}
		var snap struct {
			Balances []struct {
				Account string `json:"account"`
				Balance int64  `json:"balance"`
			} `json:"balances"`
			Settlements []any `json:"settlements"`
			Refunds     []any `json:"refunds"`
		}
		if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
			t.Fatal(err)
		}
		if len(snap.Balances) != 1 {
			t.Fatalf("round %d balances=%+v want exactly the winner's one combo", round, snap.Balances)
		}
		if got := fmt.Sprintf("proc-%02d", winner); snap.Balances[0].Account != got ||
			snap.Balances[0].Balance != int64(2000+winner) {
			t.Fatalf("round %d winner balance=%+v want %s=%d",
				round, snap.Balances[0], got, 2000+winner)
		}
		if len(snap.Settlements) != 0 || len(snap.Refunds) != 0 {
			t.Fatalf("fresh ledger history must be empty: %d/%d", len(snap.Settlements), len(snap.Refunds))
		}
	}
}

// 一半进程经真实目录、一半经指向该目录的符号链接并发 init 同一目标：
// 恰好一个成功；账本落在真实目录，两条路径 query 一致。
func TestCLIConcurrentInitViaDirSymlink(t *testing.T) {
	bin := buildPayflowBinary(t)
	for round := 0; round < 10; round++ {
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

		results := runConcurrentInitProcesses(t, bin, 12, func(i int) string {
			if i%2 == 0 {
				return realPath
			}
			return linkPath
		})
		winner := assertCLIExactlyOneInitWinner(t, results)

		// 目录符号链接本身保持不变。
		if fi, err := os.Lstat(linkDir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("round %d: directory symlink altered: %v", round, err)
		}
		for name, p := range map[string]string{"real": realPath, "link": linkPath} {
			r := runCLIWith(t, []string{"query", "-l", p}, "")
			if r.code != 0 {
				t.Fatalf("round %d query via %s: %s", round, name, r.err)
			}
			var snap struct {
				Balances []struct {
					Account string `json:"account"`
					Balance int64  `json:"balance"`
				} `json:"balances"`
			}
			if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
				t.Fatal(err)
			}
			if len(snap.Balances) != 1 ||
				snap.Balances[0].Account != fmt.Sprintf("proc-%02d", winner) ||
				snap.Balances[0].Balance != int64(2000+winner) {
				t.Fatalf("round %d via %s balances=%+v", round, name, snap.Balances)
			}
		}
	}
}

// 账本文件位置是指向不存在文件的符号链接时，init 返回 ledger_exists：
// 不替换链接、不顺着链接创建目标，也不输出成功结果。
func TestCLIInitRejectsDanglingSymlink(t *testing.T) {
	bin := buildPayflowBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost-ledger.json")
	link := filepath.Join(dir, "ledger.link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// 用分离的 stdout/stderr 断言通道与退出码。
	var so, se bytes.Buffer
	cmd := exec.Command(bin, "init", "-l", link)
	cmd.Stdin = bytes.NewReader([]byte(`{"balances":[{"account":"aa","asset":"usdc","balance":1}]}`))
	cmd.Stdout = &so
	cmd.Stderr = &se
	code := 0
	if runErr := cmd.Run(); runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatal(runErr)
		}
	}
	if code != 1 {
		t.Fatalf("exit code=%d want 1", code)
	}
	if so.String() != "" {
		t.Fatalf("no success output allowed on occupied path: %q", so.String())
	}
	var env map[string]any
	if err := json.Unmarshal(se.Bytes(), &env); err != nil ||
		env["error"].(map[string]any)["kind"] != "ledger_exists" {
		t.Fatalf("stderr envelope=%q", se.String())
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dangling symlink must remain in place: %v", err)
	}
	if dest, err := os.Readlink(link); err != nil || dest != target {
		t.Fatalf("symlink destination changed: %q %v", dest, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("link target must not be created: %v", err)
	}
}

// 经目录符号链接在未占用位置 init 成功；随后的 submit/refund/query
// 经真实路径与符号链接路径都能正常更新和读取。
func TestCLIInitThroughDirSymlinkThenPaymentsAndRefunds(t *testing.T) {
	bin := buildPayflowBinary(t)
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

	var so, se bytes.Buffer
	cmd := exec.Command(bin, "init", "-l", linkPath)
	cmd.Stdin = bytes.NewReader([]byte(`{"balances":[{"account":"aa","asset":"usdc","balance":1000}]}`))
	cmd.Stdout = &so
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("init through dir symlink: %v (%s)", err, se.String())
	}
	var out map[string]any
	if err := json.Unmarshal(so.Bytes(), &out); err != nil || out["status"] != "initialized" {
		t.Fatalf("init stdout=%q", so.String())
	}
	if _, err := os.Stat(realPath); err != nil {
		t.Fatalf("ledger must be created inside the real directory: %v", err)
	}

	// 经符号链接路径付款。
	submit := exec.Command(bin, "submit", "-l", linkPath)
	submit.Stdin = bytes.NewReader([]byte(`{"fee_bps":0,"intents":[
	  {"id":"p1","account":"aa","asset":"usdc","amount":300}]}`))
	if submitOut, err := submit.CombinedOutput(); err != nil {
		t.Fatalf("submit: %v %s", err, submitOut)
	}
	// 经真实路径退款。
	refund := exec.Command(bin, "refund", "-l", realPath)
	refund.Stdin = bytes.NewReader([]byte(`{"refunds":[
	  {"id":"r1","settlement_id":"p1","reason":"cancel"}]}`))
	if refundOut, err := refund.CombinedOutput(); err != nil {
		t.Fatalf("refund: %v %s", err, refundOut)
	}

	for name, p := range map[string]string{"real": realPath, "link": linkPath} {
		r := runCLIWith(t, []string{"query", "-l", p}, "")
		if r.code != 0 {
			t.Fatalf("query via %s: %s", name, r.err)
		}
		var snap struct {
			Balances    []struct{ Balance int64 } `json:"balances"`
			Settlements []struct{ ID string }     `json:"settlements"`
			Refunds     []struct{ ID string }     `json:"refunds"`
		}
		if err := json.Unmarshal([]byte(r.out), &snap); err != nil {
			t.Fatal(err)
		}
		if len(snap.Balances) != 1 || snap.Balances[0].Balance != 1000 ||
			len(snap.Settlements) != 1 || snap.Settlements[0].ID != "p1" ||
			len(snap.Refunds) != 1 || snap.Refunds[0].ID != "r1" {
			t.Fatalf("via %s snapshot mismatches after submit/refund: %s", name, r.out)
		}
	}

	// 再次 init（任一路径）都必须拒绝，余额与历史保持不变。
	for _, p := range []string{realPath, linkPath} {
		r := runCLIWith(t, []string{"init", "-l", p}, `{"balances":[]}`)
		if r.code != 1 || decodeOut(t, r.err)["error"].(map[string]any)["kind"] != "ledger_exists" {
			t.Fatalf("re-init via %s: code=%d err=%s", p, r.code, r.err)
		}
	}
	r := runCLIWith(t, []string{"query", "-l", realPath}, "")
	var snap struct {
		Balances []struct{ Balance int64 } `json:"balances"`
	}
	if err := json.Unmarshal([]byte(r.out), &snap); err != nil || snap.Balances[0].Balance != 1000 {
		t.Fatalf("rejected re-init altered the ledger: %s", r.out)
	}
}
