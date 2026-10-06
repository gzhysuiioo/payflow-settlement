package payflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// 账本磁盘格式（单文件，一个 JSON 文档，末附 sha256 校验和）：
//
//	{"version":1,
//	 "initial_balances":[{"account":"aa","asset":"usdc","balance":1000}],
//	 "balances":[{"account":"aa","asset":"usdc","balance":700}],
//	 "settlements":[{...一笔成功结算及其当时费率...}],
//	 "refunds":[{...一笔成功全额退款...}],
//	 "checksum":"<sha256 of the document without checksum>"}
//
// initial_balances 在初始化时冻结，用于检测任何对历史余额/记录的篡改；
// checksum 覆盖截断、比特翻转与所有语义编辑。
//
// refunds 字段缺省（旧版账本没有该字段）即视为空列表：旧文件可直接打开；
// 只要账本没有发生退款，写回时该字段继续缺省，读取旧账本不会改写文件。
//
// 写入采用“临时文件 + fsync + 原子 rename + 目录 fsync”，
// 因此崩溃后磁盘上只会存在完整的旧版本或完整新版本，
// 不会出现“扣款/退款无记录”或“记录未改余额”的中间状态。
const ledgerVersion = 1

// 结算结果状态，与命令行输出一一对应，可分别识别。
const (
	StatusSettled         = "settled"              // 成功扣款
	StatusDuplicate       = "duplicate"            // 幂等重试：字段与原记录一致，返回原结算/原退款，不再入账
	StatusConflict        = "conflict"             // 编号冲突：编号已成功但字段或费率/目标或原因不同
	StatusState           = "state_error"          // 非 pending
	StatusFunds           = "insufficient_balance" // 余额不足（含不存在的账户/资产组合）
	StatusLimitExceeded   = "limit_exceeded"       // 本次批次该 (账户, 资产) 组合的扣款上限不足
	StatusInvalid         = "invalid_parameter"
	StatusStorage         = "storage_error"
	StatusRefundSuccess   = "refunded"         // 成功全额退回
	StatusNotFound        = "not_found"        // 退款目标结算不存在（含原付款只曾失败）
	StatusAlreadyRefunded = "already_refunded" // 原付款已被其他退款编号全额退回
)

// 拒绝原因。
const (
	reasonEmptyID         = "intent id must not be empty"
	reasonEmptyAccount    = "account must not be empty"
	reasonEmptyAsset      = "asset must not be empty"
	reasonBadAmount       = "amount must be a positive int64"
	reasonOverflow        = "amount plus fee overflows int64"
	reasonNotPending      = "intent is not pending"
	reasonFunds           = "insufficient balance for amount plus fee"
	reasonLimitExceeded   = "charge limit exceeded for %s/%s: max_charged %d, used %d, this item charges %d"
	reasonDuplicate       = "duplicate settlement attempt"
	reasonConflict        = "settlement id conflicts with a different request"
	reasonEmptyRefundID   = "refund id must not be empty"
	reasonEmptySettlement = "settlement_id must not be empty"
	reasonEmptyReason     = "refund reason must not be empty"
	reasonSettlementGone  = "settlement %q not found"
	reasonAlreadyRefunded = "settlement %q has already been refunded by %q"
	reasonRefundConflict  = "refund id conflicts with a different request"
	reasonDuplicateRefund = "duplicate refund attempt"
)

// BalanceInit 是初始化时一个 (账户, 资产) 组合的起始余额。
type BalanceInit struct {
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Balance int64  `json:"balance"`
}

// PaymentIntent 是批次中的一项付款请求。金额使用指针以区分“缺省/null”与 0。
type PaymentIntent struct {
	ID        string `json:"id"`
	Account   string `json:"account"`
	Paymaster string `json:"paymaster"`
	Asset     string `json:"asset"`
	Amount    *int64 `json:"amount"`
	Nonce     uint64 `json:"nonce"`
	State     string `json:"state"`
}

// ChargeLimit 是本次提交中一个 (账户, 资产) 组合的扣款上限：该组合在本批次内
// 成功落账的 charged（付款金额 + 手续费）累计不得超过 max_charged。
// 上限仅对本次提交有效，不持久化；历史扣款与退款都不占本次额度。
// MaxCharged 使用指针以区分“缺省/null”与显式给出的 0（0 表示禁止该组合新增扣款）。
type ChargeLimit struct {
	Account    string `json:"account"`
	Asset      string `json:"asset"`
	MaxCharged *int64 `json:"max_charged"`
}

// FeeBatch 是一次提交：固定费率（基点）+ 有序意图列表 + 可选的本次扣款上限。
// Limits 缺省、为 null 或为空数组时不限制扣款。
type FeeBatch struct {
	FeeBps  int             `json:"fee_bps"`
	Intents []PaymentIntent `json:"intents"`
	Limits  []ChargeLimit   `json:"limits,omitempty"`
}

// Record 是一笔成功结算留下的永久记录，按成功先后顺序保存。
type Record struct {
	ID        string `json:"id"`
	Account   string `json:"account"`
	Paymaster string `json:"paymaster"`
	Asset     string `json:"asset"`
	Amount    int64  `json:"amount"`
	Nonce     uint64 `json:"nonce"`
	FeeBps    int    `json:"fee_bps"`
	Fee       int64  `json:"fee"`
	Charged   int64  `json:"charged"`
	Seq       int64  `json:"seq"`
}

// ItemResult 是批次中一项的处理结果，顺序与输入逐项对应。
type ItemResult struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Reason string  `json:"reason,omitempty"`
	Record *Record `json:"record,omitempty"`
}

// BatchResult 是一次提交的结果。空批次对应空 Results。
type BatchResult struct {
	Results []ItemResult `json:"results"`
}

