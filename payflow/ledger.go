package payflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
//	 "checksum":"<sha256 of the document without checksum>"}
//
// initial_balances 在初始化时冻结，用于检测任何对历史余额/记录的篡改；
// checksum 覆盖截断、比特翻转与所有语义编辑。
//
// 写入采用“临时文件 + fsync + 原子 rename + 目录 fsync”，
// 因此崩溃后磁盘上只会存在完整的旧版本或完整新版本，
// 不会出现“扣款无记录”或“记录未扣款”的中间状态。
const ledgerVersion = 1

// 结算结果状态，与命令行输出一一对应，可分别识别。
const (
	StatusSettled   = "settled"              // 成功扣款
	StatusDuplicate = "duplicate"            // 幂等重试：字段与原记录一致，返回原结算，不再扣款
	StatusConflict  = "conflict"             // 编号冲突：编号已成功但字段或费率不同
	StatusState     = "state_error"          // 非 pending
	StatusFunds     = "insufficient_balance" // 余额不足（含不存在的账户/资产组合）
	StatusInvalid   = "invalid_parameter"
	StatusStorage   = "storage_error"
)

// 拒绝原因。
const (
	reasonEmptyID      = "intent id must not be empty"
	reasonEmptyAccount = "account must not be empty"
	reasonEmptyAsset   = "asset must not be empty"
	reasonBadAmount    = "amount must be a positive int64"
	reasonOverflow     = "amount plus fee overflows int64"
	reasonNotPending   = "intent is not pending"
	reasonFunds        = "insufficient balance for amount plus fee"
	reasonDuplicate    = "duplicate settlement attempt"
	reasonConflict     = "settlement id conflicts with a different request"
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

// FeeBatch 是一次提交：固定费率（基点）+ 有序意图列表。
type FeeBatch struct {
	FeeBps  int             `json:"fee_bps"`
	Intents []PaymentIntent `json:"intents"`
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

// BalanceView 是一个 (账户, 资产) 组合的当前余额。
type BalanceView struct {
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Balance int64  `json:"balance"`
}

// Snapshot 是账本查询结果。
type Snapshot struct {
	Balances    []BalanceView `json:"balances"`
	Settlements []Record      `json:"settlements"`
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
}

// 磁盘文档字段（顺序即签名时的规范顺序，不得调整，否则旧文件校验失败）。
type unsignedDoc struct {
	Version     int           `json:"version"`
	Initial     []BalanceView `json:"initial_balances"`
	Balances    []BalanceView `json:"balances"`
	Settlements []Record      `json:"settlements"`
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
	if err := validateAndReplay(initial, current, records); err != nil {
		return err
	}
	s.Version = doc.Version
	s.Initial = initial
	s.Balances = current
	s.Settlements = records
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

// CreateLedger 在 path 处创建新账本。所有初始数据先校验，
// 任一不合法则返回 ErrInvalid 且不创建任何文件；
// 路径上已有账本则返回 ErrExists，绝不覆盖历史。
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

	if _, err := os.Stat(abs); err == nil {
		return ledgerError(ErrExists, "ledger already exists at %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ledgerError(ErrStorage, "stat %s: %v", abs, err)
	}
	if openRegistry[abs] != nil {
		return ledgerError(ErrExists, "ledger is already initialized in this process")
	}

	state := &ledgerState{
		Version:     ledgerVersion,
		Initial:     make(map[balanceKey]int64, len(init)),
		Balances:    make(map[balanceKey]int64, len(init)),
		Settlements: []Record{},
	}
	for _, b := range init {
		k := balanceKey{b.Account, b.Asset}
		state.Initial[k] = b.Balance
		state.Balances[k] = b.Balance
	}
	if err := atomicWrite(abs, state); err != nil {
		return err
	}
	openRegistry[abs] = &registryEntry{state: state}
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

// Ledger 是一个可持久化的本地账本。同一进程内同一路径的多次 Open
// 返回共享同一状态与互斥锁的实例，因此并发提交按同一本账本串行化。
type Ledger struct {
	path string
	reg  *registryEntry
	open bool
}

type registryEntry struct {
	mu       sync.Mutex // 保护 state；整个批次串行持有
	state    *ledgerState
	refs     int        // 打开的句柄数
	ops      int        // 正在执行的 Submit/Query 数
	failHook FailWriter // 仅测试注入
}

// tryEvict 仅在没有句柄且没有在途操作时注销注册表项。
// 调用方必须持有 openMu。
func tryEvict(path string, reg *registryEntry) {
	if reg.refs == 0 && reg.ops == 0 && openRegistry[path] == reg {
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

	openMu.Lock()
	defer openMu.Unlock()

	if reg := openRegistry[abs]; reg != nil {
		reg.refs++
		return &Ledger{path: abs, reg: reg, open: true}, nil
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

	reg := &registryEntry{state: state, refs: 1}
	openRegistry[abs] = reg
	return &Ledger{path: abs, reg: reg, open: true}, nil
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
	l.reg.refs--
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

// feeFor 计算手续费：amount*feeBps/10000 向下取整。
// amount>0、0<=feeBps<=10000 时朴素乘积可能溢出 int64，
// 拆成商和余数两部分即可在 int64 内精确完成。
func feeFor(amount int64, feeBps int) int64 {
	hi := amount / 10000
	lo := amount % 10000
	return hi*int64(feeBps) + (lo*int64(feeBps))/10000
}

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
	if it.Amount == nil || *it.Amount <= 0 {
		return 0, reasonBadAmount, false
	}
	return *it.Amount, "", true
}

// Submit 按输入顺序结算一个批次。各项独立成功或失败：后项看到前项
// 成功后的余额；任一项失败不影响其余项（存储错误只回滚出错项本身，
// 已完成的前项不回滚）。结果与输入逐项对应，空批次返回空结果。
func (l *Ledger) Submit(batch FeeBatch) (*BatchResult, error) {
	if err := l.beginOp(); err != nil {
		return nil, err
	}
	defer l.endOp()
	if batch.FeeBps < 0 || batch.FeeBps > 10000 {
		return nil, ledgerError(ErrInvalid, "fee_bps must be within [0,10000], got %d", batch.FeeBps)
	}
	out := &BatchResult{Results: make([]ItemResult, 0, len(batch.Intents))}

	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()

	for i := range batch.Intents {
		it := &batch.Intents[i]
		res := ItemResult{ID: it.ID}

		// 1. 参数校验。未通过的编号不占用去重资格。
		amount, reason, ok := validateIntent(*it)
		if !ok {
			res.Status = StatusInvalid
			res.Reason = reason
			out.Results = append(out.Results, res)
			continue
		}

		// 2. 已成功编号：字段与费率完全一致则幂等返回原记录；
		//    任一不同则编号冲突。两者都不再扣款、不改原记录。
		//    本批次内先成功的同编号项也在此被看到。
		if rec, exists := l.findRecord(it.ID); exists {
			if sameRequest(rec, it, amount, batch.FeeBps) {
				cp := rec
				res.Status = StatusDuplicate
				res.Reason = reasonDuplicate
				res.Record = &cp
			} else {
				res.Status = StatusConflict
				res.Reason = reasonConflict
			}
			out.Results = append(out.Results, res)
			continue
		}

		// 3. 状态错误单独报告（不占用去重资格，之后可重新提交）。
		if it.State != "" && it.State != "pending" {
			res.Status = StatusState
			res.Reason = reasonNotPending
			out.Results = append(out.Results, res)
			continue
		}

		// 4. 手续费与扣款总额（溢出明确按参数错误拒绝）。
		fee := feeFor(amount, batch.FeeBps)
		if fee > math.MaxInt64-amount {
			res.Status = StatusInvalid
			res.Reason = reasonOverflow
			out.Results = append(out.Results, res)
			continue
		}
		total := amount + fee

		// 5. 余额校验。不存在的账户/资产组合余额视为 0（不足）。
		key := balanceKey{it.Account, it.Asset}
		bal := l.reg.state.Balances[key]
		if total > bal {
			res.Status = StatusFunds
			res.Reason = reasonFunds
			out.Results = append(out.Results, res)
			continue
		}

		// 6. 先在内存生效，再原子持久化；保存失败回滚该项，不留成功记录。
		rec := Record{
			ID:        it.ID,
			Account:   it.Account,
			Paymaster: it.Paymaster,
			Asset:     it.Asset,
			Amount:    amount,
			Nonce:     it.Nonce,
			FeeBps:    batch.FeeBps,
			Fee:       fee,
			Charged:   total,
			Seq:       int64(len(l.reg.state.Settlements)) + 1,
		}
		rollback := l.stage(key, bal-total, rec)
		if err := l.persistLocked(); err != nil {
			rollback()
			res.Status = StatusStorage
			res.Reason = err.Error()
		} else {
			cp := rec
			res.Status = StatusSettled
			res.Record = &cp
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// stage 应用一笔扣款与记录，返回回滚函数（恢复该项执行前的余额与记录长度）。
func (l *Ledger) stage(key balanceKey, newBal int64, rec Record) func() {
	oldBal, hadKey := l.reg.state.Balances[key]
	oldLen := len(l.reg.state.Settlements)
	l.reg.state.Balances[key] = newBal
	l.reg.state.Settlements = append(l.reg.state.Settlements, rec)
	return func() {
		if hadKey {
			l.reg.state.Balances[key] = oldBal
		} else {
			delete(l.reg.state.Balances, key)
		}
		l.reg.state.Settlements = l.reg.state.Settlements[:oldLen]
	}
}

func (l *Ledger) stateView() *ledgerState { return l.reg.state }

func (l *Ledger) findRecord(id string) (Record, bool) {
	// 记录按成功顺序追加且 ID 唯一；条目规模在单机账本量级，直接扫描。
	for _, r := range l.stateView().Settlements {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

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
	return &Snapshot{Balances: bals, Settlements: recs}, nil
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
