# 链上支付与结算编排平台

## 用途

账户抽象、付款方与代付策略、批量结算、幂等与对账、失败补偿、手续费与限额治理、可审计流水。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `payflow/`，命令入口位于 `cmd/payflow/`。

```bash
go run ./cmd/payflow demo
go run ./cmd/payflow version
go test ./...
```

## 本地账本（init / submit / refund / query）

账本是用户指定路径上的单个 JSON 文件，关闭程序后再次打开可继续提交和查询。
每笔付款的余额变化与成功记录在同一个原子写入中落盘（临时文件 fsync → rename → 目录 fsync），
文件带 sha256 校验和与冻结的初始余额，崩溃后重开只能看到完整的旧状态或新状态，
损坏文件一律报错，不会被当成空账本重新开始。

### init — 初始化账户余额

输入为 JSON，每个 (账户, 资产) 组合给出非负 int64 余额。
空编号/空资产/负余额/重复组合都会拒绝，且**不创建账本文件**；
对已有路径重复初始化报错，绝不覆盖历史。

占用检查与创建是内核中的同一个原子操作（临时文件 fsync 后以 `link(2)`
排他发布，而非 rename 覆盖），因此多个进程同时初始化同一尚不存在的账本时
恰好一个成功，其余返回 `ledger_exists`，胜出账本完整保留自己的初始余额，
不会混入败者数据。正常账本、损坏或空的普通文件、目录，以及账本文件位置上
的符号链接（即使链接目标不存在）都按已占用拒绝：不替换链接、不顺着悬空
链接创建目标文件、不改动任何原内容；但仍允许经指向目录的符号链接在一个
尚未占用的文件位置创建新账本。

```bash
payflow init -l ./ledger.json <<'JSON'
{
  "balances": [
    {"account": "aa-1", "asset": "usdc", "balance": 2000},
    {"account": "aa-2", "asset": "eth", "balance": 5}
  ]
}
JSON
```

### submit — 提交 JSON 支付批次

一个批次携带固定费率 `fee_bps`（0–10000 基点）和**有序**意图列表；
结果逐项对应输入顺序，空批次返回 `{"results":[]}`。
各项独立成功或失败：后项能看到前项成功后的余额，任一失败不影响其余项。

```bash
payflow submit -l ./ledger.json <<'JSON'
{
  "fee_bps": 30,
  "limits": [
    {"account": "aa-1", "asset": "usdc", "max_charged": 2000}
  ],
  "intents": [
    {"id":"p1","account":"aa-1","paymaster":"pm-1","asset":"usdc","amount":1500,"nonce":1,"state":"pending"},
    {"id":"p2","account":"aa-1","asset":"usdc","amount":900}
  ]
}
JSON
```

- 手续费 = `amount * fee_bps / 10000`，向下取整（如 1500×30bp = 4）；
  `amount + fee` 从该账户同一资产扣除，溢出 int64 明确拒绝。
- `amount` 必须为正 int64；`id`、`account`、`asset` 不得为空。
- `state` 缺省视为 `"pending"`；非 pending 返回 `state_error`。
- 不存在的账户/资产组合按余额 0 处理，返回 `insufficient_balance`。
- 每项状态可分别识别：`settled` / `duplicate` / `conflict` /
  `state_error` / `insufficient_balance` / `limit_exceeded` /
  `invalid_parameter` / `storage_error`。

**本次批次扣款上限（可选）**：`limits` 为本次提交设置按 (账户, 资产) 组合的
扣款上限，`max_charged` 计入付款金额和手续费，各组合分别计算、互不通借。
上限仅对本次提交有效：每次提交从零计算，历史扣款和退款都不占本次额度；
只有成功落账的付款消耗额度，累计恰好等于上限仍允许成功。某项会使累计
扣款超出上限时返回 `limit_exceeded`（原因含该组合的上限、已用额度与本项
需扣金额），该项不扣余额、不留记录、不占编号与额度，后项继续处理；同一
编号改为较小金额仍可在本批次内成功。`limits` 缺省、为 `null` 或为空数组
时不限制扣款；未列出的组合沿用无限制行为。同时超限和余额不足时报告
`limit_exceeded`；限额变化不属于付款字段变化，不影响幂等判断。
每个限额项的 `account`、`asset` 必须非空，同一组合不得重复，`max_charged`
必须明确提供非负 int64 整数（`0` 表示禁止该组合新增扣款）；任一限额项
不合法或组合重复时整个批次返回 `invalid_parameter`、退出码 1，任何意图
都不执行（空意图列表同样检查）。

