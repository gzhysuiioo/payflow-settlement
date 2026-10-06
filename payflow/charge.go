package payflow

import "math"

// 本文件是“一笔付款的金额规则”的唯一维护处，三个入口共用同一套约束：
//   - 本地批次提交与预览（Ledger.Submit / Ledger.Preview，非法费率是批次级
//     错误，金额问题是逐项错误）；
//   - 遗留单笔结算（Execute，数值非法与总额溢出为 rejected、余额不足为 failed）；
//   - 打开账本时的历史校验（历史付款的金额、费率、手续费或扣款总额不符即
//     corrupt_ledger，即使文件校验和正确也不能接受）。
//
// 规则本身：
//
//	金额必须为正 int64；
//	费率为 [0,10000] 基点（0 不收手续费，10000 时手续费等于金额）；
//	手续费 = floor(amount*feeBps/10000)，乘积中间值允许超过 int64；
//	扣款总额 = 金额 + 手续费；只要总额仍能由 int64 表示，就按精确总额判断
//	能否支付，只有总额本身越界才按各入口自己的约定拒绝——不能变成负数扣款
//	或普通余额不足。
//
// 各入口的错误分类、报错文字与判断顺序仍留在各自文件（例如批次级非法费率
// 整批拒绝、遗留入口先查状态与已用编号），这里只提供与错误约定无关的数值
// 原语，避免同一套金额/费率/总额约束被重复实现后逐渐分叉。

const (
	// feeBpsBase 是基点费率的分母：10000 个基点等于 100%。
	feeBpsBase = 10000
	// maxFeeBps 是允许的最大费率：10000 基点（= 100%），此时手续费等于金额。
	maxFeeBps = feeBpsBase
)

// validPaymentAmount 报告付款金额是否合法：必须为正 int64，0 与负数都不行。
func validPaymentAmount(amount int64) bool { return amount > 0 }

// validFeeBps 报告费率是否落在 [0,10000] 基点内。
func validFeeBps(feeBps int) bool { return feeBps >= 0 && feeBps <= maxFeeBps }

// feeFor 计算手续费：floor(amount*feeBps/10000)。
// 调用前要求 amount>0 且 0<=feeBps<=10000。
//
// 大额付款直接计算 amount*feeBps 可能超过 int64（例如金额 6e18、费率
// 10000 时乘积为 6e22），因此把金额拆成商和余数两部分，全程在 int64 内
// 精确完成：
//
//	amount = hi*10000 + lo
//	amount*feeBps/10000 = hi*feeBps + floor(lo*feeBps/10000)
//
// 合法输入下 hi*feeBps 不超过金额（hi <= amount/10000），lo*feeBps 小于
// 1e8，两段都不会溢出；费率不超过 10000，故手续费结果不超过金额本身。
func feeFor(amount int64, feeBps int) int64 {
	hi := amount / feeBpsBase
	lo := amount % feeBpsBase
	return hi*int64(feeBps) + (lo*int64(feeBps))/feeBpsBase
}

// amountPlusFee 在不发生 int64 溢出的前提下返回金额加手续费。
// representable 为 false 表示扣款总额超出 int64：调用方必须按各自约定拒绝，
// 绝不能使用整数回绕后的值（它可能为负）去做限额或余额判断。
func amountPlusFee(amount, fee int64) (total int64, representable bool) {
	if fee > math.MaxInt64-amount {
		return 0, false
	}
	return amount + fee, true
}

// chargeBreakdown 返回一笔付款的手续费与扣款总额。
// 调用前要求 amount 为正、feeBps 合法。手续费总能在 int64 内精确得出
// （朴素乘积可以越界，feeFor 不会）；representable 为 false 只可能是
// amount+fee 超出 int64，此时只能按“总额溢出”拒绝，不能继续限额或余额判断。
func chargeBreakdown(amount int64, feeBps int) (fee, total int64, representable bool) {
	fee = feeFor(amount, feeBps)
	total, representable = amountPlusFee(amount, fee)
	return fee, total, representable
}

// validateStoredCharge 校验账本历史中一笔成功付款保存的金额、费率、手续费
// 与扣款总额是否仍符合当前规则。打开账本（validateAndReplay）时使用：
// 任一项不符都是账本损坏，即使文件校验和正确也不能接受。
//
// 判定顺序与报错文字保持账本历史校验的原有约定，逐字不变：
// 非正金额（含负手续费、非正扣款）→ 非法费率 → 手续费与公式不符 →
// 扣款总额不等于金额加手续费（含总额本身超出 int64）。
func validateStoredCharge(id string, amount int64, feeBps int, fee, charged int64) error {
	if !validPaymentAmount(amount) || fee < 0 || !validPaymentAmount(charged) {
		return ledgerError(ErrCorrupt, "settlement %q has non-positive amounts", id)
	}
	if !validFeeBps(feeBps) {
		return ledgerError(ErrCorrupt, "settlement %q has invalid fee bps", id)
	}
	if want := feeFor(amount, feeBps); want != fee {
		return ledgerError(ErrCorrupt, "settlement %q fee mismatch: file %d, want %d", id, fee, want)
	}
	// 手续费已证明等于 feeFor(amount,feeBps)（<= amount）；总额仍可能超过
	// int64（大额高费率），那种历史里不可能保存下真正的 charged，同样按
	// “charged != amount + fee”判损坏。
	if total, ok := amountPlusFee(amount, fee); !ok || charged != total {
		return ledgerError(ErrCorrupt, "settlement %q charged != amount + fee", id)
	}
	return nil
}
