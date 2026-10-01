package payflow

import (
	"math/big"
	"sort"
)

// 对账流水的四种逐条结果。
const (
	ReconcileMatched       = "matched"           // 与账本记录逐字段一致
	ReconcileFieldMismatch = "field_mismatch"    // 找到账本记录但字段不符
	ReconcileMissingStatus = "missing_in_ledger" // 账本中无对应成功记录
	ReconcileDuplicate     = "duplicate"         // 同类型同编号出现多次
)

// 流水种类。
const (
	KindPayment = "payment"
	KindRefund  = "refund"
)

// StatementEntry 是对账流水中的一条。kind 区分 payment 与 refund，
// id 是对应的付款或退款编号；退款还须提供 settlement_id（原付款编号）。
// amount、fee、charged 使用指针以区分“缺省/null”与 0。
type StatementEntry struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	SettlementID string `json:"settlement_id,omitempty"`
	Account      string `json:"account"`
	Asset        string `json:"asset"`
	Amount       *int64 `json:"amount"`
	Fee          *int64 `json:"fee"`
	Charged      *int64 `json:"charged"`
}

// ReconcileSide 是可比较的字段集合（账本侧或流水侧）。
type ReconcileSide struct {
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Amount  int64  `json:"amount"`
	Fee     int64  `json:"fee"`
	Charged int64  `json:"charged"`
}

// ReconcileDiff 列出一个不符字段及其账本值、流水值。
type ReconcileDiff struct {
	Field     string `json:"field"`
	Ledger    any    `json:"ledger"`
	Statement any    `json:"statement"`
}

// ReconcileRefundInfo 是对账报告中附带的退款记录摘要。
type ReconcileRefundInfo struct {
	ID           string `json:"id"`
	SettlementID string `json:"settlement_id"`
	Account      string `json:"account"`
	Asset        string `json:"asset"`
	Amount       int64  `json:"amount"`
	Fee          int64  `json:"fee"`
	Charged      int64  `json:"charged"`
	Seq          int64  `json:"seq"`
}

// ReconcileSettlementInfo 是对账报告中附带的结算记录摘要。
type ReconcileSettlementInfo struct {
	ID      string `json:"id"`
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Amount  int64  `json:"amount"`
	Fee     int64  `json:"fee"`
	Charged int64  `json:"charged"`
	Seq     int64  `json:"seq"`
}

// ReconcileLedger 是匹配到的账本记录及其相关记录。
// 付款记录附带针对它的退款记录；退款记录附带原付款结算记录。
type ReconcileLedger struct {
	ReconcileSide
	Seq        int64                    `json:"seq"`
	Refunds    []ReconcileRefundInfo    `json:"refunds,omitempty"`
	Settlement *ReconcileSettlementInfo `json:"settlement,omitempty"`
}

// ReconcileResult 是一条流水的对账结果，顺序与输入逐项对应。
type ReconcileResult struct {
	Kind         string           `json:"kind"`
	ID           string           `json:"id"`
	Status       string           `json:"status"`
	Positions    []int            `json:"positions,omitempty"`
	Fields       []ReconcileDiff  `json:"fields,omitempty"`
	SettlementID string           `json:"settlement_id,omitempty"`
	Ledger       *ReconcileLedger `json:"ledger,omitempty"`
	Statement    *ReconcileSide   `json:"statement,omitempty"`
}

// ReconcileMissing 是账本中有但流水中没有对应条目的记录。
type ReconcileMissing struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	SettlementID string `json:"settlement_id,omitempty"`
	Account      string `json:"account"`
	Asset        string `json:"asset"`
	Amount       int64  `json:"amount"`
	Fee          int64  `json:"fee"`
	Charged      int64  `json:"charged"`
	Seq          int64  `json:"seq"`
}