**幂等与冲突**：编号成功落账后，再次提交时若全部付款字段
（账户、付款方、资产、金额、nonce）与费率都与原请求相同，返回原结算记录并标记
`duplicate`，不再扣款；任一不同则返回 `conflict`，原记录保持不变。
未成功的编号（余额不足、状态错误、参数错误、存储错误）不占用去重资格，可以重新提交；
同批次内的重复编号遵循同样规则。

存储失败时该项返回 `storage_error` 并回滚到执行前状态（余额不变、不留成功记录），
已完成的前项不回滚；同一进程多次打开同一路径并发提交按同一本账本串行处理，
相同编号最终只有一笔成功记录，争用仅够一笔的余额时只有一笔成功。

### Go 示例：直接使用 Ledger.Submit 用本地账本付款

Go 调用者不必经过命令行：`CreateLedger` 在尚未占用的路径初始化余额，
`Open` 打开账本，`Ledger.Submit(FeeBatch)` 提交有序批次，`Query` 读回
余额与成功记录。**账本自己保存余额和成功记录并在每笔成功时原子落盘**，
调用者只需在结束时 `Close` 句柄。

特别注意：`Submit` 返回的错误为 `nil` 只表示**批次本身合法并被受理**，
绝不代表每项付款都成功——重复、超限、余额不足等都出现在逐项结果中，
调用者仍必须按输入顺序读取每一项的 `Status`。

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

// PaymentIntent.Amount 与 ChargeLimit.MaxCharged 在公开类型中都是 *int64：
// 用指针区分“未提供/null”与显式给出的 0（显式 0 金额非法；
// 显式 max_charged=0 表示禁止该组合新增扣款）。构造时取 int64 变量地址即可。
func intPtr(v int64) *int64 { return &v }

func main() {
	// 在尚未占用的路径初始化：只用一个账户的一种资产，余额 100。
	path := filepath.Join(os.TempDir(), "payflow-submit-example.json")
	_ = os.Remove(path) // 仅为让示例可重复运行；真实程序不要删除已有账本
	if err := payflow.CreateLedger(path, []payflow.BalanceInit{
		{Account: "aa-1", Asset: "usdc", Balance: 100},
	}); err != nil {
		panic(err) // 路径已占用时分类是 ledger_exists
	}

	l, err := payflow.Open(path)
	if err != nil {
		panic(err)
	}
	defer l.Close() // 结束时关闭账本句柄

	// 费率 1000 基点（10%），本批次 aa-1/usdc 扣款上限 88（含手续费）。
	// 有序请求：p1 支付 60 → 原样重复 p1 → p2 支付 30 → 同一 p2 支付 20。
	res, err := l.Submit(payflow.FeeBatch{
		FeeBps: 1000,
		Limits: []payflow.ChargeLimit{
			{Account: "aa-1", Asset: "usdc", MaxCharged: intPtr(88)},
		},
		Intents: []payflow.PaymentIntent{
			{ID: "p1", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: intPtr(60)},
			{ID: "p1", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: intPtr(60)},
			{ID: "p2", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: intPtr(30)},
			{ID: "p2", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: intPtr(20)},
		},
	})
	if err != nil {
		panic(err) // 整批被拒（费率越界、限额不合法等）时 res 为 nil
	}

	// 按输入顺序逐项处理：先区分成功、重复与失败，再访问记录详情。
	for _, r := range res.Results {
		switch r.Status {
		case payflow.StatusSettled, payflow.StatusDuplicate:
			// 只有 settled / duplicate 保证 Record 非 nil；
			// duplicate 携带的就是 p1 的原记录（同一 seq、同一扣款额）。
			fmt.Printf("%-3s %-15s amount=%d fee=%d charged=%d seq=%d\n",
				r.ID, r.Status, r.Record.Amount, r.Record.Fee, r.Record.Charged, r.Record.Seq)
		default:
			// 其余状态没有记录：失败原因读 r.Reason，不要解引用 r.Record。
			fmt.Printf("%-3s %-15s reason=%q\n", r.ID, r.Status, r.Reason)
		}
	}

	// 余额与成功记录都已由账本保存并落盘：直接查询读回，
	// 无须像旧版 Execute 示例那样自行维护和扣减余额。
	snap, err := l.Query()
	if err != nil {
		panic(err)
	}
	for _, b := range snap.Balances {
		fmt.Printf("balance %s/%s=%d\n", b.Account, b.Asset, b.Balance)
	}
	fmt.Printf("settlement history: %d record(s)\n", len(snap.Settlements))
	for _, r := range snap.Settlements {
		fmt.Printf("  seq=%d %s amount=%d fee=%d charged=%d\n", r.Seq, r.ID, r.Amount, r.Fee, r.Charged)
	}
}
```

输出（手续费 = 金额×1000/10000，向下取整）：

```text
p1  settled         amount=60 fee=6 charged=66 seq=1
p1  duplicate       amount=60 fee=6 charged=66 seq=1
p2  limit_exceeded  reason="charge limit exceeded for aa-1/usdc: max_charged 88, used 66, this item charges 33"
p2  settled         amount=20 fee=2 charged=22 seq=2
balance aa-1/usdc=12
settlement history: 2 record(s)
  seq=1 p1 amount=60 fee=6 charged=66
  seq=2 p2 amount=20 fee=2 charged=22
