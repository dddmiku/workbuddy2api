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
| `previous_response_id` | 不支持，返回 400 |
| `store=true` | 不支持，返回 400 |
| `background=true` | 不支持，返回 400 |
| 服务端 `conversation` / `prompt` | 不支持非空值 |
| 自动 `truncation` | 不支持 |
| 内置 web search 等工具 | 不支持，返回明确错误 |
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
| `content_blocked` | 上游内容策略拒绝 |
| `context_length_exceeded` | 模型上下文超限 |
| `rate_limit_exceeded` | 账号或模型限流，需要等待恢复 |

渠道拒绝与内容拒绝是不同原因，不能通过错误码 `11128` 单独判断。网关不会因这类请求错误替换用户正文或修改其他会话的系统指令。

`upstream_channel_rejected` 的触发面已定位到系统说明：真实请求里移除全部工具声明仍然被拒，换成中性说明后立即通过；把说明开头声明自己属于 Codex CLI / OpenAI 的句子改写后，整段说明也能通过。工具分组、模型名与用户消息不是触发点。网关保持正文原样，需要由客户端自带中性说明（见 [Codex 接入](codex.md)）。

## 命名空间工具

新版 Codex 会把 MCP 与内部工具按 `tools[].type="namespace"` 分组上报。网关把分组内的 function/custom 工具展开成上游可用的扁平函数名（`mcp__node_repl__js` 这种三段式），并在返回工具调用时还原成客户端要的形状：

```json
{"type":"function_call","name":"js","namespace":"mcp__node_repl","call_id":"call_1","arguments":"{\"code\":\"1+1\"}"}
```

历史里的 `function_call` / `custom_tool_call` 也会按同样规则折回扁平名，模型看到的调用名前后一致。分组内允许 function 与 custom，不允许继续嵌套命名空间；`/v1/chat/completions` 不接受命名空间工具。

## 验证范围

已在官方 codex-cli 0.153.4 中，以独立模型说明配置调用 `cn:deepseek-v4.1-flash`，完成文件读取、代码修改、工具往返、两轮续接及 strict JSON Schema 输出。默认 codex 请求的渠道限制另有失败记录。

公开仓库保存合成回归数据；原始会话记录、账号凭据、个人环境信息和生产日志不作为测试资源发布。global 账号、全部模型和全部客户端组合仍需分别验证。
