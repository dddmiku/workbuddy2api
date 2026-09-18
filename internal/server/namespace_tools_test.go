// ═══ 更新日志 ═══
// 2026-09-17：锁定命名空间工具（Codex 0.155 的 tools[].type=namespace）在请求、历史与回程的完整映射。
// 2026-09-18：保留工具自身描述和分组说明，避免扁平化丢失使用上下文。
package server

import (
	"encoding/json"
	"strings"
	"testing"
)

const namespaceRequest = `{"model":"cn:deepseek-v4.1-flash","stream":false,"input":"hi","tools":[
  {"type":"function","name":"standalone","description":"top level","parameters":{"type":"object"}},
  {"type":"namespace","name":"mcp__node_repl","description":"node repl","tools":[
    {"type":"function","name":"js","description":"run js","parameters":{"type":"object","properties":{"code":{"type":"string"}}}},
    {"type":"custom","name":"apply_patch","description":"patch"}]},
  {"type":"namespace","name":"multi_agent_v1","description":"agents","tools":[
    {"type":"function","name":"spawn_agent","description":"spawn","parameters":{"type":"object"}}]},
  {"type":"web_search"}]}`

func chatToolsByName(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatalf("chat body: %v", err)
	}
	result := map[string]map[string]any{}
	for _, raw := range chat["tools"].([]any) {
		tool := raw.(map[string]any)
		fn := tool["function"].(map[string]any)
		result[fn["name"].(string)] = fn
	}
	return result
}

func TestNamespaceToolsFlattenForUpstream(t *testing.T) {
	body, req, err := responsesToChat([]byte(namespaceRequest))
	if err != nil {
		t.Fatal(err)
	}
	tools := chatToolsByName(t, body)
	for _, name := range []string{"standalone", "mcp__node_repl__js", "mcp__node_repl__apply_patch", "multi_agent_v1__spawn_agent"} {
		if tools[name] == nil {
			t.Fatalf("flat tool %q missing: %v", name, tools)
		}
	}
	if _, dropped := tools["web_search"]; dropped {
		t.Fatalf("web_search must stay dropped, got %v", tools)
	}
	if tools["mcp__node_repl__js"]["description"] != "run js\n\nNamespace mcp__node_repl: node repl" {
		t.Errorf("description lost: %v", tools["mcp__node_repl__js"])
	}
	custom := tools["mcp__node_repl__apply_patch"]["parameters"].(map[string]any)
	if _, ok := custom["properties"].(map[string]any)["input"]; !ok {
		t.Errorf("custom tool inside a namespace must be bridged with an input field: %v", custom)
	}
	if !req.customTools["mcp__node_repl__apply_patch"] || req.customTools["mcp__node_repl__js"] {
		t.Errorf("custom bookkeeping wrong: %v", req.customTools)
	}
	alias := req.toolAliases["mcp__node_repl__js"]
	if alias.Namespace != "mcp__node_repl" || alias.Name != "js" || alias.Custom {
		t.Errorf("alias wrong: %+v", alias)
	}
}

func TestNamespaceToolCallRestoredForClient(t *testing.T) {
	names, values := contractWriter(t, namespaceRequest,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"mcp__node_repl__js","arguments":"{\"code\":\"1+1\"}"}}]},"finish_reason":"tool_calls"}]}`)
	last := values[len(values)-1]["response"].(map[string]any)
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("terminal=%s", names[len(names)-1])
	}
	var call map[string]any
	for _, raw := range last["output"].([]any) {
		item := raw.(map[string]any)
		if item["type"] == "function_call" {
			call = item
		}
	}
	if call == nil {
		t.Fatalf("no function_call item: %v", last["output"])
	}
	if call["name"] != "js" || call["namespace"] != "mcp__node_repl" {
		t.Fatalf("namespace call not restored: %v", call)
	}
	if call["arguments"] != `{"code":"1+1"}` {
		t.Fatalf("arguments changed: %v", call["arguments"])
	}
}

func TestNamespaceCustomCallRestoredForClient(t *testing.T) {
	_, values := contractWriter(t, namespaceRequest,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"mcp__node_repl__apply_patch","arguments":"{\"input\":\"*** Begin Patch ***\"}"}}]},"finish_reason":"tool_calls"}]}`)
	last := values[len(values)-1]["response"].(map[string]any)
	for _, raw := range last["output"].([]any) {
		item := raw.(map[string]any)
		if item["type"] != "custom_tool_call" {
			continue
		}
		if item["name"] != "apply_patch" || item["namespace"] != "mcp__node_repl" {
			t.Fatalf("custom namespace call not restored: %v", item)
		}
		if item["input"] != "*** Begin Patch ***" {
			t.Fatalf("custom input not unwrapped: %v", item)
		}
		return
	}
	t.Fatalf("no custom_tool_call item: %v", last["output"])
}

