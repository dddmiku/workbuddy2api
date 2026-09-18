// ═══ 更新日志 ═══
// 2026-09-18：复现多 choice 串流交叉污染与已完成流中的非法内容被静默忽略，锁定独立聚合和失败终态。
package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func protocolDiagnosisSSE(t *testing.T, frames ...map[string]any) string {
	t.Helper()
	var result strings.Builder
	for _, frame := range frames {
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		result.WriteString("data: " + string(raw) + "\n\n")
	}
	result.WriteString("data: [DONE]\n\n")
	return result.String()
}

func protocolDiagnosisChoice(index int, content, finish string, tools bool) map[string]any {
	delta := map[string]any{"content": content}
	if tools {
		delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call_" + content, "type": "function", "function": map[string]any{"name": content, "arguments": "{}"}}}
	}
	return map[string]any{"index": index, "delta": delta, "finish_reason": finish}
}

func TestProtocolDiagnosisStreamToolNamesAreScopedToChoice(t *testing.T) {
	raw := protocolDiagnosisSSE(t, map[string]any{"choices": []any{
		protocolDiagnosisChoice(0, "alpha", "tool_calls", true),
		protocolDiagnosisChoice(1, "beta", "tool_calls", true),
	}})
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	found := map[int]string{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		choices, _ := frame["choices"].([]any)
		for _, item := range choices {
			choice := item.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			calls, _ := delta["tool_calls"].([]any)
			for _, call := range calls {
				fn := call.(map[string]any)["function"].(map[string]any)
				name, _ := fn["name"].(string)
				found[int(choice["index"].(float64))] = name
			}
		}
	}
	if found[0] != "alpha" || found[1] != "beta" {
		t.Fatalf("choice 1 lost its independent tool name: %v", found)
	}
}

func TestProtocolDiagnosisAggregateKeepsChoicesIndependent(t *testing.T) {
	for _, tools := range []bool{false, true} {
		finish0, finish1 := "stop", "length"
		if tools {
			finish0, finish1 = "tool_calls", "tool_calls"
		}
		raw := protocolDiagnosisSSE(t, map[string]any{"choices": []any{
			protocolDiagnosisChoice(1, "beta", finish1, tools),
			protocolDiagnosisChoice(0, "alpha", finish0, tools),
		}})
		response, err := Aggregate(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(response)
		var decoded map[string]any
		_ = json.Unmarshal(encoded, &decoded)
		choices := decoded["choices"].([]any)
		if len(choices) != 2 {
			t.Fatalf("choices merged into one: %s", encoded)
		}
		for index, name := range []string{"alpha", "beta"} {
			choice := choices[index].(map[string]any)
			message := choice["message"].(map[string]any)
			if int(choice["index"].(float64)) != index || message["content"] != name {
				t.Fatalf("choice identity/content changed: %v", choice)
			}
			if tools {
				calls := message["tool_calls"].([]any)
				fn := calls[0].(map[string]any)["function"].(map[string]any)
				if len(calls) != 1 || fn["name"] != name || fn["arguments"] != "{}" {
					t.Fatalf("choices shared tool arguments: %v", calls)
				}
			} else if choice["finish_reason"] != []string{finish0, finish1}[index] {
				t.Fatal("choices shared a finish reason")
			}
		}
	}
}

func TestProtocolDiagnosisMalformedContentCannotDisappearAfterFinish(t *testing.T) {
	for _, field := range []string{"content", "reasoning_content", "refusal"} {
		for _, value := range []any{false, float64(3), map[string]any{"text": "lost"}, []any{"lost"}} {
			t.Run(field+"/"+strings.ReplaceAll(string(mustProtocolJSON(t, value)), "/", "_"), func(t *testing.T) {
				raw := protocolDiagnosisSSE(t,
					map[string]any{"choices": []any{protocolDiagnosisChoice(0, "prefix", "stop", false)}},
					map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{field: value}, "finish_reason": "stop"}}},
				)
				if _, err := Aggregate(strings.NewReader(raw)); err == nil {
					t.Error("malformed content became a successful aggregate")
				}
				rec := httptest.NewRecorder()
				if err := Stream(rec, strings.NewReader(raw)); err == nil {
					t.Error("malformed content became a successful stream")
				}
			})
		}
	}
}

func mustProtocolJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProtocolDiagnosisMalformedChoicesAndDeltaAreErrors(t *testing.T) {
	for _, frame := range []map[string]any{
		{"choices": map[string]any{"index": 0}},
		{"choices": []any{map[string]any{"index": 0, "delta": "lost", "finish_reason": "stop"}}},
		{"choices": []any{map[string]any{"index": 0, "message": "lost", "finish_reason": "stop"}}},
	} {
		raw := protocolDiagnosisSSE(t, map[string]any{"choices": []any{protocolDiagnosisChoice(0, "prefix", "stop", false)}}, frame)
		if _, err := Aggregate(strings.NewReader(raw)); err == nil {
			t.Errorf("malformed completion shape ignored: %v", frame)
		}
	}
}

func TestProtocolDiagnosisNullOutputFieldsRemainPlaceholders(t *testing.T) {
	raw := protocolDiagnosisSSE(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "kept", "refusal": nil, "reasoning_content": nil, "tool_calls": nil, "function_call": nil}, "finish_reason": "stop"}}})
	response, err := Aggregate(strings.NewReader(raw))
	if err != nil || response == nil {
		t.Fatalf("null placeholders rejected: %v", err)
	}
}