// ReconcileTotals 是某 (账户, 资产) 组合的合计。金额用十进制整数字符串，
// 累计超过 int64 仍精确（net = charges - refunds）。
type ReconcileTotals struct {
	Charges string `json:"charges"`
	Refunds string `json:"refunds"`
	Net     string `json:"net"`
}

// ReconcileSummary 同时列出账本侧与流水侧的合计及净差额，
// 差额方向为流水减账本（statement.net - ledger.net）。
type ReconcileSummary struct {
	Account   string          `json:"account"`
	Asset     string          `json:"asset"`
	Ledger    ReconcileTotals `json:"ledger"`
	Statement ReconcileTotals `json:"statement"`
	NetDiff   string          `json:"net_difference"`
}

// ReconcileMaxSeq 是本次所见付款、退款的最大成功序号；空历史为零。
type ReconcileMaxSeq struct {
	Payments int64 `json:"payments"`
	Refunds  int64 `json:"refunds"`
}

// ReconcileReport 是对账的完整报告。
type ReconcileReport struct {
	Entries []ReconcileResult  `json:"entries"`
	Missing []ReconcileMissing `json:"missing"`
	Summary []ReconcileSummary `json:"summary"`
	MaxSeq  ReconcileMaxSeq    `json:"max_seq"`
}

// ValidateStatement 校验流水条目的字段完整性与数值范围。
// 任何不合法都返回 ErrInvalid，整次不输出部分报告。
func ValidateStatement(entries []StatementEntry) error {
	for i, e := range entries {
		if e.Kind != KindPayment && e.Kind != KindRefund {
			return ledgerError(ErrInvalid, "entry %d: unknown kind %q", i, e.Kind)
		}
		if e.ID == "" {
			return ledgerError(ErrInvalid, "entry %d: id must not be empty", i)
		}
		if e.Account == "" {
			return ledgerError(ErrInvalid, "entry %d: account must not be empty", i)
		}
		if e.Asset == "" {
			return ledgerError(ErrInvalid, "entry %d: asset must not be empty", i)
		}
		if e.Kind == KindRefund && e.SettlementID == "" {
			return ledgerError(ErrInvalid, "entry %d: settlement_id must not be empty for refunds", i)
		}
		if e.Amount == nil {
			return ledgerError(ErrInvalid, "entry %d: amount must be provided", i)
		}
		if e.Fee == nil {
			return ledgerError(ErrInvalid, "entry %d: fee must be provided", i)
		}
		if e.Charged == nil {
			return ledgerError(ErrInvalid, "entry %d: charged must be provided", i)
		}
		if *e.Amount <= 0 {
			return ledgerError(ErrInvalid, "entry %d: amount must be positive", i)
		}
		if *e.Charged <= 0 {
			return ledgerError(ErrInvalid, "entry %d: charged must be positive", i)
		}
		if *e.Fee < 0 {
			return ledgerError(ErrInvalid, "entry %d: fee must be non-negative", i)
		}
	}
	return nil
}

