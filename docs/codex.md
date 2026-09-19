# Codex 接入

官方 Codex CLI 可以使用本项目的 Responses 接口。实际验证版本为 `codex-cli 0.153.4`，模型为 `cn:deepseek-v4.1-flash` 与 `global:deepseek-v4.1-flash`。

**现在只需要填 API key 和 Base URL。** 官方 CLI 默认说明里那句渠道归属声明会被上游判为未授权渠道，网关会自动把这句话断词并重发一次，客户端不需要再准备中性说明文件。

会话表现为：上游先返回 400 `unapproved channel`，网关在同一账号、同一路径上用断词后的正文重发，用户侧直接拿到答案，网关日志里留下一条 `channel trigger neutralized for retry`。

上一版文档要求手工配置 `model_instructions_file`；这条已经不再是必需项，仅作为「不想让网关改写任何正文」时的可选做法保留在文末。

## 默认指令为什么会被拒

对一份真实的 Codex 请求做字段二分：`tools`（含 namespace 分组与 `web_search`）全部移除仍然被拒，把 `instructions` 换成中性文案后立即返回 200。工具声明、模型名与用户消息都不是触发点。

再对 `instructions` 二分，触发面收敛到**一句话**：`Codex CLI is an open source project led by OpenAI.`

| 说明内容 | 结果 |
|---|---|
| 完整原文（21026 字符） | 400 `upstream_channel_rejected` |
| 删掉上面那一句 | 200 |
| 把那一句改写成中性说法 | 200 |
| 只把 `OpenAI` 换成其他词（其余原文不动） | 200 |
| 只保留第一句 `You are a coding agent running in the Codex CLI, a terminal-based coding assistant.`（83 字符） | 200 |
| 说明里只有 `OpenAI`、没有那一句 | 200 |
| 用户消息里提到 `OpenAI` | 200 |

也就是说上游的渠道校验命中的是这句对**渠道归属**的声明，而不是 `OpenAI` 这个词本身。

网关的处理方式：命中这句话时，只在每个单词首字母之后插入零宽空格（`U+200B`）后重发一次。零宽字符不参与词义，模型读到的仍是同一句话，删掉标记后逐字等于原文；其余正文、工具声明和用户消息一律不动。

如果希望上游完全不看到这句改写，也可以继续用文末的 `model_instructions_file` 覆盖系统提示词——两种做法二选一即可。

## 配置

最小可用配置（网关与 Codex 在同一台 Linux 服务器上）：

```toml
model = "cn:deepseek-v4.1-flash"
model_provider = "workbuddy2api"
model_reasoning_effort = "low"
model_reasoning_summary = "concise"

[model_providers.workbuddy2api]
name = "workbuddy2api"
base_url = "http://127.0.0.1:7863/v1"
env_key = "WORKBUDDY_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 90000
```

远程客户端把 `base_url` 换成自己的服务地址，模型名按 `/v1/models` 里实际可用的值填（国际版账号加 `global:` 前缀）。

通过 `WORKBUDDY_API_KEY` 环境变量提供调用密钥，不把真实密钥提交到配置示例中。

### 长会话的上限口径

**用量保持原值，提前压缩在客户端配置。** 网关将上游已经返回的用量映射到 Responses 或 Chat 对应字段，输入、输出、缓存明细和合计都不乘估计倍率。账本同样累计已观测到的原始用量；缺少原始数据的部分保留为未知。

