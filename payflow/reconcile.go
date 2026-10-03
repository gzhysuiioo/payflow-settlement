package payflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
)

// 离线流水对账：把外部支付渠道提供的有序流水与本地账本逐笔核对。
// 对账是只读操作，不改变余额、历史或账本文件。
//
// 逐条结果（保持输入顺序）有五种：
//   - matched：编号命中一条成功记录，且全部核对字段一致；
//   - field_mismatch：编号命中成功记录，但至少一个字段不同（逐个列出差异）；
//   - missing_in_ledger：账本中没有该编号的成功记录；
//   - duplicate：同一(kind,id)在输入中出现多次，整组全部标为重复并附全部
//     输入位置，不再判断字段差异，也不再把对应账本记录列为缺失；
//   - out_of_scope：命中同类型同编号的成功记录，但该记录的成功序号不在本次
//     核对范围内（after < seq <= through）。不比较字段、不计入流水侧汇总；
//     该编号在流水中出现多次时全部按此状态处理（优先于整组重复）。
//
// 付款编号与退款编号分别匹配，同名也不会混为一笔。两类流水的逐条规则完全
// 相同，只在匹配对象（结算记录/退款记录）、携带的关联（退款编号/原付款编号）
// 与特有核对字段（退款的 settlement_id）上有区别，统一由 reconFlowKind 抽象。
// 只核对成功记录：已退款的付款仍按原扣款核对，退款单独核对。账本中没有任何
// 对应流水的入选记录另列缺失（先按付款成功顺序，再按退款成功顺序）。范围只
// 影响记录是否参加核对，不切断关联：入选退款仍可指向范围外付款，入选付款仍
// 携带范围外退款的信息。

const (
	ReconMatched         = "matched"
	ReconFieldMismatch   = "field_mismatch"
	ReconMissingInLedger = "missing_in_ledger"
	ReconDuplicate       = "duplicate"
	ReconOutOfScope      = "out_of_scope"
)

// amountJSON 是必须提供、且为 int64 整数的金额。
// 使用指针 + 自定义解码区分“缺省/null/非数字/越界”等情形。
type amountJSON struct {
	v int64
}

func (a *amountJSON) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return fmt.Errorf("amount must be an int64 integer, not null")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var v int64
	if err := dec.Decode(&v); err != nil {
		return err
	}
	// 单个数字字面量必须恰好结束。
	if tok, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("invalid integer amount (unexpected token %v)", tok)
		}
		return err
	}
	a.v = v
	return nil
}

// flowEntryJSON 是流水的严格解码形态。未知字段按标准库惯例忽略。
type flowEntryJSON struct {
	Kind         string      `json:"kind"`
	ID           string      `json:"id"`
	Account      string      `json:"account"`
	Asset        string      `json:"asset"`
	Amount       *amountJSON `json:"amount"`
	Fee          *amountJSON `json:"fee"`
	Charged      *amountJSON `json:"charged"`
	SettlementID string      `json:"settlement_id"`
}

// flowRequestJSON 是对账输入：{"entries":[...]}。
type flowRequestJSON struct {
	Entries []flowEntryJSON `json:"entries"`
}

// FlowEntry 是校验通过后的一条流水。Index 是从 0 开始的输入位置。
type FlowEntry struct {
	Index        int
	Kind         string
	ID           string
	Account      string
	Asset        string
	Amount       int64
	Fee          int64
	Charged      int64
	SettlementID string // 仅退款有意义
}

// FieldDiff 是一个不同字段：账本值与流水值逐字并列。
type FieldDiff struct {
	Field  string `json:"field"`
	Ledger string `json:"ledger"`
	Flow   string `json:"flow"`
}

