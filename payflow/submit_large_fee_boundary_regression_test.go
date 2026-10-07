package payflow

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 本文件为 Submit 的大额手续费补充回归保障，重点锁定“金额只增加一个最小
// 单位、手续费却刚好跨过整数界限”的相邻金额：
//
//	金额 4000000000000000333，费率 30 基点：
//	  手续费 = floor(4000000000000000333*30/10000) = 12000000000000000
//	  扣款总额 = 4012000000000000333
//	金额只增加一个最小单位到 4000000000000000334：
//	  手续费跨过整数界限变为 12000000000000001
//	  扣款总额 = 4012000000000000335（比上一笔多 2，而不是只多 1）
//
// 金额与费率的乘积（约 1.2e20）远超 int64 上界（约 9.22e18），但金额、
// 手续费与扣款总额各自都能由 int64 表示；合法付款不能因为中间乘积大就被
// 当成数值溢出（invalid_parameter）拒绝。余额不足仍只是逐项业务失败，
// 携带这一项的合法批次必须正常返回逐项结果。
//
// 判定一律以 Ledger.Submit 的逐项结果、成功记录（Record）与 Query 的
// 余额/历史为准：手续费按 amount*feeBps/10000 向下取整，扣款总额包含
// 本金与手续费。请求使用新付款编号、state=pending，且不设置本次扣款
// 上限，避免其他拒绝条件掩盖金额判断。
const (
	largeBoundaryFeeBps = 30
	largeAmountTrailing = int64(4000000000000000333) // 末尾 333
	largeFeeTrailing    = int64(12000000000000000)
	largeTotalTrailing  = largeAmountTrailing + largeFeeTrailing // 4012000000000000333

	largeAmountStepped = int64(4000000000000000334)           // 只多一个最小单位，末尾 334
	largeFeeStepped    = int64(12000000000000001)             // 手续费跨过整数界限
	largeTotalStepped  = largeAmountStepped + largeFeeStepped // 4012000000000000335
)

// largeBoundaryCases 是两笔相邻大额付款的精确期望：金额相差 1，
// 手续费相差 1，扣款总额相差 2——两者既不是相同金额，后者的扣款
// 总额也不是只增加一个单位。
func largeBoundaryCases() []struct {
	name   string
	id     string
	amount int64
	fee    int64
	total  int64
} {
	return []struct {
		name   string
		id     string
		amount int64
		fee    int64
		total  int64
	}{
		{"amount_trailing_333", "big-333", largeAmountTrailing, largeFeeTrailing, largeTotalTrailing},
		{"amount_trailing_334", "big-334", largeAmountStepped, largeFeeStepped, largeTotalStepped},
	}
}

// TestLargeFeeBoundaryConstants 锁定相邻金额的全部数值关系：金额差 1、
// 手续费差 1、扣款总额差 2，且三个金额层面的值均为正 int64；乘积远超
// int64 上界时 feeFor/chargeFor 仍给出精确手续费与总额（ok=true），
// 不能误报溢出。
func TestLargeFeeBoundaryConstants(t *testing.T) {
	if largeAmountStepped-largeAmountTrailing != 1 {
		t.Fatalf("adjacent amounts must differ by exactly 1: %d vs %d",
			largeAmountStepped, largeAmountTrailing)
	}
	if largeFeeStepped-largeFeeTrailing != 1 {
		t.Fatalf("fee must step across the integer boundary by 1: %d vs %d",
			largeFeeStepped, largeFeeTrailing)
	}
	if largeTotalStepped-largeTotalTrailing != 2 {
		t.Fatalf("charged total must grow by 2 (principal and fee each by 1), got delta %d: %d vs %d",
			largeTotalStepped-largeTotalTrailing, largeTotalStepped, largeTotalTrailing)
	}
	if largeAmountTrailing == largeAmountStepped {
		t.Fatal("the two adjacent amounts must not be treated as the same amount")
	}
	for _, tc := range largeBoundaryCases() {
		if got := feeFor(tc.amount, largeBoundaryFeeBps); got != tc.fee {
			t.Fatalf("%s: feeFor(%d, 30)=%d want %d", tc.name, tc.amount, got, tc.fee)
		}
		fee, total, ok := chargeFor(tc.amount, largeBoundaryFeeBps)
		if !ok {
			t.Fatalf("%s: representable amount/total must not be flagged overflow", tc.name)
		}
		if fee != tc.fee || total != tc.total {
			t.Fatalf("%s: chargeFor fee=%d total=%d want fee=%d total=%d",
				tc.name, fee, total, tc.fee, tc.total)
		}
		if total != tc.amount+tc.fee {
			t.Fatalf("%s: charged must include principal plus fee: total=%d amount=%d fee=%d",
				tc.name, total, tc.amount, tc.fee)
		}
	}
}

