// Package payflow implements payment intent settlement and reconciliation.
package payflow

import (
	"fmt"
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
// 判断顺序保持旧接口约定：非 pending 状态、已结算编号优先于本次金额检查。
// 金额必须为正、费率必须在 [0,10000] 基点内；手续费按
// amount*feeBps/10000 向下取整（使用与账本一致的溢出安全算法，乘积中间值
// 不会溢出），扣款总额为金额加手续费。金额或费率不合法、总额超出 int64
// 时一律 rejected 且不扣款、不留结算引用、不占用编号；总额可表示但余额
// 不足时 failed；只有成功才标记编号。
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
		return Settlement{
			Intent: intent.ID,
			Status: "rejected",
			Reason: fmt.Sprintf("fee_bps must be within [0,10000], got %d", feeBps),
		}
	}
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