// ReconRecordRef 指向一条账本成功记录，并携带其关联记录（付款被退时的退款
// 编号、退款的原付款编号），使每个能定位到账本的结果都可追溯。
type ReconRecordRef struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	Account      string `json:"account"`
	Asset        string `json:"asset"`
	Amount       int64  `json:"amount"`
	Fee          int64  `json:"fee"`
	Charged      int64  `json:"charged"`
	Seq          int64  `json:"seq"`
	SettlementID string `json:"settlement_id,omitempty"` // 退款记录：原付款编号
	RefundID     string `json:"refund_id,omitempty"`     // 结算已被退款时：退款编号
	RefundSeq    int64  `json:"refund_seq,omitempty"`
}

// ReconResult 是一条输入流水的核对结果，顺序与输入一致。
type ReconResult struct {
	Index     int             `json:"index"`
	Kind      string          `json:"kind"`
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Positions []int           `json:"positions,omitempty"` // duplicate：整组全部输入位置
	Diffs     []FieldDiff     `json:"diffs,omitempty"`
	Record    *ReconRecordRef `json:"record,omitempty"` // 命中账本记录时带上，可追溯关联结算/退款
}

// ReconTotalRow 是一个 (账户, 资产) 组合单侧的金额汇总。金额使用十进制整数
// 字符串，累计可超过 int64 仍保持精确；不同资产各自一行，不相加、不抵消。
type ReconTotalRow struct {
	Account       string `json:"account"`
	Asset         string `json:"asset"`
	ChargedTotal  string `json:"charged_total"`
	RefundedTotal string `json:"refunded_total"`
	NetCharged    string `json:"net_charged"`
}

// ReconNetDiff 是一个组合的净扣款差额，方向为流水减账本。
type ReconNetDiff struct {
	Account          string `json:"account"`
	Asset            string `json:"asset"`
	LedgerNetCharged string `json:"ledger_net_charged"`
	FlowNetCharged   string `json:"flow_net_charged"`
	NetChargedDiff   string `json:"net_charged_diff"`
}

// ReconTotals 是账本侧与流水侧各自的汇总。
type ReconTotals struct {
	Ledger []ReconTotalRow `json:"ledger"`
	Flow   []ReconTotalRow `json:"flow"`
}

// ReconReport 是一次对账的完整报告。
type ReconReport struct {
	Results         []ReconResult    `json:"results"`
	MissingPayments []ReconRecordRef `json:"missing_payments"`
	MissingRefunds  []ReconRecordRef `json:"missing_refunds"`
	Totals          ReconTotals      `json:"totals"`
	NetDiff         []ReconNetDiff   `json:"net_diff"`
	MaxPaymentSeq   int64            `json:"max_payment_seq"`
	MaxRefundSeq    int64            `json:"max_refund_seq"`
	PaymentAfter    int64            `json:"payment_after"`
	PaymentThrough  int64            `json:"payment_through"`
	RefundAfter     int64            `json:"refund_after"`
	RefundThrough   int64            `json:"refund_through"`
}

// ReconcileBounds 限定付款与退款各自的核对范围：每类记录只核对成功序号
// 满足 after < seq <= through 的记录（下界排除、上界包含）。两类记录各用
// 自己的序号，分别选择范围，不根据退款引用的付款序号判断退款是否入选。
//
// 指针为 nil 表示该侧不限制：下界缺省取 0，上界缺省取本次所见该类记录的
// 最大成功序号（即完整历史）。显式给出的上下界必须是非负 int64，且
// after <= through、through 不超过对应类别的最大成功序号；after == through
// 表示范围为空（该类记录全部不参加核对）。
type ReconcileBounds struct {
	PaymentAfter   *int64
	PaymentThrough *int64
	RefundAfter    *int64
	RefundThrough  *int64
}