// TestSubmitLargeFeeBoundarySettlesAtExactBalance 覆盖成功路径：在余额恰好
// 等于各自扣款总额的独立账本中提交两种请求，都必须作为新付款 settled，
// 查询余额为零，历史中只增加对应的一条成功记录；记录完整保留原金额的
// 末尾数字（333/334）与对应手续费，两笔的金额/手续费/扣款总额互不混淆。
func TestSubmitLargeFeeBoundarySettlesAtExactBalance(t *testing.T) {
	for _, tc := range largeBoundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			path := newTestLedger(t, []BalanceInit{
				{Account: "aa", Asset: "usdc", Balance: tc.total},
			})
			l := openOrFail(t, path)

			it := intent(tc.id, "aa", "usdc", tc.amount) // 新编号、state=pending、无 limits
			res, err := l.Submit(FeeBatch{FeeBps: largeBoundaryFeeBps, Intents: []PaymentIntent{it}})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if len(res.Results) != 1 {
				t.Fatalf("want exactly 1 per-item result, got %d", len(res.Results))
			}
			item := res.Results[0]

			// 逐项结果：作为新付款 settled，携带完整成功记录且不带失败原因。
			wantRecord := Record{
				ID:        tc.id,
				Account:   "aa",
				Paymaster: "pm",
				Asset:     "usdc",
				Amount:    tc.amount,
				Nonce:     1,
				FeeBps:    largeBoundaryFeeBps,
				Fee:       tc.fee,
				Charged:   tc.total,
				Seq:       1,
			}
			assertItemResult(t, item, StatusSettled, "", wantRecord)

			// 公开 JSON 格式回归：原金额末尾数字、手续费与扣款总额逐字保留，
			// 不截断、不近似、不出现科学计数法，也不携带溢出/不足之类的原因。
			raw, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			for _, frag := range []string{
				`"id":"` + tc.id + `"`,
				`"status":"settled"`,
				`"amount":` + jsonInt(tc.amount),
				`"fee_bps":30`,
				`"fee":` + jsonInt(tc.fee),
				`"charged":` + jsonInt(tc.total),
				`"seq":1`,
			} {
				if !bytes.Contains(raw, []byte(frag)) {
					t.Fatalf("settled result JSON missing %s:\n%s", frag, raw)
				}
			}

			// 查询余额为零；历史中只增加对应的一条成功记录，字段逐项精确。
			if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
				t.Fatalf("balance after settle=%d want 0", bal)
			}
			snap := mustQuery(t, l)
			if !reflect.DeepEqual(snap.Balances, []BalanceView{
				{Account: "aa", Asset: "usdc", Balance: 0},
			}) {
				t.Fatalf("balances: %+v", snap.Balances)
			}
			if len(snap.Settlements) != 1 {
				t.Fatalf("settlement count=%d want 1: %+v", len(snap.Settlements), snap.Settlements)
			}
			if snap.Settlements[0] != wantRecord {
				t.Fatalf("stored record:\n got %+v\nwant %+v", snap.Settlements[0], wantRecord)
			}

			// 关闭重开：磁盘上的记录同样精确保留末尾数字与手续费。
			l.Close()
			l2 := openOrFail(t, path)
			snap2 := mustQuery(t, l2)
			if len(snap2.Settlements) != 1 || snap2.Settlements[0] != wantRecord {
				t.Fatalf("reopen record:\n got %+v\nwant %+v", snap2.Settlements, wantRecord)
			}
			if bal, _ := l2.Balance("aa", "usdc"); bal != 0 {
				t.Fatalf("balance after reopen=%d want 0", bal)
			}

			// 原请求重提是幂等重复：仍携带同一条精确记录、不再扣款，
			// 反向证明上一笔是按新编号首次成功。
			dup := mustSettle(t, l2, largeBoundaryFeeBps, it)
			assertItemResult(t, dup, StatusDuplicate, reasonDuplicate, wantRecord)
			if bal, _ := l2.Balance("aa", "usdc"); bal != 0 {
				t.Fatalf("duplicate resubmit charged again: bal=%d want 0", bal)
			}
			if len(mustQuery(t, l2).Settlements) != 1 {
				t.Fatalf("duplicate must not add a second record")
			}
		})
	}
}

