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
- 配置允许携带业务字段之外的附加字段（负责人、备注、标签等），位置与取值范围见下文“附加信息”；它们只随配置通过格式校验，不参与求值。
- 字符串中的 `\uXXXX` 转义必须组成完整字符：高代理项只能与紧接着的低代理项配对，孤立的高/低代理项或不完整、顺序错误的配对（无论出现在字段名还是字符串值中）都会使读取失败；真正的 `�`（U+FFFD）仍是合法字符串。
- 两个文件会先完整校验（含未选中或已关闭的开关）；任何参数/文件/JSON/字段/唯一性错误都以非零状态退出，标准输出为空，标准错误指出具体字段与位置。

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

### 附加信息：负责人、备注与标签

配置中除了本页前面列出的业务字段外，可以在任何对象里再加自己的字段，用来记录负责人、备注、标签、工单号等。这些附加字段可以出现在：

- 配置顶层（与 `flags` 并列）；
- 开关对象中（与 `key`/`enabled`/`default`/`rules` 并列）；
- 规则对象中（与 `id`/`value`/`conditions` 并列）；
- 条件对象中（与 `attribute`/`op`/`value` 并列）。

字段值可以是任意合法的 JSON 值：字符串、数字、布尔值、`null`、数组或对象，数组与对象内部也能继续嵌套任意层级。下面这份 `config-meta.json` 在四个层级都放了附加信息（含一个嵌套对象 `review` 和数组 `tags`）：

```json
{
  "owner": "payments-platform",
  "note": "checkout rollout config",
  "flags": [
    {
      "key": "new-checkout",
      "enabled": true,
      "default": true,
      "owner": "alice@example.com",
      "tags": ["checkout", "rollout"],
      "rules": [
        {
          "id": "r-block-cn-mobile",
          "value": false,
          "owner": "bob@example.com",
          "conditions": [
            {"attribute": "region", "op": "eq", "value": "cn", "note": "mainland China"},
            {"attribute": "platform", "op": "in", "value": ["ios", "android"], "review": {"by": "carol", "ticket": "PAY-1423"}}
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

上下文仍只包含求值要用到的属性，不含任何备注：

```json
{"region": "cn", "platform": "ios", "plan": "pro"}
```

```bash
go run ./cmd/payflow evaluate config-meta.json new-checkout context.json
```

```json
{"key":"new-checkout","value":false,"reason":"rule","ruleId":"r-block-cn-mobile"}
```

输出与不带附加字段时完全相同。附加字段不会成为上下文属性，也不改变规则选择、布尔结果、命中原因或规则编号，结果对象也不会把这些内容带回来——上面的输出里没有 `owner`、`tags`、`note`、`review`。因此把 `owner` 从 `"alice@example.com"` 改成别人、或只修改顶层 `note` 的措辞，只要业务字段（`key`/`enabled`/`default`/`rules` 与各条件）保持不变，求值输出就逐字节一致。规则优先级仍只取决于规则在数组中的次序，与附加字段无关。

### 附加信息也要通过整份配置的格式校验

附加字段在语义上被忽略，但它们仍是 JSON 文档的一部分，必须与业务字段一起通过整份配置的格式校验。校验对所有对象一视同仁，包括附加字段内部嵌套的对象：同一对象内重复声明同名字段仍会被拒绝。例如把上面 `r-block-cn-mobile` 第二个条件里的 `review` 写成同层两个 `by`：

```json
"review": {"by": "carol", "by": "dave", "ticket": "PAY-1423"}
```

```bash
go run ./cmd/payflow evaluate config-meta-dup.json new-checkout context.json
```

命令以非零状态退出，标准输出为空，标准错误指出重复原因与具体位置：

```
config.flags[0].rules[0].conditions[1].review.by: duplicate field "by"
```

位置一路定位到嵌套附加对象内的重复字段（用 `go run` 运行时标准错误还会追加一行 `exit status 1`；程序本身只输出上面那一行）。注意限制的是“同一个对象内”：不同对象分别使用同名字段是合法的——顶层、开关、规则、条件各自放一个 `note`，或两个条件各放一个 `note`，互不冲突。

这一检查覆盖整份配置，与该开关是否被选中、是否启用无关。即使重复字段位于未被请求的开关、或已关闭的开关内，也得不到正常求值结果。例如把重复字段放到已关闭的 `legacy-report` 上，再去求值合法的 `new-checkout`：

```json
{
  "key": "legacy-report",
  "enabled": false,
  "default": true,
  "review": {"by": "x", "by": "y"},
  "rules": []
}
```

```bash
go run ./cmd/payflow evaluate config-meta-dup-disabled.json new-checkout context.json
```

仍以非零状态退出、标准输出为空，标准错误为：

```
config.flags[1].review.by: duplicate field "by"
```

数字只要 JSON 语法合法就可以作为附加信息，包括超出浮点表示范围的大数字。例如顶层放一个 `1e400` 的配置可以正常求值，它只是被当作附加信息保留、不参与判断（上下文用空对象 `{}`）：

```json
{
  "weight": 1e400,
  "flags": [
    {"key": "f", "enabled": true, "default": false, "rules": []}
  ]
}
```

```json
{"key":"f","value":false,"reason":"default","ruleId":null}
```

但语法本身不合法的数字不是“数字过大”，而是 JSON 语法错误。把指数数字写漏（`1e+` 中 `+` 之后没有数字）：

```json
{
  "weight": 1e+,
  "flags": [
    {"key": "f", "enabled": true, "default": false, "rules": []}
  ]
}
```

```bash
go run ./cmd/payflow evaluate config-meta-badnum.json f context-empty.json
```

以非零状态退出、标准输出为空，标准错误报告的是 JSON 语法错误而非“数字过大”：

```
config: invalid JSON: invalid character ',' in exponent of numeric literal
```

### 附加信息的边界

附加字段只承载说明性信息，不能替代缺失或类型错误的业务字段：

- 业务字段的必填与唯一性要求原样适用。例如开关缺少 `default`，即使旁边放了 `"owner": "alice@example.com"` 等附加字段，仍报 `config.flags[0].default: field is required`；规则 id 重复、开关键重复等唯一性检查也照常执行。
- 业务字段放错类型不会因为“另有附加信息”而被接受（如 `enabled` 写成字符串、`eq` 的 `value` 写成数组）。
- 上下文仍是“属性名到字符串”的对象，可以为 `{}`，但每个属性的值都必须是字符串。配置里的嵌套备注不能照搬到上下文：`{"plan":"pro","review":{"by":"carol"}}` 会报 `context.review: value must be a string`。配置中的附加信息只存在于配置文件里，求值时不会进入上下文。

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