// PreviewResult 是一次提交预览（--dry-run）的结果。DryRun 恒为 true，
// 用于与真实提交输出区分；Results 与输入逐项对应，状态、失败原因与结算
// 记录格式与 Submit 完全一致——其中 settled 表示“按当前账本预计能够成功”，
// 携带的记录使用从当前最大付款序号之后连续递增的预计序号。
// 预览不写入账本，序号、余额与本批次额度都不会被预留。
type PreviewResult struct {
	DryRun  bool         `json:"dry_run"`
	Results []ItemResult `json:"results"`
}

// RefundRequest 是退款批次中的一项：退款编号、原付款编号、退款原因，三者均须非空。
type RefundRequest struct {
	ID           string `json:"id"`
	SettlementID string `json:"settlement_id"`
	Reason       string `json:"reason"`
}

// RefundBatch 是一次退款提交：有序的退款请求列表。
type RefundBatch struct {
	Refunds []RefundRequest `json:"refunds"`
}

// RefundRecord 是一笔成功全额退款留下的永久记录，按退款成功先后顺序保存。
// 退款金额与去向（账户/资产/总额）全部由原结算记录确定，因此一并冗余记录，
// 使每条退款都能脱离上下文自解释、可追溯原结算。
// AfterSeq 是退款落账时结算历史的长度：余额校验要求该结算在此时点之前发生。
type RefundRecord struct {
	ID           string `json:"id"`
	SettlementID string `json:"settlement_id"`
	Reason       string `json:"reason"`
	Account      string `json:"account"`
	Asset        string `json:"asset"`
	Amount       int64  `json:"amount"`
	Fee          int64  `json:"fee"`
	Charged      int64  `json:"charged"`
	AfterSeq     int64  `json:"after_seq"`
	Seq          int64  `json:"seq"`
}

// RefundResult 是一项退款的处理结果。成功与幂等重复携带原付款编号、账户、
// 资产、退款总额与原因；失败只带状态与说明。
type RefundResult struct {
	ID           string        `json:"id"`
	Status       string        `json:"status"`
	Reason       string        `json:"reason,omitempty"`
	SettlementID string        `json:"settlement_id,omitempty"`
	Account      string        `json:"account,omitempty"`
	Asset        string        `json:"asset,omitempty"`
	Charged      int64         `json:"charged,omitempty"`
	Record       *RefundRecord `json:"record,omitempty"`
}

// RefundBatchResult 是一次退款提交的结果，顺序与输入逐项对应。
type RefundBatchResult struct {
	Results []RefundResult `json:"results"`
}

// BalanceView 是一个 (账户, 资产) 组合的当前余额。
type BalanceView struct {
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Balance int64  `json:"balance"`
}

// Snapshot 是账本查询结果。Refunds 按退款成功先后排列，无退款时为空列表。
type Snapshot struct {
	Balances    []BalanceView  `json:"balances"`
	Settlements []Record       `json:"settlements"`
	Refunds     []RefundRecord `json:"refunds"`
}

// LedgerError 携带可机读分类（与 ItemResult.Status 对齐）和说明。
type LedgerError struct {
	Kind    string
	Message string
}

func (e *LedgerError) Error() string { return e.Kind + ": " + e.Message }

// 错误分类。
const (
	ErrInvalid = "invalid_parameter"
	ErrExists  = "ledger_exists"
	ErrCorrupt = "corrupt_ledger"
	ErrStorage = "storage_error"
	ErrClosed  = "ledger_closed"
	ErrNotInit = "ledger_not_initialized"
)

