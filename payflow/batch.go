package payflow

import (
	"fmt"
	"math"
)

// 本文件把付款批次的两层关注点彻底分开维护：
//
//   - 付款规则（evaluatePayment）：只根据当前账本视图与本批次已用额度判断
//     一项付款的业务结果（非法参数 / 重复 / 冲突 / 状态错误 / 手续费溢出 /
//     超限 / 余额不足 / 可成功），不修改任何状态，也不知道结果如何保存。
//   - 保存结果（paymentSink）：只负责把判定为可成功的一笔扣款落到某个账本
//     视图上并报告成功与否（真实提交原子落盘、失败回滚；只读预览只推进
//     内存副本、绝不触碰磁盘），不参与任何付款规则判断。
//
// paymentEngine 按输入顺序驱动两者：先逐项跑规则，再把可成功项交给 sink；
// 只有 sink 确认成功的扣款才推进余额视图（后项可见）并消耗本批次额度与
// 成功序号。因此真实提交与只读预览共用同一条规则流水线，行为逐项一致，
// 差别仅收敛在各自的 sink 实现里。

// paymentVerdict 是 evaluatePayment 对一项付款的纯业务判定。
type paymentVerdict struct {
	status string // 非 paymentPayable 时为最终状态；见下方常量
	reason string
	record Record // status 为 duplicate 时携带原记录；payable 时为拟落账记录
	amount int64  // 校验通过后的付款金额
	key    balanceKey
	total  int64 // amount + fee（payable 时有效）
}

const (
	// paymentPayable 表示该项通过全部付款规则，可以交给 sink 保存。
	paymentPayable = "__payable__"
)

// evaluatePayment 只做付款规则判断，不读取也不修改调用方状态之外的任何东西：
// view 是“当前可见账本”（真实提交即真实账本，预览为推进中的副本），
// used 是本批次各组合已被“保存成功”的扣款累计（含手续费）。
//
// 判断顺序与历史行为一致：参数合法性 → 已成功编号的重复/冲突 → 状态 →
// 手续费与溢出 → 本次批次限额（先于余额） → 余额。未成功的编号不会出现在
// view.Settlements 中，因此不占用去重资格，后项可用同一编号重新付款。
func evaluatePayment(it *PaymentIntent, feeBps int, limits map[balanceKey]int64, view paymentView, used map[balanceKey]int64) paymentVerdict {
	// 1. 参数校验。未通过的编号不占用去重资格。
	amount, reason, ok := validateIntent(*it)
	if !ok {
		return paymentVerdict{status: StatusInvalid, reason: reason}
	}

	// 2. 已成功编号：字段与费率完全一致则幂等返回原记录；任一不同则编号冲突。
	//    两者都不再扣款、不改原记录。本批次内先（预计）成功的同编号项也在
	//    view 中可见。
	if rec, exists := findRecordIn(view.settlements(), it.ID); exists {
		if sameRequest(rec, it, amount, feeBps) {
			return paymentVerdict{status: StatusDuplicate, reason: reasonDuplicate, record: rec}
		}
		return paymentVerdict{status: StatusConflict, reason: reasonConflict}
	}

	// 3. 状态错误单独报告（不占用去重资格，之后可重新提交）。
	if it.State != "" && it.State != "pending" {
		return paymentVerdict{status: StatusState, reason: reasonNotPending}
	}

	// 4. 手续费与扣款总额（溢出明确按参数错误拒绝）。
	fee := feeFor(amount, feeBps)
	if fee > math.MaxInt64-amount {
		return paymentVerdict{status: StatusInvalid, reason: reasonOverflow}
	}
	total := amount + fee
	key := balanceKey{it.Account, it.Asset}

	// 5. 本次批次限额（先于余额判断：同时超限与余额不足时报告超限）。
	//    恰好达到上限仍允许；用 total > max-used 而非 used+total > max，
	//    避免累计接近 int64 上界时溢出放行。失败不消耗额度。
	if max, capped := limits[key]; capped {
		if u := used[key]; total > max-u {
			return paymentVerdict{
				status: StatusLimitExceeded,
				reason: fmt.Sprintf(reasonLimitExceeded, it.Account, it.Asset, max, u, total),
			}
		}
	}

	// 6. 余额校验。不存在的账户/资产组合余额视为 0（不足）。
	if total > view.balance(key) {
		return paymentVerdict{status: StatusFunds, reason: reasonFunds}
	}

	// 7. 全部规则通过：构造拟落账记录（序号按当前可见历史长度连续递增）。
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
		Seq:       int64(view.historyLen()) + 1,
	}
	return paymentVerdict{status: paymentPayable, amount: amount, key: key, total: total, record: rec}
}

// paymentView 是规则判断需要的只读账本视图：按组合取余额、按编号查成功
// 记录、读取当前成功历史长度。真实提交与预览各自提供实现。
type paymentView interface {
	balance(key balanceKey) int64
	settlements() []Record
	historyLen() int
}

