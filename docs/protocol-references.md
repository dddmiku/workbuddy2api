# 协议实现参考

本页记录实际阅读过的开源转换代码、采用的原则和未直接沿用的行为。链接固定到具体提交，避免默认分支变化后无法复核。实现参考不是协议规范，也不等同于这些项目或本项目已经通过全部客户端、模型和渠道组合。

## 固定版本与许可证

| 项目 | 固定提交 | 许可证范围 |
|---|---|---|
| [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) | [b773607e3e7756dc6020a291825e4eb08899595a](https://github.com/router-for-me/CLIProxyAPI/commit/b773607e3e7756dc6020a291825e4eb08899595a) | [MIT](https://github.com/router-for-me/CLIProxyAPI/blob/b773607e3e7756dc6020a291825e4eb08899595a/LICENSE) |
| [cc-switch](https://github.com/farion1231/cc-switch) | [06082e189d65e6d6dbadc35dacdac1ce6c79d89a](https://github.com/farion1231/cc-switch/commit/06082e189d65e6d6dbadc35dacdac1ce6c79d89a) | [MIT](https://github.com/farion1231/cc-switch/blob/06082e189d65e6d6dbadc35dacdac1ce6c79d89a/LICENSE) |
| [LiteLLM](https://github.com/BerriAI/litellm) | [8fc9c46d1aeda6d009718cd22684a526fa4647fd](https://github.com/BerriAI/litellm/commit/8fc9c46d1aeda6d009718cd22684a526fa4647fd) | [根 LICENSE](https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/LICENSE) 将 enterprise/ 与其余代码分开；本页引用的 litellm/ 文件适用其 MIT 部分，不包含企业目录实现 |

本项目使用独立实现与自身回归测试，没有把参考项目的整段实现或其源码副本加入仓库。

## 采用的原则

| 主题 | 已读源码 | 本项目采用方式 |
|---|---|---|
| 流式工具状态与增量 | CLIProxyAPI [状态、待发送参数和已发长度][cli-delta]；LiteLLM [工具事件队列与增量补齐][lite-tools] | 名称与 call_id 就绪后建立 item；保留尚未发送的参数，避免早到内容丢失或重复发送 |
| 终态与末尾 usage | CLIProxyAPI [将响应终态延后至 DONE][cli-terminal] | 直接 Chat 的约束包装保持正文实时输出，完整消费和校验后再发送成功 finish、末尾 usage 与 DONE |
| 事件身份稳定 | LiteLLM [最终快照复用流式 item ID][lite-ids] | 增量、item.done 和最终 output 保持一致身份，不在收尾时另造一套调用标识 |
| 长工具名与双向别名 | CLIProxyAPI [64 字节边界][cli-names]、[命名碰撞消歧][cli-collisions]、[tool_choice 共用名称解析][cli-choice] | 独立实现稳定摘要别名并保留 UTF-8 边界；覆盖顶层和命名空间工具、历史、指定工具选择及回程恢复 |
| 空工具控制字段 | cc-switch [无出站工具时清理 tool_choice 与 parallel_tool_calls][cc-empty-tools] | 只在省略/auto/none 等可安全清理的情形使用；required 或 forced 没有可执行工具时仍前置拒绝 |
| 不把丢失工具当成功 | cc-switch [在成功分支检查丢失的工具调用][cc-tool-completion] | 结合本项目的参数完整性、工具选择和 schema 校验，在执行终态前阻止不可用调用 |

## 未直接沿用的行为

- CLIProxyAPI 的所查 [普通函数工具转换][cli-function] 复制名称、描述和 parameters，没有复制工具 strict；它不是本项目严格参数校验的替代方案。本项目在成功终态前自行检查 strict 函数 schema 和工具选择。
- LiteLLM 的 [命名空间展开][lite-namespace] 在该桥接中直接拼接 namespace__name，并对特定碰撞报错；这些函数没有与 CLIProxyAPI 相同的长名缩短机制。本项目用同一套别名映射处理所有相关路径。
- LiteLLM 的 [custom 包装][lite-custom] 使用 content 字符串；本项目使用 input。包装字段和解包策略不能混用。它的无效 JSON 原样回退不用于放松本项目的执行边界。
- LiteLLM 的 [Chat 桥接状态映射][lite-status] 对缺失或未知 finish_reason 默认 completed，并由该桥接 [构造 ResponseCompletedEvent][lite-ids]；其 [原生 Responses 路径][lite-native-terminal] 则区分 completed、incomplete、failed。cc-switch 的 [不完整收尾][cc-tool-completion] 也存在事件名与内层状态分开的兼容表示。本项目保留独立的 response.incomplete / response.failed 语义。
- LiteLLM 的 [流式文本选择][lite-choices] 明确只取第一 choice；本项目直接 Chat 的完成契约逐 choice 检查，不沿用“第一条代表全部”的假设。
- LiteLLM 的 [schema 调整][lite-schema] 带 provider/model 条件，本项目不把这种特定后端兼容策略扩散成对所有 schema 的全局改写。
- 参考项目可能实现 tool_search 或其他客户端桥接；本项目对 web_search/tool_search 只接受声明后过滤，没有据此新增搜索、工具发现、原生 CFG 或服务端工具执行能力。

以上是指定源码路径的比较，不是对参考项目整体运行时行为的判定。

## 当前保证与边界

本项目支持的工具策略、长名、refusal、未知历史 item、内容适配和服务端能力边界，以 [接口兼容性](compatibility.md) 为准。custom 当前通过最终 input/item.done 交付，不把参考实现中出现的专用输入事件直接等同于本项目已支持的事件族。

实现变更由公开合成回归覆盖，例如 [协议约束与别名回归](../internal/server/protocol_diagnosis_test.go)、[Responses 输出完整性](../internal/server/responses_output_integrity_test.go) 和 [请求结构校验](../internal/server/request_validation_test.go)。测试不会用真实账号、个人会话或生产日志作为公开夹具。

这些协议处理不能替客户端判断整个任务是否完成，也不能保证模型在纯文字预告后一定继续调用工具。

[cli-delta]: https://github.com/router-for-me/CLIProxyAPI/blob/b773607e3e7756dc6020a291825e4eb08899595a/internal/translator/openai/openai/responses/openai_openai-responses_response.go#L337-L399
[cli-terminal]: https://github.com/router-for-me/CLIProxyAPI/blob/b773607e3e7756dc6020a291825e4eb08899595a/internal/translator/openai/openai/responses/openai_openai-responses_response.go#L763-L775
[cli-names]: https://github.com/router-for-me/CLIProxyAPI/blob/b773607e3e7756dc6020a291825e4eb08899595a/internal/translator/openai/openai/responses/openai_openai-responses_tools.go#L370-L414
[cli-collisions]: https://github.com/router-for-me/CLIProxyAPI/blob/b773607e3e7756dc6020a291825e4eb08899595a/internal/translator/openai/openai/responses/openai_openai-responses_tools.go#L126-L188
[cli-choice]: https://github.com/router-for-me/CLIProxyAPI/blob/b773607e3e7756dc6020a291825e4eb08899595a/internal/translator/openai/openai/responses/openai_openai-responses_request.go#L370-L406
[cli-function]: https://github.com/router-for-me/CLIProxyAPI/blob/b773607e3e7756dc6020a291825e4eb08899595a/internal/translator/openai/openai/responses/openai_openai-responses_tools.go#L240-L257
[cc-empty-tools]: https://github.com/farion1231/cc-switch/blob/06082e189d65e6d6dbadc35dacdac1ce6c79d89a/src-tauri/src/proxy/providers/transform_codex_chat.rs#L315-L341
[cc-tool-completion]: https://github.com/farion1231/cc-switch/blob/06082e189d65e6d6dbadc35dacdac1ce6c79d89a/src-tauri/src/proxy/providers/streaming_codex_chat.rs#L535-L574
[lite-tools]: https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/litellm/responses/litellm_completion_transformation/streaming_iterator.py#L213-L390
[lite-ids]: https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/litellm/responses/litellm_completion_transformation/streaming_iterator.py#L1237-L1278
[lite-namespace]: https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/litellm/responses/litellm_completion_transformation/transformation.py#L1889-L1975
[lite-custom]: https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/litellm/responses/litellm_completion_transformation/custom_tools.py#L96-L213
[lite-status]: https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/litellm/responses/litellm_completion_transformation/transformation.py#L2271-L2296
[lite-native-terminal]: https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/litellm/llms/openai/responses/transformation.py#L659-L703
[lite-choices]: https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/litellm/responses/litellm_completion_transformation/streaming_iterator.py#L1223-L1235
[lite-schema]: https://github.com/BerriAI/litellm/blob/8fc9c46d1aeda6d009718cd22684a526fa4647fd/litellm/llms/openai/responses/transformation.py#L410-L488
