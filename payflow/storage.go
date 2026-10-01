package payflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
)

// atomicWrite 将 v 以 JSON 原子写入 path：
//  1. 写入同目录临时文件并 fsync；
//  2. rename 覆盖目标文件（同文件系统内原子）；
//  3. 尽力 fsync 目录，帮助 rename 跨掉电容灾。
//
// 关键不变式：磁盘上的目标文件在任意时刻要么是完整旧内容，
// 要么是完整新内容（临时文件先 fsync 后改名，JSON 只写一个文档）。
// 进程在保存期间崩溃，重开只能看到完整的旧状态或新状态，
// 不会出现“扣款无记录”或“记录未扣款”。
func atomicWrite(path string, v any) error {
	perm := os.FileMode(0o600)
	data, err := json.Marshal(v)
	if err != nil {
		return ledgerError(ErrStorage, "encode ledger: %v", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ledgerError(ErrStorage, "create ledger directory %s: %v", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".ledger-*.tmp")
	if err != nil {
		return ledgerError(ErrStorage, "create temp file in %s: %v", dir, err)
	}
	tmpName := tmp.Name()
	removeTemp := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		removeTemp()
		return ledgerError(ErrStorage, "write temp file: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		removeTemp()
		return ledgerError(ErrStorage, "fsync temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		removeTemp()
		return ledgerError(ErrStorage, "close temp file: %v", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		removeTemp()
		return ledgerError(ErrStorage, "chmod temp file: %v", err)
	}
	// rename 之后新文件已完整就位。即便此后目录 fsync 失败，也不能再回滚：
	// 回滚内存会与磁盘（已是新状态）分叉。目录 fsync 仅影响掉电后目录项的
	// 持久性，而可能出现的两种结局（旧文件/新文件）都是完整状态。
	if err := os.Rename(tmpName, path); err != nil {
		removeTemp()
		return ledgerError(ErrStorage, "rename into place: %v", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// validateAndReplay 校验全部成功记录并从冻结的初始余额重放：
// ID 唯一、序列连续、费用一致、int64 不溢出、任一时点余额非负，
// 且重放结果必须等于文件中的最终余额。任何不符都判定账本损坏。
func validateAndReplay(initial, final map[balanceKey]int64, records []Record) error {
	charged := make(map[balanceKey]int64)
	seen := map[string]bool{}
	for i, r := range records {
		if r.ID == "" {
			return ledgerError(ErrCorrupt, "settlement %d has empty id", i)
		}
		if seen[r.ID] {
			return ledgerError(ErrCorrupt, "duplicate settlement id %q", r.ID)
		}
		seen[r.ID] = true
		if r.Account == "" || r.Asset == "" {
			return ledgerError(ErrCorrupt, "settlement %q missing account or asset", r.ID)
		}
		if _, ok := initial[balanceKey{r.Account, r.Asset}]; !ok {
			return ledgerError(ErrCorrupt, "settlement %q touches account/asset absent from initial balances", r.ID)
		}
		if r.Amount <= 0 || r.Fee < 0 || r.Charged <= 0 {
			return ledgerError(ErrCorrupt, "settlement %q has non-positive amounts", r.ID)
		}
		if r.FeeBps < 0 || r.FeeBps > 10000 {
			return ledgerError(ErrCorrupt, "settlement %q has invalid fee bps", r.ID)
		}
		if want := feeFor(r.Amount, r.FeeBps); want != r.Fee {
			return ledgerError(ErrCorrupt, "settlement %q fee mismatch: file %d, want %d", r.ID, r.Fee, want)
		}
		if r.Charged != r.Amount+r.Fee {
			return ledgerError(ErrCorrupt, "settlement %q charged != amount + fee", r.ID)
		}
		if int64(i)+1 != r.Seq {
			return ledgerError(ErrCorrupt, "settlement %q sequence gap", r.ID)
		}
		k := balanceKey{r.Account, r.Asset}
		sum := charged[k]
		if r.Charged > math.MaxInt64-sum {
			return ledgerError(ErrCorrupt, "settlement %q cumulative charges overflow int64", r.ID)
		}
		charged[k] = sum + r.Charged
	}

	// 初始余额由初始化数据冻结；累计扣款不得超过它。
	for k, start := range initial {
		if charged[k] > start {
			// 找到第一笔导致负余额的记录以给出更明确的报错。
			var running int64
			for _, r := range records {
				if r.Account != k.account || r.Asset != k.asset {
					continue
				}
				if r.Charged > start-running {
					return ledgerError(ErrCorrupt, "settlement %q drives %s/%s negative", r.ID, r.Account, r.Asset)
				}
				running += r.Charged
			}
			return ledgerError(ErrCorrupt, "total charges for %s/%s exceed initial balance %d", k.account, k.asset, start)
		}
	}

	// 按记录顺序重放，末态必须与文件中的最终余额逐组合一致。
	for k, start := range initial {
		got := start - charged[k]
		if want, present := final[k]; !present || got != want {
			return ledgerError(ErrCorrupt, "final balance mismatch for %s/%s: replay %d, file %v", k.account, k.asset, got, want)
		}
	}
	for k := range final {
		if _, ok := initial[k]; !ok {
			return ledgerError(ErrCorrupt, "final balance contains %s/%s absent from initial balances", k.account, k.asset)
		}
	}
	return nil
}

// checksumHex 返回 data 的 sha256 十六进制摘要，作为文档完整性凭证。
func checksumHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
