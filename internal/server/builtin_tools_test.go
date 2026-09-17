// ═══ 更新日志 ═══
// 2026-09-18：锁定内置工具的"接受但丢弃"契约：客户端（Codex 0.156）默认带 tool_search，
// 拒绝会让整个会话不可用；未知类型仍必须明确拒绝而不是静默放过。
package server

import (
	"strings"
	"testing"
)

const builtinToolsRequest = `{"model":"global:deepseek-v4.1-flash","stream":false,"input":"hi","tools":[
  {"type":"function","name":"exec_command","description":"run","parameters":{"type":"object"}},
  {"type":"tool_search","description":"deferred tool discovery","execution":"client"},
  {"type":"web_search"},
  {"type":"web_search_preview_2025_03_11"}]}`

func TestBuiltinToolsAreAcceptedAndDropped(t *testing.T) {
	body, _, err := responsesToChat([]byte(builtinToolsRequest))
	if err != nil {
		t.Fatalf("内置工具不应导致整条请求被拒: %v", err)
	}
	tools := chatToolsByName(t, body)
	if tools["exec_command"] == nil {
		t.Fatalf("function 工具必须保留: %v", tools)
	}
	for _, dropped := range []string{"tool_search", "web_search"} {
		if _, present := tools[dropped]; present {
			t.Fatalf("内置工具 %q 不应转发到上游: %v", dropped, tools)
		}
	}
	if len(tools) != 1 {
		t.Fatalf("上游只应看到 1 个 function 工具，实际 %d: %v", len(tools), tools)
	}
}

func TestBuiltinToolFamiliesAcceptDatedVariants(t *testing.T) {
	for _, kind := range []string{
		"tool_search", "tool_search_2026_01_01", "web_search_2025_08_26",
		"web_search_preview", "web_search_preview_2025_03_11",
	} {
		if !isUnimplementedBuiltinTool(kind) {
			t.Errorf("%q 应按内置工具接受", kind)
		}
	}
	for _, kind := range []string{"", "future_builtin", "function", "custom", "namespace", "toolsearch",
		"file_search", "mcp", "image_generation", "computer_use", "local_shell"} {
		if isUnimplementedBuiltinTool(kind) {
			t.Errorf("%q 不应被当成内置工具", kind)
		}
	}
}

// TestServerSideBuiltinToolsStayRejected 服务端能力（文件检索 / MCP / 图片生成等）
// 仍然明确报错：静默丢弃会让用户以为这些能力在生效。
func TestServerSideBuiltinToolsStayRejected(t *testing.T) {
	cases := map[string]string{
		"file_search":      `{"type":"file_search","vector_store_ids":["vs_1"]}`,
		"mcp":              `{"type":"mcp","server_url":"https://example.invalid"}`,
		"image_generation": `{"type":"image_generation"}`,
		"computer_use":     `{"type":"computer_use"}`,
		"local_shell":      `{"type":"local_shell"}`,
	}
	for name, tool := range cases {
		request := `{"model":"m","input":"hi","tools":[` + tool + `]}`
		if _, _, err := responsesToChat([]byte(request)); err == nil {
			t.Errorf("%s 仍应明确拒绝", name)
		} else if !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s 的错误信息应说明不支持: %v", name, err)
		}
	}
}

func TestUnknownToolTypeStaysRejected(t *testing.T) {
	request := `{"model":"m","input":"hi","tools":[{"type":"future_builtin","name":"x"}]}`
	if _, _, err := responsesToChat([]byte(request)); err == nil {
		t.Fatal("未知工具类型必须继续拒绝，避免静默丢工具造成客户端行为不一致")
	} else if !strings.Contains(err.Error(), "future_builtin") {
		t.Fatalf("错误信息应带工具类型: %v", err)
	}
}

func TestBuiltinToolInsideNamespaceStaysRejected(t *testing.T) {
	request := `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"tool_search"}]}]}`
	if _, _, err := responsesToChat([]byte(request)); err == nil {
		t.Fatal("命名空间内声明内置工具必须拒绝：命名空间只允许 function/custom")
	}
}

func TestChatEndpointStillRejectsBuiltinTools(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"tools":[{"type":"tool_search"}]}`)
	if err := validateChatRequest(body); err == nil {
		t.Fatal("chat 端点不是 Responses 协议，仍应拒绝内置工具声明")
	}
}
