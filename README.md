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
  `state_error` / `insufficient_balance` / `invalid_parameter` / `storage_error`。

**幂等与冲突**：编号成功落账后，再次提交时若全部付款字段
（账户、付款方、资产、金额、nonce）与费率都与原请求相同，返回原结算记录并标记
`duplicate`，不再扣款；任一不同则返回 `conflict`，原记录保持不变。
未成功的编号（余额不足、状态错误、参数错误、存储错误）不占用去重资格，可以重新提交；
同批次内的重复编号遵循同样规则。

存储失败时该项返回 `storage_error` 并回滚到执行前状态（余额不变、不留成功记录），
已完成的前项不回滚；同一进程多次打开同一路径并发提交按同一本账本串行处理，
相同编号最终只有一笔成功记录，争用仅够一笔的余额时只有一笔成功。

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

### 标志与退出码

- `-l, --ledger <path>`：账本路径（init/submit/refund/query 必需）；
  `-f, --file <path>`：从文件读 JSON 输入（缺省读标准输入）。
- 成功退出码 0；批次级错误（如非法费率、账本不存在/损坏/已存在）退出码 1，
  stderr 输出 `{"error":{"kind":...,"message":...}}`。批次内逐项失败仍属正常结果。

### 旧版账本兼容

没有 `refunds` 字段的旧版账本无需任何转换即可查询、付款和退款；读取旧账本
以及在其上继续付款都不会改写文件（仍不含该字段），只有首次成功退款后文件才
带上 `refunds`。若退款引用了不存在的结算、同一付款被重复退回、退款金额偏离
原扣款，或历史收支与最终余额不符，账本都会被判定为 `corrupt_ledger` 而拒绝打开。

## 遗留入口

- `payflow demo`（无参数时同样运行）：内存中的单笔结算与对账演示；
- `payflow version`：版本号；
- `payflow.Execute` / `payflow.Reconcile` API 保持可用。

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
