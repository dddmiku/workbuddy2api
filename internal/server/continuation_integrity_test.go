// ═══ 更新日志 ═══
// 2026-09-18：为工具终态缺失调用、非法工具列表及命名空间子工具丢失补回归，避免协议损坏被伪装为成功正文。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

const continuationIntegrityProse = "Let me run the remaining lookup."

type continuationIntegrityReply struct {
	response map[string]any
	names    []string
	values   []map[string]any
	code     int
	err      error
}

func continuationIntegrityRequest(t *testing.T, stream bool) *responsesRequest {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model": "cn:deepseek-v4.1-flash", "stream": stream, "input": "finish the lookup",
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 不加 text.format，确保失败来自工具协议，而非预告正文不符合 JSON schema。
	_, req, err := responsesToChat(raw)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func continuationIntegrityFrame(t *testing.T, output map[string]any, reason string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"index": 0, "delta": output, "finish_reason": reason,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func continuationIntegrityJSON(t *testing.T, req *responsesRequest, chat map[string]any) continuationIntegrityReply {
	t.Helper()
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, req)
	if err := rw.ValidateCompletion(chat); err != nil {
		return continuationIntegrityReply{err: err}
	}
	body, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	rw.Header().Set("Content-Type", "application/json")
	if _, err := rw.Write(body); err != nil {
		return continuationIntegrityReply{err: err}
	}
	rw.finish()
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("non-stream response is not JSON: %v", err)
	}
	return continuationIntegrityReply{response: response, code: rec.Code}
}

func continuationIntegritySSE(t *testing.T, req *responsesRequest, frames ...string) continuationIntegrityReply {
	t.Helper()
	raw := outputIntegritySSE(frames...)
	if !req.Stream {
		chat, err := upstream.Aggregate(strings.NewReader(raw))
		if err != nil {
			return continuationIntegrityReply{err: err}
		}
		return continuationIntegrityJSON(t, req, chat)
	}
	names, values, err := outputIntegrityStream(t, req, raw)
	var response map[string]any
	if len(values) > 0 {
		response, _ = values[len(values)-1]["response"].(map[string]any)
	}
	return continuationIntegrityReply{response: response, names: names, values: values, code: http.StatusOK, err: err}
}

func continuationIntegrityComplete(t *testing.T, mode string, fields map[string]any, reason string) continuationIntegrityReply {
	t.Helper()
	req := continuationIntegrityRequest(t, mode == "stream")
	output := map[string]any{"content": continuationIntegrityProse}
	for key, value := range fields {
		output[key] = value
	}
	if mode == "json" {
		return continuationIntegrityJSON(t, req, map[string]any{"choices": []any{map[string]any{
			"index": 0, "message": output, "finish_reason": reason,
		}}})
	}
	return continuationIntegritySSE(t, req, continuationIntegrityFrame(t, output, reason))
}

func continuationIntegrityMustFail(t *testing.T, reply continuationIntegrityReply, stream bool) {
	t.Helper()
	if !stream {
		if reply.err != nil {
			return
		}
		if (reply.code < http.StatusBadRequest && reply.response["status"] != "failed") || reply.response["error"] == nil {
			t.Fatalf("invalid tool output was accepted: http=%d status=%v", reply.code, reply.response["status"])
		}
		return
	}
	final := outputIntegrityFinal(t, reply.names, reply.values, "response.failed")
	if final["status"] != "failed" || final["error"] == nil {
		t.Fatalf("invalid tool output has no failed response diagnostic: status=%v", final["status"])
	}
	for i, event := range reply.names {
		if event == "response.completed" || event == "response.function_call_arguments.done" {
			t.Errorf("successful event escaped from invalid tool output: %s", event)
		}
		if event == "response.output_item.done" {
			item, _ := reply.values[i]["item"].(map[string]any)
			if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
				t.Error("invalid tool output emitted an executable item")
			}
		}
	}
}

func continuationIntegrityMustComplete(t *testing.T, reply continuationIntegrityReply, stream bool) {
	t.Helper()
	if reply.err != nil || reply.code >= http.StatusBadRequest || reply.response["status"] != "completed" {
		t.Fatalf("valid completion rejected: http=%d status=%v err=%v", reply.code, reply.response["status"], reply.err)
	}
	if stream {
		outputIntegrityFinal(t, reply.names, reply.values, "response.completed")
	}
}

func continuationIntegrityCalls(response map[string]any) []map[string]any {
	var calls []map[string]any
	items, _ := response["output"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
			calls = append(calls, item)
		}
	}
	return calls
}

func TestContinuationIntegrityToolFinishRequiresActualCall(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"absent", nil},
		{"null_tools", map[string]any{"tool_calls": nil}},
		{"empty_tools", map[string]any{"tool_calls": []any{}}},
		{"empty_legacy_placeholder", map[string]any{"function_call": map[string]any{"name": "", "arguments": ""}}},
	}
	for _, reason := range []string{"tool_calls", "function_call"} {
		for _, test := range cases {
			for _, mode := range []string{"stream", "aggregate", "json"} {
				t.Run(reason+"/"+test.name+"/"+mode, func(t *testing.T) {
					reply := continuationIntegrityComplete(t, mode, test.fields, reason)
					continuationIntegrityMustFail(t, reply, mode == "stream")
				})
			}
		}
	}
}