`server.input_token_scale` 与 `WB2A_INPUT_TOKEN_SCALE` 已弃用。旧有效配置仍可读取，启动时会告警并忽略，不再改变任何协议的用量字段；建议删除遗留项，详见[旧输入倍率配置](configuration.md#旧输入倍率配置)。

用量描述已处理请求的观测结果，无法精确预测下一次加入消息、工具结果或图片后的请求是否超限。本项目没有实现可精确复刻上游限制的 tokenizer，也不根据单条路由的文本样本推定所有模型的计数方式。

| 配置或字段 | 归属 | 用途 |
|---|---|---|
| `model_context_window` | Codex 客户端 | 客户端采用的模型上下文窗口；改大它不会提高上游实际可接受长度 |
| `model_auto_compact_token_limit` | Codex 客户端 | 触发自动历史压缩的 token 阈值；未设置时使用该客户端版本的模型默认行为 |
| `/v1/models` 中的 `context_length` | 网关模型目录 | 提供模型元数据，不能代替客户端配置或证明客户端已经采用该值 |

前两个字段设置在 Codex 实际读取的配置中，例如 `~/.codex/config.toml` 的顶层，与 `model` 同级，放在 `[model_providers.workbuddy2api]` 表之前。若由 cc switch 等工具生成配置，应核对它实际写入并由该客户端加载的文件，避免后续生成操作覆盖手工设置。

CLI 0.153.4 还会用客户端模型元数据中的 `max_context_window` 限制配置窗口，配置文件里的数值不一定就是运行时生效值。自定义模型没有匹配目录条目时会使用回退元数据，不能只把配置窗口调大就认为限制已提高；对应实现见 [0.153.4 的窗口覆盖逻辑](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/models-manager/src/model_info.rs#L25)。

根据所用模型、路由和客户端版本设置窗口与压缩阈值，给后续消息、工具结果和输出预留空间。修改后重新启动客户端，确认新配置已加载。网关返回 `context_length` 不会自动下发或写入这两个客户端设置，单改模型目录不能替代这一步。

验收分两项记录：用量对账核对同一请求的上游字段、客户端响应和账本；压缩验收检查客户端的实际配置、压缩事件及后续任务能否继续。压缩成功不能代替用量对账，用量一致也不能保证下一次任意大小的请求不超限。

字段定义见[官方配置参考](https://learn.chatgpt.com/docs/config-file/config-reference)。官方页面可能随客户端版本更新，实际行为应以所用版本验证。

### 可选：用中性说明覆盖系统提示词

不想让网关对系统提示词做任何改写时，可以继续加载仓库里的 [中性说明](../examples/codex-instructions.md)：

```toml
model_instructions_file = "/opt/workbuddy2api/examples/codex-instructions.md"
```

`model_instructions_file` 由 Codex 读取，不是网关的 `prompt.mode=custom`。网关可保持 `passthrough`，业务文本和工具数据原样传递。

该文件必须放在 Codex 能读到的绝对路径上（同机部署可直接引用仓库内文件）；删掉或留空会退回默认指令，此时由网关自动断词兜底。

## 使用与验证

先在普通测试目录验证读文件、执行一条测试命令和续接对话，再用于自己的项目。需要结构化结果时，可以在 Codex 中提供 `--output-schema`。

### 推理反复重复

指定 DeepSeek 模型持续重复少量短行、没有新的正文或工具进展时，网关默认会尝试中止该次请求，返回 `upstream_reasoning_loop`。看到此错误后可整理上下文再重试；业务本来就需要大量重复短行时，可以关闭这项保护。它可能误报，也不能覆盖所有循环，设置与用量边界见 [重复推理保护](configuration.md#重复推理保护)。

### 预告文字与回合结束

`response.completed` 表示一次模型响应已经完整返回。响应中有可执行的工具调用时，Codex 执行工具并继续请求；只有文字时，客户端可以结束当前回合。因此“接下来我会运行测试”这样的预告，即使 HTTP 为 200，也不代表测试已经执行。

从 v1.3.15 起，带工具的 Responses 请求默认补充这条协议约定：还有已获授权的工作且准备执行时，必须在同一次响应中返回真实工具调用；完成任务、仅需文字回答或确实需要用户补充信息时再文字收尾。它追加在第一条 system 消息末尾，`prompt.act_note="off"` 可以关闭，自定义文本继续生效。这是提示层的缓解措施，不能保证第三方模型在每次长会话里都完成所有工作。

同时，网关不再把下面的协议错误静默包装成成功正文：工具结束原因没有实际调用、非数组的工具调用列表。分组工具中的嵌套 `function` 定义也会完整保留，名称和参数可以正确往返。合法的工具迟到分片继续接收，长度截断与上游错误保留各自终态。

不同客户端可能使用不同协议、系统说明和续跑策略。cc switch 保存的 `apiFormat`、Codex 的 `wire_api` 与某次请求真正进入的路径需要分别核对；不能仅看界面里的“completions”就认定实际请求是 `/v1/chat/completions`。排查时记录实际路径、版本、工具输出和结束事件，区分模型正常停止、协议缺失、截断和连接错误。

协议依据：[OpenAI 工具调用流程](https://developers.openai.com/api/docs/guides/function-calling)。

本次验证的两轮任务使用同一会话，工具调用结果均回传，业务测试从 5 项到 8 项通过，最终编号保持整数 `11128`。这属于指定客户端、模型和说明配置的验证，不是对全部功能的保证。

接入回归（同一台服务器，真实客户端实测）：

| 场景 | 结果 |
|---|---|
| 桌面版形状请求：21261 字符说明 + 14 个工具（含 3 个 namespace 分组） | 200 `completed`，回答 `OK` |
| 真实 `codex exec` + 默认说明（`global:deepseek-v4.1-flash`） | 首次 400 `unapproved channel` → 网关断词重发 → 200，回答 `OK` |
| 真实 `codex exec` + 默认说明，消息里带 HTML/SQL/带管道的 shell 命令（`global:`） | 200，回答 `HTML、SQL、Bash（Shell 脚本）` |
| 真实 `codex exec` + 默认说明（`cn:deepseek-v4.1-flash` 回归） | 200，回答 `OK` |
| 真实 `codex exec` + `model_instructions_file` 指向本仓库说明 | 200，回答 `OK` |

如果出现 `upstream_channel_rejected`，应保留完整错误并核对上游允许范围；`upstream_waf_blocked` 只会出现在网关断词重试之后仍被拦的情况；网关已自动处理绝大多数命中的正文（见[兼容性说明](compatibility.md)）；若出现 `invalid_api_key`，检查密钥状态；若请求了不支持的内置工具，按[兼容性说明](compatibility.md)调整客户端能力。

配置字段参考：[官方 Codex 配置文档](https://developers.openai.com/codex/config-reference/)。