func ledgerError(kind, format string, args ...any) error {
	return &LedgerError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// KindOf 返回错误的分类；非账本错误按存储错误处理。
func KindOf(err error) string {
	var le *LedgerError
	if errors.As(err, &le) {
		return le.Kind
	}
	return ErrStorage
}

// FailWriter 注入存储故障：返回 true 时本次持久化失败，
// 内存状态必须回滚到该项执行之前。仅用于测试。
type FailWriter interface {
	FailNextPersist() bool
}

type balanceKey struct {
	account string
	asset   string
}

type ledgerState struct {
	Version     int
	Initial     map[balanceKey]int64 // 初始化时冻结的起始余额，仅用于完整性校验
	Balances    map[balanceKey]int64 // 当前余额
	Settlements []Record
	Refunds     []RefundRecord
}

// 磁盘文档字段（顺序即签名时的规范顺序，不得调整，否则旧文件校验失败）。
// Refunds 使用 omitempty：旧版账本缺省该字段即可读入为 nil（视为空），
// 且无退款的账本写回时继续省略该字段。
type unsignedDoc struct {
	Version     int            `json:"version"`
	Initial     []BalanceView  `json:"initial_balances"`
	Balances    []BalanceView  `json:"balances"`
	Settlements []Record       `json:"settlements"`
	Refunds     []RefundRecord `json:"refunds,omitempty"`
}

type signedDoc struct {
	unsignedDoc
	Checksum string `json:"checksum"`
}

func balanceList(m map[balanceKey]int64) []BalanceView {
	out := make([]BalanceView, 0, len(m))
	for k, v := range m {
		out = append(out, BalanceView{Account: k.account, Asset: k.asset, Balance: v})
	}
	sortBalances(out)
	return out
}

func balanceMap(list []BalanceView) (map[balanceKey]int64, error) {
	out := make(map[balanceKey]int64, len(list))
	for i, b := range list {
		if b.Account == "" || b.Asset == "" {
			return nil, ledgerError(ErrCorrupt, "balance %d has empty account or asset", i)
		}
		if b.Balance < 0 {
			return nil, ledgerError(ErrCorrupt, "negative balance for %s/%s", b.Account, b.Asset)
		}
		k := balanceKey{b.Account, b.Asset}
		if _, dup := out[k]; dup {
			return nil, ledgerError(ErrCorrupt, "duplicate balance entry for %s/%s", b.Account, b.Asset)
		}
		out[k] = b.Balance
	}
	return out, nil
}

// MarshalJSON 输出带 sha256 校验和的单个 JSON 文档。
func (s *ledgerState) MarshalJSON() ([]byte, error) {
	settlements := s.Settlements
	if settlements == nil {
		settlements = []Record{}
	}
	doc := unsignedDoc{
		Version:     s.Version,
		Initial:     balanceList(s.Initial),
		Balances:    balanceList(s.Balances),
		Settlements: settlements,
		Refunds:     s.Refunds, // nil 时因 omitempty 省略，保持无退款账本的旧格式
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	sum := checksumHex(payload)
	out := make([]byte, 0, len(payload)+len(sum)+16)
	out = append(out, payload[:len(payload)-1]...) // 去掉结尾 '}'
	out = append(out, []byte(`,"checksum":"`+sum+`"}`)...)
	return out, nil
}

// UnmarshalJSON 校验 checksum、结构不变式并重放全部记录。
func (s *ledgerState) UnmarshalJSON(data []byte) error {
	var doc signedDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc.Checksum == "" {
		return ledgerError(ErrCorrupt, "missing checksum")
	}
	payload, err := json.Marshal(doc.unsignedDoc)
	if err != nil {
		return ledgerError(ErrCorrupt, "re-encode ledger: %v", err)
	}
	if got := checksumHex(payload); got != doc.Checksum {
		return ledgerError(ErrCorrupt, "checksum mismatch: file %s, want %s", doc.Checksum, got)
	}
	if doc.Version != ledgerVersion {
		return ledgerError(ErrCorrupt, "unsupported ledger version %d", doc.Version)
	}
	initial, err := balanceMap(doc.Initial)
	if err != nil {
		return err
	}
	current, err := balanceMap(doc.Balances)
	if err != nil {
		return err
	}
	records := doc.Settlements
	if records == nil {
		records = []Record{}
	}
	refunds := doc.Refunds // 旧版账本缺省该字段，nil 视为空列表
	if refunds == nil {
		refunds = []RefundRecord{}
	}
	if err := validateAndReplay(initial, current, records, refunds); err != nil {
		return err
	}
	s.Version = doc.Version
	s.Initial = initial
	s.Balances = current
	s.Settlements = records
	s.Refunds = refunds
	return nil
}

func decodeState(data []byte) (*ledgerState, error) {
	var state ledgerState
	if err := json.Unmarshal(data, &state); err != nil {
		if le, ok := err.(*LedgerError); ok && le.Kind == ErrCorrupt {
			return nil, err
		}
		return nil, ledgerError(ErrCorrupt, "parse ledger: %v", err)
	}
	return &state, nil
}

// canonicalLedgerPath 把绝对路径解析为账本的规范路径：解析目录与文件本身
// 的符号链接，使同一账本经不同路径（如指向账本所在目录的软链接）打开时
// 共享同一注册表项、同一份内存状态与互斥锁。账本文件尚不存在时（未初始化）
// 解析其父目录；父目录也无法解析时退回原路径。
func canonicalLedgerPath(abs string) string {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(resolved, filepath.Base(abs))
	}
	return abs
}

// CreateLedger 在 path 处创建新账本。所有初始数据先校验，
// 任一不合法则返回 ErrInvalid 且不创建任何文件。
//
// “路径是否占用”与“创建账本”由内核一步原子完成（见 createExclusive）：
//   - 路径上已有正常账本、损坏或空的普通文件、目录：返回 ErrExists，
//     原内容逐字节不变；
//   - 账本文件位置上是符号链接时（无论链接指向文件还是目录、目标是否
//     存在）：返回 ErrExists，链接本身与其去向都不变，也不会顺着悬空
//     链接把目标文件创建出来；
//   - 经指向目录的符号链接访问一个尚未占用的文件位置时创建照常成功。
//
// 因此多个进程同时初始化同一尚不存在的账本（包括一方经真实目录、
// 另一方经指向该目录的符号链接访问同一目标）时，恰好一个成功，
// 其余返回 ErrExists，磁盘上完整保留胜出方的初始余额，绝不覆盖历史。
// 已有句柄打开（或有在途操作）的路径同样拒绝初始化。
func CreateLedger(path string, init []BalanceInit) error {
	if err := validateInitial(init); err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ledgerError(ErrStorage, "resolve path: %v", err)
	}

	openMu.Lock()
	defer openMu.Unlock()

	// 注册表按规范路径索引（父目录符号链接解析到真实目录），与 Open 一致。
	// 注意 canonicalLedgerPath 对悬空符号链接只解析到其父目录、basename
	// 仍是链接名；而下面的发布操作刻意使用字面 abs 而非该解析结果，确保
	// 账本位置本身是符号链接时由内核原子拒绝，绝不顺着链接创建目标。
	if openRegistry[canonicalLedgerPath(abs)] != nil {
		// 已有句柄打开（或有在途操作）的路径不能再初始化：即便文件恰好被
		// 外部删除，也不能用一本新账顶替正在使用的账本。
		return ledgerError(ErrExists, "ledger is already initialized in this process")
	}

	// 账本是否存在、内容是否完好一律以磁盘的原子创建结果为准；
	// CreateLedger 不预登记内存状态，首次 Open 必须实际读取并校验该文件。
	state := &ledgerState{
		Version:     ledgerVersion,
		Initial:     make(map[balanceKey]int64, len(init)),
		Balances:    make(map[balanceKey]int64, len(init)),
		Settlements: []Record{},
		Refunds:     []RefundRecord{},
	}
	for _, b := range init {
		k := balanceKey{b.Account, b.Asset}
		state.Initial[k] = b.Balance
		state.Balances[k] = b.Balance
	}
	// 发布使用字面绝对路径：父目录中的符号链接照常解析（允许经目录软链接
	// 在尚未占用的文件位置创建新账本），账本位置本身的符号链接则被
	// link(2) 原子拒绝；占用竞争失败返回 ErrExists，写入故障返回
	// ErrStorage，两种情况下路径上都不会留下半成品。
	if err := createExclusive(abs, state); err != nil {
		return err
	}
	// 不登记 openRegistry：尚未有任何句柄，首次 Open 必须以磁盘文件为准，
	// 这样在初始化与首次打开之间删除、清空或篡改文件都会被如实发现。
	return nil
}

