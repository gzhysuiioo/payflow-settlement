// Package payflow implements payment intent settlement and reconciliation.
package payflow

import (
	"math"
	"sort"
)

// Intent is one payment request from an account abstraction wallet.
type Intent struct {
	ID        string
	Account   string
	Paymaster string
	Amount    int64
	Asset     string
	Nonce     uint64
	State     string
}

// Settlement is the ledger effect of executing an intent.
type Settlement struct {
	Intent  string
	Charged int64
	FeeBps  int
	Ref     string
	Status  string
	Reason  string
}

// Execute applies fee policy and enforces per-intent idempotency.
//
// 判断顺序保持旧接口兼容：非 pending 状态、编号已结算两类拒绝优先于本次
// 参数检查；随后依次校验金额为正、费率在 [0,10000] 基点内、金额加手续费
// 不溢出 int64；最后才按准确总额判断余额。只有成功时才把编号加入 spent，
// 任何拒绝/失败都返回零扣款额与空结算引用且不占用编号。
func Execute(intent Intent, feeBps int, spent map[string]bool, balance int64) Settlement {
	if intent.State != "pending" {
		return Settlement{Intent: intent.ID, Status: "rejected", Reason: "intent is not pending"}
	}
	if spent[intent.ID] {
		return Settlement{Intent: intent.ID, Status: "rejected", Reason: "duplicate settlement attempt"}
	}
	if intent.Amount <= 0 {
		return Settlement{Intent: intent.ID, Status: "rejected", Reason: reasonBadAmount}
	}
	if feeBps < 0 || feeBps > 10000 {
		return Settlement{Intent: intent.ID, Status: "rejected", Reason: reasonBadFeeBps}
	}
	// feeFor 拆商拆余计算，amount*feeBps 中途超过 int64 也能得到准确手续费。
	fee := feeFor(intent.Amount, feeBps)
	if fee > math.MaxInt64-intent.Amount {
		return Settlement{Intent: intent.ID, Status: "rejected", Reason: reasonOverflow}
	}
	total := intent.Amount + fee
	if total > balance {
		return Settlement{Intent: intent.ID, Status: "failed", Reason: "insufficient balance for amount plus fee"}
	}
	spent[intent.ID] = true
	return Settlement{Intent: intent.ID, Charged: total, FeeBps: feeBps, Ref: "settle:" + intent.ID, Status: "settled"}
}

// Reconcile reports intents with no matching settlement, in stable order.
func Reconcile(intents []Intent, settlements []Settlement) []string {
	seen := map[string]bool{}
	for _, settlement := range settlements {
		if settlement.Status == "settled" {
			seen[settlement.Intent] = true
		}
	}
	var gaps []string
	for _, intent := range intents {
		if !seen[intent.ID] {
			gaps = append(gaps, intent.ID)
		}
	}
	sort.Strings(gaps)
	return gaps
}
