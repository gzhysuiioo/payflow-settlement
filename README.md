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
两个文件均为 UTF-8 JSON：

```bash
go run ./cmd/payflow evaluate config.json new-checkout context.json
```

成功时标准输出仅包含一个对象：

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro"}
```

- `reason`：`disabled`（开关关闭，固定 false）、`rule`（命中规则，`ruleId` 为该规则 id）、`default`（无规则命中，取 default，`ruleId` 为 null）。
- 配置顶层为 `{"flags":[...]}`；每个开关含非空 `key`、布尔 `enabled`/`default`、`rules`；规则含非空 `id`、布尔 `value` 与非空 `conditions`；条件含非空 `attribute`、`op`（`eq`/`in`）和 `value`（eq 为字符串，in 为非空字符串数组）。
- 上下文为属性名到字符串的对象，可为 `{}`，空字符串是合法值；属性缺失即条件不成立。比较区分大小写、不裁剪空白。
- 字符串中的 `\uXXXX` 转义必须组成完整字符：高代理项只能与紧接着的低代理项配对，孤立的高/低代理项或不完整、顺序错误的配对（无论出现在字段名还是字符串值中）都会使读取失败；真正的 `�`（U+FFFD）仍是合法字符串。
- 两个文件会先完整校验（含未选中或已关闭的开关）；任何参数/文件/JSON/字段/唯一性错误都以非零状态退出，标准输出为空，标准错误指出具体字段与位置。

### 完整示例：两条规则同时成立时如何决定结果

把下面内容原样保存为 UTF-8 的 `config.json`。开关 `new-checkout` 有两条 id 不同、
返回值相反的规则：`r-eu-pro-block` 同时用 `eq` 检查 `plan`、用 `in` 检查 `region`，
两个条件都成立才命中，命中返回 `false`；`r-pro-allow` 只检查 `plan`，命中返回 `true`：

```json
{
  "flags": [
    {
      "key": "new-checkout",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "r-eu-pro-block",
          "value": false,
          "conditions": [
            { "attribute": "plan", "op": "eq", "value": "pro" },
            { "attribute": "region", "op": "in", "value": ["eu-west", "eu-central"] }
          ]
        },
        {
          "id": "r-pro-allow",
          "value": true,
          "conditions": [
            { "attribute": "plan", "op": "eq", "value": "pro" }
          ]
        }
      ]
    }
  ]
}
```

再保存 `context.json`，这个上下文同时满足两条规则：

```json
{ "plan": "pro", "region": "eu-west" }
```

运行 `go run ./cmd/payflow evaluate config.json new-checkout context.json`，输出：

```json
{"key":"new-checkout","value":false,"reason":"rule","ruleId":"r-eu-pro-block"}
```

规则按配置数组中的次序逐个检查，首条所有条件都成立的规则立即决定结果。这里
`r-eu-pro-block` 排在前面且命中，结果取它的 `false`；命中返回 `false` 的规则同样
立即生效，不会继续寻找后面返回 `true` 的 `r-pro-allow`。要交换结果，只需交换两条
规则在数组中的位置。

### 属性缺失：不等于空字符串，部分条件成立不算命中

沿用上面的 `config.json`，改用缺少 `region` 的上下文：

```json
{ "plan": "pro" }
```

输出：

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro-allow"}
```

`region` 缺失时它的 `in` 条件不成立——缺失不会当成空字符串参与比较，只有 `plan`
一个条件成立的 `r-eu-pro-block` 不能获胜；求值继续考虑后面的规则，于是命中
`r-pro-allow`。反过来，如果上下文里写 `"region": ""`，空字符串本身是合法属性值，
会正常参与比较，只是不等于列表中的任何一项。

### 没有规则命中：取 default，ruleId 为 null

沿用同一 `config.json`，上下文改为：

```json
{ "plan": "free", "region": "eu-west" }
```

输出：

```json
{"key":"new-checkout","value":false,"reason":"default","ruleId":null}
```

两条规则都不命中，结果取开关的 `default`（本例为 `false`），`reason` 为 `default`，
`ruleId` 为 `null`。注意这个 `false` 与第一个示例的 `false` 来源不同：前者是命中
规则，这里是默认值，区分依据是 `reason` 与 `ruleId`。

### 已关闭的开关：固定 false，不看默认值与规则

`enabled` 为 `false` 的合法开关无论 `default` 和规则如何都固定返回 `false`。例如
把配置改为（`default` 为 `true`、规则本来也命中）：

```json
{
  "flags": [
    {
      "key": "new-checkout",
      "enabled": false,
      "default": true,
      "rules": [
        {
          "id": "r-pro-allow",
          "value": true,
          "conditions": [
            { "attribute": "plan", "op": "eq", "value": "pro" }
          ]
        }
      ]
    }
  ]
}
```

对 `{ "plan": "pro", "region": "eu-west" }` 求值仍输出：

```json
{"key":"new-checkout","value":false,"reason":"disabled","ruleId":null}
```

### 配置错误：未选中的开关也须通过校验

两个文件都先完整校验通过后才求值，选择哪个开关——包括已关闭的开关——都不会绕过
这一步。例如下面的配置中，请求的 `new-checkout` 本身合法且已关闭，但同一配置里的
`beta-dashboard` 缺少必填的 `default` 字段：

```json
{
  "flags": [
    {
      "key": "new-checkout",
      "enabled": false,
      "default": false,
      "rules": []
    },
    {
      "key": "beta-dashboard",
      "enabled": true,
      "rules": []
    }
  ]
}
```

运行 `go run ./cmd/payflow evaluate config.json new-checkout context.json` 仍以非零
状态退出，标准输出为空，标准错误指出问题字段与位置：

```text
config.flags[1].default: field is required
```

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