func validateInitial(init []BalanceInit) error {
	seen := map[balanceKey]bool{}
	for i, b := range init {
		if b.Account == "" {
			return ledgerError(ErrInvalid, "initial balance %d: account must not be empty", i)
		}
		if b.Asset == "" {
			return ledgerError(ErrInvalid, "initial balance %d: asset must not be empty", i)
		}
		if b.Balance < 0 {
			return ledgerError(ErrInvalid, "initial balance %d (%s/%s): balance must be non-negative", i, b.Account, b.Asset)
		}
		k := balanceKey{b.Account, b.Asset}
		if seen[k] {
			return ledgerError(ErrInvalid, "initial balance %d: duplicate entry for %s/%s", i, b.Account, b.Asset)
		}
		seen[k] = true
	}
	return nil
}

// Ledger 是一个可持久化的本地账本。同一进程内指向同一账本文件的多次 Open
// （包括经目录软链接的不同路径）返回共享同一状态与互斥锁的实例，
// 因此并发提交按同一本账本串行化。
type Ledger struct {
	path string
	reg  *registryEntry
	open bool
}

type registryEntry struct {
	mu       sync.Mutex // 保护 state；整个批次串行持有
	state    *ledgerState
	holders  map[*Ledger]struct{} // 当前打开的句柄
	ops      int                  // 正在执行的 Submit/Query 数（含已关闭句柄的在途操作）
	failHook FailWriter           // 仅测试注入
}

// alive 报告该注册表项是否仍须保留：至少还有一个打开的句柄，或一个
// （可能来自已关闭句柄的）在途操作。全为零时该项可被注销，此后的 Open
// 重新以磁盘文件为准。
// 调用方必须持有 openMu。
func (r *registryEntry) alive() bool {
	return len(r.holders) > 0 || r.ops > 0
}

// tryEvict 仅在没有句柄且没有在途操作时注销注册表项。
// 调用方必须持有 openMu。
func tryEvict(path string, reg *registryEntry) {
	if !reg.alive() && openRegistry[path] == reg {
		delete(openRegistry, path)
	}
}

var (
	openMu       sync.Mutex
	openRegistry = map[string]*registryEntry{}
)

// Open 打开（或复用）path 处的账本。账本必须已由 CreateLedger 初始化；
// 文件缺失返回 ErrNotInit，内容损坏返回 ErrCorrupt，不会被当成空账本重来。
func Open(path string) (*Ledger, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, ledgerError(ErrStorage, "resolve path: %v", err)
	}
	abs = canonicalLedgerPath(abs)

	openMu.Lock()
	defer openMu.Unlock()

	if reg := openRegistry[abs]; reg != nil {
		// 同进程已有句柄（或在途操作）承接该账本：共享当前完整状态，不重新读盘。
		// 注册表项只在存在活动句柄/在途操作期间保留，因此走到这里时磁盘文件
		// 的状态已不再权威——例如它可能正被一笔在途付款以原子写替换。
		h := &Ledger{path: abs, reg: reg, open: true}
		reg.holders[h] = struct{}{}
		return h, nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ledgerError(ErrNotInit, "no ledger at %s; initialize it first", abs)
		}
		return nil, ledgerError(ErrStorage, "read %s: %v", abs, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, ledgerError(ErrCorrupt, "ledger file %s is empty", abs)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var rawDoc json.RawMessage
	if err := dec.Decode(&rawDoc); err != nil {
		return nil, ledgerError(ErrCorrupt, "parse %s: %v", abs, err)
	}
	// 不允许尾随内容：防止文件被拼接/截断后被误读为合法。
	if tok, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, ledgerError(ErrCorrupt, "unexpected trailing data after ledger document (token %v)", tok)
		}
		return nil, ledgerError(ErrCorrupt, "trailing data after ledger document: %v", err)
	}
	state, err := decodeState(rawDoc)
	if err != nil {
		return nil, err
	}

	reg := &registryEntry{state: state, holders: map[*Ledger]struct{}{}}
	h := &Ledger{path: abs, reg: reg, open: true}
	reg.holders[h] = struct{}{}
	openRegistry[abs] = reg
	return h, nil
}

// Close 释放句柄；同一进程可再次 Open 同一文件并继续使用。
// 若关闭时有在途提交，注册表项保留到在途操作结束，避免并发 Open
// 从磁盘重新加载而丢失正在写入的状态。
func (l *Ledger) Close() error {
	openMu.Lock()
	defer openMu.Unlock()
	if !l.open {
		return nil
	}
	l.open = false
	delete(l.reg.holders, l)
	tryEvict(l.path, l.reg)
	return nil
}

// beginOp 标记一次在途操作。返回 nil 表示句柄已关闭。
func (l *Ledger) beginOp() error {
	openMu.Lock()
	defer openMu.Unlock()
	if !l.open {
		return ledgerError(ErrClosed, "ledger handle is closed")
	}
	l.reg.ops++
	return nil
}

func (l *Ledger) endOp() {
	openMu.Lock()
	defer openMu.Unlock()
	l.reg.ops--
	tryEvict(l.path, l.reg)
}

// 付款金额、费率、手续费与扣款总额的数值规则集中在 charge.go
// （amountValid/feeBpsValid/feeFor/chargeFor），本地结算、遗留单笔结算
// 与账本历史校验共用同一实现。

func validateIntent(it PaymentIntent) (int64, string, bool) {
	if it.ID == "" {
		return 0, reasonEmptyID, false
	}
	if it.Account == "" {
		return 0, reasonEmptyAccount, false
	}
	if it.Asset == "" {
		return 0, reasonEmptyAsset, false
	}
	if it.Amount == nil || !amountValid(*it.Amount) {
		return 0, reasonBadAmount, false
	}
	return *it.Amount, "", true
}