// ReconcileStatement 把流水与账本逐笔核对，返回完整报告。
// 整次对账在账本锁内完成，因此同一进程并发付款/退款时，
// 整份报告对应同一完整账本状态，不会看到半更新。
// 对账不改变余额、历史或账本文件。
func (l *Ledger) ReconcileStatement(entries []StatementEntry) (*ReconcileReport, error) {
	if err := l.beginOp(); err != nil {
		return nil, err
	}
	defer l.endOp()

	if err := ValidateStatement(entries); err != nil {
		return nil, err
	}

	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()

	st := l.stateView()

	// 按 (kind, id) 分组，标记重复组。
	type groupKey struct {
		kind string
		id   string
	}
	groups := map[groupKey][]int{}
	for i, e := range entries {
		k := groupKey{e.Kind, e.ID}
		groups[k] = append(groups[k], i)
	}

	// 记录已被流水“覆盖”的账本记录编号（单条匹配或重复组都算覆盖），
	// 用于缺失列表与最大序号。
	coveredSettlements := map[string]bool{}
	coveredRefunds := map[string]bool{}

	results := make([]ReconcileResult, len(entries))
	for i, e := range entries {
		res := ReconcileResult{Kind: e.Kind, ID: e.ID}
		positions := groups[groupKey{e.Kind, e.ID}]

		if len(positions) > 1 {
			// 同类型同编号多次出现：整组标为重复，附全部输入位置，
			// 不判断字段差异，也不把该账本记录列为缺失。
			res.Status = ReconcileDuplicate
			res.Positions = positions
			if e.Kind == KindPayment {
				coveredSettlements[e.ID] = true
			} else {
				coveredRefunds[e.ID] = true
			}
			results[i] = res
			continue
		}

		// 单条流水：在账本中查找对应成功记录。
		if e.Kind == KindPayment {
			rec, ok := findSettlement(st.Settlements, e.ID)
			if !ok {
				res.Status = ReconcileMissingStatus
				res.Statement = statementSide(e)
			} else {
				coveredSettlements[e.ID] = true
				res.SettlementID = rec.ID
				ledgerSide := settlementLedgerSide(rec, st.Refunds)
				res.Ledger = &ledgerSide
				res.Statement = statementSide(e)
				diffs := comparePayment(rec, e)
				if len(diffs) == 0 {
					res.Status = ReconcileMatched
				} else {
					res.Status = ReconcileFieldMismatch
					res.Fields = diffs
				}
			}
		} else {
			rf, ok := findRefundRecord(st.Refunds, e.ID)
			if !ok {
				res.Status = ReconcileMissingStatus
				res.Statement = statementSide(e)
			} else {
				coveredRefunds[e.ID] = true
				res.SettlementID = rf.SettlementID
				ledgerSide := refundLedgerSide(rf, st.Settlements)
				res.Ledger = &ledgerSide
				res.Statement = statementSide(e)
				diffs := compareRefund(rf, e)
				if len(diffs) == 0 {
					res.Status = ReconcileMatched
				} else {
					res.Status = ReconcileFieldMismatch
					res.Fields = diffs
				}
			}
		}
		results[i] = res
	}

	// 缺失记录：账本中有但流水未覆盖的，先付款（按成功顺序），再退款（按成功顺序）。
	missing := []ReconcileMissing{}
	for _, rec := range st.Settlements {
		if !coveredSettlements[rec.ID] {
			missing = append(missing, missingFromSettlement(rec))
		}
	}
	for _, rf := range st.Refunds {
		if !coveredRefunds[rf.ID] {
			missing = append(missing, missingFromRefund(rf))
		}
	}

	summary := buildSummary(st, entries)

	// 本次所见最大成功序号（被覆盖的记录，含重复组）；空历史为零。
	maxPay, maxRef := int64(0), int64(0)
	for _, rec := range st.Settlements {
		if coveredSettlements[rec.ID] && rec.Seq > maxPay {
			maxPay = rec.Seq
		}
	}
	for _, rf := range st.Refunds {
		if coveredRefunds[rf.ID] && rf.Seq > maxRef {
			maxRef = rf.Seq
		}
	}

	return &ReconcileReport{
		Entries: results,
		Missing: missing,
		Summary: summary,
		MaxSeq:  ReconcileMaxSeq{Payments: maxPay, Refunds: maxRef},
	}, nil
}

