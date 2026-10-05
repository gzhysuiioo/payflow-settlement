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
- 配置允许在顶层以及开关、规则、条件对象中携带业务字段之外的附加字段（如负责人、备注、标签）：值可以是字符串、数字、布尔、`null`、数组或对象并继续嵌套，语法合法的大数字（如 `1e400`）也算可接受的附加信息；它们不成为上下文属性、不参与规则判断，也不会出现在结果对象中，详见下文“配置中的附加信息”。
- 上下文为属性名到字符串的对象，可为 `{}`，空字符串是合法值；属性缺失即条件不成立。比较区分大小写、不裁剪空白。
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

### 配置中的附加信息（负责人、备注、标签）

维护配置时可以直接在配置里记录负责人、备注、标签等治理信息，无需另开文件。业务字段（顶层 `flags`，开关的 `key`/`enabled`/`default`/`rules`，规则的 `id`/`value`/`conditions`，条件的 `attribute`/`op`/`value`）之外的任何附加字段都会被原样接受并忽略，具体规则如下：

- 附加字段可以放在四个位置：配置顶层对象、开关对象、规则对象、条件对象。
- 字段值可以是任意合法 JSON 值：字符串、数字、布尔值、`null`、数组或对象；数组与对象内部还能继续嵌套任意深度。
- 只要 JSON 语法合法，大数字也算可接受的附加信息——例如 `1e400`（超出浮点表示范围）不会被拒绝，它只是一个不参与求值的数字。
- 附加字段**不是**上下文属性：求值时既不会把它们并入上下文，也不会影响规则选择、布尔结果、命中原因（`reason`）或命中规则编号（`ruleId`）；成功输出只有 `key`/`value`/`reason`/`ruleId` 四个字段，任何附加信息都不会被带回。

把下面这份配置保存为 `config-meta.json`——它在顶层、开关、规则和条件上分别放了附加字段，规则的 `review` 是一个嵌套对象（内部还有 `approvers` 数组），开关上的 `rolloutWeight` 是 `1e400`：

```json
{
  "owner": "payments-platform",
  "schemaVersion": 2,
  "flags": [
    {
      "key": "new-checkout",
      "enabled": true,
      "default": false,
      "owner": "@alice",
      "labels": ["checkout", "gradual-rollout"],
      "rolloutWeight": 1e400,
      "rules": [
        {
          "id": "r-pro",
          "value": true,
          "note": "付费计划开放新结算页",
          "review": {
            "required": true,
            "approvers": ["@alice", "@bob"]
          },
          "conditions": [
            {
              "attribute": "plan",
              "op": "eq",
              "value": "pro",
              "rationale": null
            }
          ]
        }
      ]
    }
  ]
}
```

`context-meta.json` 只包含业务属性，不要把配置里的备注照搬进来：

```json
{"plan": "pro"}
```

```bash
go run ./cmd/payflow evaluate config-meta.json new-checkout context-meta.json
```

输出与没有任何附加字段时完全相同：

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro"}
```

只改附加信息、不动任何业务字段时，求值输出保持一致。例如负责人从 `@alice` 交接给 `@carol`：顶层 `owner` 改为 `"growth-platform"`，开关 `owner` 改为 `"@carol"`，`labels` 换成 `["checkout", "emergency-rollback"]`，规则 `note`、`review.approvers` 和条件 `rationale` 一并改写——但 `key`/`enabled`/`default`、规则的 `id`/`value`、条件的 `attribute`/`op`/`value` 全部不变。用同一份 `context-meta.json` 重新求值，输出仍是：

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro"}
```

因此可以放心地在配置里维护负责人、备注、标签、审阅记录等：它们是给人和审计流程看的，求值器对它们视而不见。

#### 附加信息也要通过整份配置的格式校验

“被忽略”不等于“不检查”。附加字段与业务字段共用同一份 JSON 文档格式校验：同一对象内重复声明同名字段、非法 UTF-8、不成对的代理项转义、JSON 语法错误或第二个顶层值，出现在任何位置（包括附加字段的深层嵌套中）都会让整份配置被拒绝。校验覆盖所有开关，问题即使位于未被选中或已关闭的开关中，也得不到正常求值结果。

例如下面这份配置——请求的 `new-checkout` 合法，但同一配置中已关闭、且本次根本不会被选中的 `legacy-report` 在其嵌套附加对象 `meta` 里把 `owner` 写了两遍：

```json
{
  "flags": [
    {
      "key": "new-checkout",
      "enabled": true,
      "default": false,
      "rules": [
        {"id": "r-pro", "value": true, "conditions": [
          {"attribute": "plan", "op": "eq", "value": "pro"}
        ]}
      ]
    },
    {
      "key": "legacy-report",
      "enabled": false,
      "default": true,
      "meta": {"owner": "@alice", "owner": "@bob"},
      "rules": []
    }
  ]
}
```

```bash
go run ./cmd/payflow evaluate config-meta-dup.json new-checkout context-meta.json
```

命令以非零状态退出，标准输出为空，标准错误同时给出重复原因与具体位置（用 `go run` 运行时还会在标准错误追加一行 `exit status 1`；程序本身只输出下面那一行）：

```
config.flags[1].meta.owner: duplicate field "owner"
```

重复判定只针对**同一个对象**：不同对象各自使用同名字段完全合法——顶层、开关、规则、条件分别放一个 `"owner"` 不会冲突，同一规则的条件数组里多个条件各自携带同名字段也可以，只有在一个 `{}` 内部把同一个名字写两次才是错误。

数字方面也要区分“大”与“语法错误”：`1e400` 是语法合法的数字（如上面的完整示例所示，配置正常通过），而缺少指数数字的 `1e+` 根本不是合法 JSON 数字。例如：

```json
{"weight": 1e+, "flags": []}
```

它报告的是 JSON 语法错误，而不是笼统的“数字过大”：

```
config: invalid JSON: invalid character ',' in exponent of numeric literal
```

#### 附加字段的边界

- 附加字段不能替代缺少或类型错误的业务字段：开关仍必须提供非空 `key`、布尔 `enabled`/`default` 和 `rules`，规则仍必须提供非空 `id`、布尔 `value` 和非空 `conditions`，条件仍必须提供非空 `attribute`、`op` 与类型正确的 `value`；开关键与规则 id 在各自范围内的唯一性要求也继续适用。例如 `"default": false` 不能省，多写一个 `"owner": "@alice"` 不弥补缺失。
- 上下文仍然只是“属性名到字符串”的对象：每个顶层属性的值必须是字符串，`{}` 合法、空字符串合法，但对象、数组、数字等一律按类型错误拒绝。配置附加字段里的嵌套备注不能照搬到上下文——上下文里写 `"plan": {"name": "pro"}` 会得到 `context.plan: value must be a string`。

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