func TestContinuationIntegrityMalformedToolListCannotBecomeCompletedProse(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"object", map[string]any{"id": "call_wrong_shape", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}},
		{"string", "lookup"},
		{"number", float64(1)},
		{"boolean", false},
	}
	for _, test := range cases {
		for _, mode := range []string{"stream", "aggregate", "json"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				// stop 也必须校验列表形状；仅检查工具 finish 会漏掉这类静默丢失。
				reply := continuationIntegrityComplete(t, mode, map[string]any{"tool_calls": test.value}, "stop")
				continuationIntegrityMustFail(t, reply, mode == "stream")
			})
		}
	}
}

func TestContinuationIntegrityEmptyToolFieldsAllowNormalStop(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"absent", nil},
		{"null", map[string]any{"tool_calls": nil, "function_call": nil}},
		{"empty_array", map[string]any{"tool_calls": []any{}}},
		{"empty_legacy_placeholder", map[string]any{"function_call": map[string]any{"name": "", "arguments": ""}}},
	}
	for _, test := range cases {
		for _, mode := range []string{"stream", "aggregate", "json"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				reply := continuationIntegrityComplete(t, mode, test.fields, "stop")
				continuationIntegrityMustComplete(t, reply, mode == "stream")
				if calls := continuationIntegrityCalls(reply.response); len(calls) != 0 {
					t.Fatalf("empty tool field created %d calls", len(calls))
				}
				var text strings.Builder
				items, _ := reply.response["output"].([]any)
				for _, raw := range items {
					item, _ := raw.(map[string]any)
					parts, _ := item["content"].([]any)
					for _, rawPart := range parts {
						part, _ := rawPart.(map[string]any)
						if part["type"] == "output_text" {
							value, _ := part["text"].(string)
							text.WriteString(value)
						}
					}
				}
				if text.String() != continuationIntegrityProse {
					t.Fatal("normal stop text was changed or discarded")
				}
			})
		}
	}
}

func TestContinuationIntegrityLateCallsBeforeDoneSurvive(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		reason := "tool_calls"
		function := map[string]any{"name": "lookup", "arguments": `{"invoice_id":11128}`}
		fields := map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_late", "type": "function", "function": function,
		}}}
		if legacy {
			reason = "function_call"
			fields = map[string]any{"function_call": function}
		}
		for _, firstFinish := range []string{"stop", reason} {
			for _, mode := range []string{"stream", "aggregate"} {
				t.Run(reason+"/after_"+firstFinish+"/"+mode, func(t *testing.T) {
					req := continuationIntegrityRequest(t, mode == "stream")
					reply := continuationIntegritySSE(t, req,
						continuationIntegrityFrame(t, map[string]any{"content": continuationIntegrityProse}, firstFinish),
						continuationIntegrityFrame(t, fields, reason),
					)
					continuationIntegrityMustComplete(t, reply, req.Stream)
					calls := continuationIntegrityCalls(reply.response)
					if len(calls) != 1 {
						t.Fatalf("late call was lost or duplicated: calls=%d", len(calls))
					}
					call := calls[0]
					if call["type"] != "function_call" || call["name"] != "lookup" || call["arguments"] != `{"invoice_id":11128}` || call["status"] != "completed" {
						t.Fatal("late call identity, arguments, or completion status changed")
					}
					callID, _ := call["call_id"].(string)
					if callID == "" || (!legacy && callID != "call_late") {
						t.Fatal("late call lost its correlation ID")
					}
				})
			}
		}
	}
}

func TestContinuationIntegrityNestedNamespaceFunctionIsPreservedOrRejected(t *testing.T) {
	const raw = `{"model":"cn:deepseek-v4.1-flash","input":"run lookup","tools":[{"type":"namespace","name":"billing","tools":[{"type":"function","function":{"name":"lookup","description":"Read one invoice","parameters":{"type":"object","properties":{"invoice_id":{"type":"integer"}},"required":["invoice_id"],"additionalProperties":false},"strict":true}}]}]}`
	for _, mode := range []string{"stream", "aggregate"} {
		t.Run(mode, func(t *testing.T) {
			body, req, err := responsesToChat([]byte(raw))
			if err != nil {
				t.Log("nested namespace function was explicitly rejected")
				return
			}
			chat := decodeChat(t, body)
			tools, _ := chat["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("accepted namespace child disappeared: forwarded tools=%d", len(tools))
			}
			tool, _ := tools[0].(map[string]any)
			function, _ := tool["function"].(map[string]any)
			name, _ := function["name"].(string)
			wantParameters := map[string]any{
				"type": "object", "properties": map[string]any{"invoice_id": map[string]any{"type": "integer"}},
				"required": []any{"invoice_id"}, "additionalProperties": false,
			}
			if name == "" || function["description"] != "Read one invoice" || function["strict"] != true || !reflect.DeepEqual(function["parameters"], wantParameters) {
				t.Fatal("accepted nested function lost its name, description, strictness, or parameter schema")
			}
			req.Stream = mode == "stream"
			reply := continuationIntegritySSE(t, req, continuationIntegrityFrame(t, map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "call_invoice", "type": "function",
					"function": map[string]any{"name": name, "arguments": `{"invoice_id":11128}`}}},
			}, "tool_calls"))
			continuationIntegrityMustComplete(t, reply, req.Stream)
			calls := continuationIntegrityCalls(reply.response)
			if len(calls) != 1 || calls[0]["type"] != "function_call" || calls[0]["namespace"] != "billing" || calls[0]["name"] != "lookup" || calls[0]["call_id"] != "call_invoice" || calls[0]["arguments"] != `{"invoice_id":11128}` {
				t.Fatal("accepted nested function did not round-trip to its client namespace and name")
			}
		})
	}
}