// TestSubmitLargeFeeBoundaryOneUnitShortIsPerItemFailure 覆盖失败路径：
// 余额比各自扣款总额少一个最小单位时返回 insufficient_balance，原因明确
// 表示余额不足，不携带成功记录；余额、历史与账本文件都保持提交前状态。
// 注意付款本金本身仍小于可用余额，拒绝纯粹因为计入手续费后差一个单位。
func TestSubmitLargeFeeBoundaryOneUnitShortIsPerItemFailure(t *testing.T) {
	for _, tc := range largeBoundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			shortByOne := tc.total - 1
			path := newTestLedger(t, []BalanceInit{
				{Account: "aa", Asset: "usdc", Balance: shortByOne},
			})
			l := openOrFail(t, path)

			// 前置事实：本金本身仍小于可用余额，差的是手续费那一个单位。
			if tc.amount >= shortByOne {
				t.Fatalf("principal %d must be strictly below available balance %d; "+
					"the rejection must come from the fee, not the principal",
					tc.amount, shortByOne)
			}

			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			it := intent(tc.id, "aa", "usdc", tc.amount) // 新编号、state=pending、无 limits
			res, err := l.Submit(FeeBatch{FeeBps: largeBoundaryFeeBps, Intents: []PaymentIntent{it}})
			if err != nil {
				t.Fatalf("a per-item business failure must not abort the batch: %v", err)
			}
			if len(res.Results) != 1 {
				t.Fatalf("want exactly 1 per-item result, got %d", len(res.Results))
			}
			item := res.Results[0]
			if item.ID != tc.id {
				t.Fatalf("result id=%q want %q", item.ID, tc.id)
			}
			if item.Status != StatusFunds {
				t.Fatalf("status=%s want %s: %+v", item.Status, StatusFunds, item)
			}
			if item.Reason != reasonFunds {
				t.Fatalf("reason=%q want %q", item.Reason, reasonFunds)
			}
			if item.Record != nil {
				t.Fatalf("insufficient_balance must not carry a success record: %+v", item.Record)
			}

			// 余额与历史保持提交前状态：没有扣款，也没有新增成功记录。
			if bal, _ := l.Balance("aa", "usdc"); bal != shortByOne {
				t.Fatalf("balance changed after rejection: %d want %d", bal, shortByOne)
			}
			snap := mustQuery(t, l)
			if len(snap.Settlements) != 0 {
				t.Fatalf("failed payment must leave no success record: %+v", snap.Settlements)
			}
			if !reflect.DeepEqual(snap.Balances, []BalanceView{
				{Account: "aa", Asset: "usdc", Balance: shortByOne},
			}) {
				t.Fatalf("balances after rejection: %+v", snap.Balances)
			}

			// 账本文件逐字节不变：失败不触发任何落盘。
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("ledger file changed on a rejected payment")
			}

			// 重开后磁盘状态同样未变，失败编号未被占用。
			l.Close()
			l2 := openOrFail(t, path)
			if bal, _ := l2.Balance("aa", "usdc"); bal != shortByOne {
				t.Fatalf("balance after reopen=%d want %d", bal, shortByOne)
			}
			if len(mustQuery(t, l2).Settlements) != 0 {
				t.Fatalf("failed payment must leave no record on disk")
			}
		})
	}
}

// TestSubmitLargeFeeBoundaryFailureDoesNotAbortValidBatch 锁定余额不足只是
// 逐项业务失败：合法批次中前项大额付款差一个单位被拒，批次本身仍退出
// 正常并逐项返回结果，后项照常成功；只有成功项扣款、落盘并留下记录。
func TestSubmitLargeFeeBoundaryFailureDoesNotAbortValidBatch(t *testing.T) {
	shortByOne := largeTotalTrailing - 1
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: shortByOne}, // 大额项差一单位
		{Account: "bb", Asset: "usdc", Balance: 1003},       // 1000@30bps => fee 3, total 1003
	})
	l := openOrFail(t, path)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	res, err := l.Submit(FeeBatch{FeeBps: largeBoundaryFeeBps, Intents: []PaymentIntent{
		intent("big-333", "aa", "usdc", largeAmountTrailing), // 计入手续费后差一单位
		intent("small", "bb", "usdc", 1000),                  // 同批次后项照常成功
	}})
	if err != nil {
		t.Fatalf("a per-item failure must not surface as a batch error: %v", err)
	}
	if got := statuses(res); !reflect.DeepEqual(got, []string{StatusFunds, StatusSettled}) {
		t.Fatalf("statuses=%v want [insufficient_balance settled]", got)
	}
	failed, settled := res.Results[0], res.Results[1]
	if failed.Reason != reasonFunds || failed.Record != nil {
		t.Fatalf("failed item must only carry the funds reason: %+v", failed)
	}
	wantSmall := Record{
		ID: "small", Account: "bb", Paymaster: "pm", Asset: "usdc",
		Amount: 1000, Nonce: 1, FeeBps: 30, Fee: 3, Charged: 1003, Seq: 1,
	}
	assertItemResult(t, settled, StatusSettled, "", wantSmall)

	// 失败项不动 aa/usdc；成功项精确扣光 bb/usdc；历史只有成功项一条。
	if bal, _ := l.Balance("aa", "usdc"); bal != shortByOne {
		t.Fatalf("aa/usdc changed: %d want %d", bal, shortByOne)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 0 {
		t.Fatalf("bb/usdc balance=%d want 0", bal)
	}
	snap := mustQuery(t, l)
	if len(snap.Settlements) != 1 || snap.Settlements[0] != wantSmall {
		t.Fatalf("history must contain only the successful item: %+v", snap.Settlements)
	}

	// 对照：批次里的成功项确实触发了一次正常落账（失败项本身不写盘）。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatalf("the successful later item must have persisted the ledger")
	}
}

// jsonInt 以十进制字符串返回 int64，用于在 JSON 片段中精确比对大整数。
func jsonInt(v int64) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