```

**限额如何累计**：上限只统计本次批次内**成功落账**的扣款（付款金额连同
手续费），从零累计：

1. `p1` 付 60：手续费 6，扣款总额 66 ≤ 88，成功；本批次已用 66，余额 100 → 34。
2. 原样重复的 `p1`：全部付款字段与费率都与原请求相同，判为 `duplicate`，
   **返回 p1 的原记录**（seq、金额、手续费、扣款额完全一致），不发生新扣款，
   已用额度仍为 66。
3. `p2` 付 30：手续费 3，需扣 33；66 + 33 = 99 > 88，判为
   `limit_exceeded`，原因里带有该组合的上限 88、已用 66 与本项需扣 33。
   该项不扣余额、不留成功记录、不占编号也不消耗额度——已用仍是 66。
4. 同一编号 `p2` 改成付 20：失败不占编号，所以这是一项全新请求；
   手续费 2，需扣 22，66 + 22 = 88 **恰好等于上限**——累计等于上限仍允许
   成功，于是判为 `settled`，余额 34 → 12，已用额度到 88 用满。

最终查询：余额 12，成功历史只有 `p1`、`p2` 两条（重复项不新增记录，
超限项没有成功记录）。这也印证了余额来自查询结果、账本早已完成扣款，
调用者不需要自己保存或扣减余额。

**两层失败要分开处理**：

- **整批错误（`Submit` 返回非 nil error，结果为 nil，不执行任何付款）**：
  费率越界（不在 0–10000 基点）或限额不合法（账户/资产为空、同一组合重复、
  `max_charged` 缺省或为负）都在批次执行前校验，整批返回
  `invalid_parameter`，即使意图列表为空也一样检查，任何一项都不会执行。
  分类与说明这样读取：

  ```go
  res, err := l.Submit(batch)
  if err != nil {
      var le *payflow.LedgerError
      if errors.As(err, &le) {
          // le.Kind 是可机读分类，如 invalid_parameter / ledger_closed；
          // le.Message 是人类可读说明。也可以直接用 payflow.KindOf(err)。
      }
      return // 没有逐项结果可读
  }
  ```

- **逐项失败（`Submit` 正常返回，error 为 nil）**：合法批次中的
  `limit_exceeded`、`insufficient_balance`、`state_error`、`conflict` 等
  都只落在对应项的 `Status` 上，原因读该项的 `Reason` 字段；整批照常返回，
  后项继续处理，前面成功的扣款也已落盘。访问 `Record` 前必须先确认状态是
  `settled` 或 `duplicate`，其余状态 `Record` 为 nil。

**与旧版 `Execute` 的区别**：旧版 `Execute` 对已成功的重复编号一律返回
`rejected`（原因 `duplicate settlement attempt`），且**不返回原结算记录**，
改不改字段都一样；本地账本把“原样重试”识别为 `duplicate` 并**附回原记录**、
不新增扣款，把“同编号但字段或费率变化”识别为 `conflict`、原记录保持不变。
此外旧接口的余额与编号表要调用者自己维护，本地账本则由 `Query` 直接读回
已持久化的余额与成功历史。

### submit --dry-run — 扣款前预览结算结果

在真正扣款前，可在 `submit` 命令上添加 `--dry-run`，沿用账本路径、
标准输入和 `-f/--file` 输入文件方式查看这批意图的**预计**结算结果：

```bash
payflow submit -l ./ledger.json --dry-run <<'JSON'
{
  "fee_bps": 0,
  "intents": [
    {"id":"p1","account":"aa-1","asset":"usdc","amount":70},
    {"id":"p2","account":"aa-1","asset":"usdc","amount":40}
  ]
}
JSON
```

输出为带 `dry_run: true` 标记、`results` 按输入顺序排列的结果，每项继续
使用与真实提交完全相同的状态、失败原因及结算记录格式；其中 `settled`
表示“按当前账本预计能够成功”。例如余额 100、费率为零，依次付款 70 和 40：
首项预计成功、次项 `insufficient_balance`（后项看到前项预计成功后的可用
余额），而不是两项都成功。

- 预览按整批顺序判断：后项看到前项**预计成功**后的可用余额和本批次已用限额。
- 预计成功的新付款使用从**当前最大付款序号之后连续递增**的序号；失败、
  重复和冲突不消耗序号。
- 已有成功编号仍按原付款字段（账户、付款方、资产、金额、nonce）与费率判断
  重复或冲突，重复项携带原记录，不占余额和本批次额度；**原付款已退款也遵守
  同样规则**。预览中首次预计成功的编号对本批次后续项同样生效：相同请求显示
  重复，字段不同显示冲突。失败项不占编号，后项用同一编号改成合法且可支付的
  请求仍可预计成功。
- 手续费、金额溢出、状态和限额判断沿用实际提交规则；同时超限与余额不足仍
  先报告 `limit_exceeded`；限额只累计本次预计成功的扣款并计入手续费。
- **预览过程中不写入账本**：结束后查询到的余额、付款、退款历史与预览前完全
  一致，账本文件内容不变，任何编号、序号和额度都不被预留。连续预览相同批次
  且账本未变化时结果一致；随后真实提交这些新付款仍作为首次提交处理。预览只
  预测业务结果，不承诺实际提交时的写入成功（因此预览结果不会出现
  `storage_error`）。
- 非法 JSON、费率或限额仍按批次级错误处理，退出码 1 且不输出部分结果；
  空批次也检查费率和限额。逐项业务失败正常输出并退出 0；合法空批次返回
  带预览标记的空结果列表 `{"dry_run":true,"results":[]}`。账本不存在或损坏
  时沿用现有错误分类，不创建或修复账本。
- 不启用 `--dry-run` 时，提交行为和输出格式保持原样。Go 调用者使用
  `payflow.Ledger.Preview(FeeBatch)` 获得同样的预览能力，返回
  `*PreviewResult`（含 `DryRun bool` 与 `Results`）；现有
  `payflow.Ledger.Submit` 接口保持不变。

### refund — 提交已结算付款的全额退款

输入为 JSON，`refunds` 是**有序**列表；每项含退款编号 `id`、原付款编号
`settlement_id` 和退款原因 `reason`，三者都必须非空。空列表返回
`{"results":[]}`。退款编号与付款编号分别计数、互不占用，字符串相同也允许。

```bash
payflow refund -l ./ledger.json <<'JSON'
{
  "refunds": [
    {"id":"r1","settlement_id":"p1","reason":"customer cancelled"},
    {"id":"r2","settlement_id":"p2","reason":"duplicate charge"}
  ]
}
JSON
```

- 成功时把原结算记录的 `charged`（付款金额 **连同当时已扣手续费**）全额退回
  原账户、原资产；退款金额与去向完全由原结算确定。原结算记录及其编号、顺序
  保持不变，只是可被标记为已退款。
- 各项状态可分别识别：`refunded` / `duplicate` / `conflict` /
  `already_refunded` / `not_found` / `invalid_parameter` / `storage_error`。

**幂等与冲突**：参数合法时，已成功的退款编号优先判断——目标结算与原因都相同
返回 `duplicate` 及原退款记录，不再入账；任一不同返回 `conflict`。使用新退款
编号时，找不到成功结算（**包括原付款只曾失败**）返回 `not_found`；该结算已被
其他退款编号退回则返回 `already_refunded`，不增加余额。未成功的申请
（参数错误、目标不存在、已退款、存储错误）不占用退款编号，之后可修改原因或
目标再提交。

退款不影响原付款的幂等资格：退款后用相同字段重试原付款仍返回原结算
（`duplicate`），合法字段有变仍为 `conflict`，都不会再次扣款；退回的余额可
立即用于后续付款。

批次逐项对应输入顺序、各自独立：后项能看到前项的成功退款，任一失败不影响其余。
某项保存失败返回 `storage_error`，该项既不增加余额也不留退款记录，前项成功
保留、后项继续。同一进程多个句柄并发退款或付款时按同一本账本串行处理：每笔
原付款最多退回一次。

### query — 查询余额、结算与退款记录

```bash
payflow query -l ./ledger.json
```

返回当前全部余额（按账户、资产排序）、按**结算成功先后顺序**排列的结算记录，
以及按**退款成功先后顺序**排列的退款记录（无退款时为空列表）。每条结算可追溯
到原意图（id/账户/付款方/资产/金额/nonce）与当时费率、手续费、扣款总额；
每条退款可追溯原结算（`settlement_id`），并记录退回的账户、资产、金额、
手续费、退款总额与原因。

### reconcile — 离线流水对账

把外部渠道的有序流水与指定账本逐笔核对。**只读**：不改变余额、历史或账本文件，
与同一进程内并发的 submit/refund 串行，整份报告对应同一个完整账本状态。
输入沿用 `-l/--ledger` 与 `-f/--file`（缺省读标准输入）。

```bash
payflow reconcile -l ./ledger.json <<'JSON'
{
  "entries": [
    {"kind":"payment","id":"p1","account":"aa-1","asset":"usdc","amount":1500,"fee":4,"charged":1504},
    {"kind":"refund","id":"r1","account":"aa-1","asset":"usdc","amount":1500,"fee":4,"charged":1504,"settlement_id":"p1"}
  ]
}
JSON
```

- `entries` 是**有序**列表；`kind` 为 `payment` 或 `refund`，`id` 是对应的付款
  或退款编号。付款编号与退款编号**分别匹配**，字符串相同也不会混为一笔。
- 每条提供 `account`、`asset`、`amount`、`fee`、`charged`；退款还必须提供
  `settlement_id`（原付款编号）。编号、账户、资产与退款原付款编号不得为空；
  三个金额都必须提供且为 int64 整数，`amount`、`charged` 为正，`fee` 非负。
- 只核对成功记录：已退款的付款仍按原扣款核对，退款单独核对。

**按成功序号限定核对范围**：外部渠道按批次提供流水时，可用四个标志只核对
指定批次的记录，避免把账本里其他批次的付款和退款列为缺失。两类记录各用
自己的成功序号，分别选择范围，不根据退款引用的付款序号判断退款是否入选：

- `--payment-after` / `--payment-through`：付款记录的排除下界与包含上界；
- `--refund-after` / `--refund-through`：退款记录的排除下界与包含上界。

范围为 `after < seq <= through`：下界排除、上界包含。下界缺省取零，上界
缺省取本次所见该类记录的最大序号（即完整历史）；上下界相等表示范围为空。
不带新标志时沿用全量对账。边界必须是非负 int64 整数，且下界不大于上界、
上界不超过对应类别的最大序号；非法边界整次返回 `invalid_parameter`、退出
码 1，不输出部分报告。空历史的最大序号为零。

`results` 逐条对应输入顺序，五种状态：

- `matched`：命中同类型同编号的成功记录，账户、资产、三个金额
  （退款再加原付款编号）全部一致；
- `field_mismatch`：命中记录但有字段不同，`diffs` 列出**每个**不同字段的
  账本值与流水值；
- `missing_in_ledger`：账本没有该编号的成功记录；
- `duplicate`：同一类型和编号在输入中出现多次时，**整组**全部标为重复并附
  全部输入位置 `positions`，不再判断字段差异，对应账本记录也不列为缺失；
- `out_of_scope`：命中同类型同编号的成功记录，但该记录的成功序号不在本次
  核对范围内。携带账本记录，不比较字段，也不计入流水侧汇总；该编号在输入
  中出现多次时全部按此状态处理（优先于整组重复）。

`missing_payments` / `missing_refunds` 只列账本中**入选**且没有任何对应流水
的成功记录（先按付款成功顺序，再按退款成功顺序）；空列表意味着没有入选
记录缺失。所有能定位到账本记录的结果（匹配、字段不符、重复、范围外、缺失
列表）都携带记录详情及关联：付款带 `refund_id`（若已退款），退款带
`settlement_id`。范围不切断关联：入选退款可以指向范围外付款，入选付款也
可带范围外退款的信息。

`totals` 按账户、资产排序，分别给出账本与流水的扣款合计、退款合计、净扣款
（扣款减退款）；`net_diff` 给出逐组合的净扣款差额，方向为**流水减账本**。
**账本侧合计只包含入选记录**：只选退款时扣款合计为零，退款按所选记录合计，
不会顺带计入原付款。**流水侧合计不包含范围外项**，但范围内重复项与账本外
流水仍逐条计入。不同资产各自一行，绝不相加，总额相等也不能抵消逐笔差异。
所有汇总金额都是十进制整数字符串，累计超过 int64 仍精确。
`max_payment_seq` / `max_refund_seq` 是完整历史的最大成功序号，空历史为零；
`payment_after` / `payment_through` / `refund_after` / `refund_through` 是
本次报告实际采用的四个边界。

字段缺失、类型不符、未知 kind、数值越界、非法 JSON 或非法序号边界时，整次
返回 `invalid_parameter`、退出码 1，**不输出部分报告**；输入有效但有差异仍
正常输出报告并退出 0。账本不存在（`ledger_not_initialized`）、损坏
（`corrupt_ledger`）与输入文件读取失败（`storage_error`）沿用现有分类。
兼容旧版无退款账本；旧的 `payflow.Execute` / `payflow.Reconcile` API 保持
不变（全量对账）；按范围对账使用 `payflow.ReconcileFlowRanged`。

### 标志与退出码

- `-l, --ledger <path>`：账本路径（init/submit/refund/query/reconcile 必需）；
  `-f, --file <path>`：从文件读 JSON 输入（缺省读标准输入）。
- `submit` 另有 `--dry-run`：只预览预计结算结果，不写入账本，输出带
  `dry_run:true` 标记。
- `reconcile` 另有 `--payment-after`、`--payment-through`、`--refund-after`、
  `--refund-through`，分别限定付款与退款记录的核对序号范围（`after < seq <= through`）。
- 成功退出码 0；批次级错误（如非法费率、账本不存在/损坏/已存在、对账输入或
  序号边界非法）退出码 1，stderr 输出 `{"error":{"kind":...,"message":...}}`。
  批次内逐项失败、对账发现差异仍属正常结果。

### 旧版账本兼容

没有 `refunds` 字段的旧版账本无需任何转换即可查询、付款和退款；读取旧账本
以及在其上继续付款都不会改写文件（仍不含该字段），只有首次成功退款后文件才
带上 `refunds`。若退款引用了不存在的结算、同一付款被重复退回、退款金额偏离
原扣款，或历史收支与最终余额不符，账本都会被判定为 `corrupt_ledger` 而拒绝打开。

## 遗留入口

- `payflow demo`（无参数时同样运行）：内存中的单笔结算与对账演示；
- `payflow version`：版本号；
- `payflow.Execute` / `payflow.Reconcile` API 保持可用。

### payflow.Execute — 内存中的单笔结算

```go
func Execute(intent Intent, feeBps int, spent map[string]bool, balance int64) Settlement
```

`Execute` 只做**单笔**判断：它既不代表调用者保存余额，也不把结果写进
本地账本，连续付款时的余额维护完全由调用者负责。

- `balance` 是**这一次调用**的可用余额，由调用者保存并在调用间传递。
  `Execute` 不会修改它。成功返回的 `Charged` 已包含手续费
  （`amount + fee`），调用者据此更新自己保存的余额：
  `balance -= settlement.Charged`。只有 `settled` 才能这样扣减；
  `failed` / `rejected` 的 `Charged` 为 0，不能用于扣减余额。
- `spent` 是调用者创建并保留的可写编号标记表，必须在连续调用间复用
  **同一份**才能起到幂等作用。只有成功付款才把编号写入；余额不足
  （`failed`）或参数、状态被拒绝（`rejected`）都不占用编号，因此失败
  的编号可以修改请求后重新提交。
- `intent.State` 必须显式为 `"pending"`，空状态或其他状态一律
  `rejected`（原因 `intent is not pending`）。
- 判断顺序：非 pending 状态 → 重复编号 → 金额/费率/扣款总额合法性 →
  余额。状态分类：`settled` 成功；`failed` 仅表示请求合法但
  `amount + fee` 超过本次可用余额；`rejected` 表示状态、重复编号或
  参数（金额非正、费率越界、总额溢出 int64）问题。
- **重复成功编号**：返回 `rejected`、原因 `duplicate settlement
  attempt`，**不返回原结算记录**；即使改变付款字段或费率，也只是同样
  的 `rejected`——旧接口没有本地账本 `submit` 的 `duplicate`（附原
  记录）与 `conflict` 分类。

完整示例：同一账户、同一资产，起始余额 2000、费率 30 基点，连续付款
并据返回结果维护余额。注意第二次调用传入的是上一次成功扣款后剩下的
496，而不是重新用起始余额 2000：

```go
package main