func TestNamespaceHistoryMapsBackToFlatName(t *testing.T) {
	body := []byte(`{"model":"cn:deepseek-v4.1-flash","stream":false,"tools":[
	  {"type":"namespace","name":"mcp__node_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object"}}]}],
	  "input":[{"type":"function_call","name":"js","namespace":"mcp__node_repl","call_id":"call_1","arguments":"{}"},
	           {"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	assistant := messages[0].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	name := calls[0].(map[string]any)["function"].(map[string]any)["name"]
	if name != "mcp__node_repl__js" {
		t.Fatalf("history call name=%v want flat upstream name", name)
	}
	if messages[1].(map[string]any)["tool_call_id"] != "call_1" {
		t.Fatalf("tool result lost its call id: %v", messages[1])
	}
}

func TestNamespaceToolChoiceMapsToFlatName(t *testing.T) {
	body := []byte(`{"model":"cn:deepseek-v4.1-flash","stream":false,"input":"hi","tool_choice":{"type":"function","name":"js","namespace":"mcp__node_repl"},"tools":[
	  {"type":"namespace","name":"mcp__node_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object"}}]}]}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	choice := chat["tool_choice"].(map[string]any)
	if got := choice["function"].(map[string]any)["name"]; got != "mcp__node_repl__js" {
		t.Fatalf("tool_choice name=%v", got)
	}
}

func TestNamespaceFlatNameCollisionDisambiguated(t *testing.T) {
	body := []byte(`{"model":"m","stream":false,"input":"hi","tools":[
	  {"type":"function","name":"ns__js","parameters":{"type":"object"}},
	  {"type":"namespace","name":"ns","tools":[{"type":"function","name":"js","parameters":{"type":"object"}}]}]}`)
	chatBody, req, err := responsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := chatToolsByName(t, chatBody)
	if tools["ns__js"] == nil || tools["ns__js__2"] == nil {
		t.Fatalf("collision not disambiguated: %v", tools)
	}
	alias := req.toolAliases["ns__js__2"]
	if alias.Namespace != "ns" || alias.Name != "js" {
		t.Fatalf("alias for disambiguated name wrong: %+v", alias)
	}
}

func TestNamespaceValidationRules(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty tools", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[]}]}`, "non-empty array"},
		{"nested namespace", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"namespace","name":"inner","tools":[{"type":"function","name":"f","parameters":{}}]}]}]}`, "cannot be nested"},
		{"missing name", `{"model":"m","input":"hi","tools":[{"type":"namespace","tools":[{"type":"function","name":"f","parameters":{}}]}]}`, "must be a nonempty string"},
		{"bad child", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"future_builtin"}]}]}`, "not supported"},
		{"declared builtin inside namespace", `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"web_search"}]}]}`, "not supported"},
	}
	for _, test := range cases {
		_, _, err := responsesToChat([]byte(test.body))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err=%v want %q", test.name, err, test.want)
		}
	}
	if _, _, err := responsesToChat([]byte(namespaceRequest)); err != nil {
		t.Fatalf("valid namespace request rejected: %v", err)
	}
	// chat/completions 不接受 Responses 专属的命名空间分组。
	if err := validateChatRequest([]byte(`{"model":"m","messages":[],"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","parameters":{}}]}]}`)); err == nil {
		t.Fatal("chat endpoint accepted a namespace tool")
	}
}

func TestNamespaceNonStreamCustomCallRestored(t *testing.T) {
	chat := map[string]any{
		"id": "ns1", "object": "chat.completion", "created": float64(1700000000), "model": "deepseek-v4.1-flash",
		"choices": []any{map[string]any{
			"index": float64(0), "finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_ns", "type": "function",
					"function": map[string]any{"name": "mcp__node_repl__js", "arguments": `{"code":"1"}`},
				}},
			},
		}},
	}
	req := &responsesRequest{
		customTools: map[string]bool{},
		toolAliases: map[string]toolAlias{"mcp__node_repl__js": {Namespace: "mcp__node_repl", Name: "js"}},
	}
	obj := chatToResponses(chat, "cn:deepseek-v4.1-flash", req)
	for _, raw := range obj["output"].([]any) {
		item := raw.(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		if item["name"] != "js" || item["namespace"] != "mcp__node_repl" {
			t.Fatalf("non-stream namespace call not restored: %v", item)
		}
		return
	}
	t.Fatalf("no function_call item: %v", obj["output"])
}
