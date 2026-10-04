# 链上支付与结算编排平台

## 用途

账户抽象、付款方与代付策略、批量结算、幂等与对账、失败补偿、手续费与限额治理、可审计流水。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `payflow/`，命令入口位于 `cmd/payflow/`。

```bash
go run ./cmd/payflow demo
go run ./cmd/payflow version
go run ./cmd/payflow evaluate config.json <flag-key> context.json
go test ./...
```

## 离线特性开关求值

`evaluate` 在本机离线对单个特性开关求值，依次传入配置文件路径、开关键与上下文文件路径，
两个文件均为 UTF-8 JSON。成功时标准输出仅包含一个对象：

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro"}
```

- `reason`：`disabled`（开关关闭，固定 false）、`rule`（命中规则，`ruleId` 为该规则 id）、`default`（无规则命中，取 default，`ruleId` 为 null）。
- 配置顶层为 `{"flags":[...]}`；每个开关含非空 `key`、布尔 `enabled`/`default`、`rules`；规则含非空 `id`、布尔 `value` 与非空 `conditions`；条件含非空 `attribute`、`op`（`eq`/`in`）和 `value`（eq 为字符串，in 为非空字符串数组）。
- 上下文为属性名到字符串的对象，可为 `{}`，空字符串是合法值；属性缺失即条件不成立。比较区分大小写、不裁剪空白。
- 字符串中的 `\uXXXX` 转义必须组成完整字符：高代理项只能与紧接着的低代理项配对，孤立的高/低代理项或不完整、顺序错误的配对（无论出现在字段名还是字符串值中）都会使读取失败；真正的 `�`（U+FFFD）仍是合法字符串。
- 两个文件会先完整校验（含未选中或已关闭的开关）；任何参数/文件/JSON/字段/唯一性错误都以非零状态退出，标准输出为空，标准错误指出具体字段与位置。
- 每个文件必须恰好是一个完整 JSON 文档：首尾允许 JSON 规定的空白（空格、制表符、回车、换行），但完整文档之后出现任何额外内容——即使是合法的第二个 JSON 值或未写完的片段——整个文件都按 JSON 无效拒绝，不会只取前半段求值。

### 完整示例：多条规则与命中优先级

把下面两份文件保存为 UTF-8 JSON。`config.json` 中 `new-checkout` 的两条规则 id 不同、返回相反的布尔值：第一条同时用 `eq` 和 `in` 检查两个属性，第二条展示后续匹配；另含一个已关闭的开关 `legacy-report`：

```json
{
  "flags": [
    {
      "key": "new-checkout",
      "enabled": true,
      "default": true,
      "rules": [
        {
          "id": "r-block-cn-mobile",
          "value": false,
          "conditions": [
            {"attribute": "region", "op": "eq", "value": "cn"},
            {"attribute": "platform", "op": "in", "value": ["ios", "android"]}
          ]
        },
        {
          "id": "r-pro",
          "value": true,
          "conditions": [
            {"attribute": "plan", "op": "eq", "value": "pro"}
          ]
        }
      ]
    },
    {
      "key": "legacy-report",
      "enabled": false,
      "default": true,
      "rules": [
        {
          "id": "r-everyone",
          "value": true,
          "conditions": [
            {"attribute": "plan", "op": "in", "value": ["free", "pro"]}
          ]
        }
      ]
    }
  ]
}
```

`context.json`（同时满足两条规则）：

```json
{"region": "cn", "platform": "ios", "plan": "pro"}
```

运行：

```bash
go run ./cmd/payflow evaluate config.json new-checkout context.json
```

输出：

```json
{"key":"new-checkout","value":false,"reason":"rule","ruleId":"r-block-cn-mobile"}
```

这个上下文对两条规则都成立（`region`/`platform` 命中第一条，`plan` 命中第二条），但规则按配置数组中的次序逐条检查，第一条所有条件都成立的规则立即决定结果：命中 `false` 同样立刻定案，不会继续寻找后面返回 `true` 的规则。因此这里采用 `r-block-cn-mobile` 的 `false`；规则优先级完全取决于数组次序，把两条规则互换位置，结果就是 `r-pro` 的 `true`。

### 属性缺失与默认值

沿用上面的开关，换一个缺少 `region` 的上下文：

```json
{"platform": "ios", "plan": "pro"}
```

```bash
go run ./cmd/payflow evaluate config.json new-checkout context-missing.json
```

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro"}
```

属性缺失不等于空字符串：第一条规则的 `eq` 条件因 `region` 缺失而不成立，即使 `platform` 的 `in` 条件成立，仅有部分条件成立的规则也不会获胜，求值继续考虑后面的规则，最终命中 `r-pro`。注意空字符串本身仍是合法属性值——若条件写成 `{"attribute":"region","op":"eq","value":""}`，只有上下文显式给出 `"region":""` 才成立，缺失不成立。字符串比较区分大小写、不裁剪空白：`"CN"`、`"cn "` 都不等于 `"cn"`。

再换一个没有任何规则命中的上下文：

```json
{"region": "us", "platform": "web", "plan": "free"}
```

```bash
go run ./cmd/payflow evaluate config.json new-checkout context-none.json
```

```json
{"key":"new-checkout","value":true,"reason":"default","ruleId":null}
```

两条规则都不命中时取开关的 `default`（这里为 `true`），`reason` 为 `default`、`ruleId` 为 `null`，与命中规则的输出（`reason` 为 `rule`、`ruleId` 为规则 id）明确区分。

### 关闭的开关与配置校验

同一配置中的 `legacy-report` 已关闭（`"enabled": false`）。用上面第一个上下文求值，即使它的规则本来会命中：

```bash
go run ./cmd/payflow evaluate config.json legacy-report context.json
```

```json
{"key":"legacy-report","value":false,"reason":"disabled","ruleId":null}
```

合法的关闭开关固定返回 `false`（`reason` 为 `disabled`），不采用它的 `default` 或任何规则。

配置与上下文两个文件在任何求值之前都会先完成校验，包括未被选中的开关和已关闭的开关。例如换成下面这份配置——请求的 `new-checkout` 完全合法，但同一配置中的另一个开关 `beta-dashboard` 缺少必填的 `default` 字段：

```json
{
  "flags": [
    {
      "key": "new-checkout",
      "enabled": true,
      "default": true,
      "rules": [
        {
          "id": "r-pro",
          "value": true,
          "conditions": [
            {"attribute": "plan", "op": "eq", "value": "pro"}
          ]
        }
      ]
    },
    {
      "key": "beta-dashboard",
      "enabled": true,
      "rules": []
    }
  ]
}
```

```bash
go run ./cmd/payflow evaluate config-bad.json new-checkout context.json
```

仍以非零状态退出，标准输出为空，标准错误指出问题所在的字段与位置：

```
config.flags[1].default: field is required
```

（用 `go run` 运行时它会在标准错误追加自己的一行 `exit status 1`；程序本身只输出上面那一行。）

把请求的开关键换成合法但已关闭的开关也不会绕过这一步：只要配置中任何开关校验失败，整个命令同样非零退出。只有整份配置通过校验后，关闭的开关才按上一条所述固定返回 `false`。

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
