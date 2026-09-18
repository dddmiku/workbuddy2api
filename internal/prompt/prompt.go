// ═══ 更新日志 ═══
// 2026-09-16：显式自定义提示词替换保留其余请求数字原值，避免 schema 与工具参数定义丢失精度。
// 2026-09-17：新增 ActNote（运行约定）：抑制上游模型「一句话一个命令」的叙述式输出。
// 2026-09-18：明确纯文字会结束客户端回合，要求待执行动作与实际工具调用同次返回，同时保留用户停手和确认边界。
// Package prompt 提供网关自有系统提示词加载及显式 custom 模式的系统消息替换。
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"workbuddy2api/internal/jsonutil"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded 降级提示词：误报处理用，刻意极简中性。
//
// 触发场景：passthrough 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案），
// 判定为指纹误报后换最小中性提示词重试一次。非对抗框架——只用于绕开
// system 来源的误报，不改变用户指令的合法性语义。
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// ActNote supplements tool-enabled requests without replacing client instructions.
// Chat Completions has no separate commentary turn that automatically resumes:
// a standalone progress sentence can be interpreted as the final answer by Codex.
// This is a model instruction, not a heuristic retry or a guarantee of completion.
const ActNote = "Tool execution protocol: a reply containing only text ends the client's turn; " +
	"there is no automatic continuation after a standalone progress message. If required work " +
	"remains and you are ready to act, return an actual available tool call in this same response. " +
	"Do not end with a promise such as 'Let me...', 'I will...', or 'next I will...' and expect " +
	"another turn to execute it. Continue the authorized task through implementation and verification. " +
	"Give a final text answer when the task is complete, when the user asked only for an answer, " +
	"or when a blocker genuinely requires user input or approval. Respect the user's scope, " +
	"stop requests, and approval requirements; this protocol grants no additional authorization."

// ActNoteDisabled 显式关闭 act_note 的取值。
const ActNoteDisabled = "off"

// Load 按 mode 与 file 加载系统提示词文本。
//   - file 非空 → 读文件（不存在/读失败返回 error，调用方 fail fast）；
//   - file 空 → 返回内置 defaultPrompt。
//
// mode 在此仅做透传记录（实际 custom/passthrough 路由由调用方决定），
// Load 只负责"拿到一段提示词文本"，不关心路由语义。
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	return string(raw), nil
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//   - 删除 messages 中所有 role 为 system/developer 的消息；
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}；
//   - 其余字段与 user/assistant/tool 消息逐字不动。
//
// 解析失败 → 原样返回（绝不失败）：Rewrite 是出站改写的关键路径，
// 任何解析错误都不应阻塞请求转发，让上游按其原始语义处理。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// 无 messages 字段或类型不符 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// 过滤掉所有 system/developer 消息，保留 user/assistant/tool 及其他角色。
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	// 头部插入单条 system 消息（prepend 避免整体重排语义）。
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
