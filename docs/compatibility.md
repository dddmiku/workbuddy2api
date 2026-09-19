# 接口兼容性

网关适配 `/v1/responses` 与 `/v1/chat/completions`，把请求转换为上游格式，并把结果还原给客户端。请求结构可以接受，不代表选定的上游模型、账号或渠道一定支持该能力。

## Responses

| 能力 | 当前行为 |
|---|---|
| 文本输入与历史消息 | 支持；客户端应发送所需的完整历史 |
| 图片输入 | 转换已支持的图片内容块，受入站大小和出站图片预算限制 |
| function 工具 | 支持声明、调用、结果与历史往返；`strict:true` 参数在成功终态前按 JSON Schema 校验 |
| custom 工具 | 桥接为 `{input: string}` 函数参数，回程恢复原始 `input` 和 `custom_tool_call` |
| namespace 工具分组 | 展开 function/custom 子工具，回程恢复 `name` 与 `namespace`；不支持分组继续嵌套 |
| `tool_choice` | 支持 auto、none、required、指定 function/custom，以及下文的 allowed_tools 子集 |
| `parallel_tool_calls` | 保留；显式 false 时检查成功结果是否返回多个调用 |
| `text.format` | 支持 text、json_object、json_schema；结构化格式检查最终文本输出 |
| `text.verbosity` | 接受有效字符串声明但不转发；上游没有对应的风格开关 |
| `reasoning.effort` / `summary` | 转发，具体可用档位由模型决定 |
| `prompt_cache_key` | 保留；它不是服务端历史存储 |
| `truncation` | 接受有效声明但不实现本地自动截断；是否接受上下文由上游决定 |
| 未知历史 item 类型 | 忽略其不支持的元数据，不把它提升为用户消息或聊天正文 |
| 未知内容块类型 | Responses 内容适配器返回 400，避免接受后静默丢失正文 |
| `previous_response_id`、`store=true`、`background=true` | 不支持，返回 400 |
| 非空 `conversation` / `prompt`、`item_reference` | 不支持服务端会话、模板和引用解析，返回 400 |

历史 item 与消息 content 是两个层级：未知的状态/扩展 item 可以忽略；已知消息里的正文不能因为转换器不认识内容类型而被静默丢掉。已支持的文字、图片和 refusal 内容会保留，工具结果中的 refusal 也不会变成空输出。

custom 的 grammar/CFG 描述仅作为模型提示，不提供原生语法执行或语法约束保证。当前 custom 调用通过最终 `input` 与 `response.output_item.done` 交付；不声明完整支持专用 `custom_tool_call_input.delta/done` 事件族。

## 工具选择与严格约束

| 选择方式 | 成功完成时的约束 |
|---|---|
| `auto` | 模型可返回正文或工具调用 |
| `none` | 不得返回工具调用 |
| `required` | 至少声明一个可执行工具，且成功结果必须包含调用 |
| 指定 function/custom | 必须恰好调用一次所指定的工具 |
| `allowed_tools` | 只向上游提供选中的工具，并检查返回调用没有超出该子集；mode 支持 auto 或 required |

`allowed_tools` 是 `tool_choice` 的一种对象形式，不是附加后按 auto 忽略。下面是选择已声明命名空间函数时的 `tool_choice` 值：

```json
{
  "type": "allowed_tools",
  "mode": "required",
  "tools": [
    {"type": "function", "namespace": "catalog", "name": "lookup"}
  ]
}
```

引用必须对应已声明且可执行的 function/custom 工具，类型和命名空间也必须匹配。非法 mode、未声明引用、required 下的空子集返回 400；重复的同一子集引用会去重。原始工具声明和返回时的公开名称保持不变。

`strict:true` 函数参数按声明的 JSON Schema 校验，非法 schema 或外部 schema 引用在请求阶段拒绝。`strict:false` 或未声明 strict 的函数保持尽力模式，不额外套用严格参数 schema；工具名称、参数形状和完整性仍按相应协议路径检查。

