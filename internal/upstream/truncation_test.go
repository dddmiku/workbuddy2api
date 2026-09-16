// ═══ 更新日志 ═══
// 2026-09-16：验证正常终态也拒绝残缺参数，并保留 length、合法 JSON 大数与无参数工具。
package upstream

import (
	"strings"
	"testing"
)

// TestIsTruncatedArguments 单元：空串（无参工具）合法；非空不可解析=截断；可解析=完整。
func TestIsTruncatedArguments(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},                 // 无参工具空分片
		{"   ", false},              // 纯空白
		{"{}", false},               // 合法空对象
		{`{"a":1}`, false},          // 合法对象
		{"null", false},             // 能解析（类型错误交给 schema，非截断）
		{"[1,2]", false},            // 能解析（数组非对象，非截断）
		{"1e999", false},            // 合法 JSON 数字，不由网关限制调用方数值精度
		{`{"command": "ls -`, true}, // 半截 JSON
		{`{"a":`, true},             // 半截键
		{`\deveco-code-rust\crates\deveco`, true}, // 非 JSON 文本
	}
	for _, c := range cases {
		if got := isTruncatedArguments(c.raw); got != c.want {
			t.Errorf("isTruncatedArguments(%q)=%v want %v", c.raw, got, c.want)
		}
	}
}

// TestDropTruncatedToolCalls 过滤残缺调用，完整调用正例零改动。
func TestDropTruncatedToolCalls(t *testing.T) {
	calls := []map[string]any{
		{"id": "ok", "function": map[string]any{"name": "read", "arguments": `{"file_path":"a.go"}`}},
		{"id": "bad", "function": map[string]any{"name": "read", "arguments": `{"file_path": "`}},
		{"id": "empty", "function": map[string]any{"name": "list_dir", "arguments": ""}},
		{"id": "nofn"},
	}
	out := dropTruncatedToolCalls(calls)
	if len(out) != 2 {
		t.Fatalf("kept=%d want 2, out=%#v", len(out), out)
	}
	if out[0]["id"] != "ok" || out[1]["id"] != "empty" {
		t.Fatalf("order/kept wrong: %#v", out)
	}
}

// TestAggregateTruncatedToolCalls finish_reason==length + 残缺 arguments →
// 不把脏参数交给客户端（tool_calls 整体剔除 / 只留完整）。
func TestAggregateTruncatedToolCalls(t *testing.T) {
	raw := `data: {"id":"x1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"call_bad","type":"function","function":{"name":"read","arguments":"{\"file_path\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish_reason=%v want length", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("truncated tool_calls should NOT be handed to client: %#v", msg["tool_calls"])
	}
}

// TestAggregateToolCallsLengthComplete 正例零改动：finish_reason==length 但参数完整
// （以及无参数空串）→ 原样保留。
func TestAggregateToolCallsLengthComplete(t *testing.T) {
	raw := `data: {"id":"x1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"file_path\":\"a.go\"}"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("complete tool_calls under length must be preserved: %#v", msg["tool_calls"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["arguments"] != `{"file_path":"a.go"}` {
		t.Fatalf("complete arguments mutated: %v", fn["arguments"])
	}
}

// TestAggregateToolCallsNormalFinishRejectsPartial 不允许成功终态掩盖残缺工具参数。
func TestAggregateToolCallsNormalFinishRejectsPartial(t *testing.T) {
	raw := `data: {"id":"x1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"file_path\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err == nil || resp != nil {
		t.Fatalf("partial tool arguments became a successful completion: resp=%#v err=%v", resp, err)
	}
}