// validateLimits 校验本次提交的扣款上限：账户与资产必须非空，同一组合不得重复，
// max_charged 必须明确提供且为非负 int64。任一不合法则整个批次拒绝（ErrInvalid），
// 与意图列表是否为空无关。合法时返回按组合索引的上限表；无上限时返回 nil。
func validateLimits(limits []ChargeLimit) (map[balanceKey]int64, error) {
	if len(limits) == 0 {
		return nil, nil
	}
	out := make(map[balanceKey]int64, len(limits))
	for i, l := range limits {
		if l.Account == "" {
			return nil, ledgerError(ErrInvalid, "limit %d: account must not be empty", i)
		}
		if l.Asset == "" {
			return nil, ledgerError(ErrInvalid, "limit %d: asset must not be empty", i)
		}
		if l.MaxCharged == nil {
			return nil, ledgerError(ErrInvalid, "limit %d (%s/%s): max_charged must be provided", i, l.Account, l.Asset)
		}
		if *l.MaxCharged < 0 {
			return nil, ledgerError(ErrInvalid, "limit %d (%s/%s): max_charged must be non-negative, got %d", i, l.Account, l.Asset, *l.MaxCharged)
		}
		k := balanceKey{l.Account, l.Asset}
		if _, dup := out[k]; dup {
			return nil, ledgerError(ErrInvalid, "limit %d: duplicate entry for %s/%s", i, l.Account, l.Asset)
		}
		out[k] = *l.MaxCharged
	}
	return out, nil
}

// Submit 按输入顺序结算一个批次。各项独立成功或失败：后项看到前项
// 成功后的余额；任一项失败不影响其余项（存储错误只回滚出错项本身，
// 已完成的前项不回滚）。结果与输入逐项对应，空批次返回空结果。
//
// 若批次携带 limits，则每个列出的 (账户, 资产) 组合在本次提交内的成功扣款
// 累计（含手续费）不得超过其 max_charged：超限的项返回 limit_exceeded，
// 不扣余额、不留记录、不占编号与额度；未列出的组合不限制。额度按本次提交
// 从零计算，只有成功落账的扣款消耗额度，历史扣款与退款均不计入。
func (l *Ledger) Submit(batch FeeBatch) (*BatchResult, error) {
	if err := l.beginOp(); err != nil {
		return nil, err
	}
	defer l.endOp()
	limits, err := validateBatch(batch)
	if err != nil {
		return nil, err
	}

	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()

	results := runBatch(batch, limits, l.reg.state, l.persistEffect)
	return &BatchResult{Results: results}, nil
}

// Preview 在不写入账本的前提下，按当前账本预计算一个批次的结算结果，
// 语义与 Submit 逐项一致（状态、失败原因、结算记录格式均相同）：
//   - 后项看到前项“预计成功”后的可用余额与本批次已用限额；
//   - 预计成功的新付款使用从当前最大付款序号之后连续递增的序号，
//     失败、重复、冲突不消耗序号；
//   - 已有成功编号仍按原付款字段与费率判断重复或冲突（原付款已退款也一样），
//     重复项携带原记录，不占余额与本批次额度；本批次首次预计成功的编号对
//     后续项同样生效（相同请求重复、字段不同冲突），失败项不占编号；
//   - 手续费、金额溢出、状态与限额判断沿用实际提交规则，同时超限与余额不足
//     仍先报告超限，限额只累计本次预计成功的扣款（含手续费）。
//
// 预览全程在当前状态的深拷贝上进行：不持久化、不修改真实余额与历史，
// 任何编号、序号和额度都不被预留；连续预览相同批次（账本未变化）结果一致，
// 随后真实提交仍按首次提交处理。预览只预测业务结果，不承诺真实提交时的
// 写入成功（因此预览结果中不会出现 storage_error）。费率或限额非法属于
// 批次级错误，返回 invalid_parameter 且不产生部分结果（空批次同样检查）。
func (l *Ledger) Preview(batch FeeBatch) (*PreviewResult, error) {
	if err := l.beginOp(); err != nil {
		return nil, err
	}
	defer l.endOp()
	limits, err := validateBatch(batch)
	if err != nil {
		return nil, err
	}

	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()

	// 在快照副本上模拟：预览结束后真实状态与磁盘文件保持原样。
	sim := cloneState(l.reg.state)
	results := runBatch(batch, limits, sim, simulateEffect)
	return &PreviewResult{DryRun: true, Results: results}, nil
}

// validateBatch 执行批次级校验：费率范围与本次扣款上限。任一不合法都返回
// 批次级错误（invalid_parameter），调用方不得执行任何意图（空批次同样检查）。
func validateBatch(batch FeeBatch) (map[balanceKey]int64, error) {
	if !feeBpsValid(batch.FeeBps) {
		return nil, ledgerError(ErrInvalid, "%s", reasonBadFeeBps(batch.FeeBps))
	}
	// 限额非法是批次级错误：任何意图都不执行（空意图列表同样检查）。
	return validateLimits(batch.Limits)
}

// ledgerEffect 是一笔预计成功落账（付款扣款或退款退回）的全部账本效果：
// 变动哪个组合、余额增减多少、向哪一类成功历史追加什么记录。delta 为正
// 表示增加余额（退款），为负表示减少余额（付款扣款）。它由各自的业务规则
// 产生，本身尚未生效；是否生效、如何生效（真实落盘或预览模拟）由生效策略
// 决定。付款与退款共用这一个效果结构，因此“改余额 + 追加成功记录 +
// 保存失败恢复原状”的落账步骤只需在 applyEffect 一处维护。
type ledgerEffect struct {
	key      balanceKey
	delta    int64
	settling *Record       // 付款成功时追加到结算历史；退款时为 nil
	refund   *RefundRecord // 退款成功时追加到退款历史；付款时为 nil
}