func findSettlement(records []Record, id string) (Record, bool) {
	for _, r := range records {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

func findRefundRecord(refunds []RefundRecord, id string) (RefundRecord, bool) {
	for _, r := range refunds {
		if r.ID == id {
			return r, true
		}
	}
	return RefundRecord{}, false
}

func statementSide(e StatementEntry) *ReconcileSide {
	return &ReconcileSide{
		Account: e.Account,
		Asset:   e.Asset,
		Amount:  *e.Amount,
		Fee:     *e.Fee,
		Charged: *e.Charged,
	}
}

func settlementLedgerSide(rec Record, refunds []RefundRecord) ReconcileLedger {
	side := ReconcileSide{
		Account: rec.Account,
		Asset:   rec.Asset,
		Amount:  rec.Amount,
		Fee:     rec.Fee,
		Charged: rec.Charged,
	}
	var related []ReconcileRefundInfo
	for _, rf := range refunds {
		if rf.SettlementID == rec.ID {
			related = append(related, ReconcileRefundInfo{
				ID: rf.ID, SettlementID: rf.SettlementID,
				Account: rf.Account, Asset: rf.Asset,
				Amount: rf.Amount, Fee: rf.Fee, Charged: rf.Charged,
				Seq: rf.Seq,
			})
		}
	}
	return ReconcileLedger{
		ReconcileSide: side,
		Seq:           rec.Seq,
		Refunds:       related,
	}
}

func refundLedgerSide(rf RefundRecord, settlements []Record) ReconcileLedger {
	side := ReconcileSide{
		Account: rf.Account,
		Asset:   rf.Asset,
		Amount:  rf.Amount,
		Fee:     rf.Fee,
		Charged: rf.Charged,
	}
	var settlement *ReconcileSettlementInfo
	for _, rec := range settlements {
		if rec.ID == rf.SettlementID {
			settlement = &ReconcileSettlementInfo{
				ID: rec.ID, Account: rec.Account, Asset: rec.Asset,
				Amount: rec.Amount, Fee: rec.Fee, Charged: rec.Charged,
				Seq: rec.Seq,
			}
			break
		}
	}
	return ReconcileLedger{
		ReconcileSide: side,
		Seq:           rf.Seq,
		Settlement:    settlement,
	}
}

func comparePayment(rec Record, e StatementEntry) []ReconcileDiff {
	var diffs []ReconcileDiff
	if rec.Account != e.Account {
		diffs = append(diffs, ReconcileDiff{Field: "account", Ledger: rec.Account, Statement: e.Account})
	}
	if rec.Asset != e.Asset {
		diffs = append(diffs, ReconcileDiff{Field: "asset", Ledger: rec.Asset, Statement: e.Asset})
	}
	if rec.Amount != *e.Amount {
		diffs = append(diffs, ReconcileDiff{Field: "amount", Ledger: rec.Amount, Statement: *e.Amount})
	}
	if rec.Fee != *e.Fee {
		diffs = append(diffs, ReconcileDiff{Field: "fee", Ledger: rec.Fee, Statement: *e.Fee})
	}
	if rec.Charged != *e.Charged {
		diffs = append(diffs, ReconcileDiff{Field: "charged", Ledger: rec.Charged, Statement: *e.Charged})
	}
	return diffs
}

func compareRefund(rf RefundRecord, e StatementEntry) []ReconcileDiff {
	var diffs []ReconcileDiff
	if rf.SettlementID != e.SettlementID {
		diffs = append(diffs, ReconcileDiff{Field: "settlement_id", Ledger: rf.SettlementID, Statement: e.SettlementID})
	}
	if rf.Account != e.Account {
		diffs = append(diffs, ReconcileDiff{Field: "account", Ledger: rf.Account, Statement: e.Account})
	}
	if rf.Asset != e.Asset {
		diffs = append(diffs, ReconcileDiff{Field: "asset", Ledger: rf.Asset, Statement: e.Asset})
	}
	if rf.Amount != *e.Amount {
		diffs = append(diffs, ReconcileDiff{Field: "amount", Ledger: rf.Amount, Statement: *e.Amount})
	}
	if rf.Fee != *e.Fee {
		diffs = append(diffs, ReconcileDiff{Field: "fee", Ledger: rf.Fee, Statement: *e.Fee})
	}
	if rf.Charged != *e.Charged {
		diffs = append(diffs, ReconcileDiff{Field: "charged", Ledger: rf.Charged, Statement: *e.Charged})
	}
	return diffs
}

func missingFromSettlement(rec Record) ReconcileMissing {
	return ReconcileMissing{
		Kind: KindPayment, ID: rec.ID, SettlementID: rec.ID,
		Account: rec.Account, Asset: rec.Asset,
		Amount: rec.Amount, Fee: rec.Fee, Charged: rec.Charged,
		Seq: rec.Seq,
	}
}

func missingFromRefund(rf RefundRecord) ReconcileMissing {
	return ReconcileMissing{
		Kind: KindRefund, ID: rf.ID, SettlementID: rf.SettlementID,
		Account: rf.Account, Asset: rf.Asset,
		Amount: rf.Amount, Fee: rf.Fee, Charged: rf.Charged,
		Seq: rf.Seq,
	}
}

type summaryKey struct {
	account string
	asset   string
}

func addBig(m map[summaryKey]*big.Int, k summaryKey, v *big.Int) {
	if existing, ok := m[k]; ok {
		existing.Add(existing, v)
	} else {
		m[k] = new(big.Int).Set(v)
	}
}

// buildSummary 按 (账户, 资产) 分别汇总账本侧与流水侧的扣款、退款与净扣款。
// 流水侧包括重复项和账本外的项；不同资产不相加。金额用 big.Int 精确表示。
func buildSummary(st *ledgerState, entries []StatementEntry) []ReconcileSummary {
	ledgerCharges := map[summaryKey]*big.Int{}
	ledgerRefunds := map[summaryKey]*big.Int{}
	for _, rec := range st.Settlements {
		k := summaryKey{rec.Account, rec.Asset}
		addBig(ledgerCharges, k, big.NewInt(rec.Charged))
	}
	for _, rf := range st.Refunds {
		k := summaryKey{rf.Account, rf.Asset}
		addBig(ledgerRefunds, k, big.NewInt(rf.Charged))
	}

	stmtCharges := map[summaryKey]*big.Int{}
	stmtRefunds := map[summaryKey]*big.Int{}
	for _, e := range entries {
		k := summaryKey{e.Account, e.Asset}
		if e.Kind == KindPayment {
			addBig(stmtCharges, k, big.NewInt(*e.Charged))
		} else {
			addBig(stmtRefunds, k, big.NewInt(*e.Charged))
		}
	}

	keySet := map[summaryKey]bool{}
	for k := range ledgerCharges {
		keySet[k] = true
	}
	for k := range ledgerRefunds {
		keySet[k] = true
	}
	for k := range stmtCharges {
		keySet[k] = true
	}
	for k := range stmtRefunds {
		keySet[k] = true
	}

	keys := make([]summaryKey, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].account != keys[j].account {
			return keys[i].account < keys[j].account
		}
		return keys[i].asset < keys[j].asset
	})

	out := make([]ReconcileSummary, 0, len(keys))
	for _, k := range keys {
		lc := ledgerCharges[k]
		if lc == nil {
			lc = big.NewInt(0)
		}
		lr := ledgerRefunds[k]
		if lr == nil {
			lr = big.NewInt(0)
		}
		sc := stmtCharges[k]
		if sc == nil {
			sc = big.NewInt(0)
		}
		sr := stmtRefunds[k]
		if sr == nil {
			sr = big.NewInt(0)
		}

		lnet := new(big.Int).Sub(lc, lr)
		snet := new(big.Int).Sub(sc, sr)
		diff := new(big.Int).Sub(snet, lnet)

		out = append(out, ReconcileSummary{
			Account: k.account,
			Asset:   k.asset,
			Ledger: ReconcileTotals{
				Charges: lc.String(),
				Refunds: lr.String(),
				Net:     lnet.String(),
			},
			Statement: ReconcileTotals{
				Charges: sc.String(),
				Refunds: sr.String(),
				Net:     snet.String(),
			},
			NetDiff: diff.String(),
		})
	}
	return out
}
