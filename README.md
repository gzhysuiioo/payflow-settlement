# 链上支付与结算编排平台

## 用途

账户抽象、付款方与代付策略、批量结算、幂等与对账、失败补偿、手续费与限额治理、可审计流水。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `payflow/`，命令入口位于 `cmd/payflow/`。

```bash
go run ./cmd/payflow demo
go run ./cmd/payflow version
go run ./cmd/payflow evaluate config.json <flag-key> context.json
go run ./cmd/payflow evaluate config.json <flag-key> context.json --explain
go test ./...
```

## 离线特性开关求值

`evaluate` 在本机离线对单个特性开关求值，依次传入配置文件路径、开关键与上下文文件路径，
两个文件均为 UTF-8 JSON。命令行用法见本页各小节；在自己的 Go 程序里直接调用同一套离线
求值（解析、普通求值、解释求值、结果序列化与错误处理）见文末“在 Go 程序中直接调用”。
成功时标准输出仅包含一个对象：

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro"}
```

- `reason`：`disabled`（开关关闭，固定 false）、`rule`（命中规则，`ruleId` 为该规则 id）、`default`（无规则命中，取 default，`ruleId` 为 null）。
- 配置顶层为 `{"flags":[...]}`；每个开关含非空 `key`、布尔 `enabled`/`default`、`rules`；规则含非空 `id`、布尔 `value` 与非空 `conditions`；条件含非空 `attribute`、`op`（`eq`/`in`）和 `value`（eq 为字符串，in 为非空字符串数组）。
- 配置允许在顶层以及开关、规则、条件对象中携带业务字段之外的附加字段（如负责人、备注、标签）：值可以是字符串、数字、布尔、`null`、数组或对象并继续嵌套，语法合法的大数字（如 `1e400`）也算可接受的附加信息；它们不成为上下文属性、不参与规则判断，也不会出现在结果对象中，详见下文“配置中的附加信息”。
- 上下文为属性名到字符串的对象，可为 `{}`，空字符串是合法值；属性缺失即条件不成立。比较区分大小写、不裁剪空白。
- 字符串中的 `\uXXXX` 转义必须组成完整字符：高代理项只能与紧接着的低代理项配对，孤立的高/低代理项或不完整、顺序错误的配对（无论出现在字段名还是字符串值中）都会使读取失败；真正的 `�`（U+FFFD）仍是合法字符串。
- 开关键按 JSON 解码后的文字精确查找：配置中同一名称的直接字符写法与合法 `\uXXXX` 转义写法等价，命令行参数则按传入的字面字符串查找、不会再次解码；大小写、首尾空白与组合字符序列都不自动合并。详见下文“开关键的写法与精确查找”。
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

### 解释模式：`--explain`

想知道结果究竟采用了哪条规则、或为什么落到默认值，可以在前三个参数之后再加第四个参数 `--explain`。它只在**第四个参数位置**被识别：

```bash
go run ./cmd/payflow evaluate config.json new-checkout context-missing.json --explain
```

成功时标准输出仍只有一个 JSON 对象：前四个字段 `key`/`value`/`reason`/`ruleId` 与不带选项时逐字一致，其后追加一个 `explanation` 对象。沿用上文缺少 `region` 的上下文（`{"platform":"ios","plan":"pro"}`）：

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro","explanation":{"outcome":"first matching rule \"r-pro\" decides the result; later rules are not considered","rules":[{"ruleId":"r-block-cn-mobile","match":false,"conditions":[{"attribute":"region","op":"eq","compareValue":"cn","actualValue":null,"missing":true,"match":false},{"attribute":"platform","op":"in","compareValue":["ios","android"],"actualValue":"ios","missing":false,"match":true}]},{"ruleId":"r-pro","match":true,"conditions":[{"attribute":"plan","op":"eq","compareValue":"pro","actualValue":"pro","missing":false,"match":true}]}]}}
```

`explanation` 的内容：

- `outcome`：一句话说明最终结果的依据——由首条命中规则定案、没有规则命中而采用默认值、还是开关关闭固定返回 `false`。
- `rules`：**按配置数组顺序**列出实际考虑过的规则，每条给出 `ruleId`、规则是否整体命中（`match`）以及该规则**全部条件**的逐条判断。即使规则已因前一个条件不成立而确定不命中，后面的条件也照样给出判断，不会只列到第一个失败条件。
- 每个条件记录五要素：`attribute`（属性名）、`op`（比较方式 `eq`/`in`）、`compareValue`（配置中的比较值：eq 为字符串、in 为字符串数组）、`actualValue`（上下文实际值）、`missing`（属性是否缺失）与 `match`（该条件是否成立）。据此可以区分三种不成立：
  - **属性缺失**：`missing` 为 `true`、`actualValue` 为 `null`；
  - **值不相等**（eq）：`missing` 为 `false`、`actualValue` 是实际字符串但 `match` 为 `false`；
  - **不在候选列表**（in）：`missing` 为 `false`、`actualValue` 有值但不等于 `compareValue` 列表中的任何成员。
- 属性缺失与显式空字符串严格区分：上下文写 `"region":""` 时 `actualValue` 是 `""`、`missing` 为 `false`；只有完全没有这个属性时才是 `actualValue:null`、`missing:true`。
- 所有字符串都按 JSON 解码后的内容展示：in 列表中的空字符串、重复成员与先后次序原样保留（如 `["","a","a"]`），不裁剪空白、不合并大小写。

解释与最终结果严格对应：规则按配置次序逐条判断，**第一条全部条件成立的规则决定结果并立即停止**——即使它返回 `false`，其后的规则也不会出现在 `rules` 里；在它之前未命中的规则仍保留。上文 `r-block-cn-mobile` 返回 `false`，当上下文同时满足两条规则（`region=cn`、`platform=android`、`plan=pro`）时，解释里只有 `r-block-cn-mobile` 一条，且结果为 `false`：