// applyEffect 把一笔预计成功的落账效果应用到状态 st：余额变动与对应成功
// 记录在同一步完成，查询不会看到只有余额变化而没有记录的状态。返回回滚
// 函数，恢复该项执行前的余额与对应历史长度（只撤销该项自己的变化，
// 不动另一类历史、不动之前已成功的记录）。付款与退款的落账与回滚都走
// 这一个出口。
func applyEffect(st *ledgerState, eff ledgerEffect) func() {
	oldBal, hadKey := st.Balances[eff.key]
	st.Balances[eff.key] = oldBal + eff.delta

	var oldSettlements, oldRefunds int
	if eff.settling != nil {
		oldSettlements = len(st.Settlements)
		st.Settlements = append(st.Settlements, *eff.settling)
	} else {
		oldRefunds = len(st.Refunds)
		st.Refunds = append(st.Refunds, *eff.refund)
	}

	return func() {
		if hadKey {
			st.Balances[eff.key] = oldBal
		} else {
			delete(st.Balances, eff.key)
		}
		if eff.settling != nil {
			st.Settlements = st.Settlements[:oldSettlements]
		} else {
			st.Refunds = st.Refunds[:oldRefunds]
		}
	}
}

// commitEffect 是落账生效策略：把一笔预计成功的效果应用到状态 st 上。
// 返回 nil 表示已生效；返回非 nil 表示保存失败，且 st 必须已恢复到
// 该项执行之前的余额与历史。
type commitEffect func(st *ledgerState, eff ledgerEffect) error

// runBatch 按输入顺序在给定状态 st 上处理整个批次，返回逐项结果。
// 付款规则（judgeIntent）与落账生效（commit）分离：规则只读取 st 与
// 本批次已用额度给出判定和预计落账效果，不改动任何状态；生效策略决定
// 效果如何落地——真实提交先改内存再原子落盘、失败回滚并报告
// storage_error，预览只在副本上推进内存状态、绝不落盘。两种操作共用
// 同一份规则，因此逐项判定完全一致。调用方必须持有 reg.mu。
func runBatch(batch FeeBatch, limits map[balanceKey]int64, st *ledgerState, commit commitEffect) []ItemResult {
	out := make([]ItemResult, 0, len(batch.Intents))

	// 本次批次内各组合已消耗的额度，从零开始，仅成功生效的扣款计入。
	used := make(map[balanceKey]int64, len(limits))

	for i := range batch.Intents {
		it := &batch.Intents[i]

		// 1. 规则判定：纯读取，不产生任何副作用。
		res, eff := judgeIntent(it, batch.FeeBps, limits, used, st)
		if eff == nil {
			out = append(out, res)
			continue
		}

		// 2. 生效：保存失败时 commit 已回滚该项（余额与历史恢复原样），
		//    该项报告 storage_error，不留记录、不占额度，后项继续处理。
		if err := commit(st, *eff); err != nil {
			res.Status = StatusStorage
			res.Reason = err.Error()
			out = append(out, res)
			continue
		}

		// 3. 只有成功生效的扣款才消耗本批次额度（预览同样如此）。
		used[eff.key] += -eff.delta
		cp := *eff.settling
		res.Status = StatusSettled
		res.Record = &cp
		out = append(out, res)
	}
	return out
}

// judgeIntent 按付款规则评估一项请求，只读取状态与额度用量，不产生副作用。
// 返回逐项结果；预计成功时结果只填编号，落账效果以 ledgerEffect 描述，
// 由调用方交给生效策略落地。判定顺序即对外约定：参数校验、已成功编号的
// 重复/冲突、状态、溢出、本批次限额（先于余额）、余额。
func judgeIntent(it *PaymentIntent, feeBps int, limits map[balanceKey]int64, used map[balanceKey]int64, st *ledgerState) (ItemResult, *ledgerEffect) {
	res := ItemResult{ID: it.ID}

	// 1. 参数校验。未通过的编号不占用去重资格。
	amount, reason, ok := validateIntent(*it)
	if !ok {
		res.Status = StatusInvalid
		res.Reason = reason
		return res, nil
	}

	// 2. 已成功编号：字段与费率完全一致则幂等返回原记录；
	//    任一不同则编号冲突。两者都不再扣款、不改原记录。
	//    本批次内先（预计）成功的同编号项也在此被看到。
	if rec, exists := findRecordIn(st.Settlements, it.ID); exists {
		if sameRequest(rec, it, amount, feeBps) {
			cp := rec
			res.Status = StatusDuplicate
			res.Reason = reasonDuplicate
			res.Record = &cp
		} else {
			res.Status = StatusConflict
			res.Reason = reasonConflict
		}
		return res, nil
	}

	// 3. 状态错误单独报告（不占用去重资格，之后可重新提交）。
	if it.State != "" && it.State != "pending" {
		res.Status = StatusState
		res.Reason = reasonNotPending
		return res, nil
	}

	// 4. 手续费与扣款总额（溢出明确按参数错误拒绝）。
	fee, total, ok := chargeFor(amount, feeBps)
	if !ok {
		res.Status = StatusInvalid
		res.Reason = reasonOverflow
		return res, nil
	}

	key := balanceKey{it.Account, it.Asset}

	// 5. 本次批次限额（先于余额判断：同时超限与余额不足时报告超限）。
	//    恰好达到上限仍允许；用 total > max-used 而非 used+total > max，
	//    避免累计接近 int64 上界时溢出放行。失败不消耗额度。
	if max, capped := limits[key]; capped {
		if u := used[key]; total > max-u {
			res.Status = StatusLimitExceeded
			res.Reason = fmt.Sprintf(reasonLimitExceeded, it.Account, it.Asset, max, u, total)
			return res, nil
		}
	}

	// 6. 余额校验。不存在的账户/资产组合余额视为 0（不足）。
	if total > st.Balances[key] {
		res.Status = StatusFunds
		res.Reason = reasonFunds
		return res, nil
	}

	// 7. 预计成功：描述落账效果（余额扣减总额、序号按当前结算历史长度
	//    连续递增）。
	rec := Record{
		ID:        it.ID,
		Account:   it.Account,
		Paymaster: it.Paymaster,
		Asset:     it.Asset,
		Amount:    amount,
		Nonce:     it.Nonce,
		FeeBps:    feeBps,
		Fee:       fee,
		Charged:   total,
		Seq:       int64(len(st.Settlements)) + 1,
	}
	return res, &ledgerEffect{
		key:      key,
		delta:    -total,
		settling: &rec,
	}
}

