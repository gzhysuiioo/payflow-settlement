package payflow

import (
	"fmt"
	"math"
)

// 付款数值约束的唯一维护处：金额、费率、手续费与扣款总额的规则只在
// 这里定义。本地结算（Submit/Preview 共用的 judgeIntent/validateBatch）、
// 遗留单笔结算（Execute）与账本历史校验（validateAndReplay）都从这里
// 取判定结果，再按各自入口对外的错误约定报告（invalid_parameter /
// rejected / corrupt_ledger），因此三处的数值语义始终一致。
//
// 规则：金额必须为正 int64；费率为 0 至 10000 基点；手续费按
// amount*feeBps/10000 向下取整；扣款总额为金额加手续费，总额必须
// 可表示为 int64。

// maxFeeBps 是费率上界（基点）：0 表示不收手续费，10000 表示手续费
// 等于付款金额。
const maxFeeBps = 10000

// amountValid 报告付款金额是否为正 int64。
func amountValid(amount int64) bool {
	return amount > 0
}

// feeBpsValid 报告费率是否在 [0, maxFeeBps] 基点内。
func feeBpsValid(feeBps int) bool {
	return feeBps >= 0 && feeBps <= maxFeeBps
}

// reasonBadFeeBps 是费率越界的统一说明，本地批次与遗留单笔共用同一措辞。
func reasonBadFeeBps(feeBps int) string {
	return fmt.Sprintf("fee_bps must be within [0,10000], got %d", feeBps)
}

// feeFor 计算手续费：amount*feeBps/10000 向下取整。
// amount>0、0<=feeBps<=10000 时朴素乘积可能溢出 int64，
// 拆成商和余数两部分即可在 int64 内精确完成。
func feeFor(amount int64, feeBps int) int64 {
	hi := amount / 10000
	lo := amount % 10000
	return hi*int64(feeBps) + (lo*int64(feeBps))/10000
}

// chargeFor 计算一笔付款的手续费与扣款总额（金额 + 手续费）。
// 前提：amount 与 feeBps 已分别通过 amountValid 与 feeBpsValid。
// 乘法中间结果不会溢出（见 feeFor），因此大额付款按精确金额判断；
// 只有最终总额超出 int64 时 ok 为 false（此时 fee 仍是精确值，
// total 无意义），调用方按各自约定把总额溢出报告为参数错误、
// 拒绝或账本损坏。
func chargeFor(amount int64, feeBps int) (fee, total int64, ok bool) {
	fee = feeFor(amount, feeBps)
	if fee > math.MaxInt64-amount {
		return fee, 0, false
	}
	return fee, amount + fee, true
}