// validateFlowEntry 校验一条流水的语义约束（JSON 解析与 Go 直接调用共用，
// 保证两种提交方式对相同内容给出一致判断）：kind 只能是 payment/refund；
// 编号、账户、资产不能为空；amount、charged 必须为正，fee 必须非负；
// refund 还必须带非空 settlement_id。i 是该条目在传入列表中的位置。
//
// 注意这里只校验输入本身：amount+fee 是否等于 charged、退款引用的原付款
// 是否与账本一致属于核对结论（field_mismatch），不是参数错误。
func validateFlowEntry(i int, e FlowEntry) error {
	at := func(msg string) error {
		return ledgerError(ErrInvalid, "reconcile entry %d: %s", i, msg)
	}
	if e.Kind != "payment" && e.Kind != "refund" {
		return at(fmt.Sprintf("unknown kind %q (want payment or refund)", e.Kind))
	}
	if e.ID == "" {
		return at("id must not be empty")
	}
	if e.Account == "" {
		return at("account must not be empty")
	}
	if e.Asset == "" {
		return at("asset must not be empty")
	}
	if e.Amount <= 0 {
		return at("amount must be a positive int64")
	}
	if e.Fee < 0 {
		return at("fee must be a non-negative int64")
	}
	if e.Charged <= 0 {
		return at("charged must be a positive int64")
	}
	if e.Kind == "refund" && e.SettlementID == "" {
		return at("settlement_id must not be empty for a refund")
	}
	return nil
}

