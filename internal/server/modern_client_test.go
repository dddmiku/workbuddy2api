// ═══ 更新日志 ═══
// 2026-09-18：用一条"新版 Codex 客户端真实形态"的请求锁定兼容边界：风格/提示类字段
// 一律接受并忽略，能力/状态类字段才报错。此前 namespace / tool_search / text.verbosity
// 都是各自炸过一次才发现，这个用例把整面一次性钉住。
// 2026-09-18：allowed_tools 现在实际约束转发子集，原始 custom/namespace 声明仍保留供响应回显。
package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// modernClientRequest 汇总 Codex 0.156 桌面端会发送的字段：工具分组、延迟工具发现、
// 输出详略、自动截断策略、白名单式工具选择、推理档位、加密推理历史、客户端元数据。
const modernClientRequest = `{
  "model": "global:deepseek-v4.1-flash",
  "instructions": "You are Codex.",
  "input": [
    {"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "think"}], "encrypted_content": "blob"},
    {"type": "tool_search_call", "call_id": "ts_1", "query": "find a tool"},
    {"type": "tool_search_output", "call_id": "ts_1", "output": "tool list"},
    {"role": "user", "content": "hi"}
  ],
  "tools": [
    {"type": "function", "name": "exec_command", "description": "run", "parameters": {"type": "object"}},
    {"type": "custom", "name": "apply_patch", "description": "patch"},
    {"type": "namespace", "name": "mcp__node_repl", "tools": [
      {"type": "function", "name": "js", "parameters": {"type": "object"}}]},
    {"type": "tool_search", "description": "deferred discovery", "execution": "client"},
    {"type": "web_search"}
  ],
  "tool_choice": {"type": "allowed_tools", "tools": [{"type": "function", "name": "exec_command"}]},
  "parallel_tool_calls": false,
  "reasoning": {"effort": "medium", "summary": "concise"},
  "text": {"verbosity": "low"},
  "include": ["reasoning.encrypted_content"],
  "prompt_cache_key": "session-abc",
  "client_metadata": {"session_id": "s1"},
  "store": false,
  "stream": true
}`

func TestModernCodexRequestIsAccepted(t *testing.T) {
	if err := requestValidationResponses([]byte(modernClientRequest)); err != nil {
		t.Fatalf("新版客户端请求不应被拒: %v", err)
	}
	body, req, err := responsesToChat([]byte(modernClientRequest))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatalf("chat body: %v", err)
	}
	// 风格/提示类字段不进上游 body：上游没有对应字段，带上只会被拒。
	for _, forbidden := range []string{"verbosity", "truncation", "text", "client_metadata"} {
		if _, present := chat[forbidden]; present {
			t.Errorf("上游 body 不应带 %q: %v", forbidden, chat[forbidden])
		}
	}
	// 只转发 allowed_tools 选中的函数；其它声明仍留在 req 的映射与回显中。
	tools := chatToolsByName(t, body)
	for _, name := range []string{"exec_command"} {
		if tools[name] == nil {
			t.Errorf("工具 %q 丢失: %v", name, tools)
		}
	}
	if len(tools) != 1 {
		t.Errorf("上游应只看到白名单中的 1 个函数工具，实际 %d: %v", len(tools), tools)
	}
	if req.customTools["apply_patch"] != true || req.toolAliases["mcp__node_repl__js"].Namespace != "mcp__node_repl" {
		t.Errorf("工具映射记录不对: custom=%v alias=%+v", req.customTools, req.toolAliases)
	}
	// 未知历史项被忽略，正常消息保留。
	messages, _ := chat["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("messages 为空: %v", chat)
	}
	if !strings.Contains(string(body), "hi") {
		t.Errorf("用户消息丢失: %s", body)
	}
}

func TestModernCodexRequestStreamsToCompletion(t *testing.T) {
	names, values := contractWriter(t, modernClientRequest,
		`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	last := values[len(values)-1]["response"].(map[string]any)
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("terminal=%s body=%v", names[len(names)-1], last)
	}
	var builder strings.Builder
	for _, raw := range last["output"].([]any) {
		item, _ := raw.(map[string]any)
		if item["type"] != "message" {
			continue
		}
		for _, rawPart := range item["content"].([]any) {
			part, _ := rawPart.(map[string]any)
			if text, _ := part["text"].(string); text != "" {
				builder.WriteString(text)
			}
		}
	}
	if !strings.Contains(builder.String(), "ok") {
		t.Fatalf("输出文本丢失: %q", builder.String())
	}
}

// TestServerCapabilitiesStayRejected 只有真正做不到的能力/状态字段才报错。
func TestServerCapabilitiesStayRejected(t *testing.T) {
	cases := map[string]string{
		"background":           `{"model":"m","input":"hi","background":true}`,
		"store":                `{"model":"m","input":"hi","store":true}`,
		"previous_response_id": `{"model":"m","input":"hi","previous_response_id":"resp_1"}`,
		"conversation":         `{"model":"m","input":"hi","conversation":"conv_1"}`,
		"prompt":               `{"model":"m","input":"hi","prompt":{"id":"pmpt_1"}}`,
		"item_reference":       `{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`,
		"file_search":          `{"model":"m","input":"hi","tools":[{"type":"file_search"}]}`,
		"mcp":                  `{"model":"m","input":"hi","tools":[{"type":"mcp","server_url":"https://example.invalid"}]}`,
	}
	for name, body := range cases {
		if err := requestValidationResponses([]byte(body)); err == nil {
			t.Errorf("%s 应保持明确报错", name)
		}
	}
}