import (
	"fmt"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

func main() {
	balance := int64(2000)     // 调用者自己保存的可用余额
	spent := map[string]bool{} // 调用者自己保留的编号标记表

	pay := func(id string, amount int64) {
		s := payflow.Execute(payflow.Intent{
			ID: id, Account: "aa-1", Paymaster: "pm-1",
			Asset: "usdc", Amount: amount, Nonce: 1, State: "pending",
		}, 30, spent, balance)
		fmt.Printf("%s amount=%d -> status=%s charged=%d reason=%q\n",
			id, amount, s.Status, s.Charged, s.Reason)
		if s.Status == "settled" {
			balance -= s.Charged // 只有成功才扣减；Charged 已含手续费
		}
		fmt.Printf("  caller balance=%d spent=%v\n", balance, spent)
	}

	pay("pay-1", 1500) // settled：扣 1504，余 496
	pay("pay-2", 900)  // failed：需 902 > 496，不扣余额、不占编号
	pay("pay-2", 400)  // settled：失败编号可复用，扣 401，余 95
	pay("pay-1", 1500) // rejected：重复成功编号，不返回原记录
}
```

输出：

```text
pay-1 amount=1500 -> status=settled charged=1504 reason=""
  caller balance=496 spent=map[pay-1:true]
pay-2 amount=900 -> status=failed charged=0 reason="insufficient balance for amount plus fee"
  caller balance=496 spent=map[pay-1:true]
pay-2 amount=400 -> status=settled charged=401 reason=""
  caller balance=95 spent=map[pay-1:true pay-2:true]
pay-1 amount=1500 -> status=rejected charged=0 reason="duplicate settlement attempt"
  caller balance=95 spent=map[pay-1:true pay-2:true]
```

可以看到：`pay-2` 第一次失败（`failed`）后余额不变、`spent` 里没有
`pay-2`，所以改成 400 用同一编号仍能成功；而 `pay-1` 已成功落进
`spent`，重复提交只会得到 `rejected`。

**与 `payflow demo` 的关系**：demo 为每次 `Execute` 调用都传入固定余额
2000，含义是“每次调用各自独立、都按 2000 可用余额判断”，因此 demo 中
第二笔 900（需扣 902）也会成功。这不是多笔付款共用一份余额依次扣减的
批次结果；要表达“同一余额上的连续付款”，需像上面示例一样由调用者自己
维护余额。

**需要自动持久化时**：需要自动保存余额、保留成功记录、原样重试返回原
结算记录（`duplicate`）或字段变化报 `conflict` 的调用者，应使用本地
账本提交功能（`payflow init` + `payflow submit` 命令，或 Go 的
`payflow.Ledger.Submit`），见上文「本地账本」一节。

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