// ParseReconcileRequest 严格解析对账输入。
// 字段缺失、类型不符、未知 kind、数值越界或非法 JSON 均返回 invalid_parameter；
// 任一条目不合法则整次拒绝，不产生部分结果。
func ParseReconcileRequest(raw []byte) ([]FlowEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var req flowRequestJSON
	if err := dec.Decode(&req); err != nil {
		return nil, ledgerError(ErrInvalid, "parse reconcile JSON: %v", err)
	}
	if tok, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, ledgerError(ErrInvalid, "reconcile: unexpected trailing content after JSON document (token %v)", tok)
		}
		return nil, ledgerError(ErrInvalid, "reconcile: %v", err)
	}
	// entries 必须显式提供；{} 等缺省列表的文档视为非法输入，
	// 只有 {"entries":[]} 才表示“空列表：全部账本记录缺失”。
	if req.Entries == nil {
		return nil, ledgerError(ErrInvalid, "reconcile: entries is required")
	}

	entries := make([]FlowEntry, 0, len(req.Entries))
	for i, e := range req.Entries {
		// 结构性约束（字段必须提供且为 int64）只存在于 JSON 入口。
		if e.Amount == nil {
			return nil, ledgerError(ErrInvalid, "reconcile entry %d: amount is required and must be an int64 integer", i)
		}
		if e.Fee == nil {
			return nil, ledgerError(ErrInvalid, "reconcile entry %d: fee is required and must be an int64 integer", i)
		}
		if e.Charged == nil {
			return nil, ledgerError(ErrInvalid, "reconcile entry %d: charged is required and must be an int64 integer", i)
		}
		entry := FlowEntry{
			Index:        i,
			Kind:         e.Kind,
			ID:           e.ID,
			Account:      e.Account,
			Asset:        e.Asset,
			Amount:       e.Amount.v,
			Fee:          e.Fee.v,
			Charged:      e.Charged.v,
			SettlementID: e.SettlementID,
		}
		// 语义约束与 Go 入口共用同一套校验，报错信息也保持一致。
		if err := validateFlowEntry(i, entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// reconSide 累计单侧（账本或流水）的金额。流水侧可能包含账本外的组合，
// 且累计允许超过 int64，因此一律用大整数。
type reconSide struct {
	charged  map[balanceKey]*big.Int
	refunded map[balanceKey]*big.Int
}

func newReconSide() reconSide {
	return reconSide{charged: map[balanceKey]*big.Int{}, refunded: map[balanceKey]*big.Int{}}
}

func (s *reconSide) addCharged(k balanceKey, v int64) {
	x := s.charged[k]
	if x == nil {
		x = new(big.Int)
		s.charged[k] = x
	}
	x.Add(x, big.NewInt(v))
}

func (s *reconSide) addRefund(k balanceKey, v int64) {
	x := s.refunded[k]
	if x == nil {
		x = new(big.Int)
		s.refunded[k] = x
	}
	x.Add(x, big.NewInt(v))
}

// idUse 记录一个 (kind,id) 在输入中出现的全部位置。
type idUse struct{ positions []int }

// reconFlowKind 抽象一类流水（付款或退款）的逐条核对差异。两类共用同一套
// 逐条规则（范围外/重复/命中/缺失、流水与账本两侧合计、缺失枚举），各自只
// 提供：自己的编号索引与成功序号、携带的关联记录、特有核对字段，以及该类
// 金额计入扣款侧还是退款侧。
type reconFlowKind interface {
	// name 是流水类别名 "payment" 或 "refund"，两类编号空间互不相通。
	name() string
	// inRange 判断成功序号是否入选本次核对范围。
	inRange(seq int64) bool
	// lookup 按本类编号空间查找成功记录，返回其成功序号与携带关联的引用。
	lookup(id string) (seq int64, ref ReconRecordRef, found bool)
	// diffs 逐字段比较命中记录与流水；无差异时返回空切片。
	diffs(e *FlowEntry) []FieldDiff
	// addFlow 把一条流水的 charged 计入流水侧的扣款或退款合计。
	addFlow(side *reconSide, e *FlowEntry)
	// addLedger 把一条入选账本记录的 charged 计入账本侧的扣款或退款合计。
	addLedger(side *reconSide, ref ReconRecordRef)
	// markCovered/isCovered 记录“唯一出现且命中入选记录”的编号（含字段不符）。
	markCovered(id string)
	isCovered(id string) bool
	// forEachRecord 按本类成功顺序遍历全部成功记录（含范围外，关联不切断）。
	forEachRecord(fn func(seq int64, ref ReconRecordRef))
}

// paymentKind 以成功结算记录为匹配对象；关联信息是退回该结算的退款编号。
type paymentKind struct {
	byID        map[string]*Record
	refundOfPay map[string]*RefundRecord // 始终基于完整历史：范围不切断关联
	records     []Record
	after       int64
	through     int64
	covered     map[string]bool
}

func newPaymentKind(st *ledgerState, after, through int64) *paymentKind {
	k := &paymentKind{
		byID:        make(map[string]*Record, len(st.Settlements)),
		refundOfPay: make(map[string]*RefundRecord, len(st.Refunds)),
		records:     st.Settlements,
		after:       after,
		through:     through,
		covered:     map[string]bool{},
	}
	// 结算编号 / 退款编号各自索引：同名编号也不会混为一笔。
	for i := range st.Settlements {
		k.byID[st.Settlements[i].ID] = &st.Settlements[i]
	}
	for i := range st.Refunds {
		k.refundOfPay[st.Refunds[i].SettlementID] = &st.Refunds[i]
	}
	return k
}

func (k *paymentKind) name() string { return "payment" }

func (k *paymentKind) inRange(seq int64) bool { return seq > k.after && seq <= k.through }

func (k *paymentKind) lookup(id string) (int64, ReconRecordRef, bool) {
	r := k.byID[id]
	if r == nil {
		return 0, ReconRecordRef{}, false
	}
	return r.Seq, k.recordRef(r), true
}

// recordRef 构造结算记录引用；该结算已被退款时附上退款编号与序号，
// 即使退款记录本身在本次范围外也保留关联。
func (k *paymentKind) recordRef(r *Record) ReconRecordRef {
	ref := ReconRecordRef{
		Kind:    "payment",
		ID:      r.ID,
		Account: r.Account,
		Asset:   r.Asset,
		Amount:  r.Amount,
		Fee:     r.Fee,
		Charged: r.Charged,
		Seq:     r.Seq,
	}
	if rf := k.refundOfPay[r.ID]; rf != nil {
		ref.RefundID = rf.ID
		ref.RefundSeq = rf.Seq
	}
	return ref
}

func (k *paymentKind) diffs(e *FlowEntry) []FieldDiff {
	r := k.byID[e.ID]
	return commonFieldDiffs(r.Account, r.Asset, r.Amount, r.Fee, r.Charged, e)
}

func (k *paymentKind) addFlow(side *reconSide, e *FlowEntry) {
	side.addCharged(balanceKey{e.Account, e.Asset}, e.Charged)
}

func (k *paymentKind) addLedger(side *reconSide, ref ReconRecordRef) {
	side.addCharged(balanceKey{ref.Account, ref.Asset}, ref.Charged)
}

func (k *paymentKind) markCovered(id string) { k.covered[id] = true }

func (k *paymentKind) isCovered(id string) bool { return k.covered[id] }

func (k *paymentKind) forEachRecord(fn func(seq int64, ref ReconRecordRef)) {
	for i := range k.records {
		r := &k.records[i]
		fn(r.Seq, k.recordRef(r))
	}
}

// refundKind 以成功退款记录为匹配对象；关联信息是退款的原付款编号。
type refundKind struct {
	byID    map[string]*RefundRecord
	records []RefundRecord
	after   int64
	through int64
	covered map[string]bool
}

func newRefundKind(st *ledgerState, after, through int64) *refundKind {
	k := &refundKind{
		byID:    make(map[string]*RefundRecord, len(st.Refunds)),
		records: st.Refunds,
		after:   after,
		through: through,
		covered: map[string]bool{},
	}
	for i := range st.Refunds {
		k.byID[st.Refunds[i].ID] = &st.Refunds[i]
	}
	return k
}

func (k *refundKind) name() string { return "refund" }

func (k *refundKind) inRange(seq int64) bool { return seq > k.after && seq <= k.through }

func (k *refundKind) lookup(id string) (int64, ReconRecordRef, bool) {
	r := k.byID[id]
	if r == nil {
		return 0, ReconRecordRef{}, false
	}
	return r.Seq, k.recordRef(r), true
}

// recordRef 构造退款记录引用并附上原付款编号；原付款在范围外也保留关联。
func (k *refundKind) recordRef(r *RefundRecord) ReconRecordRef {
	return ReconRecordRef{
		Kind:         "refund",
		ID:           r.ID,
		Account:      r.Account,
		Asset:        r.Asset,
		Amount:       r.Amount,
		Fee:          r.Fee,
		Charged:      r.Charged,
		Seq:          r.Seq,
		SettlementID: r.SettlementID,
	}
}

func (k *refundKind) diffs(e *FlowEntry) []FieldDiff {
	r := k.byID[e.ID]
	diffs := commonFieldDiffs(r.Account, r.Asset, r.Amount, r.Fee, r.Charged, e)
	// 退款额外核对原付款编号，排在共有字段之后。
	if r.SettlementID != e.SettlementID {
		diffs = append(diffs, FieldDiff{Field: "settlement_id", Ledger: r.SettlementID, Flow: e.SettlementID})
	}
	return diffs
}

func (k *refundKind) addFlow(side *reconSide, e *FlowEntry) {
	side.addRefund(balanceKey{e.Account, e.Asset}, e.Charged)
}

func (k *refundKind) addLedger(side *reconSide, ref ReconRecordRef) {
	side.addRefund(balanceKey{ref.Account, ref.Asset}, ref.Charged)
}

func (k *refundKind) markCovered(id string) { k.covered[id] = true }

func (k *refundKind) isCovered(id string) bool { return k.covered[id] }

func (k *refundKind) forEachRecord(fn func(seq int64, ref ReconRecordRef)) {
	for i := range k.records {
		r := &k.records[i]
		fn(r.Seq, k.recordRef(r))
	}
}

// commonFieldDiffs 比较付款与退款共有的五个核对字段：账户、资产、amount、
// fee、charged。追加顺序即字段差异在报告中的列出顺序。
func commonFieldDiffs(account, asset string, amount, fee, charged int64, e *FlowEntry) []FieldDiff {
	var diffs []FieldDiff
	addStr := func(field, ledger, flow string) {
		if ledger != flow {
			diffs = append(diffs, FieldDiff{Field: field, Ledger: ledger, Flow: flow})
		}
	}
	addInt := func(field string, ledger, flow int64) {
		if ledger != flow {
			diffs = append(diffs, FieldDiff{Field: field, Ledger: fmt.Sprintf("%d", ledger), Flow: fmt.Sprintf("%d", flow)})
		}
	}
	addStr("account", account, e.Account)
	addStr("asset", asset, e.Asset)
	addInt("amount", amount, e.Amount)
	addInt("fee", fee, e.Fee)
	addInt("charged", charged, e.Charged)
	return diffs
}

// ReconcileFlow 对一批外部流水与当前账本状态做只读对账，核对范围为完整
// 历史（付款与退款都取各自全部成功记录）。等价于 ReconcileFlowRanged 传入
// 空边界。
//
// entries 与命令行 {"entries":[...]} 适用同一套输入检查：kind 只能是
// payment/refund，编号/账户/资产非空，amount、charged 为正，fee 非负，
// refund 必须带 settlement_id。任一条目不合法即返回 invalid_parameter
// （报告为 nil，不产生部分结果），Go 调用者无需先把条目转成 JSON 解析。
// nil 或空列表表示没有外部流水，正常列出核对范围内的账本缺失记录。
func (l *Ledger) ReconcileFlow(entries []FlowEntry) (*ReconReport, error) {
	return l.ReconcileFlowRanged(entries, ReconcileBounds{})
}

// resolveReconRange 确定一类记录的实际核对边界 (after, through]，并校验：
// 边界必须非负、after 不大于 through、through 不超过该类最大成功序号。
// 空历史的最大序号为零，因此缺省边界为 (0,0]，范围为空。
func resolveReconRange(kind string, after, through *int64, maxSeq int64) (int64, int64, error) {
	a := int64(0)
	if after != nil {
		a = *after
	}
	t := maxSeq
	if through != nil {
		t = *through
	}
	if a < 0 {
		return 0, 0, ledgerError(ErrInvalid, "reconcile %s bounds: after must be a non-negative int64, got %d", kind, a)
	}
	if t < 0 {
		return 0, 0, ledgerError(ErrInvalid, "reconcile %s bounds: through must be a non-negative int64, got %d", kind, t)
	}
	if a > t {
		return 0, 0, ledgerError(ErrInvalid, "reconcile %s bounds: after %d must not exceed through %d", kind, a, t)
	}
	if t > maxSeq {
		return 0, 0, ledgerError(ErrInvalid, "reconcile %s bounds: through %d exceeds max seq %d", kind, t, maxSeq)
	}
	return a, t, nil
}

// ReconcileFlowRanged 对一批外部流水与当前账本状态做只读对账，只核对成功
// 序号落在边界内的记录（after < seq <= through）。边界不改变账本状态，
// 整份报告仍在同一把账本互斥锁内生成，对应同一个完整账本状态。
//
// 范围只影响记录是否参加核对，不切断关联：入选退款仍可指向范围外付款，
// 入选付款仍携带范围外退款的信息。命中同类型同编号但记录在范围外的流水
// 返回 out_of_scope（携带账本记录、不比较字段、不计入流水侧汇总）；该编号
// 在流水中出现多次时全部按 out_of_scope 处理。
func (l *Ledger) ReconcileFlowRanged(entries []FlowEntry, bounds ReconcileBounds) (*ReconReport, error) {
	if err := l.beginOp(); err != nil {
		return nil, err
	}
	defer l.endOp()

	// 与命令行入口相同的输入检查：按传入列表顺序找到第一条非法流水即整次
	// 拒绝（invalid_parameter），报告为空、不产生部分汇总，也不触碰账本。
	// 位置用列表下标而非调用者给出的 Index；校验只读、不改写传入条目。
	for i := range entries {
		if err := validateFlowEntry(i, entries[i]); err != nil {
			return nil, err
		}
	}

	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()

	st := l.stateView()

	maxPay := int64(len(st.Settlements))
	maxRef := int64(len(st.Refunds))
	payAfter, payThrough, err := resolveReconRange("payment", bounds.PaymentAfter, bounds.PaymentThrough, maxPay)
	if err != nil {
		return nil, err
	}
	refAfter, refThrough, err := resolveReconRange("refund", bounds.RefundAfter, bounds.RefundThrough, maxRef)
	if err != nil {
		return nil, err
	}

	// 两类各建自己的编号索引与核对范围；kinds 的顺序（先付款后退款）也是
	// 缺失记录的输出顺序。关联索引始终基于完整历史，范围不切断付款与退款
	// 之间的关联。
	payK := newPaymentKind(st, payAfter, payThrough)
	refK := newRefundKind(st, refAfter, refThrough)
	kinds := []reconFlowKind{payK, refK}
	kindByName := map[string]reconFlowKind{"payment": payK, "refund": refK}

	// 每个类别各自统计 (kind,id) 在输入中出现的全部位置：同名编号跨类别不
	// 相通；同类别出现多次则整组重复，无论内容是否相同。
	uses := make(map[string]map[string]*idUse, len(kinds))
	for _, kd := range kinds {
		uses[kd.name()] = map[string]*idUse{}
	}
	for i := range entries {
		e := &entries[i]
		m := uses[e.Kind]
		u := m[e.ID]
		if u == nil {
			u = &idUse{}
			m[e.ID] = u
		}
		u.positions = append(u.positions, e.Index)
	}

	report := &ReconReport{
		Results:        make([]ReconResult, 0, len(entries)),
		NetDiff:        []ReconNetDiff{},
		MaxPaymentSeq:  maxPay,
		MaxRefundSeq:   maxRef,
		PaymentAfter:   payAfter,
		PaymentThrough: payThrough,
		RefundAfter:    refAfter,
		RefundThrough:  refThrough,
	}

	ledgerSide := newReconSide()
	flowSide := newReconSide()

	// 逐条结果（保持输入顺序）。两类共用同一套判定，只经 reconFlowKind
	// 区分匹配对象、关联信息与金额计入哪一侧。
	for i := range entries {
		e := &entries[i]
		kd := kindByName[e.Kind]
		res := ReconResult{Index: e.Index, Kind: e.Kind, ID: e.ID}
		group := uses[kd.name()][e.ID]
		seq, ref, found := kd.lookup(e.ID)

		// 1. 命中同类型同编号但记录在范围外：携带记录与关联，不比较字段、
		//    不计入流水侧汇总；该编号出现多次时也全部按此处理（优先于重复）。
		if found && !kd.inRange(seq) {
			res.Status = ReconOutOfScope
			res.Record = &ref
			report.Results = append(report.Results, res)
			continue
		}

		// 2. 同类型同编号在输入中出现多次：整组标注重复并附全部输入位置，
		//    不再判断字段差异；每一条仍逐条计入流水侧合计，命中记录则携带。
		if len(group.positions) > 1 {
			res.Status = ReconDuplicate
			res.Positions = append([]int(nil), group.positions...)
			kd.addFlow(&flowSide, e)
			if found {
				res.Record = &ref
			}
			report.Results = append(report.Results, res)
			continue
		}

		// 3. 唯一条目：逐条计入流水侧（账本中没有对应记录也不漏算）；命中
		//    入选记录则逐字段核对，否则账本缺这笔。
		kd.addFlow(&flowSide, e)
		if found {
			kd.markCovered(e.ID)
			res.Record = &ref
			if diffs := kd.diffs(e); len(diffs) > 0 {
				res.Status = ReconFieldMismatch
				res.Diffs = diffs
			} else {
				res.Status = ReconMatched
			}
		} else {
			res.Status = ReconMissingInLedger
		}
		report.Results = append(report.Results, res)
	}

	// 账本侧合计只包含入选记录：已退款的付款仍按原扣款计入扣款侧，退款单独
	// 计入退款侧；只选退款时扣款合计为零，不会顺带计入原付款。同一次按成功
	// 顺序的遍历也枚举缺失：入选、未被唯一命中覆盖、且不属于重复组的记录。
	missing := make(map[string][]ReconRecordRef, len(kinds))
	for _, kd := range kinds {
		list := []ReconRecordRef{}
		kd.forEachRecord(func(seq int64, ref ReconRecordRef) {
			if !kd.inRange(seq) {
				return
			}
			kd.addLedger(&ledgerSide, ref)
			if kd.isCovered(ref.ID) {
				return
			}
			// 重复组对应的账本记录不列为缺失（字段不符已算覆盖，也不会到此）。
			if u := uses[kd.name()][ref.ID]; u != nil && len(u.positions) > 1 {
				return
			}
			list = append(list, ref)
		})
		missing[kd.name()] = list
	}
	report.MissingPayments = missing["payment"]
	report.MissingRefunds = missing["refund"]

	report.Totals = ReconTotals{
		Ledger: buildTotalRows(ledgerSide),
		Flow:   buildTotalRows(flowSide),
	}
	report.NetDiff = buildNetDiff(ledgerSide, flowSide)
	return report, nil
}

// sideNet 返回某组合的净扣款（扣款 - 退款）。
func sideNet(side reconSide, k balanceKey) *big.Int {
	charged := new(big.Int)
	if x := side.charged[k]; x != nil {
		charged.Set(x)
	}
	refunded := new(big.Int)
	if x := side.refunded[k]; x != nil {
		refunded.Set(x)
	}
	return charged.Sub(charged, refunded)
}

func allKeys(a, b reconSide) []balanceKey {
	seen := map[balanceKey]bool{}
	var keys []balanceKey
	add := func(m map[balanceKey]*big.Int) {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	add(a.charged)
	add(a.refunded)
	add(b.charged)
	add(b.refunded)
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].account != keys[j].account {
			return keys[i].account < keys[j].account
		}
		return keys[i].asset < keys[j].asset
	})
	return keys
}

