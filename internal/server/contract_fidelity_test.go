// ═══ 更新日志 ═══
// 2026-09-16：用真实 Codex 请求字段的最小形式锁定参数保真、结构化输出和异常终态。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

const contractRequest = `{"model":"cn:deepseek-v4.1-flash","input":"hello","stream":true,"reasoning":{"effort":"low","summary":"concise"},"parallel_tool_calls":false,"prompt_cache_key":"session-keep","metadata":{"conversation_id":"conv-keep"},"text":{"format":{"type":"json_schema","name":"result","strict":true,"schema":{"type":"object","properties":{"invoice_id":{"type":"integer"}},"required":["invoice_id"],"additionalProperties":false}}}}`

func TestContractRequestFieldsPreserved(t *testing.T) {
	body, _, err := responsesToChat([]byte(contractRequest))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"reasoning_effort": "low", "reasoning_summary": "concise", "parallel_tool_calls": false, "prompt_cache_key": "session-keep"} {
		if got[key] != want {
			t.Errorf("%s=%v want %v", key, got[key], want)
		}
	}
	format, ok := got["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("text.format lost: %s", body)
	}
	schema := format["json_schema"].(map[string]any)
	if schema["strict"] != true || schema["name"] != "result" {
		t.Fatalf("schema options lost: %v", schema)
	}
}

func TestContractUnsupportedContinuationRejected(t *testing.T) {
	_, _, err := responsesToChat([]byte(`{"model":"cn:deepseek-v4.1-flash","input":"continue","previous_response_id":"resp_previous"}`))
	if err == nil {
		t.Fatal("previous_response_id silently ignored instead of explicit unsupported error")
	}
}

func TestContractMalformedRootRejected(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"model":"x","input":"hi","text":{"format":{"type":"unknown"}}}`} {
		if _, _, err := responsesToChat([]byte(raw)); err == nil {
			t.Errorf("invalid request accepted: %s", raw)
		}
	}
}

func contractWriter(t *testing.T, request string, frames ...string) ([]string, []map[string]any) {
	t.Helper()
	_, req, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, req)
	rw.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range frames {
		_, err = rw.Write([]byte("data: " + frame + "\n\n"))
		if err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	return eventsOf(t, rec.Body.String())
}

func TestContractPartialErrorNeverCompletes(t *testing.T) {
	names, values := contractWriter(t, `{"model":"x","stream":true,"input":"hi"}`, `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`, `{"error":{"code":"upstream_error","message":"disconnected"}}`)
	last := values[len(values)-1]["response"].(map[string]any)
	if names[len(names)-1] != "response.failed" || last["status"] != "failed" || last["error"] == nil {
		t.Fatalf("error reported as %s: %v", names[len(names)-1], last)
	}
}

func TestContractLengthUsesIncompleteEventAndTools(t *testing.T) {
	names, values := contractWriter(t, `{"model":"x","stream":true,"input":"hi"}`, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"invoice_id\":"}}]}}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)
	last := values[len(values)-1]["response"].(map[string]any)
	if names[len(names)-1] != "response.incomplete" || last["status"] != "incomplete" {
		t.Errorf("terminal=%s status=%v", names[len(names)-1], last["status"])
	}
	for _, name := range names {
		if name == "response.function_call_arguments.done" {
			t.Error("truncated arguments emitted as done")
		}
	}
	for _, item := range last["output"].([]any) {
		entry := item.(map[string]any)
		if entry["type"] == "function_call" && entry["status"] == "completed" {
			t.Error("truncated tool marked completed")
		}
	}
}

func TestContractInvalidSchemaOutputNotReportedSuccess(t *testing.T) {
	for _, text := range []string{"plain paragraph", `{"invoice_id":"wrong-type"}`, `{"invoice_id":11128,"extra":true}`} {
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": "stop"}}})
		names, values := contractWriter(t, contractRequest, string(raw))
		last := values[len(values)-1]["response"].(map[string]any)
		if names[len(names)-1] != "response.failed" || last["status"] != "failed" {
			t.Errorf("invalid structured output succeeded: %s", text)
		}
	}
}

func TestContractValidSchemaAndResponseEcho(t *testing.T) {
	names, values := contractWriter(t, contractRequest, `{"choices":[{"index":0,"delta":{"content":"{\"invoice_id\":11128}"},"finish_reason":"stop"}]}`)
	last := values[len(values)-1]["response"].(map[string]any)
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("valid output failed: %v", last)
	}
	if last["parallel_tool_calls"] != false {
		t.Errorf("parallel control not echoed: %v", last["parallel_tool_calls"])
	}
	reasoning, ok := last["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" {
		t.Errorf("reasoning not echoed: %v", last["reasoning"])
	}
	text, _ := last["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Error("structured format not echoed")
	}
}

func TestContractJSONLengthIsIncomplete(t *testing.T) {
	chat := map[string]any{"choices": []any{map[string]any{"finish_reason": "length", "message": map[string]any{"role": "assistant", "content": "partial"}}}}
	got := chatToResponses(chat, "x", nil)
	if got["status"] != "incomplete" || got["incomplete_details"] == nil {
		t.Fatalf("truncated nonstream output=%v", got)
	}
}

func TestContractValidToolRoundDoesNotRequireFinalSchema(t *testing.T) {
	names, _ := contractWriter(t, contractRequest, `{"choices":[{"index":0,"delta":{"content":"checking","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"invoice_id\":11128}"}}]},"finish_reason":"tool_calls"}]}`)
	if !strings.Contains(strings.Join(names, ","), "response.completed") {
		t.Fatal("valid tool round blocked by final-output schema")
	}
}