```json
{"key":"new-checkout","value":false,"reason":"rule","ruleId":"r-block-cn-mobile","explanation":{"outcome":"first matching rule \"r-block-cn-mobile\" decides the result; later rules are not considered","rules":[{"ruleId":"r-block-cn-mobile","match":true,"conditions":[{"attribute":"region","op":"eq","compareValue":"cn","actualValue":"cn","missing":false,"match":true},{"attribute":"platform","op":"in","compareValue":["ios","android"],"actualValue":"android","missing":false,"match":true}]}]}}
```

所有规则都未命中时，`rules` 会列出这些规则及其条件判断，`reason` 为 `default`，`outcome` 说明采用了开关的默认值（上下文 `{"region":"us","platform":"web","plan":"free"}`）：

```json
{"key":"new-checkout","value":true,"reason":"default","ruleId":null,"explanation":{"outcome":"no rule matched; the flag default is used","rules":[{"ruleId":"r-block-cn-mobile","match":false,"conditions":[{"attribute":"region","op":"eq","compareValue":"cn","actualValue":"us","missing":false,"match":false},{"attribute":"platform","op":"in","compareValue":["ios","android"],"actualValue":"web","missing":false,"match":false}]},{"ruleId":"r-pro","match":false,"conditions":[{"attribute":"plan","op":"eq","compareValue":"pro","actualValue":"free","missing":false,"match":false}]}]}}
```

开关关闭时固定返回 `false`，解释只说明“关闭导致固定 `false`”，`rules` 为空列表——即使上下文本会命中某条规则，也不会把默认值或那条可能命中的规则写成结果依据。对上文的 `legacy-report` 求值：

```bash
go run ./cmd/payflow evaluate config.json legacy-report context.json --explain
```

```json
{"key":"legacy-report","value":false,"reason":"disabled","ruleId":null,"explanation":{"outcome":"flag is disabled; result is fixed to false without evaluating rules or the default","rules":[]}}
```

规则列表为空的开关（`"rules":[]`）返回默认结果，解释中的 `rules` 同样为空数组。

参数规则：

- 只有 `evaluate <config.json> <flag-key> <context.json>` 三个参数时是普通模式，输出仍是既有的四字段 JSON 对象，不含 `explanation`。
- `--explain` 只在第四个参数位置生效；第四个参数写成其他任何内容（如 `--verbose`）、或给出第五个参数，都作为参数错误以非零状态退出，标准输出为空，原因写入标准错误。
- 前三个参数位置中出现字面文本 `--explain` 时，仍分别按配置路径、开关键、上下文路径处理，不会被误当成选项——例如把开关键就命名为 `--explain` 时，会按该字面键去配置中查找（找不到则报未找到）。
- 带不带 `--explain` 都先完整校验整份配置与上下文（含未选中或已关闭的开关）；任何参数、文件或校验失败都非零退出、标准输出为空、原因写入标准错误，不会输出半份解释。

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

### 开关键的写法与精确查找

`evaluate` 的第二个参数是开关键，配置中的 `key` 与它按字符串精确相等匹配。这里要分清两层：配置文件是 JSON，JSON 字符串里既可以直接书写字符，也可以用合法的 `\uXXXX` 转义书写同一个字符（BMP 之外的字符用一对相邻的高/低代理项转义）——这些只是同一文字的不同 JSON 写法，读取时先解码成真实文字，再参与唯一性判断与查找；命令行参数则不是 JSON，它按传入的字面字符串查找，反斜杠就是普通字符，不会被再当作转义解码一次。

#### 直接字符、合法转义与混合写法指向同一个开关

以开关键 `café🎉` 为例（c、a、f、é＝U+00E9、🎉＝U+1F389；🎉 在 `\uXXXX` 写法中需要代理项对 `\ud83c\udf89`）。下面三份配置只有 `key` 的 JSON 写法不同，解码后都是同一个名称。上下文统一保存为 `context-pro.json`：

```json
{"plan": "pro"}
```

`config-direct.json`（直接字符）：

```json
{"flags":[{"key":"café🎉","enabled":true,"default":false,"rules":[{"id":"r-pro","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}
```

`config-escape.json`（全部用合法 `\uXXXX` 转义）：

```json
{"flags":[{"key":"caf\u00e9\ud83c\udf89","enabled":true,"default":false,"rules":[{"id":"r-pro","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}
```

`config-mixed.json`（直接字符与转义混合书写，é 直接写出、🎉 用代理项对）：

```json
{"flags":[{"key":"café\ud83c\udf89","enabled":true,"default":false,"rules":[{"id":"r-pro","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}
```

三份配置解码后的开关键完全相同。命令参数传**解码后的真实文字**（在 UTF-8 终端直接键入，用单引号包住以免 shell 解释特殊字符）：

```bash
go run ./cmd/payflow evaluate config-direct.json 'café🎉' context-pro.json
go run ./cmd/payflow evaluate config-escape.json 'café🎉' context-pro.json
go run ./cmd/payflow evaluate config-mixed.json 'café🎉' context-pro.json
```

三次输出逐字节相同：

```json
{"key":"café🎉","value":true,"reason":"rule","ruleId":"r-pro"}
```

只改名称的 JSON 书写形式不会改变求值结果：选中的是同一个开关，结果中的 `key`、布尔值、`reason`、`ruleId` 全部一致。

命令行参数按传入的字符串原样查找，不会把其中的反斜杠文字当作 JSON 再解码。若把转义的**字面文本**（反斜杠加字母数字本身）作为参数传给 `config-escape.json`，它匹配不到任何解码后的名称：

```bash
go run ./cmd/payflow evaluate config-escape.json 'caf\u00e9\ud83c\udf89' context-pro.json
```

```
evaluate: flag key "caf\\u00e9\\ud83c\\udf89" not found in config "config-escape.json"
```

命令非零退出、标准输出为空；错误信息用引号引用参数原文，其中的反斜杠按字符串引用规则再转义一次，所以显示为双写。要选中上面的开关，参数只能是解码后的真实文字 `café🎉`，而不是它的转义外形。

#### 名称冲突按 JSON 解码后的文字判定

