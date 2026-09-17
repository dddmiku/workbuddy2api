# Codex 接入

官方 Codex CLI 可以使用本项目的 Responses 接口。实际验证版本为 `codex-cli 0.153.4`，模型为 `cn:deepseek-v4.1-flash`。

**只填写 API key 和 URL 不一定可用。** 原生默认 codex 请求曾被上游以未获准渠道拒绝；已通过的配置显式选择了一份独立、中性的模型说明。该结果不等于默认配置通过，也不代表其他版本与模型已经验证。

## 默认指令为什么会被拒

对一份真实的 desktop Codex 请求做字段二分后可以定位：`tools`（含 namespace 分组与 `web_search`）全部移除仍然被拒，把 `instructions` 换成中性文案后立即返回 200。

继续对 `instructions` 二分：整段原文被拒，只保留第一句（`You are a coding agent running in the Codex CLI, a terminal-based coding assistant.`，83 字符）可以通过；把同样开头里的 `Codex CLI` / `led by OpenAI` 换成中性说法后，整段也能通过。也就是说，上游的渠道校验命中的是系统说明里**声明自己是 Codex CLI / OpenAI 产品**的那类句子，而不是工具声明、模型名或消息内容。

网关不会替换这些正文（正文保真是刻意的设计），因此默认指令的客户端需要用下面这份中性说明覆盖系统提示词。

## 配置

下面使用网关与 Codex 位于同一台 Linux 服务器的路径。将 `model_instructions_file` 改为本机 [说明文件](../examples/codex-instructions.md) 的绝对路径；远程客户端还需修改 `base_url`。

```toml
model = "cn:deepseek-v4.1-flash"
model_provider = "workbuddy2api"
model_reasoning_effort = "low"
model_reasoning_summary = "concise"
model_instructions_file = "/opt/workbuddy2api/examples/codex-instructions.md"

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

通过 `WORKBUDDY_API_KEY` 环境变量提供调用密钥，不把真实密钥提交到配置示例中。该说明只适合本 provider，选择其他模型时应使用相应配置。

`model_instructions_file` 由 Codex 读取，不是网关的 `prompt.mode=custom`。网关可保持 `passthrough`，业务文本和工具数据原样传递。

`examples/codex-instructions.md` 必须放在 Codex 能读到的绝对路径上（同机部署可直接引用仓库内文件）；把该文件删掉或留空会退回默认指令，请求又会撞上上游渠道校验。

## 使用与验证

先在普通测试目录验证读文件、执行一条测试命令和续接对话，再用于自己的项目。需要结构化结果时，可以在 Codex 中提供 `--output-schema`。

本次验证的两轮任务使用同一会话，工具调用结果均回传，业务测试从 5 项到 8 项通过，最终编号保持整数 `11128`。这属于指定客户端、模型和说明配置的验证，不是对全部功能的保证。

如果出现 `upstream_channel_rejected`，应保留完整错误并核对上游允许范围；若出现 `invalid_api_key`，检查密钥状态；若请求了不支持的内置工具，按[兼容性说明](compatibility.md)调整客户端能力。

配置字段参考：[官方 Codex 配置文档](https://developers.openai.com/codex/config-reference/)。
