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
// 付款编号与退款编号分别匹配，同名也不会混为一笔。只核对成功记录：
// 已退款的付款仍按原扣款核对，退款单独核对。账本中没有任何对应流水的入选
// 记录另列缺失（先按付款成功顺序，再按退款成功顺序）。范围只影响记录是否
// 参加核对，不切断关联：入选退款仍可指向范围外付款，入选付款仍携带范围外
// 退款的信息。

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

// add 把一条流水或账本记录计入对应合计：payment 计入扣款，refund 计入退款。
// 两类记录共用同一套累计逻辑，差别只在目标合集。
func (s *reconSide) add(kind string, k balanceKey, v int64) {
	m := s.charged
	if kind == "refund" {
		m = s.refunded
	}
	x := m[k]
	if x == nil {
		x = new(big.Int)
		m[k] = x
	}
	x.Add(x, big.NewInt(v))
}

// reconLane 汇集一类流水（付款或退款）参与逐条核对所需的全部账本视图与
// 判定状态。付款与退款的逐条流程（范围外 → 整组重复 → 唯一核对）完全相同，
// 差异只在匹配对象、关联信息与核对字段，全部收敛到各自的 lane：
//   - records/byID：按成功顺序的记录引用（付款携带其退款编号，退款携带原
//     付款编号），编号索引按类各自独立，同名编号也不会混为一笔；
//   - inRange：该类记录自己的序号范围判断；
//   - seen/covered/missing：输入位置统计、入选记录覆盖情况与缺失列表。
type reconLane struct {
	kind    string
	inRange func(seq int64) bool
	records []ReconRecordRef           // 按成功顺序排列
	byID    map[string]*ReconRecordRef // 指向 records 中的元素
	seen    map[string][]int           // 编号 → 全部输入位置
	covered map[string]bool            // 唯一出现且命中入选记录的编号（含字段不符）
	missing []ReconRecordRef           // 入选但流水未覆盖的记录，按成功顺序
}

func newReconLane(kind string, inRange func(seq int64) bool) *reconLane {
	return &reconLane{
		kind:    kind,
		inRange: inRange,
		byID:    map[string]*ReconRecordRef{},
		seen:    map[string][]int{},
		covered: map[string]bool{},
		missing: []ReconRecordRef{},
	}
}

// index 在 records 全部就位后建立编号索引（元素地址在追加完成后才稳定）。
func (l *reconLane) index() {
	for i := range l.records {
		l.byID[l.records[i].ID] = &l.records[i]
	}
}

