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
- 开关键在配置中按 JSON 解码后的文字登记：直接 UTF-8 字符与合法 `\uXXXX` 转义（含成对代理项）是同一名字的不同写法；命令行的开关键参数则按传入的字符串原样精确查找，不会再被当作 JSON 解码。大小写、首尾空白、组合字符与预组合字符都不做归一化，详见下文“开关键的写法：直接字符、`\uXXXX` 转义与精确查找”。
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

### 开关键的写法：直接字符、`\uXXXX` 转义与精确查找

前面的示例都用普通英文开关键（如 `new-checkout`）。开关键也可以包含任意 Unicode 文字，例如重音字母和表情。这里有两层约定，先各记一句：

1. **配置是 JSON 文本**：开关键写在 JSON 字符串里，既可以直接写 UTF-8 字符，也可以写合法的 `\uXXXX` 转义（超出基本多文种平面的字符，如表情，用一对高/低代理项表示），还可以在同一个名字里混合书写。配置读入时这些转义会被解码，三种写法登记的是**同一个名字**。
2. **命令行参数不是 JSON**：`evaluate` 的第二个参数按传入的字符串原样使用，不会把其中的 `\uXXXX` 当 JSON 再解码一次。要选中某个开关，参数必须是该名字**解码后的真实文字**。

#### 三种 JSON 写法选中同一个开关

下面三份配置只有开关键与规则 id 的 JSON 书写形式不同，业务内容完全一致。开关真实名字是 `café🚀`（`é` 为 U+00E9，`🚀` 为 U+1F680），规则真实 id 是 `r-é🚀`。

`config-key-direct.json`（直接字符）：

```json
{
  "flags": [
    {
      "key": "café🚀",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "r-é🚀",
          "value": true,
          "conditions": [
            {"attribute": "plan", "op": "eq", "value": "pro"}
          ]
        }
      ]
    }
  ]
}
```

`config-key-escaped.json`（全部用 `\uXXXX` 转义，表情用成对代理项 `\uD83D\uDE80`）：

```json
{
  "flags": [
    {
      "key": "caf\u00E9\uD83D\uDE80",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "r-\u00E9\uD83D\uDE80",
          "value": true,
          "conditions": [
            {"attribute": "plan", "op": "eq", "value": "pro"}
          ]
        }
      ]
    }
  ]
}
```

`config-key-mixed.json`（同一名字里直接字符与转义混用）：

```json
{
  "flags": [
    {
      "key": "caf\u00E9🚀",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "r-é\uD83D\uDE80",
          "value": true,
          "conditions": [
            {"attribute": "plan", "op": "eq", "value": "pro"}
          ]
        }
      ]
    }
  ]
}
```

`context.json`：

```json
{"plan": "pro"}
```

对三份配置分别用**解码后的真实文字** `café🚀` 作为参数（终端能直接输入 UTF-8 就直接写；bash 也可用 ANSI-C 引用生成，如 `$'café\U0001F680'`）：

```bash
go run ./cmd/payflow evaluate config-key-direct.json  "café🚀" context.json
go run ./cmd/payflow evaluate config-key-escaped.json "café🚀" context.json
go run ./cmd/payflow evaluate config-key-mixed.json   "café🚀" context.json
```

三次输出逐字节相同——`key`、布尔值、`reason`、`ruleId` 全都一样，结果里的名称是解码后的真实文字，而不是配置中的转义写法：

```json
{"key":"café🚀","value":true,"reason":"rule","ruleId":"r-é🚀"}
```

只改名称在 JSON 里的书写形式（直接字符 ↔ 合法转义 ↔ 混合）不会改变求值结果。规则 id 遵循同一套解码规则，所以三份配置命中的都是同一个 `r-é🚀`。

#### 命令行参数按传入的字符串查找，不会再次解码

参数不经过 JSON 解码，因此把反斜杠文字原样传进去，找到的是“名字里真的含反斜杠与字母 u 的普通文本”的开关；它不会被翻译成 `é`。例如对着上面的 `config-key-escaped.json` 传入字面文本 `caf\u00E9\uD83D\uDE80`（21 个普通 ASCII 字符，单引号可阻止 shell 处理反斜杠）：

```bash
go run ./cmd/payflow evaluate config-key-escaped.json 'caf\u00E9\uD83D\uDE80' context.json
```

该配置解码后只有 `café🚀`，没有名字为 `caf\u00E9\uD83D\uDE80` 的开关，所以非零退出、标准输出为空，标准错误报告未找到（用 `go run` 运行时还会追加一行 `exit status 1`）：

```
evaluate: flag key "caf\\u00E9\\uD83D\\uDE80" not found in config "config-key-escaped.json"
```

错误信息里的 `\\` 只是错误格式对反斜杠的显示转义，不代表名字被再次解码。**结论：配置里可以用 `\uXXXX` 写法，但命令参数一律传解码后的真实文字。**

#### 重名按解码后的文字判定，整份配置会被拒绝

开关键是否重复，也按 JSON 解码后的文字判断。下面这份 `config-key-dup.json` 中，第三个开关用全转义写法声明了与第二个开关相同的名字 `café🚀`；这两个冲突开关都处于关闭状态，配置里另有一个完全合法且本次请求的开关 `good`：

```json
{
  "flags": [
    {"key": "good", "enabled": true, "default": true, "rules": []},
    {"key": "café🚀", "enabled": false, "default": false, "rules": []},
    {"key": "caf\u00E9\uD83D\uDE80", "enabled": false, "default": false, "rules": []}
  ]
}
```

即使请求的是另一个合法开关 `good`，整份配置仍被拒绝——非零退出、标准输出为空，错误指出后一个开关键的位置以及第一次声明的位置：