工具参数完整性在本次上游流的 `[DONE]` 或合法 EOF 收尾时校验。早到的 `finish_reason` 后仍可接收参数增量或完整消息快照；正文、参数和已观测用量继续流式发送，成功结束标记在校验通过后发送一次。这样既能接住迟到的完整参数，也不会先宣告成功再报告残参或上游错误。真正缺失、残缺或不是合法 JSON 的参数仍返回错误，不自动补写或猜测工具操作；`length`、`content_filter` 保留原有不完整语义。

`text.format=json_schema` 检查最终文本是否匹配 schema；`json_object` 要求完整 JSON 对象，不能带 Markdown 围栏或尾随文字。可以先返回工具调用，随后再给最终结构化文本。纯拒答以及明确的 length/content_filter 不完整结果有独立语义，不会被 required 或指定工具约束强改成工具违约。

## 内置工具的边界

`web_search`、`tool_search` 及其版本后缀声明可以出现在两种接口中，但会从出站工具列表过滤。接受声明只用于兼容客户端默认元数据，不代表网关已实现联网搜索、工具发现或客户端内置工具执行。

- 过滤后没有可执行工具，且选择为省略、auto 或 none 时，可移除无意义的工具控制字段。
- 只剩不可执行内置工具却要求 required，或强制选择被过滤的工具时，前置返回 400，不发送上游请求，也不静默降级为 auto。
- 混合声明中的 function/custom 仍按工具选择规则处理。
- 需要服务端能力的 file_search、mcp、image_generation、computer_use、code_interpreter、local_shell 等内置类型不受支持，返回明确错误。

这里区分的是工具的 type；用户声明的普通 function 不会仅因名称类似内置工具就被当成内置类型。

## 命名空间与长工具名

命名空间子函数支持 Responses 顶层 `name/parameters` 形式，也支持嵌套 `function.name/function.parameters` 形式。分组的使用说明会保留到出站描述。相同命名空间内的重复工具身份会被拒绝，不靠覆盖顺序猜选工具。

一般出站名使用 `命名空间__工具名`。超过 64 字节时生成稳定短别名，采用保留 UTF-8 边界的前缀与摘要后缀，碰撞后缀也受长度限制。顶层长工具名同样处理。声明、历史调用、工具选择和回程还原共用映射；客户端继续使用原始名称，无需自行猜短别名。

命名空间调用的回程形状例如：

```json
{
  "type": "function_call",
  "name": "lookup",
  "namespace": "catalog",
  "call_id": "call_example",
  "arguments": "{\"query\":\"hello\"}"
}
```

`/v1/chat/completions` 不接受 namespace/custom 类型声明；需要它们时使用 Responses 接口。直接 Chat 的普通 function 长名与兼容的 legacy function 字段也会同步映射和恢复。

## Chat Completions

直接 Chat 支持普通消息、function 工具和兼容的 legacy `functions/function_call` 形态。`response_format` 支持 text、json_object、json_schema；存在工具选择、严格参数或结构化输出要求时，复用上述成功完成校验，并逐个检查返回的 choice。

流式请求中的正文、refusal 和工具参数增量保持实时输出。需要契约校验或别名还原时，仅推迟成功的 finish、末尾 usage 和 `[DONE]`；完整结果校验后再发终态。迟到的上游错误保留错误信封，不先发成功结束再补报错。非流式输出违约返回 502。

已经发出的增量无法撤回，因此调用方必须检查最终状态或流内错误。收到 HTTP 200 或若干正文片段不能视为完整成功。Chat 的其他结构合法的内容形态可继续交给选定上游处理，不等同于网关保证该模态可用。

## 完成状态

- 正常完成：`response.completed`，内部状态为 completed。
- 输出上限或内容过滤截断：`response.incomplete`，携带 `incomplete_details`。
- 上游流错误、意外结束或输出契约不满足：`response.failed` 或相应 HTTP 错误。
- refusal 按拒答字段和 `response.refusal.delta/done` 保留，不伪装成普通正文，也不把纯拒答当作缺少必需工具。
- 不完整或无效的工具调用不会作为可执行的成功 `output_item.done` 交付。
- `tool_calls/function_call` 结束原因没有实际调用时，返回 missing_tool_call 失败。
- 非数组 tool_calls 是格式错误；null 或空数组配合正常正文结束仍可接受。