// paymentSink 承接一项已通过全部付款规则的扣款：
//   - persist 把 rec（扣款 total）保存到它背后的账本视图中，返回 nil 表示
//     保存成功、非 nil 表示保存失败（真实提交必须已回滚该项，使视图恢复到
//     该笔之前的余额与历史）；
//   - view 返回 sink 当前维护的账本视图，供下一项规则判断看到此前保存成功
//     的结果。
//
// 只有 Commit 成功的付款才会被 engine 计入余额推进、成功序号与本批次额度；
// 预览 sink 的 Commit 恒成功且绝不落盘，因此预览结果不会出现 storage_error。
type paymentSink interface {
	view() paymentView
	commit(key balanceKey, total int64, rec Record) error
}

// paymentEngine 按输入顺序在固定的规则/限额配置下驱动一个批次：逐项评估、
// 把可成功项交给 sink，并只在 sink 确认成功后消耗本批次额度。
// engine 本身不区分真实提交与预览——差异全部在 sink 一侧。
type paymentEngine struct {
	feeBps int
	limits map[balanceKey]int64
	sink   paymentSink
	used   map[balanceKey]int64 // 各组合本批次已保存成功的扣款（含手续费）
}

func newPaymentEngine(feeBps int, limits map[balanceKey]int64, sink paymentSink) *paymentEngine {
	return &paymentEngine{
		feeBps: feeBps,
		limits: limits,
		sink:   sink,
		used:   make(map[balanceKey]int64, len(limits)),
	}
}

// process 处理整批意图，返回与输入逐项对应的结果。保存失败只影响该项：
// 报告 storage_error，视图已由 sink 回滚，额度与序号不消耗，后项继续。
func (e *paymentEngine) process(intents []PaymentIntent) []ItemResult {
	out := make([]ItemResult, 0, len(intents))
	for i := range intents {
		it := &intents[i]
		res := ItemResult{ID: it.ID}

		v := evaluatePayment(it, e.feeBps, e.limits, e.sink.view(), e.used)
		if v.status != paymentPayable {
			res.Status = v.status
			res.Reason = v.reason
			if v.status == StatusDuplicate {
				cp := v.record // 重复项携带原记录
				res.Record = &cp
			}
			out = append(out, res)
			continue
		}

		// 可成功项交给 sink 保存；保存失败时视图已恢复到该项之前，
		// 不消耗额度与成功序号，编号仍可被后项（或后续批次）重新使用。
		rec := v.record
		if err := e.sink.commit(v.key, v.total, rec); err != nil {
			res.Status = StatusStorage
			res.Reason = err.Error()
			out = append(out, res)
			continue
		}

		// 只有保存成功的扣款消耗额度（预览同样如此）并回报成功记录。
		e.used[v.key] += v.total
		cp := rec
		res.Status = StatusSettled
		res.Record = &cp
		out = append(out, res)
	}
	return out
}

// statePaymentView 以可变账本状态直接实现 paymentView。
type statePaymentView struct{ st *ledgerState }

func (v statePaymentView) balance(key balanceKey) int64 { return v.st.Balances[key] }
func (v statePaymentView) settlements() []Record        { return v.st.Settlements }
func (v statePaymentView) historyLen() int              { return len(v.st.Settlements) }

// persistingSink 用于真实提交：在共享账本状态上先生效一笔扣款，再原子落盘；
// 落盘失败回滚该项（恢复该项之前的余额与历史），前项成功保留、后项继续。
// 调用方必须持有 reg.mu。
type persistingSink struct {
	l  *Ledger
	st *ledgerState
}

func newPersistingSink(l *Ledger, st *ledgerState) *persistingSink {
	return &persistingSink{l: l, st: st}
}

func (s *persistingSink) view() paymentView { return statePaymentView{s.st} }

func (s *persistingSink) commit(key balanceKey, total int64, rec Record) error {
	rollback := stageOn(s.st, key, s.st.Balances[key]-total, rec)
	if err := s.l.persistLocked(); err != nil {
		rollback()
		return err
	}
	return nil
}

// previewSink 用于只读预览：在账本快照的独立副本上推进内存状态，绝不落盘，
// 因此既不会修改真实余额与历史，也不可能产生 storage_error。
type previewSink struct{ sim *ledgerState }

func newPreviewSink(sim *ledgerState) *previewSink { return &previewSink{sim: sim} }

func (s *previewSink) view() paymentView { return statePaymentView{s.sim} }

func (s *previewSink) commit(key balanceKey, total int64, rec Record) error {
	s.sim.Balances[key] = s.sim.Balances[key] - total
	s.sim.Settlements = append(s.sim.Settlements, rec)
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

// stageOn 在指定状态上应用一笔扣款与记录，返回回滚函数（恢复该项执行前的
// 余额与记录长度）。
func stageOn(st *ledgerState, key balanceKey, newBal int64, rec Record) func() {
	oldBal, hadKey := st.Balances[key]
	oldLen := len(st.Settlements)
	st.Balances[key] = newBal
	st.Settlements = append(st.Settlements, rec)
	return func() {
		if hadKey {
			st.Balances[key] = oldBal
		} else {
			delete(st.Balances, key)
		}
		st.Settlements = st.Settlements[:oldLen]
	}
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