开关键在 `flags` 数组范围内必须唯一，而“是否重复”按 JSON **解码后**的文字判断：一个开关用真实字符、另一个开关用合法转义声明同一个名称，与把同一名字原样抄两遍一样属于重复。例如 `config-key-conflict.json`——`flags[0]` 直接写出 `café🎉`，`flags[1]` 用转义写出同一个名称（且它是未启用的开关），`flags[2]` 则是另一个完全合法的 `new-checkout`：

```json
{
  "flags": [
    {
      "key": "café🎉",
      "enabled": true,
      "default": false,
      "rules": [
        {"id": "r-pro", "value": true, "conditions": [
          {"attribute": "plan", "op": "eq", "value": "pro"}
        ]}
      ]
    },
    {
      "key": "caf\u00e9\ud83c\udf89",
      "enabled": false,
      "default": true,
      "rules": []
    },
    {
      "key": "new-checkout",
      "enabled": true,
      "default": false,
      "rules": [
        {"id": "r-pro2", "value": true, "conditions": [
          {"attribute": "plan", "op": "eq", "value": "pro"}
        ]}
      ]
    }
  ]
}
```

即使请求的是重复名称本身，整份配置也会被拒绝：

```bash
go run ./cmd/payflow evaluate config-key-conflict.json 'café🎉' context-pro.json
```

```
config.flags[1].key: duplicate flag key "café🎉" (first at flags[0])
```

错误指出后一个开关键的位置（`config.flags[1].key`）以及第一次声明的位置（`flags[0]`），命令非零退出、标准输出为空（用 `go run` 运行时标准错误还会追加一行 `exit status 1`，程序本身只输出上面那一行）。

这种冲突无法通过选择另一个合法开关绕过：`flags[1]` 既是重复项又处于关闭状态、本次也不会被选中，但整份配置先于求值完整校验。选择无关的 `new-checkout` 得到的是同样的错误，而不是求值结果：

```bash
go run ./cmd/payflow evaluate config-key-conflict.json new-checkout context-pro.json
```

```
config.flags[1].key: duplicate flag key "café🎉" (first at flags[0])
```

这与上文“关闭的开关与配置校验”一致：唯一性是整份配置的校验项，覆盖未选中和已关闭的开关。（注意区分另一种重复：同一个 JSON 对象内把同名字段写两遍属于“duplicate field”语法格式错误，见下文“附加信息也要通过整份配置的格式校验”；这里是两个开关对象声明了解码后相同的 `key`。）

#### 精确查找不做大小写、空白或等价字符归一化

查找是逐码点的字符串相等，不会自动合并任何看起来相似的名称：大小写不同是不同名称；首尾空白按原样保留、不裁剪；组合字符序列（基字符后跟组合用附加符号）与预组合字符也不做等价归一化。下面这份 `config-similar.json` 先声明四个外形相近、规则 id 各不相同的开关，它们的 `default` 都为 `false`、都以 `plan` 等于 `pro` 为唯一条件，因此返回的 `ruleId` 能直接显示究竟选中了谁（下一小节还会加入第五个名称——字面的 `\uXXXX` 文本）：

```json
{
  "flags": [
    {
      "key": "café🎉",
      "enabled": true,
      "default": false,
      "rules": [
        {"id": "r-base", "value": true, "conditions": [
          {"attribute": "plan", "op": "eq", "value": "pro"}
        ]}
      ]
    },
    {
      "key": "Café🎉",
      "enabled": true,
      "default": false,
      "rules": [
        {"id": "r-upper", "value": true, "conditions": [
          {"attribute": "plan", "op": "eq", "value": "pro"}
        ]}
      ]
    },
    {
      "key": "café🎉 ",
      "enabled": true,
      "default": false,
      "rules": [
        {"id": "r-trail", "value": true, "conditions": [
          {"attribute": "plan", "op": "eq", "value": "pro"}
        ]}
      ]
    },
    {
      "key": "café🎉",
      "enabled": true,
      "default": false,
      "rules": [
        {"id": "r-nfd", "value": true, "conditions": [
          {"attribute": "plan", "op": "eq", "value": "pro"}
        ]}
      ]
    }
  ]
}
```

四个名称的差别在于：`r-base` 是基准 `café🎉`（é 为单个码点 U+00E9）；`r-upper` 首字母大写；`r-trail` 末尾多一个空格；`r-nfd` 的 é 是两个码点——普通字母 e（U+0065）后接组合用锐音符 U+0301，渲染外形几乎一样，但字符串不同（开头空白同理，例如 `" café🎉"` 也不会与基准匹配）。依次传入精确名称：

```bash
go run ./cmd/payflow evaluate config-similar.json 'café🎉' context-pro.json
# {"key":"café🎉","value":true,"reason":"rule","ruleId":"r-base"}

go run ./cmd/payflow evaluate config-similar.json 'Café🎉' context-pro.json
# {"key":"Café🎉","value":true,"reason":"rule","ruleId":"r-upper"}

go run ./cmd/payflow evaluate config-similar.json 'café🎉 ' context-pro.json
# {"key":"café🎉 ","value":true,"reason":"rule","ruleId":"r-trail"}
```

第三条输出的 `key` 在结束引号前同样保留那个空格（`"café🎉 "`），传入不带空格的名称只会得到 `r-base`。组合字符写法的名称要用 e 加 U+0301 的形式传入；不方便直接键入组合字符时可以用 `printf` 按字节生成（`\314\201` 是 U+0301 的 UTF-8 字节，`\360\237\216\211` 是 🎉 的 UTF-8 字节）：

```bash
go run ./cmd/payflow evaluate config-similar.json "$(printf 'cafe\314\201\360\237\216\211')" context-pro.json
```

```json
{"key":"café🎉","value":true,"reason":"rule","ruleId":"r-nfd"}
```

它选中的是 `r-nfd` 而不是 `r-base`：输入与结果中的名称都逐码点保留“e ＋ 组合用锐音符”这一序列，不会归并成预组合的 é。大小写与空白的这种精确性与上下文字符串比较一致（区分大小写、不裁剪空白），但归一化边界只针对名称本身——求值器从不做 Unicode 等价折叠。

