package payflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"syscall"
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

// createExclusive 在 path 处排他地创建新账本文件，绝不覆盖任何已有路径项。
//
// 与 atomicWrite 的关键区别在最后一步：临时文件完整落盘后，用 link(2)
// （ATOMIC_CREATE：目标存在即失败）把它发布到 path，而不是 rename。
// 这使“检查路径是否占用”和“创建账本”成为内核里的同一个原子操作：
//   - 普通文件、目录、损坏或空文件：EEXIST，原内容逐字节不变；
//   - path 上是符号链接（无论其目标存在与否、指向文件还是目录）：
//     link 不跟随符号链接，一律 EEXIST，链接本身与其去向保持不变，
//     也不会顺着悬空链接把目标文件创建出来；
//   - 经指向目录的符号链接访问一个尚未占用的文件位置时，目录项原本
//     不存在，创建照常成功。
//
// 多个进程同时初始化同一尚不存在的路径时，只有一个 link 成功；
// 失败者拿到 EEXIST 并清理自己的临时文件，不会触碰胜出者的文件。
// 由于发布用的是硬链接而非 rename，胜出者磁盘上的文件从发布那一刻起
// 就是已 fsync 的完整内容（临时文件与 path 指向同一个 inode），
// 崩溃后同样只可能看到“完整新账本”或“路径不存在”，没有半成品。
//
// path 占用时返回带 ErrExists 的账本错误；其余创建/写入失败返回
// ErrStorage 且不留下账本文件或临时文件。
func createExclusive(path string, v any) error {
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
	if err := os.Chmod(tmpName, 0o600); err != nil {
		removeTemp()
		return ledgerError(ErrStorage, "chmod temp file: %v", err)
	}

	// 原子排他发布：path 上存在任何路径项（含悬空/指向文件或目录的符号链接）
	// 都返回 EEXIST，绝不替换它，也不跟随它。
	if err := os.Link(tmpName, path); err != nil {
		removeTemp()
		if errors.Is(err, syscall.EEXIST) {
			return ledgerError(ErrExists, "ledger already exists at %s", path)
		}
		return ledgerError(ErrStorage, "create ledger at %s: %v", path, err)
	}
	// path 与临时文件现在是同一个 inode；移除临时名字不影响已发布的账本。
	removeTemp()
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// validateAndReplay 校验全部成功记录并从冻结的初始余额重放结算与退款：
// ID 唯一、序列连续、费用一致、int64 不溢出、退款只能引用此前存在的结算且
// 每笔结算最多退回一次、退款金额必须等于原扣款、任一时点余额非负，
// 且重放结果必须等于文件中的最终余额。任何不符都判定账本损坏。
//
// 结算与退款按各自成功历史交错：退款 r 只允许发生在序号 <= r.AfterSeq 的
// 结算之后（AfterSeq 是退款落账时结算历史的长度）。这保留了
// “付款 → 退款 → 再付款”的真实时间顺序，使中间余额与最终余额都可验证。
func validateAndReplay(initial, final map[balanceKey]int64, records []Record, refunds []RefundRecord) error {
	// 先校验结算自身的结构不变式（余额相关的判定由后面的交错重放完成，
	// 因为退款回收的余额允许同一笔钱被再次扣出，累计毛扣款可能超过初始余额）。
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
	}

	// 校验退款自身的不变式：字段非空、序列连续、引用的结算在退款时点已存在、
	// 金额与去向与原结算逐字一致、每笔结算最多退回一次、退款编号全局唯一。
	// 此外付款历史只会追加，因此按退款成功顺序（即记录顺序）看，after_seq
	// 必须单调不减：后一笔退款落账时，已存在的结算不可能比前一笔退款时更少。
	// 该规则针对整本账本的结算历史，与退款属于哪个账户、资产或引用哪笔付款无关。
	seenRefund := map[string]bool{}
	refundedBy := map[string]string{} // 结算编号 -> 首个退回它的退款编号
	prevAfterSeq := int64(-1)
	prevRefundID := ""
	for i, rf := range refunds {
		if rf.ID == "" {
			return ledgerError(ErrCorrupt, "refund %d has empty id", i)
		}
		if seenRefund[rf.ID] {
			return ledgerError(ErrCorrupt, "duplicate refund id %q", rf.ID)
		}
		seenRefund[rf.ID] = true
		if rf.SettlementID == "" || rf.Reason == "" {
			return ledgerError(ErrCorrupt, "refund %q missing settlement_id or reason", rf.ID)
		}
		if int64(i)+1 != rf.Seq {
			return ledgerError(ErrCorrupt, "refund %q sequence gap", rf.ID)
		}
		if rf.AfterSeq < 0 || rf.AfterSeq > int64(len(records)) {
			return ledgerError(ErrCorrupt, "refund %q references settlement outside history (after_seq=%d)", rf.ID, rf.AfterSeq)
		}
		if rf.AfterSeq < prevAfterSeq {
			return ledgerError(ErrCorrupt,
				"refund %q (seq %d) has after_seq %d, but previous refund %q (seq %d) already saw %d settlements: payment history only grows",
				rf.ID, rf.Seq, rf.AfterSeq, prevRefundID, int64(i), prevAfterSeq)
		}
		prevAfterSeq = rf.AfterSeq
		prevRefundID = rf.ID
		var target *Record
		for j := range records {
			if records[j].Seq <= rf.AfterSeq && records[j].ID == rf.SettlementID {
				target = &records[j]
				break
			}
		}
		if target == nil {
			return ledgerError(ErrCorrupt, "refund %q references non-existent settlement %q", rf.ID, rf.SettlementID)
		}
		if rf.Account != target.Account || rf.Asset != target.Asset ||
			rf.Amount != target.Amount || rf.Fee != target.Fee || rf.Charged != target.Charged {
			return ledgerError(ErrCorrupt, "refund %q amount or destination differs from settlement %q", rf.ID, rf.SettlementID)
		}
		if rf.Charged <= 0 {
			return ledgerError(ErrCorrupt, "refund %q has non-positive charged amount", rf.ID)
		}
		if other, dup := refundedBy[rf.SettlementID]; dup {
			return ledgerError(ErrCorrupt, "settlement %q refunded twice: %q and %q", rf.SettlementID, other, rf.ID)
		}
		refundedBy[rf.SettlementID] = rf.ID
	}

	// 按成功时间交错重放：第 s 笔结算之后落入 after_seq==s 的全部退款
	// （退款之间按其自身序号；用分桶而非假定 after_seq 单调，以抵抗篡改数据）。
	// net[k] 为该组合截至当前时点的“净扣款”（扣款增加、退款减少），
	// 必须始终落在 [0, initial]，且不发生 int64 溢出。
	buckets := make([][]int, len(records)+1) // after_seq -> refunds 下标
	for i := range refunds {
		buckets[refunds[i].AfterSeq] = append(buckets[refunds[i].AfterSeq], i)
	}
	net := make(map[balanceKey]int64)
	for s := range records {
		r := &records[s]
		k := balanceKey{r.Account, r.Asset}
		sum := net[k]
		if r.Charged > math.MaxInt64-sum {
			return ledgerError(ErrCorrupt, "settlement %q cumulative charges overflow int64", r.ID)
		}
		sum += r.Charged
		if sum > initial[k] {
			return ledgerError(ErrCorrupt, "settlement %q drives %s/%s negative during replay", r.ID, r.Account, r.Asset)
		}
		net[k] = sum

		for _, ri := range buckets[int64(s)+1] {
			rf := &refunds[ri]
			rk := balanceKey{rf.Account, rf.Asset}
			cur := net[rk]
			if rf.Charged > cur {
				return ledgerError(ErrCorrupt, "refund %q exceeds charged amount for %s/%s at its time", rf.ID, rf.Account, rf.Asset)
			}
			net[rk] = cur - rf.Charged
		}
	}
	// after_seq==0 的退款不可能引用任何已存在结算，前面理应已拒绝；
	// 此处兜底，任何未被消费的退款都视为顺序损坏。
	if len(buckets[0]) != 0 {
		return ledgerError(ErrCorrupt, "refund %q precedes every settlement", refunds[buckets[0][0]].ID)
	}

	// 交错重放后的末态必须与文件中的最终余额逐组合一致。
	for k, start := range initial {
		got := start - net[k]
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