```
$ go run ./cmd/payflow evaluate config-key-dup.json good context.json
config.flags[2].key: duplicate flag key "café🚀" (first at flags[1])
exit status 1
```

这与前文“关闭的开关与配置校验”一致：所有开关（含未选中、已关闭者）都先完成校验，名称冲突不因为这次不选它就被放过；改选任何一个合法开关都拿不到结果，只有消除解码后同名的声明，配置才可用。（同一开关内规则 id 的唯一性按同样方式判定：直接字符与合法转义写出的相同 id 也算重复，错误形如 `config.flags[i].rules[j].id: duplicate rule id ... (first at rules[...])`；不同开关各自使用相同的规则 id 则不冲突。）

#### 精确查找的边界：大小写、空白与组合字符都不归一化

查找是逐字符精确匹配，不合并大小写、不裁剪首尾空白，也不做 Unicode 规范化。下面这份 `config-names.json` 放了五个外观相近、实际不同的名字，并用各自的 `default` 标记究竟选中了谁：

```json
{
  "flags": [
    {"key": "feature-a", "enabled": true, "default": false, "rules": []},
    {"key": "Feature-A", "enabled": true, "default": true, "rules": []},
    {"key": " feature-a", "enabled": true, "default": true, "rules": []},
    {"key": "café", "enabled": true, "default": false, "rules": []},
    {"key": "cafe\u0301", "enabled": true, "default": true, "rules": []}
  ]
}
```

第四个开关键的 `é` 是预组合字符 U+00E9；第五个写成 `e` 后接组合重音符 U+0301（JSON 即 `cafe\u0301`）——两者屏幕上都显示为 `café`，但字符序列不同，是两个独立开关。用空上下文 `context-empty.json`（内容就是 `{}`）分别精确请求：

```
$ go run ./cmd/payflow evaluate config-names.json "feature-a" context-empty.json
{"key":"feature-a","value":false,"reason":"default","ruleId":null}
$ go run ./cmd/payflow evaluate config-names.json "Feature-A" context-empty.json
{"key":"Feature-A","value":true,"reason":"default","ruleId":null}
$ go run ./cmd/payflow evaluate config-names.json " feature-a" context-empty.json
{"key":" feature-a","value":true,"reason":"default","ruleId":null}
$ go run ./cmd/payflow evaluate config-names.json "café" context-empty.json
{"key":"café","value":false,"reason":"default","ruleId":null}
$ go run ./cmd/payflow evaluate config-names.json $'cafe\u0301' context-empty.json
{"key":"café","value":true,"reason":"default","ruleId":null}
```

大小写（`feature-a` 与 `Feature-A`）、前导空白（` feature-a`）、组合字符与预组合字符（两个 `café`）都各自独立、互不顶替；两个 `café` 的 `value` 一假一真，正说明选中的是不同开关。反过来，请求只是相似、并不存在的精确名称时仍报告未找到（非零退出、标准输出为空），不会回退到近似名称：

```
$ go run ./cmd/payflow evaluate config-names.json "FEATURE-A" context-empty.json
evaluate: flag key "FEATURE-A" not found in config "config-names.json"
$ go run ./cmd/payflow evaluate config-names.json "cafe" context-empty.json
evaluate: flag key "cafe" not found in config "config-names.json"
$ go run ./cmd/payflow evaluate config-names.json "feature-a " context-empty.json
evaluate: flag key "feature-a " not found in config "config-names.json"
```

（终端不便输入组合字符时，可用 bash 的 ANSI-C 引用 `$'cafe\u0301'` 生成解码后的文字；注意这是 shell 的引号机制，不是程序在解码。以上错误行之后同样会由 `go run` 追加 `exit status 1`。）

#### 字面 `\uXXXX` 文本本身也可以是合法名称

JSON 中反斜杠本身需要转义，因此 `"caf\\u00E9"` 解码后得到的是普通文本 `café`（c、a、f、反斜杠、u、0、0、E、9 共 9 个字符），它与真正的 `café` 是两个不同的开关键。`config-key-literal.json` 让两者并存：

```json
{
  "flags": [
    {
      "key": "caf\\u00E9",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "r-\\u00E9",
          "value": true,
          "conditions": [
            {"attribute": "plan", "op": "eq", "value": "pro"}
          ]
        }
      ]
    },
    {"key": "café", "enabled": true, "default": true, "rules": []}
  ]
}
```

配置合法本身就说明两个名字不冲突。请求时同样传解码后的文字：传 `café` 选中第二个开关（无规则命中，取默认值，`ruleId` 为 null）；传单引号包裹的字面文本 `'caf\u00E9'` 选中第一个开关，结果中的 `key`/`ruleId` 是原样保留的反斜杠文字：

```
$ go run ./cmd/payflow evaluate config-key-literal.json "café" context.json
{"key":"café","value":true,"reason":"default","ruleId":null}
$ go run ./cmd/payflow evaluate config-key-literal.json 'caf\u00E9' context.json
{"key":"caf\\u00E9","value":true,"reason":"rule","ruleId":"r-\\u00E9"}
```

输出中的 `\\` 是 JSON 对结果字符串里那个反斜杠的标准转义，对应的名字就是 9 个字符的 `café`。

小结：**配置端**按 JSON 解码后的文字登记开关键——直接字符、合法 `\uXXXX` 转义、成对代理项与混合书写彼此等价，而 `\\uXXXX` 是字面反斜杠文本；**命令端**按传入字符串原样精确查找，不做 JSON 解码，也不做大小写、空白或规范化合并；**唯一性**按解码后的文字在各自范围内判定，冲突使整份配置被拒绝，与该开关这次是否被选中、是否启用无关；找不到精确名称时以非零退出、标准输出为空，并在标准错误报告 `flag key ... not found`。

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