#### 字面 `\uXXXX` 文本本身也可以是合法开关键

转义外形不只能表示“真正的字符”：在 JSON 字符串里把反斜杠自身转义（`\\`），解码后留下的就是一段包含反斜杠与字母 u 的普通文本。若想让开关键恰好是字面文本 `caf\u00e9\ud83c\udf89`（21 个普通字符，每个反斜杠都是真实字符），配置里要写双反斜杠。把它作为第五个开关加入 `config-similar.json`：

```json
    {
      "key": "caf\\u00e9\\ud83c\\udf89",
      "enabled": true,
      "default": false,
      "rules": [
        {"id": "r-literal", "value": true, "conditions": [
          {"attribute": "plan", "op": "eq", "value": "pro"}
        ]}
      ]
    }
```

它与真正的 `café🎉`（5 个字符）是两个不同的开关键，尽管前者看起来正好像是后者的“转义写法”。参数传单反斜杠的字面文本（单引号内反斜杠保持字面，不被 shell 或求值器解码）：

```bash
go run ./cmd/payflow evaluate config-similar.json 'caf\u00e9\ud83c\udf89' context-pro.json
```

```json
{"key":"caf\\u00e9\\ud83c\\udf89","value":true,"reason":"rule","ruleId":"r-literal"}
```