// simulateEffect 是预览的生效策略：只在（副本）状态上推进余额与对应
// 历史，绝不触碰磁盘，因此不会失败，预览结果中也不会出现 storage_error。
func simulateEffect(st *ledgerState, eff ledgerEffect) error {
	applyEffect(st, eff)
	return nil
}

// persistEffect 是真实落账（付款与退款共用）的生效策略：先在内存生效
// （余额变动与成功记录同一步完成），再原子落盘；落盘失败回滚该项
// （applyEffect 返回的回滚恢复余额与对应历史），并返回存储错误。
func (l *Ledger) persistEffect(st *ledgerState, eff ledgerEffect) error {
	rollback := applyEffect(st, eff)
	if err := l.persistLocked(); err != nil {
		rollback()
		return err
	}
	return nil
}

// cloneState 返回账本状态的独立深拷贝，供预览在副本上模拟而不触碰真实状态。
func cloneState(st *ledgerState) *ledgerState {
	cp := &ledgerState{
		Version:     st.Version,
		Initial:     make(map[balanceKey]int64, len(st.Initial)),
		Balances:    make(map[balanceKey]int64, len(st.Balances)),
		Settlements: append([]Record(nil), st.Settlements...),
		Refunds:     append([]RefundRecord(nil), st.Refunds...),
	}
	for k, v := range st.Initial {
		cp.Initial[k] = v
	}
	for k, v := range st.Balances {
		cp.Balances[k] = v
	}
	return cp
}