// attach 把命中记录（含关联信息）的独立副本挂到结果上。
func attachRef(rec *ReconRecordRef) *ReconRecordRef {
	cp := *rec
	return &cp
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
	inPayRange := func(seq int64) bool { return seq > payAfter && seq <= payThrough }
	inRefRange := func(seq int64) bool { return seq > refAfter && seq <= refThrough }

	// 结算编号 / 退款编号各自索引：同名编号也不会混为一笔。
	// 关联索引始终基于完整历史：范围不切断付款与退款之间的关联，
	// 入选退款仍可指向范围外付款，入选付款仍携带范围外退款的信息。
	refundOfPay := make(map[string]*RefundRecord, len(st.Refunds))
	for i := range st.Refunds {
		refundOfPay[st.Refunds[i].SettlementID] = &st.Refunds[i]
	}
	payLane := newReconLane("payment", inPayRange)
	for i := range st.Settlements {
		r := &st.Settlements[i]
		payLane.records = append(payLane.records, paymentRef(r, refundOfPay[r.ID]))
	}
	payLane.index()
	refundLane := newReconLane("refund", inRefRange)
	for i := range st.Refunds {
		refundLane.records = append(refundLane.records, refundRef(&st.Refunds[i]))
	}
	refundLane.index()
	// lanes 的顺序即缺失报告与账本侧合计的处理顺序：先付款后退款。
	lanes := []*reconLane{payLane, refundLane}
	laneOf := map[string]*reconLane{"payment": payLane, "refund": refundLane}

	// 统计每个 (kind,id) 在输入中出现的全部位置：出现多次则整组重复，
	// 无论内容是否相同。
	for i := range entries {
		e := &entries[i]
		lane := laneOf[e.Kind]
		lane.seen[e.ID] = append(lane.seen[e.ID], e.Index)
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

	// 逐条结果（保持输入顺序）。两类流水共用同一条核对流水线，
	// 类别差异（匹配对象、关联引用、合计方向、核对字段）由 lane 提供。
	for i := range entries {
		e := &entries[i]
		lane := laneOf[e.Kind]
		res := ReconResult{Index: e.Index, Kind: e.Kind, ID: e.ID}
		k := balanceKey{e.Account, e.Asset}
		rec := lane.byID[e.ID]

		// 1. 命中同类型同编号但记录在范围外：不比较字段、不计入流水侧汇总；
		//    该编号在流水中出现多次时全部按 out_of_scope 处理（优先于整组重复）。
		if rec != nil && !lane.inRange(rec.Seq) {
			res.Status = ReconOutOfScope
			res.Record = attachRef(rec)
			report.Results = append(report.Results, res)
			continue
		}

		// 2. 记录在范围内或账本无此编号：保留整组重复判定，流水侧合计仍包含
		//    每一条（范围内重复项与账本外流水逐条计入）。
		if positions := lane.seen[e.ID]; len(positions) > 1 {
			res.Status = ReconDuplicate
			res.Positions = append([]int(nil), positions...)
			flowSide.add(lane.kind, k, e.Charged)
			if rec != nil {
				res.Record = attachRef(rec)
			}
			report.Results = append(report.Results, res)
			continue
		}

		// 3. 唯一条目：命中入选记录则逐字段核对，否则账本缺这笔。
		flowSide.add(lane.kind, k, e.Charged)
		if rec != nil {
			lane.covered[e.ID] = true
			res.Record = attachRef(rec)
			if diffs := compareFields(lane.kind, rec, e); len(diffs) > 0 {
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

	// 账本侧合计只包含入选记录：已退款的付款仍按原扣款计入，退款单独计入
	// 退款侧；只选退款时扣款合计为零，不会顺带计入原付款。
	for _, lane := range lanes {
		for i := range lane.records {
			r := &lane.records[i]
			if lane.inRange(r.Seq) {
				ledgerSide.add(lane.kind, balanceKey{r.Account, r.Asset}, r.Charged)
			}
		}
	}

	// 入选但流水未覆盖的成功记录另列缺失（先付款后退款，各按成功顺序）。
	// 唯一出现即算覆盖（含字段不符）；重复组对应记录按规则不列为缺失；
	// 范围外记录不列入缺失。
	for _, lane := range lanes {
		for i := range lane.records {
			r := &lane.records[i]
			if !lane.inRange(r.Seq) {
				continue
			}
			if lane.covered[r.ID] {
				continue
			}
			if len(lane.seen[r.ID]) > 1 {
				continue
			}
			lane.missing = append(lane.missing, *r)
		}
	}
	report.MissingPayments = payLane.missing
	report.MissingRefunds = refundLane.missing

	report.Totals = ReconTotals{
		Ledger: buildTotalRows(ledgerSide),
		Flow:   buildTotalRows(flowSide),
	}
	report.NetDiff = buildNetDiff(ledgerSide, flowSide)
	return report, nil
}

// compareFields 逐字段核对流水与命中的账本记录：账户、资产、amount、fee、
// charged 五个字段两类流水共用；退款额外核对原付款编号 settlement_id。
// 每个不同字段都保留账本值与流水值。
func compareFields(kind string, r *ReconRecordRef, e *FlowEntry) []FieldDiff {
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
	addStr("account", r.Account, e.Account)
	addStr("asset", r.Asset, e.Asset)
	addInt("amount", r.Amount, e.Amount)
	addInt("fee", r.Fee, e.Fee)
	addInt("charged", r.Charged, e.Charged)
	if kind == "refund" {
		addStr("settlement_id", r.SettlementID, e.SettlementID)
	}
	return diffs
}

func paymentRef(r *Record, rf *RefundRecord) ReconRecordRef {
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
	if rf != nil {
		ref.RefundID = rf.ID
		ref.RefundSeq = rf.Seq
	}
	return ref
}

func refundRef(r *RefundRecord) ReconRecordRef {
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