选中的是 `r-literal` 而非 `r-base`。输出本身是 JSON，结果里的反斜杠要按 JSON 规则再转义一次，所以每个 `\` 在输出中显示为 `\\`；é、🎉 等非 ASCII 字符则直接以 UTF-8 输出，不会转写成 `\uXXXX`。反过来，把这段字面文本传给只含真实字符名称的 `config-escape.json` 会报未找到——“解码后的字符”与“长得像转义的文本”永远是两个名字。

最后，精确名称不存在时仍报告未找到，不会回退到任何大小写、空白或外形相近的开关：

```bash
go run ./cmd/payflow evaluate config-similar.json 'café🎉x' context-pro.json
```

```
evaluate: flag key "café🎉x" not found in config "config-similar.json"
```

命令非零退出、标准输出为空。小结：配置里先按 JSON 解码得到真实名称，再判重、再与命令行参数做逐码点精确相等；参数里写什么就查找什么，转义只存在于 JSON 文本这一层。

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
- 上下文仍然只是“属性名到字符串”的对象：每个顶层属性的值必须是字符串，`{}` 合法、空字符串合法，但对象、数组、数字等一律按类型错误拒绝。配置附加字段里的嵌套备注不能照搬到上下文——上下文里写 `"plan": {"name": "pro"}` 会得到 `context.plan: value must be a string`。报错位置的写法、多个属性类型错误的报告次序与逐步修正示例见下文“上下文校验失败：报错位置与逐处修正”。

### 上下文校验失败：报错位置与逐处修正

上下文的每个顶层属性都必须是字符串，而校验发生在一切求值之前：整份上下文先通过校验，才会查找开关、匹配规则。校验失败时**没有求值这一步**，普通模式与 `--explain` 的行为完全相同——非零退出、标准输出为空、一行原因写入标准错误；`--explain` 也不会产出半截解释。这与“合法求值得到 `false`”是两回事，后者退出码为 0、JSON 结果照常打印在标准输出（见本节末尾对照）。

把下面这份配置保存为 `config-pro.json`——开关 `new-checkout` 已启用、默认值 `false`，只有一条规则 `r-pro`：`plan` 等于 `pro` 时返回 `true`：

```json
{
  "flags": [
    {
      "key": "new-checkout",
      "enabled": true,
      "default": false,
      "rules": [
        {
          "id": "r-pro",
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

#### 第一次：两个属性都不是字符串

`context-bad-1.json`：

```json
{"user.name":1e400,"plan":false}
```

```bash
go run ./cmd/payflow evaluate config-pro.json new-checkout context-bad-1.json
```

标准输出为空，标准错误为（用 `go run` 运行时它还会在标准错误追加一行 `exit status 1`；程序本身只输出下面这一行）：

```
context["user.name"]: value must be a string
```

报错位置写作 `context["user.name"]`：`user.name` 是**一个完整的属性名**，点号只是名字里的普通字符。上下文只有“属性名 → 字符串”这一层，不支持嵌套对象；位置里的方括号仅仅是给“不符合简单标识符（字母或下划线开头，只含字母、数字、下划线）”的字段名加上 JSON 字符串引号，并不提示要把输入改成 `{"user":{"name":...}}` 之类的嵌套结构。名字是简单标识符时才用点号连接，如下一步的 `context.plan`。

`1e400` 本身是语法合法的 JSON 数字（它作为配置里的附加字段值会被原样接受，见上文“配置中的附加信息”）；这里被拒绝不是因为数字太大或存在语法错误，而是**上下文属性值只收字符串**——数字再合法也不行。修正方式是加上引号，且加上引号后保留的是原样字符串 `"1e400"`（5 个字符），不会被自动换算成数字或 `Infinity`。

另请注意：这条规则只引用了 `plan`，并没有用到 `user.name`；但未使用的属性同样必须先通过上下文校验，不能因为规则不读它就写成非字符串值。

#### 第二次：只修第一处，报错推进到下一处

只给第一项加上引号、其余不动，存为 `context-bad-2.json`：

```json
{"user.name":"1e400","plan":false}
```

```bash
go run ./cmd/payflow evaluate config-pro.json new-checkout context-bad-2.json
```

```
context.plan: value must be a string
```

一次只报告第一处类型错误：`user.name` 修好后，同样的检查继续向后推进，这次轮到 `plan`（`false` 是布尔值，不是字符串）。按标准错误里的位置逐处修正、重新运行即可。

多个属性类型错误时，报告次序是**属性在文件中的先后次序**，不按属性名字母排序：这份文件里 `user.name` 写在 `plan` 前面，于是先报它——尽管按字母序 `plan` 应排在 `user.name` 之前。把两个字段的书写次序对调成 `{"plan":false,"user.name":1e400}`，第一处报错就变成 `context.plan: value must be a string`。报告次序取自文件令牌流而非 Go map 遍历，同一份文件多次运行，报告的属性固定不变。

#### 第三次：两处都修正，得到命中规则的结果

`context-ok.json`：

```json
{"user.name":"1e400","plan":"pro"}
```

```bash
go run ./cmd/payflow evaluate config-pro.json new-checkout context-ok.json
```

```json
{"key":"new-checkout","value":true,"reason":"rule","ruleId":"r-pro"}
```

这时上下文通过校验、规则条件成立：`value` 是结果布尔值 `true`；`reason` 为 `rule` 表示结果由规则定案（而不是取 `default` 或因开关关闭固定为假）；`ruleId` 给出定案规则的编号 `r-pro`。

#### 校验失败不等于合法求值返回 false

把上下文改成类型全部合法、但规则不命中（`plan` 不是 `pro`），存为 `context-ok-free.json`：

```json
{"user.name":"1e400","plan":"free"}
```

```bash
go run ./cmd/payflow evaluate config-pro.json new-checkout context-ok-free.json
```

```json
{"key":"new-checkout","value":false,"reason":"default","ruleId":null}
```

这是一次**成功的求值**：退出码为 0，JSON 结果打印在标准输出；`false` 是开关真实的求值结果（无规则命中，取 `default: false`，所以 `reason` 为 `default`、`ruleId` 为 `null`）。它与上文那种“输入被拒绝、没有结果对象、标准输出为空、退出码非零”的校验失败有本质区别；开关关闭时的 `{"...","value":false,"reason":"disabled","ruleId":null}` 同理也是合法求值结果，不是输入错误。

`--explain` 不会绕过或放宽校验：对第一步的 `context-bad-1.json` 加上 `--explain`，得到的仍是标准错误里同一行 `context["user.name"]: value must be a string`、非零退出、标准输出为空——既没有四字段结果，也没有 `explanation` 对象，不会产生任何部分求值或部分解释。

#### 文档格式错误先于属性类型错误

属性报错次序以“文档格式已通过校验”为前提：非法 UTF-8、不成对的 `\uXXXX` 转义、同一对象内的重复字段、JSON 语法错误或第二个顶层值、顶层不是对象等格式问题，都会先于属性值类型错误被报告。例如把 `plan` 在同一个对象里写两遍：

```json
{"plan":false,"plan":1}
```

报告的是重复字段这一格式错误，而不是其中任何一个值的类型错误：

```
context.plan: duplicate field "plan"
```

也就是说，先保证整份上下文是一份合法、无重复字段的 JSON 对象，再按文件次序逐个把属性值改成字符串，两类问题不会互相遮盖。

## 在 Go 程序中直接调用

上文各小节都是命令行输出；同一套离线求值也以 Go API 公开在包 `github.com/gzhysuiioo/payflow-settlement/payflow`，仅用标准库，没有任何网络或时间依赖。若你的程序就在本模块内，直接 `import` 该包；否则在自己的模块里 `require`（或 `replace` 到本地路径）本模块即可。入口分两类，沿用与命令行完全相同的输入校验和求值含义：

| 入口 | 输入 | 用途 |
| --- | --- | --- |
| `payflow.ParseConfig(raw []byte)` / `payflow.ParseContext(raw []byte)` | 已在内存中的 UTF-8 JSON 字节 | 不碰文件系统，直接接收 JSON 字节 |
| `payflow.LoadConfig(path)` / `payflow.LoadContext(path)` | 本地文件路径 | 读取本地文件后交给同一个 `Parse*`，读取失败保留 `os` 原始错误 |
| `(*Config).Find(key)` | 开关键 | 按解码后文字精确查找，查不到返回 `nil` |
| `(*Flag).Evaluate(ctx)` | 已校验的 `*Context` | 普通求值，返回 `EvalResult`（`Key`/`Value`/`Reason`/`RuleID`） |
| `(*Flag).EvaluateExplain(ctx)` | 已校验的 `*Context` | 解释求值，返回 `ExplainedResult`（嵌入 `EvalResult`，另带 `Explanation`） |
| `payflow.MarshalResult(...)` / `payflow.MarshalExplain(...)` | 上述结果 | 渲染成与命令行逐字一致的紧凑 JSON 对象（`[]byte`） |

`Reason` 的取值是包常量 `payflow.EvalDisabled`/`payflow.EvalRule`/`payflow.EvalDefault`；`RuleID` 是 `*string`，只有命中规则时非 nil。

### 同一个开关、两个上下文：缺失与空字符串

下面这份配置只含开关 `guest-checkout`：已启用、默认 `true`，只有一条规则 `r-block-guest`（命中时返回 `false`）。规则的两个条件同时使用 `eq`（`plan` 等于 `pro`）和 `in`（`tier` 属于候选列表），候选列表特意包含空字符串与重复成员，且保持给定次序：

```go
const configJSON = `{
  "flags": [
    {
      "key": "guest-checkout",
      "enabled": true,
      "default": true,
      "rules": [
        {
          "id": "r-block-guest",
          "value": false,
          "conditions": [
            {"attribute": "plan", "op": "eq", "value": "pro"},
            {"attribute": "tier", "op": "in", "value": ["", "a", "a"]}
          ]
        }
      ]
    }
  ]
}`
```

同一份配置分别处理两个上下文：上下文 A 缺少 `tier`，上下文 B 显式给出 `"tier":""`；两者的 `plan` 都是 `"pro"`，因此两次结果的差异只可能来自“缺失”与“空字符串”。完整程序：

```go
package main

import (
	"fmt"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

const configJSON = `{ ... 同上 ... }`

func ruleIDText(r payflow.EvalResult) string {
	if r.RuleID == nil {
		return "<nil>"
	}
	return *r.RuleID
}

func actualText(c payflow.ConditionExplanation) string {
	if c.ActualValue == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *c.ActualValue)
}

func printPlain(label string, r payflow.EvalResult) {
	fmt.Printf("[%s] value=%v reason=%q ruleId=%s\n", label, r.Value, r.Reason, ruleIDText(r))
	b, err := payflow.MarshalResult(r)
	if err != nil {
		panic(err)
	}
	fmt.Printf("[%s] json: %s\n", label, string(b))
}

func printExplain(label string, x payflow.ExplainedResult) {
	fmt.Printf("[%s] value=%v reason=%q ruleId=%s\n", label, x.Value, x.Reason, ruleIDText(x.EvalResult))
	fmt.Printf("[%s] outcome: %s\n", label, x.Explanation.Outcome)
	for _, rr := range x.Explanation.Rules {
		fmt.Printf("[%s] rule %s match=%v\n", label, rr.RuleID, rr.Match)
		for _, c := range rr.Conditions {
			fmt.Printf("[%s]   cond attribute=%q op=%q compareValue=%#v actualValue=%s missing=%v match=%v\n",
				label, c.Attribute, c.Op, c.CompareValue, actualText(c), c.Missing, c.Match)
		}
	}
	b, err := payflow.MarshalExplain(x)
	if err != nil {
		panic(err)
	}
	fmt.Printf("[%s] json: %s\n", label, string(b))
}

func main() {
	cfg, err := payflow.ParseConfig([]byte(configJSON))
	if err != nil {
		panic(err)
	}
	flag := cfg.Find("guest-checkout")
	if flag == nil {
		panic("guest-checkout not found")
	}

	// 上下文 A：缺少 tier。
	ctxMissing, err := payflow.ParseContext([]byte(`{"plan":"pro"}`))
	if err != nil {
		panic(err)
	}
	// 上下文 B：显式提供空字符串 tier。
	ctxEmpty, err := payflow.ParseContext([]byte(`{"plan":"pro","tier":""}`))
	if err != nil {
		panic(err)
	}

	// 普通求值：只要四字段结果。
	plainMissing := flag.Evaluate(ctxMissing)
	plainEmpty := flag.Evaluate(ctxEmpty)
	printPlain("missing", plainMissing)
	printPlain("empty", plainEmpty)

	// 带解释的求值：同一开关、同样两个上下文，各取一份独立记录。
	recMissing := flag.EvaluateExplain(ctxMissing)
	recEmpty := flag.EvaluateExplain(ctxEmpty)
	printExplain("missing", recMissing)
	printExplain("empty", recEmpty)

	// 普通结果与解释的最终结果逐字一致。
	fmt.Println("plain == explained final result (missing):",
		plainMissing.Equals(recMissing.EvalResult))
	fmt.Println("plain == explained final result (empty):",
		plainEmpty.Equals(recEmpty.EvalResult))
}
```

运行输出（`#v` 那行用 Go 语法直接打印出比较值的真实类型）：

```text
[missing] value=true reason="default" ruleId=<nil>
[missing] json: {"key":"guest-checkout","value":true,"reason":"default","ruleId":null}
[empty] value=false reason="rule" ruleId=r-block-guest
[empty] json: {"key":"guest-checkout","value":false,"reason":"rule","ruleId":"r-block-guest"}
[missing] value=true reason="default" ruleId=<nil>
[missing] outcome: no rule matched; the flag default is used
[missing] rule r-block-guest match=false
[missing]   cond attribute="plan" op="eq" compareValue="pro" actualValue="pro" missing=false match=true
[missing]   cond attribute="tier" op="in" compareValue=[]string{"", "a", "a"} actualValue=<nil> missing=true match=false
[missing] json: {"key":"guest-checkout","value":true,"reason":"default","ruleId":null,"explanation":{"outcome":"no rule matched; the flag default is used","rules":[{"ruleId":"r-block-guest","match":false,"conditions":[{"attribute":"plan","op":"eq","compareValue":"pro","actualValue":"pro","missing":false,"match":true},{"attribute":"tier","op":"in","compareValue":["","a","a"],"actualValue":null,"missing":true,"match":false}]}]}}
[empty] value=false reason="rule" ruleId=r-block-guest
[empty] outcome: first matching rule "r-block-guest" decides the result; later rules are not considered
[empty] rule r-block-guest match=true
[empty]   cond attribute="plan" op="eq" compareValue="pro" actualValue="pro" missing=false match=true
[empty]   cond attribute="tier" op="in" compareValue=[]string{"", "a", "a"} actualValue="" missing=false match=true
[empty] json: {"key":"guest-checkout","value":false,"reason":"rule","ruleId":"r-block-guest","explanation":{"outcome":"first matching rule \"r-block-guest\" decides the result; later rules are not considered","rules":[{"ruleId":"r-block-guest","match":true,"conditions":[{"attribute":"plan","op":"eq","compareValue":"pro","actualValue":"pro","missing":false,"match":true},{"attribute":"tier","op":"in","compareValue":["","a","a"],"actualValue":"","missing":false,"match":true}]}]}}
plain == explained final result (missing): true
plain == explained final result (empty): true
```

据此可以把调用方拿到的 Go 对象与 JSON 一一对应起来：

- **最终结果与解释严格对应**：解释记录嵌入的 `EvalResult` 与普通求值得到的 `EvalResult` 字段逐字一致（示例最后两行的 `Equals` 比较均为 `true`）。上下文 A 无规则命中，布尔值是 `true`、`Reason` 为 `payflow.EvalDefault`（即 `"default"`）、`RuleID` 为 `nil`；上下文 B 命中唯一规则，布尔值是 `false`、`Reason` 为 `payflow.EvalRule`、`RuleID` 指向 `"r-block-guest"`。
- **没有规则命中时的规则编号就是 `nil`**：Go 里 `RuleID *string == nil`，序列化成 JSON 的 `"ruleId":null`；命中规则时它是非 nil 指针，解引用得到规则 id。不要用“`RuleID` 是不是空字符串”判断，未命中用 nil 表示。
- **缺失的实际值如何表示**：`ConditionExplanation.ActualValue` 是 `*string`。上下文 A 缺少 `tier`，它是 `nil` 指针且 `Missing == true`，JSON 中是 `"actualValue":null,"missing":true`；上下文 B 显式给空字符串，它是指向 `""` 的非 nil 指针、`Missing == false`，JSON 中是 `"actualValue":"","missing":false`。二者在 Go 类型层面和 JSON 层面都不会混淆。
- **比较值的类型随运算符而定**：`CompareValue` 字段是 `any`，`eq` 条件里装的是普通 `string`（示例中 `compareValue="pro"`），`in` 条件里装的是 `[]string`（`compareValue=[]string{"", "a", "a"}`）。需要读列表时做一次类型断言：`list := cond.CompareValue.([]string)`。
- **候选次序与重复成员原样保留**：`in` 的比较值就是配置中的那个数组，空字符串排在第一位、两个 `"a"` 重复且不去重、次序不变；JSON 里同样逐字是 `["","a","a"]`。求值时按此次序逐个比较，命中任一成员即可（空字符串也能命中——上下文 B 正是因此让规则成立）。
- **JSON 与命令行一致**：`MarshalResult`/`MarshalExplain` 输出的就是上文 `evaluate`/`--explain` 命令打印的同一形态紧凑对象，`<`、`>`、`&` 不转义，非 ASCII 字符直接输出。

### 保存解释记录与后续操作的关系

每次调用 `EvaluateExplain` 都按当时的配置与上下文**重新构建并返回一份独立记录**：in 候选列表会复制一份，不与配置共享底层数组。因此为展示而修改手里这份记录的文字（`Outcome`）、实际值（`ActualValue`）或候选列表（`CompareValue`），都只作用于这份记录——不会改回原配置、原上下文，不会串到另一份已取得的记录，也不会改变之后任何一次求值；反过来，之后换用上下文再次求值，也不会覆盖此前保存的记录。

```go
package main

import (
	"fmt"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

const configJSON = `{ ... 同上 ... }`

func mustExplain(flag *payflow.Flag, raw string) payflow.ExplainedResult {
	ctx, err := payflow.ParseContext([]byte(raw))
	if err != nil {
		panic(err)
	}
	return flag.EvaluateExplain(ctx)
}

func main() {
	cfg, err := payflow.ParseConfig([]byte(configJSON))
	if err != nil {
		panic(err)
	}
	flag := cfg.Find("guest-checkout")

	// 同一份配置分别对两个上下文求值，各得一份独立记录。
	recMissing := mustExplain(flag, `{"plan":"pro"}`)
	recEmpty := mustExplain(flag, `{"plan":"pro","tier":""}`)

	tierBefore := recMissing.Explanation.Rules[0].Conditions[1]
	fmt.Printf("recMissing tier before: compareValue=%#v actualValue=%v missing=%v\n",
		tierBefore.CompareValue, tierBefore.ActualValue, tierBefore.Missing)
	emptyBefore, _ := payflow.MarshalExplain(recEmpty)

	// 只为展示而改写 recMissing：文字、缺失实际值、in 候选列表各动一处。
	recMissing.Explanation.Outcome = "rewritten by the caller for display"
	injected := "injected-for-display"
	recMissing.Explanation.Rules[0].Conditions[1].ActualValue = &injected
	recMissing.Explanation.Rules[0].Conditions[1].Missing = false
	tier := &recMissing.Explanation.Rules[0].Conditions[1]
	list := tier.CompareValue.([]string)
	list[0] = "x" // 就地改候选首项（原为空字符串）
	tier.CompareValue = append(list, "b")

	tierAfter := recMissing.Explanation.Rules[0].Conditions[1]
	fmt.Printf("recMissing tier after:  compareValue=%#v actualValue=%q missing=%v\n",
		tierAfter.CompareValue, *tierAfter.ActualValue, tierAfter.Missing)

	emptyAfter, _ := payflow.MarshalExplain(recEmpty)
	fmt.Println("recEmpty JSON unchanged after editing recMissing:",
		string(emptyBefore) == string(emptyAfter))

	// 换用上下文再次求值：新结果属于新上下文，之前保存的记录保持当时内容。
	fresh := mustExplain(flag, `{"plan":"free","tier":""}`)
	fmt.Printf("fresh on {plan:free}: value=%v reason=%q ruleId=%v\n",
		fresh.Value, fresh.Reason, fresh.RuleID)
	fmt.Printf("saved recEmpty still:  value=%v reason=%q ruleId=%q\n",
		recEmpty.Value, recEmpty.Reason, *recEmpty.RuleID)
	fmt.Println("saved recMissing outcome still edited:", recMissing.Explanation.Outcome)

	// 对照：普通结构体赋值不会复制 Rules 背后的嵌套数据，两个变量看到同一份。
	a := mustExplain(flag, `{"plan":"pro","tier":""}`)
	dup := a // 不是一次新的求值
	dup.Explanation.Rules[0].Conditions[1].CompareValue = []string{"z"}
	fmt.Printf("a sees dup's edit through shared nested data: %v (a list now %#v)\n",
		a.Explanation.Rules[0].Conditions[1].CompareValue.([]string)[0] == "z",
		a.Explanation.Rules[0].Conditions[1].CompareValue)
	again := mustExplain(flag, `{"plan":"pro","tier":""}`)
	fmt.Printf("a fresh evaluation always rebuilds its own list: %#v\n",
		again.Explanation.Rules[0].Conditions[1].CompareValue)
}
```

输出：

```text
recMissing tier before: compareValue=[]string{"", "a", "a"} actualValue=<nil> missing=true
recMissing tier after:  compareValue=[]string{"x", "a", "a", "b"} actualValue="injected-for-display" missing=false
recEmpty JSON unchanged after editing recMissing: true
fresh on {plan:free}: value=true reason="default" ruleId=<nil>
saved recEmpty still:  value=false reason="rule" ruleId="r-block-guest"
saved recMissing outcome still edited: rewritten by the caller for display
a sees dup's edit through shared nested data: true (a list now []string{"z"})
a fresh evaluation always rebuilds its own list: []string{"", "a", "a"}
```

前后对照说明：

- 被改写的 `recMissing` 的 `tier` 条件，比较值从 `[]string{"", "a", "a"}`、实际值 `nil`/`missing=true`，变成 `[]string{"x", "a", "a", "b"}`、实际值 `"injected-for-display"`/`missing=false`；但同次运行里另一份记录 `recEmpty` 序列化后逐字节不变（`... unchanged ...: true`），配置中的候选仍可由下一次求值证明是原样。
- 换成 `{"plan":"free","tier":""}` 再求值得到的是新上下文自己的结果（`true`/`default`/`nil`）；此前保存的 `recEmpty` 仍是当时的 `false`/`rule`/`r-block-guest`，被改写过的 `recMissing` 也保留展示文字。重新求值只读配置与上下文，不读任何旧记录。
- **结构体赋值不等于两份独立记录**：`dup := a` 只是一次浅拷贝，`dup` 与 `a` 仍共享 `Explanation.Rules` 背后的切片、条件和字符串数据，通过 `dup` 改嵌套字段，`a` 立刻看得到（输出中 `a` 的候选变成了 `[]string{"z"}`）。要得到互不影响的两份记录，只能对同一/另一上下文再次调用 `EvaluateExplain`——它会重建列表（末行又恢复为 `[]string{"", "a", "a"}`）；若只是想复制一份在手记录上再改，需自行深拷贝相关切片。

### 失败时如何结束操作

读取与校验发生在求值之前。任何错误都应让调用方保留具体错误并结束，**不产出求值结果**；这与“合法规则返回 `false`”的成功结果完全不同。注意区分两类现有入口：`ParseConfig`/`ParseContext` 接收 JSON 字节，`LoadConfig`/`LoadContext` 读取本地文件（读不到文件时错误来自 `os`，外层包上文件路径）；两者之后走的是同一套严格校验。查不到开关时 `Find` 返回 `nil`，需显式告知“未找到”，不能把它当成 `false` 结果，也不能在 nil 开关上继续求值。

```go
package main

import (
	"fmt"

	"github.com/gzhysuiioo/payflow-settlement/payflow"
)

func main() {
	// 入口一：直接接收 JSON 字节（不碰文件系统），与读文件走同一套严格校验。
	goodConfig := []byte(`{"flags":[{"key":"f","enabled":true,"default":true,"rules":[
	  {"id":"r-ok","value":false,"conditions":[
	    {"attribute":"plan","op":"eq","value":"pro"}]}]}]}`)
	cfg, err := payflow.ParseConfig(goodConfig)
	if err != nil {
		fmt.Println("stop, no result:", err) // 读取/校验失败：保留错误，不产出求值结果
		return
	}
	ctx, err := payflow.ParseContext([]byte(`{"plan":"pro"}`))
	if err != nil {
		fmt.Println("stop, no result:", err)
		return
	}
	// 合法规则返回 false 仍是成功结果：reason=rule，在库里不以错误呈现。
	if flag := cfg.Find("f"); flag != nil {
		r := flag.Evaluate(ctx)
		fmt.Printf("legal false is a success result: value=%v reason=%q ruleId=%q\n",
			r.Value, r.Reason, *r.RuleID)
	}

	// 入口二：读取本地文件，校验错误（含文件不存在）原样上抛。
	if _, err := payflow.LoadConfig("no-such-config.json"); err != nil {
		fmt.Println("stop, no result:", err)
	}
	if _, err := payflow.LoadContext("no-such-context.json"); err != nil {
		fmt.Println("stop, no result:", err)
	}
	// 上下文属性值不是字符串：ParseContext/LoadContext 给出的具体错误原样保留。
	if _, err := payflow.ParseContext([]byte(`{"plan":false}`)); err != nil {
		fmt.Println("stop, no result:", err)
	}
	if _, err := payflow.ParseConfig([]byte(`{"flags":[{"key":"f","enabled":"yes","default":true,"rules":[]}]}`)); err != nil {
		fmt.Println("stop, no result:", err)
	}

	// 查不到指定开关：明确告知未找到，不当成返回 false，也不继续求值。
	if f := cfg.Find("absent-flag"); f == nil {
		fmt.Println(`flag key "absent-flag" not found; stop, do not treat as false`)
	}
}
```

输出：

```text
legal false is a success result: value=false reason="rule" ruleId="r-ok"
stop, no result: cannot read config file "no-such-config.json": open no-such-config.json: no such file or directory
stop, no result: cannot read context file "no-such-context.json": open no-such-context.json: no such file or directory
stop, no result: context.plan: value must be a string
stop, no result: config.flags[0].enabled: must be a boolean
flag key "absent-flag" not found; stop, do not treat as false
```

要点：

- 本地文件不存在时，`Load*` 保留 `cannot read ... file: open ...: no such file or directory` 这样的具体错误（包上了路径与文件类别）；直接喂字节的 `Parse*` 则不会产生文件读取错误。
- 上下文属性值不是字符串（如 `{"plan":false}`）得到 `context.plan: value must be a string`；配置字段类型错误同样给出字段位置（`config.flags[0].enabled: must be a boolean`）。命令行文档中的其余校验（重复字段、代理项转义、整份配置全量校验等）在库里原样适用——`Parse*`/`Load*` 不会因为嵌入使用而放宽。
- 第一行是**成功求值**：规则 `r-ok` 合法且返回 `false`，所以 `value=false`、`reason="rule"`、`ruleId="r-ok"`，没有 error；它与上面四行“校验失败、没有结果对象”有本质区别，也与开关关闭的 `reason="disabled"`、无规则命中的 `reason="default"` 区分。
- `Find` 返回 `nil` 表示配置中没有该键：调用方应明确报“未找到”并停止，既不能把缺失误判为 `false` 布尔结果，也不能对 nil 开关调用 `Evaluate`。

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
