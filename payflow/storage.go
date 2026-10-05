package payflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
)

// encodeLedgerDoc 把账本状态序列化为一个以换行结尾的 JSON 文档。
func encodeLedgerDoc(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, ledgerError(ErrStorage, "encode ledger: %v", err)
	}
	return append(data, '\n'), nil
}

// stageTempFile 在 dir 中准备一份完整但尚未就位的账本文档：
//  1. 确保 dir 存在；
//  2. 写入同目录临时文件并 fsync；
//  3. chmod 0600。
//
// 返回临时文件路径；任一步失败都清理临时文件并返回错误。
func stageTempFile(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", ledgerError(ErrStorage, "create ledger directory %s: %v", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".ledger-*.tmp")
	if err != nil {
		return "", ledgerError(ErrStorage, "create temp file in %s: %v", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", ledgerError(ErrStorage, "write temp file: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", ledgerError(ErrStorage, "fsync temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", ledgerError(ErrStorage, "close temp file: %v", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		cleanup()
		return "", ledgerError(ErrStorage, "chmod temp file: %v", err)
	}
	return tmpName, nil
}

// fsyncDir 尽力 fsync 目录，帮助新建文件/改名跨掉电容灾。
func fsyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// atomicWrite 将 v 以 JSON 原子写入已存在账本的 path：
//  1. 写入同目录临时文件并 fsync；
//  2. rename 覆盖目标文件（同文件系统内原子）；
//  3. 尽力 fsync 目录，帮助 rename 跨掉电容灾。
//
// 关键不变式：磁盘上的目标文件在任意时刻要么是完整旧内容，
// 要么是完整新内容（临时文件先 fsync 后改名，JSON 只写一个文档）。
// 进程在保存期间崩溃，重开只能看到完整的旧状态或新状态，
// 不会出现“扣款无记录”或“记录未扣款”。
//
// 仅供已存在账本的后续更新（付款/退款）使用；初始化新建账本必须走
// createExclusiveFile，原子 rename 会覆盖同名文件与符号链接，无法用于
// “只许新建、绝不覆盖”的初始化。
func atomicWrite(path string, v any) error {
	data, err := encodeLedgerDoc(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmpName, err := stageTempFile(dir, data)
	if err != nil {
		return err
	}
	// rename 之后新文件已完整就位。即便此后目录 fsync 失败，也不能再回滚：
	// 回滚内存会与磁盘（已是新状态）分叉。目录 fsync 仅影响掉电后目录项的
	// 持久性，而可能出现的两种结局（旧文件/新文件）都是完整状态。
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return ledgerError(ErrStorage, "rename into place: %v", err)
	}
	fsyncDir(dir)
	return nil
}

// createExclusiveFile 以“只许新建、绝不覆盖”的方式在 path 处写入一份完整
// 账本文档，供 CreateLedger 初始化使用：
//   - 用 O_CREATE|O_EXCL（Unix 上另加 O_NOFOLLOW）直接在 path 上创建目标
//     文件（权限 0600），随后把完整文档写入该文件并 fsync；
//   - 占用判定全部交给内核：path 上已有普通文件（正常、损坏、空）、目录、
//     符号链接（含目标不存在的悬空链接），或另一进程/线程已抢先创建，
//     打开都以“已占用”失败，返回 occupied=true，已有内容与符号链接本身
//     原样保留；
//   - 目标文件由本次调用独占创建：写入或 fsync 失败时删除的一定是自己刚
//     创建的半成品，绝不会碰到胜出方的账本，磁盘上也不会留下能被查询当成
//     新账本的空壳/残片。
//
// 路径中间的目录符号链接照常跟随，因此经真实目录与指向该目录的软链接
// 访问同一实际目标时，内核的互斥同样生效。返回的 err 非 nil 时为存储错误。
func createExclusiveFile(path string, v any) (occupied bool, err error) {
	data, err := encodeLedgerDoc(v)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, ledgerError(ErrStorage, "create ledger directory %s: %v", filepath.Dir(path), err)
	}

	// O_EXCL 互斥、O_NOFOLLOW 拒绝文件位置符号链接：先占住名字再写内容，
	// 其他重叠的初始化此刻即拿到“已占用”，不会有两份文档相互覆盖。
	f, err := openExclusiveCreate(path)
	if err != nil {
		if isOccupiedCreateErr(err) {
			return true, nil
		}
		return false, ledgerError(ErrStorage, "create ledger %s: %v", path, err)
	}

	fail := func(format string, args ...any) error {
		_ = f.Close()
		_ = os.Remove(path) // 只可能删到本次独占创建的文件
		return ledgerError(ErrStorage, format, args...)
	}
	// 仅测试注入：模拟“名字已独占占住、内容写入失败”的存储故障，
	// 验证半成品会被删除、路径重新可被初始化。
	if failCreateWriteHook != nil && failCreateWriteHook() {
		return false, fail("injected write failure while initializing %s", path)
	}
	if _, werr := f.Write(data); werr != nil {
		return false, fail("write ledger %s: %v", path, werr)
	}
	if serr := f.Sync(); serr != nil {
		return false, fail("fsync ledger %s: %v", path, serr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(path)
		return false, ledgerError(ErrStorage, "close ledger %s: %v", path, cerr)
	}
	fsyncDir(filepath.Dir(path))
	return false, nil
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

// failCreateWriteHook 仅供测试注入：非 nil 且返回 true 时，让初始化在
// 独占创建账本文件之后、写入内容之前失败，以验证半成品清理。
var failCreateWriteHook func() bool
