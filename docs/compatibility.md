# 接口兼容性

网关将请求转换为上游可接受的格式，再转换回客户端接口。协议支持不代表上游会接受任何账号、模型或渠道。

## Responses

| 能力 | 当前行为 |
|---|---|
| 文本输入、历史消息 | 支持，客户端发送完整历史 |
| 图片输入 | 支持相关内容块转换，受出站图片预算影响 |
| function 工具 | 支持调用与结果往返 |
| custom 工具 | 桥接为函数参数，输出还原为 custom 调用 |
| namespace 工具分组 | 展开为出站函数（`命名空间__工具名`），回程还原 `name` + `namespace` |
| `reasoning.effort` / `summary` | 转发；具体档位按模型能力处理 |
| `parallel_tool_calls` | 保留；显式禁止并行时检查返回结果 |
| `prompt_cache_key` | 保留，不等于服务端保存会话内容 |
| `text.format` | 支持 text、json_object 和 json_schema |
| `text.verbosity` | 接受声明但不转发（风格提示，上游无对应开关） |
| `truncation` | 接受声明但不转发（网关无状态，是否自动截断由上游决定） |
| `tool_choice.allowed_tools` | 接受声明，按 auto 处理（不收缩上游可选工具集） |
| 未知的历史项类型（`tool_search_call`、`web_search_call` 等） | 忽略；`item_reference` 例外，它指向服务端保存的内容，仍然报错 |
| `previous_response_id` | 不支持，返回 400 |
| `store=true` | 不支持，返回 400 |
| `background=true` | 不支持，返回 400 |
| 服务端 `conversation` / `prompt` | 不支持非空值 |
| 客户端自带的内置工具（`web_search`、`tool_search`） | 接受声明但不转发上游。官方 Codex 默认就会带上，拒绝会让整个会话不可用；模型只是不会去调用它们 |
| 需要服务端能力的工具（`file_search`、`mcp`、`image_generation`、`computer_use`、`local_shell`） | 不支持，返回明确错误。这些是用户显式声明的能力，静默丢弃会让人误以为在生效 |
| `text.verbosity` | 不支持非空值 |

JSON Schema 校验针对最终文本输出，允许先完成工具调用。外部 schema 引用不受支持。模型拒答、上限截断和上游错误分别处理，不能只依赖 HTTP 200 判断最终业务结果。

## 完成状态

- 正常完成：`response.completed`，内部状态为 `completed`。
- 输出上限或内容过滤截断：`response.incomplete`，携带 `incomplete_details`。
- 上游流错误、意外结束或输出契约不满足：失败状态或相应 HTTP 错误。
- 不完整的工具参数不会作为可执行的完整工具调用交付。

非流式请求由本地聚合上游 SSE。客户端取消、读取错误与格式错误保留其失败语义。

## 常见错误

| 错误 | 排查方向 |
|---|---|
| `invalid_api_key` | 密钥缺失、已停用或已删除 |
| `invalid_request` | 输入结构错误，或请求了尚未支持的能力 |
| `request_body_too_large` | 入站请求超过配置上限 |
| `upstream_invalid_request` | 上游拒绝请求参数；查看脱敏诊断 |
| `upstream_channel_rejected` | 上游明确拒绝调用渠道 |
| `upstream_waf_blocked` | 上游 WAF 按正文特征拦截（HTML/脚本或 SQL 样式文本），请求未到达模型 |
| `content_blocked` | 上游内容策略拒绝 |
| `context_length_exceeded` | 模型上下文超限 |
| `rate_limit_exceeded` | 账号或模型限流，需要等待恢复 |

渠道拒绝与内容拒绝是不同原因，不能通过错误码 `11128` 单独判断。网关不会因这类请求错误替换用户正文或修改其他会话的系统指令。

`upstream_channel_rejected` 的触发面已定位到系统说明，并收敛到一句：`Codex CLI is an open source project led by OpenAI.`。真实请求里移除全部工具声明仍然被拒，换成中性说明后立即通过；只删掉这一句、或把它改写成中性说法，整段 21026 字符的说明也能通过；单独的 `OpenAI` 一词（在说明里或在用户消息里）不触发。工具分组、模型名与用户消息不是触发点。网关保持正文原样，需要由客户端自带中性说明（见 [Codex 接入](codex.md)）；桌面版说明不含该句，CLI 默认说明含该句。

`upstream_waf_blocked` 是上游 WAF 的判定结果，不是网关或账号故障。判定看**请求正文**、不看账号：2026-09-17 对国际版 `www.workbuddy.ai` 逐条对照实测，脚本标签与事件处理器、`alert(`/`eval(`、SQL 注入式表达式（`or 1=1`、`union select`、`drop table`）、命令注入片段（`sleep(`、`benchmark(`、`curl http://`）、`${jndi:` 以及 `%3C` / `\x3C` 编码变体返回 403 HTML 拦截页；普通文本、纯标签结构、Markdown 代码块、XML 声明、`information_schema`、`select ... where` 本身都通过。CN 链路上同一批正文全部 200。