// findRecordIn 在给定结算历史中按编号查找成功记录。
func findRecordIn(records []Record, id string) (Record, bool) {
	// 记录按成功顺序追加且 ID 唯一；单机账本量级直接扫描。
	for _, r := range records {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// validateRefundRequest 校验一项退款请求：退款编号、原付款编号、原因三者
// 都必须非空，校验顺序即对外约定（id → settlement_id → reason）。
// 未通过校验的编号不占用退款资格，之后可修改后重新提交。
func validateRefundRequest(req *RefundRequest) (string, bool) {
	if req.ID == "" {
		return reasonEmptyRefundID, false
	}
	if req.SettlementID == "" {
		return reasonEmptySettlement, false
	}
	if req.Reason == "" {
		return reasonEmptyReason, false
	}
	return "", true
}

// attachRefundDetail 把一笔成功退款（首次成功或幂等重复携带的同一条原记录）
// 的统一详情填入逐项结果：原付款编号、账户、资产、退款总额、原因与完整退款
// 记录。首次成功与重复申请共用这一个出口，保证两者拼装出的详情逐字一致。
func attachRefundDetail(res *RefundResult, rec RefundRecord) {
	res.Reason = rec.Reason
	res.SettlementID = rec.SettlementID
	res.Account = rec.Account
	res.Asset = rec.Asset
	res.Charged = rec.Charged
	cp := rec
	res.Record = &cp
}

// judgeRefund 按退款规则评估一项请求，只读取状态 st，不产生任何副作用。
// 返回逐项结果；预计成功时结果只填编号，落账效果以 ledgerEffect 描述，
// 由调用方交给生效策略落地。判定顺序即对外约定：参数校验、已成功退款编号
// 的重复/冲突（优先于目标判断，即使新目标不存在也不退化成 not_found）、
// 目标结算存在性、目标是否已被其他退款编号退回。
func judgeRefund(req *RefundRequest, st *ledgerState) (RefundResult, *ledgerEffect) {
	res := RefundResult{ID: req.ID}

	// 1. 参数校验。未通过的编号不占用退款资格。
	if reason, ok := validateRefundRequest(req); !ok {
		res.Status = StatusInvalid
		res.Reason = reason
		return res, nil
	}

	// 2. 已成功退款编号优先判定：目标与原因都相同则幂等返回原退款记录；
	//    任一不同则冲突（即使新目标不存在也不能退化成 not_found）。
	//    两者都不再入账。本批次内先成功的同编号项也在此被看到。
	if prior, exists := findRefundIn(st.Refunds, req.ID); exists {
		if prior.SettlementID == req.SettlementID && prior.Reason == req.Reason {
			res.Status = StatusDuplicate
			attachRefundDetail(&res, prior)
		} else {
			res.Status = StatusConflict
			res.Reason = reasonRefundConflict
		}
		return res, nil
	}

	// 3. 新退款编号：目标必须是一笔成功结算（原付款只曾失败也算不存在）。
	target, exists := findRecordIn(st.Settlements, req.SettlementID)
	if !exists {
		res.Status = StatusNotFound
		res.Reason = fmt.Sprintf(reasonSettlementGone, req.SettlementID)
		return res, nil
	}

	// 4. 每笔原付款最多退回一次：已被其他退款编号退回则报告已退款，不加余额。
	if prior, refunded := findRefundOfSettlementIn(st.Refunds, req.SettlementID); refunded {
		res.Status = StatusAlreadyRefunded
		res.Reason = fmt.Sprintf(reasonAlreadyRefunded, req.SettlementID, prior.ID)
		return res, nil
	}

	// 5. 预计成功：全额（amount 与当时手续费一并，即以原结算的 charged 为准）
	//    退回原账户、原资产；序号按当前退款历史长度连续递增。
	rec := RefundRecord{
		ID:           req.ID,
		SettlementID: target.ID,
		Reason:       req.Reason,
		Account:      target.Account,
		Asset:        target.Asset,
		Amount:       target.Amount,
		Fee:          target.Fee,
		Charged:      target.Charged,
		AfterSeq:     int64(len(st.Settlements)),
		Seq:          int64(len(st.Refunds)) + 1,
	}
	return res, &ledgerEffect{
		key:    balanceKey{target.Account, target.Asset},
		delta:  target.Charged,
		refund: &rec,
	}
}

// runRefundBatch 按输入顺序在给定状态 st 上处理整个退款批次，返回逐项结果。
// 退款规则（judgeRefund）与落账生效（commit）分离：规则只读取 st 给出判定
// 和预计落账效果，不改动任何状态；生效策略与付款提交共用同一套
// applyEffect/落盘/回滚步骤——真实提交先改内存再原子落盘、失败回滚并报告
// storage_error。保存失败时 commit 已回滚该项（余额与退款历史恢复原样），
// 该项不占退款编号、不把原付款标成已退款，前项成功保留、后项继续处理。
// 调用方必须持有 reg.mu。
func runRefundBatch(batch RefundBatch, st *ledgerState, commit commitEffect) []RefundResult {
	out := make([]RefundResult, 0, len(batch.Refunds))
	for i := range batch.Refunds {
		req := &batch.Refunds[i]

		// 1. 规则判定：纯读取，不产生任何副作用。
		res, eff := judgeRefund(req, st)
		if eff == nil {
			out = append(out, res)
			continue
		}

		// 2. 生效：保存失败时 commit 已回滚该项（余额与历史恢复原样），
		//    该项报告 storage_error，不留退款记录、不占退款序号，后项继续。
		if err := commit(st, *eff); err != nil {
			res.Status = StatusStorage
			res.Reason = err.Error()
			out = append(out, res)
			continue
		}

		// 3. 成功落账：与重复申请一样通过统一出口拼装详情。
		res.Status = StatusRefundSuccess
		attachRefundDetail(&res, *eff.refund)
		out = append(out, res)
	}
	return out
}

// findRefundIn 在给定退款历史中按编号查找成功记录。
func findRefundIn(refunds []RefundRecord, id string) (RefundRecord, bool) {
	// 退款按成功顺序追加且 ID 唯一；单机账本量级直接扫描。
	for _, r := range refunds {
		if r.ID == id {
			return r, true
		}
	}
	return RefundRecord{}, false
}

// findRefundOfSettlementIn 返回退回指定结算的那笔退款（每笔结算最多一笔）。
func findRefundOfSettlementIn(refunds []RefundRecord, settlementID string) (RefundRecord, bool) {
	for _, r := range refunds {
		if r.SettlementID == settlementID {
			return r, true
		}
	}
	return RefundRecord{}, false
}

// Refund 按输入顺序处理一个退款批次。各项独立处理：后项能看到前项成功退回的
// 余额；任一项失败不影响其余项（存储错误只回滚出错项本身，前项保留，后项继续）。
// 结果与输入逐项对应，空批次返回空结果。
//
// 语义：
//   - 参数合法时，已成功的退款编号优先判定：目标结算与原因都相同返回 duplicate
//     及原退款记录（不再入账）；任一不同返回 conflict（即使新目标不存在也一样，
//     不能退化成 not_found）。
//   - 新退款编号：找不到成功结算（含原付款只曾失败）返回 not_found；
//     该结算已被其他退款编号退回返回 already_refunded，不增加余额。
//   - 成功时把原记录的 charged（含当时已扣手续费）全额退回原账户、原资产；
//     退款金额与去向完全由原结算确定，原结算记录及其编号、顺序保持不变。
//   - 失败申请（参数错误、目标不存在、已退款、冲突、存储错误）不占用退款编号，
//     之后可修改原因或目标再提交。
func (l *Ledger) Refund(batch RefundBatch) (*RefundBatchResult, error) {
	if err := l.beginOp(); err != nil {
		return nil, err
	}
	defer l.endOp()

	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()

	results := runRefundBatch(batch, l.reg.state, l.persistEffect)
	return &RefundBatchResult{Results: results}, nil
}

func (l *Ledger) stateView() *ledgerState { return l.reg.state }

// sameRequest 比较全部付款字段与费率。付款字段：账户、付款方、资产、金额、nonce。
func sameRequest(r Record, it *PaymentIntent, amount int64, feeBps int) bool {
	return r.Account == it.Account &&
		r.Paymaster == it.Paymaster &&
		r.Asset == it.Asset &&
		r.Amount == amount &&
		r.Nonce == it.Nonce &&
		r.FeeBps == feeBps
}

// Query 返回余额视图（按账户、资产排序，保证确定性）与按成功先后排列的记录。
func (l *Ledger) Query() (*Snapshot, error) {
	if err := l.beginOp(); err != nil {
		return nil, err
	}
	defer l.endOp()
	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()

	st := l.stateView()
	bals := make([]BalanceView, 0, len(st.Balances))
	for k, v := range st.Balances {
		bals = append(bals, BalanceView{Account: k.account, Asset: k.asset, Balance: v})
	}
	sortBalances(bals)
	recs := make([]Record, len(st.Settlements))
	copy(recs, st.Settlements)
	refunds := make([]RefundRecord, len(st.Refunds))
	copy(refunds, st.Refunds)
	return &Snapshot{Balances: bals, Settlements: recs, Refunds: refunds}, nil
}

// Balance 返回单个 (账户, 资产) 组合的余额；组合不存在按 0 处理。
func (l *Ledger) Balance(account, asset string) (int64, error) {
	if err := l.beginOp(); err != nil {
		return 0, err
	}
	defer l.endOp()
	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()
	return l.stateView().Balances[balanceKey{account, asset}], nil
}

func sortBalances(bals []BalanceView) {
	sort.Slice(bals, func(i, j int) bool {
		if bals[i].Account != bals[j].Account {
			return bals[i].Account < bals[j].Account
		}
		return bals[i].Asset < bals[j].Asset
	})
}

// persistLocked 将当前状态原子写入磁盘；调用方必须持有 reg.mu。
func (l *Ledger) persistLocked() error {
	if l.reg.failHook != nil && l.reg.failHook.FailNextPersist() {
		return ledgerError(ErrStorage, "injected write failure")
	}
	return atomicWrite(l.path, l.stateView())
}

// SetFailHook 为该路径的账本安装存储故障注入，仅用于测试。
func SetFailHook(l *Ledger, fw FailWriter) {
	l.reg.failHook = fw
}