// buildTotalRows 按账户、资产排序输出单侧汇总；净扣款 = 扣款 - 退款。
func buildTotalRows(side reconSide) []ReconTotalRow {
	keys := allKeys(side, reconSide{})
	out := make([]ReconTotalRow, 0, len(keys))
	for _, k := range keys {
		charged := new(big.Int)
		if x := side.charged[k]; x != nil {
			charged.Set(x)
		}
		refunded := new(big.Int)
		if x := side.refunded[k]; x != nil {
			refunded.Set(x)
		}
		net := new(big.Int).Sub(new(big.Int).Set(charged), refunded)
		out = append(out, ReconTotalRow{
			Account:       k.account,
			Asset:         k.asset,
			ChargedTotal:  charged.String(),
			RefundedTotal: refunded.String(),
			NetCharged:    net.String(),
		})
	}
	return out
}

// buildNetDiff 输出每个（任一侧出现过的）组合的净扣款差额，方向为流水减账本。
func buildNetDiff(ledger, flow reconSide) []ReconNetDiff {
	out := make([]ReconNetDiff, 0)
	for _, k := range allKeys(ledger, flow) {
		lNet := sideNet(ledger, k)
		fNet := sideNet(flow, k)
		diff := new(big.Int).Sub(new(big.Int).Set(fNet), lNet)
		out = append(out, ReconNetDiff{
			Account:          k.account,
			Asset:            k.asset,
			LedgerNetCharged: lNet.String(),
			FlowNetCharged:   fNet.String(),
			NetChargedDiff:   diff.String(),
		})
	}
	return out
}