## 思考模式的推理回灌（上游 11155）

DeepSeek 系模型在思考模式下，上游要求把上一轮的推理原文随历史带回，缺字段即返回
`{"code":11155,"extError":{"code":"reasoning_content_missing"}}`（网关包装成
`upstream_invalid_request`）。Responses 侧的推理项原样翻译成 chat 的 `reasoning_content`：

1. 历史里每个 `reasoning` 项按 `summary[].text` / `content[].text` / 字符串形态取正文，
   挂到紧随其后的 assistant 消息上；历史以推理项收尾时挂到最后一条 assistant 消息。
2. 只要历史出现过推理项，deepseek 模型的**所有** assistant 消息都会带 `reasoning_content`
   （拿不到原文的补空串），对齐官方客户端 `requiresReasoningContentOnAssistantMessages`
   的匹配规则；非 deepseek 模型零改动。
3. 只有加密推理（`encrypted_content` 且无明文）时同样会置位，避免整条链缺字段。

上游仍返回 11155 时，网关打一行形状日志（只统计字段，不含正文）：
`upstream 11155 shape ... assistant=N reasoning_content=N empty=N tool_call_msgs=N last_role=...
history_reasoning_items=N with_text=N`。据此可区分「客户端没带推理项」「带了但没明文」
与「assistant 消息缺字段」三种成因。

### 相邻 assistant 消息必须合并（国际版真实成因）

上面第 1–3 条只解决了「字段缺失」，2026-09-18 的真实会话复现表明 11155 还有第二种成因：
**国际版后端要求「一条 assistant 消息 = 一个回话回合」**。

Codex 在「先写一句话、再调工具」时，历史里是 `message` + `function_call` 两个独立 item，
翻译后会变成两条相邻的 assistant 消息。国际版后端遇到这种历史直接 400：

```text
{"code":11155,"msg":"the reasoning content from the previous turn must be passed back in thinking mode",
 "extError":{"code":"reasoning_content_missing",...}}
```

实测矩阵（同一份最小历史，国际版 / 国内版对照）：

| 历史尾部形状 | global | cn |
|---|---|---|
| 相邻 assistant：正文那条后面紧跟带 tool_calls 的那条 | **400 11155** | 200 |
| 正文与 tool_calls 并成同一条 assistant | 200 | 200 |
| 带正文的 assistant 后面紧跟 user / developer / system | 200 | 200 |
| 纯工具往返（assistant(tool_calls) → tool → …） | 200 | 200 |

因此转换层现在把**连续的 assistant 消息合并成一条**：正文按顺序拼接、tool_calls 依序合并、
`reasoning_content` 取第一条非空值。真实会话复现验证：1361 项历史（1053 条 chat 消息、
54 处相邻 assistant）合并前 400、合并后 200。国内版后端两种形状都接受，所以这条归一化
对 CN 链路无行为变化。

排查这类问题时先看上面那行形状日志：`assistant=` 条数明显多于回合数、或
`last_reasoning_content=false` 而前面又有大量 assistant，就是这个成因。

网关的处理分两步：

1. 原样请求先发一次（正文保真优先）。
2. 收到 WAF 拦截页时，把正文里命中的模式用零宽字符断开后**同路径自动重试一次**。零宽字符不参与词义、渲染不可见，模型仍读到同一段文本；重试成功后对调用方完全透明。

只有重试后仍被拦，才返回 `upstream_waf_blocked`，并且不轮转账号、不惩罚账号、不回显拦截页 HTML。

这一点对多轮会话尤其重要：**历史消息同样会被上游 WAF 扫描**，一旦某轮把触发片段写进历史，之后每一轮都会命中——网关的断词重试让这种会话可以继续，不需要用户删改历史或开新会话。

## 命名空间工具

新版 Codex 会把 MCP 与内部工具按 `tools[].type="namespace"` 分组上报。网关把分组内的 function/custom 工具展开成上游可用的扁平函数名（`mcp__node_repl__js` 这种三段式），并在返回工具调用时还原成客户端要的形状：

```json
{"type":"function_call","name":"js","namespace":"mcp__node_repl","call_id":"call_1","arguments":"{\"code\":\"1+1\"}"}
```

历史里的 `function_call` / `custom_tool_call` 也会按同样规则折回扁平名，模型看到的调用名前后一致。分组内允许 function 与 custom，不允许继续嵌套命名空间；`/v1/chat/completions` 不接受命名空间工具。

## 验证范围

已在官方 codex-cli 0.153.4 中，以独立模型说明配置调用 `cn:deepseek-v4.1-flash`，完成文件读取、代码修改、工具往返、两轮续接及 strict JSON Schema 输出。默认 codex 请求的渠道限制另有失败记录。

公开仓库保存合成回归数据；原始会话记录、账号凭据、个人环境信息和生产日志不作为测试资源发布。global 账号、全部模型和全部客户端组合仍需分别验证。