工具条目建立后，增量与最终结果保持同一 item/call 身份；早于名称到达的参数会暂存，不能靠猜测工具身份提前交付。失败或截断不会用成功工具终态掩盖缺失的数据。

Responses 的同一 output item 只发送一次 `response.output_item.added`，增量与收尾引用同一身份，结束后不复用该 item ID。推理与工具或正文交错时，后续推理追加到尚未结束的 reasoning 条目；摘要增量只包含新增文本，不重放此前片段。最终 output 与流内条目的身份和顺序保持一致。

`response.completed` 是一次响应的协议终态，不是整个用户任务的验收结论。工具调用与纯文字均可正常完成；模型只用文字预告下一步时，不能仅凭协议终态判断已完成所有工作。接入约定见 [Codex 接入](codex.md#预告文字与回合结束)。

## 重复推理的失败状态

指定 DeepSeek 模型触发重复短行保护时，错误码为 `upstream_reasoning_loop`，提示「检测到重复推理循环，已停止该次请求；可整理上下文后重试」，可附字符数与重复覆盖计数。非流式 Chat 与 Responses 返回 HTTP 422；已开始的流式请求保留 HTTP 200，Chat 返回错误事件，Responses 以 `response.failed` 结束，不附带成功终态。

网关自身不对此类失败轮换账号、重试或冷却，已有会话绑定保留；客户端是否重试取决于其策略。账本保留已观测的原始用量，并同时增加失败和用量未完整返回计数。保护默认开启且可关闭；它可能误判合法短行重复，也不能检测所有循环，详细范围见 [重复推理保护](configuration.md#重复推理保护)。

## 思考模式的推理回灌（上游 11155）

Responses 的明文 reasoning 历史会转换为上游的 `reasoning_content`。DeepSeek 的兼容路径还会为相关 assistant 消息补齐字段存在性；只有 encrypted_content 时，网关不能解密并恢复推理原文。

转换层会合并连续 assistant 片段，保留正文、多模态内容、工具调用及可用推理内容，兼容对消息形状敏感的上游。11155 不能单独证明是某一个字段缺失，也可能涉及历史消息结构；应检查客户端实际发送的历史形状，而不是把所有情况归结为模型能力或账号故障。

## 常见错误

| 错误 | 排查方向 |
|---|---|
| `invalid_api_key` | 密钥缺失、停用或删除 |
| `invalid_request` | 输入结构、工具选择、schema 或不支持的服务端能力 |
| `request_body_too_large` | 入站请求超过配置上限 |
| `response_contract_violation` | 模型的成功结果未满足声明的工具或输出约束 |
| `missing_tool_call` | 工具结束原因缺少实际调用 |
| `upstream_reasoning_loop` | 重复短行推理触发保护；整理上下文后重试，合法重复场景可关闭保护 |
| `upstream_invalid_request` | 上游拒绝请求参数 |
| `upstream_channel_rejected` | 上游拒绝调用渠道 |
| `upstream_waf_blocked` | 上游入口拦截请求正文 |
| `content_blocked` | 上游内容策略拒绝 |
| `context_length_exceeded` | 模型上下文超限 |
| `rate_limit_exceeded` | 账号或模型限流 |

渠道拒绝、入口拦截和模型内容拒绝是不同原因。首次请求按正常转换发送；特定上游拦截可能触发有界的同路径字符串兼容重试，重试仍失败则返回对应错误，不保证任意请求都能通过。具体上游拦截规则不是静态协议承诺。

## 验证范围

公开回归使用合成数据，覆盖工具子集与选择、严格/非严格参数、长名往返、refusal、未知历史项隔离、正文/图片保留、SSE 增量、截断与迟到错误，以及交错推理的条目生命周期和重复推理保护。可检查 [协议回归](../internal/server/protocol_diagnosis_test.go)、[输出完整性回归](../internal/server/responses_output_integrity_test.go)、[请求结构校验](../internal/server/request_validation_test.go)、[推理生命周期回归](../internal/server/responses_reasoning_lifecycle_test.go) 和 [保护及用量回归](../internal/server/reasoning_loop_guard_test.go)。

这些检查不能等同于全部账号、模型、渠道和客户端组合均已实测通过。开源实现的固定提交、采用原则及未照搬的边界见 [协议实现参考](protocol-references.md)。
