// ═══ 更新日志 ═══
// 2026-09-16：通过真实 SSE 转换链锁定迟到工具元数据、legacy 调用、refusal 及错误后的工具终态。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

func outputIntegrityRequest(t *testing.T, custom, stream bool) *responsesRequest {
	t.Helper()
	toolType, name := "function", "lookup"
	if custom {
		toolType, name = "custom", "exec"
	}
	raw, err := json.Marshal(map[string]any{
		"model": "cn:deepseek-v4.1-flash", "input": "run the task", "stream": stream,
		"tools": []any{map[string]any{"type": toolType, "name": name}},
		"text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "final", "strict": true,
			"schema": map[string]any{"type": "object", "properties": map[string]any{"result": map[string]any{"type": "string"}}, "required": []any{"result"}, "additionalProperties": false}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, req, err := responsesToChat(raw)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func outputIntegritySSE(frames ...string) string {
	return "data: " + strings.Join(frames, "\n\ndata: ") + "\n\ndata: [DONE]\n\n"
}

func outputIntegrityStream(t *testing.T, req *responsesRequest, raw string) ([]string, []map[string]any, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, req)
	err := upstream.Stream(rw, strings.NewReader(raw))
	rw.finish()
	names, values := eventsOf(t, rec.Body.String())
	return names, values, err
}

func outputIntegrityFinal(t *testing.T, names []string, values []map[string]any, want string) map[string]any {
	t.Helper()
	if len(names) == 0 || names[len(names)-1] != want {
		t.Fatalf("terminal=%v want=%s", names, want)
	}
	return values[len(values)-1]["response"].(map[string]any)
}

func outputIntegrityCall(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	items, ok := response["output"].([]any)
	if !ok {
		t.Fatalf("response has no output items: %#v", response)
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
			return item
		}
	}
	t.Fatalf("tool call was lost: %#v", response)
	return nil
}

func TestResponsesOutputLateToolMetadata(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name, prefix, suffix, wantArgs, wantType, idPrefix := "lookup", `{"invoice_id":`, "11128}", `{"invoice_id":11128}`, "function_call", "fc_"
		if custom {
			name, prefix, suffix, wantArgs, wantType, idPrefix = "exec", `{"input":"ec`, `ho ok"}`, "echo ok", "custom_tool_call", "ctc_"
		}
		t.Run(wantType, func(t *testing.T) {
			first, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "checking", "tool_calls": []any{map[string]any{"index": 0, "id": "call_late", "type": "function", "function": map[string]any{"arguments": prefix}}}}}}})
			last, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": name, "arguments": suffix}}}}, "finish_reason": "tool_calls"}}})
			names, values, err := outputIntegrityStream(t, outputIntegrityRequest(t, custom, true), outputIntegritySSE(string(first), string(last)))
			if err != nil {
				t.Fatal(err)
			}
			final := outputIntegrityFinal(t, names, values, "response.completed")
			call := outputIntegrityCall(t, final)
			id, _ := call["id"].(string)
			if call["type"] != wantType || call["name"] != name || call["call_id"] != "call_late" || !strings.HasPrefix(id, idPrefix) {
				t.Fatalf("unstable final tool identity: %#v", call)
			}
			added := 0
			var arguments strings.Builder
			for i, event := range names {
				if event == "response.output_item.added" {
					item := values[i]["item"].(map[string]any)
					if item["type"] == "message" || item["type"] == "reasoning" {
						continue
					}
					added++
					if item["type"] != wantType || item["name"] != name || item["id"] != id || item["call_id"] != "call_late" {
						t.Errorf("tool opened before metadata was known: %#v", item)
					}
					key := "arguments"
					if custom {
						key = "input"
					}
					if item[key] != "" {
						t.Errorf("added tool repeated buffered arguments: %#v", item)
					}
				}
				if event == "response.function_call_arguments.delta" {
					if custom {
						t.Error("custom tool emitted function arguments delta")
					}
					arguments.WriteString(values[i]["delta"].(string))
				}
				if custom && event == "response.function_call_arguments.done" {
					t.Error("custom tool emitted function arguments done")
				}
			}
			if added != 1 {
				t.Errorf("tool added %d times", added)
			}
			if custom {
				if call["input"] != wantArgs {
					t.Errorf("custom input=%v", call["input"])
				}
			} else if call["arguments"] != wantArgs || arguments.String() != wantArgs {
				t.Errorf("buffered arguments lost or duplicated: final=%v deltas=%q", call["arguments"], arguments.String())
			}
		})
	}
}

func TestResponsesOutputLegacyFunctionSurvivesSchema(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name, args, wantType := "lookup", `{"invoice_id":11128}`, "function_call"
		if custom {
			name, args, wantType = "exec", `{"input":"echo ok"}`, "custom_tool_call"
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "checking", "function_call": map[string]any{"name": name, "arguments": args}}, "finish_reason": "function_call"}}})
		raw := outputIntegritySSE(string(frame))
		t.Run(wantType+"/stream", func(t *testing.T) {
			names, values, err := outputIntegrityStream(t, outputIntegrityRequest(t, custom, true), raw)
			if err != nil {
				t.Fatal(err)
			}
			call := outputIntegrityCall(t, outputIntegrityFinal(t, names, values, "response.completed"))
			if call["type"] != wantType || call["name"] != name || call["call_id"] == "" {
				t.Fatalf("legacy call lost: %#v", call)
			}
			if custom {
				if call["input"] != "echo ok" {
					t.Errorf("legacy custom input lost: %#v", call)
				}
			} else if call["arguments"] != args {
				t.Errorf("legacy arguments lost: %#v", call)
			}
		})
		t.Run(wantType+"/aggregate", func(t *testing.T) {
			chat, err := upstream.Aggregate(strings.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			rw := newResponsesWriter(rec, outputIntegrityRequest(t, custom, false))
			if err := rw.ValidateCompletion(chat); err != nil {
				t.Fatalf("legacy tool round was checked as final JSON: %v", err)
			}
			body, _ := json.Marshal(chat)
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write(body)
			rw.finish()
			var result map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			call := outputIntegrityCall(t, result)
			if result["status"] != "completed" || call["type"] != wantType || call["name"] != name || call["call_id"] == "" {
				t.Fatalf("legacy aggregate lost: %#v", result)
			}
			if custom {
				if call["input"] != "echo ok" {
					t.Errorf("legacy aggregate custom input lost: %#v", call)
				}
			} else if call["arguments"] != args {
				t.Errorf("legacy aggregate arguments lost: %#v", call)
			}
		})
	}
}

func TestResponsesOutputRefusalSurvivesSchema(t *testing.T) {
	const refusal = "Cannot fulfill this request."
	raw := outputIntegritySSE(`{"choices":[{"index":0,"delta":{"refusal":"Cannot fulfill "}}]}`, `{"choices":[{"index":0,"delta":{"refusal":"this request."},"finish_reason":"stop"}]}`)
	names, values, err := outputIntegrityStream(t, outputIntegrityRequest(t, false, true), raw)
	if err != nil {
		t.Fatal(err)
	}
	final := outputIntegrityFinal(t, names, values, "response.completed")
	var deltas strings.Builder
	done := false
	for i, event := range names {
		if event == "response.refusal.delta" {
			deltas.WriteString(values[i]["delta"].(string))
		}
		if event == "response.refusal.done" {
			done = values[i]["refusal"] == refusal
		}
		if event == "response.output_text.delta" {
			t.Error("refusal misclassified as output_text")
		}
	}
	if deltas.String() != refusal || !done {
		t.Fatalf("refusal stream lost: deltas=%q done=%t", deltas.String(), done)
	}
	assertRefusal := func(result map[string]any) {
		t.Helper()
		found := false
		for _, value := range result["output"].([]any) {
			item := value.(map[string]any)
			if item["type"] != "message" {
				continue
			}
			for _, rawPart := range item["content"].([]any) {
				part := rawPart.(map[string]any)
				if part["type"] == "refusal" && part["refusal"] == refusal {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("refusal content lost: %#v", result)
		}
	}
	assertRefusal(final)
	chat, err := upstream.Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, outputIntegrityRequest(t, false, false))
	if err := rw.ValidateCompletion(chat); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(chat)
	_, _ = rw.Write(body)
	rw.finish()
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	assertRefusal(result)
}

func TestResponsesOutputErrorNeverCompletesTools(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name, args := "lookup", `{"invoice_id":11128}`
		if custom {
			name, args = "exec", `{"input":"echo ok"}`
		}
		for _, finished := range []bool{false, true} {
			frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "checking", "tool_calls": []any{map[string]any{"index": 0, "id": "call_error", "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}}}})
			frames := []string{string(frame)}
			if finished {
				frames = append(frames, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
			}
			frames = append(frames, `{"error":{"code":"upstream_error","message":"disconnected after partial content"}}`)
			names, values, err := outputIntegrityStream(t, outputIntegrityRequest(t, custom, true), outputIntegritySSE(frames...))
			if err == nil {
				t.Fatal("error stream succeeded")
			}
			final := outputIntegrityFinal(t, names, values, "response.failed")
			if final["status"] != "failed" || final["error"] == nil {
				t.Fatalf("failure lost: %#v", final)
			}
			for i, event := range names {
				if event == "response.function_call_arguments.done" || event == "response.completed" {
					t.Errorf("successful event escaped before failure: %s", event)
				}
				if event == "response.output_item.done" {
					item := values[i]["item"].(map[string]any)
					if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
						t.Errorf("failed tool emitted an execution-triggering done event: %#v", item)
					}
				}
			}
			if call := outputIntegrityCall(t, final); call["status"] == "completed" {
				t.Fatalf("failed tool completed: %#v", call)
			}
		}
	}
}

func TestResponsesOutputLateCallID(t *testing.T) {
	frames := outputIntegritySSE(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"exec","arguments":"{\"input\":\"echo ok\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late_id"}]},"finish_reason":"tool_calls"}]}`,
	)
	names, values, err := outputIntegrityStream(t, outputIntegrityRequest(t, true, true), frames)
	if err != nil {
		t.Fatal(err)
	}
	final := outputIntegrityFinal(t, names, values, "response.completed")
	call := outputIntegrityCall(t, final)
	for i, event := range names {
		if event == "response.output_item.added" {
			item := values[i]["item"].(map[string]any)
			if item["type"] == "custom_tool_call" && (item["call_id"] != "call_late_id" || item["id"] != call["id"]) {
				t.Fatalf("call_id changed after added: %#v -> %#v", item, call)
			}
		}
	}
	if call["call_id"] != "call_late_id" || call["input"] != "echo ok" {
		t.Fatalf("late call ID or buffered input lost: %#v", call)
	}
}

func TestResponsesOutputIndicesFollowActualItemOrder(t *testing.T) {
	raw := outputIntegritySSE(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"checking"},"finish_reason":"tool_calls"}]}`,
	)
	names, values, err := outputIntegrityStream(t, outputIntegrityRequest(t, false, true), raw)
	if err != nil {
		t.Fatal(err)
	}
	final := outputIntegrityFinal(t, names, values, "response.completed")
	items := final["output"].([]any)
	for i, event := range names {
		if event == "response.output_item.added" {
			index := int(values[i]["output_index"].(float64))
			item := values[i]["item"].(map[string]any)
			if index >= len(items) || items[index].(map[string]any)["id"] != item["id"] {
				t.Fatalf("output_index does not identify final item: event=%#v final=%#v", values[i], items)
			}
		}
	}
}

func TestResponsesOutputMissingToolNameFails(t *testing.T) {
	raw := outputIntegritySSE(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_no_name","type":"function","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	names, values, _ := outputIntegrityStream(t, outputIntegrityRequest(t, false, true), raw)
	final := outputIntegrityFinal(t, names, values, "response.failed")
	if final["error"] == nil {
		t.Fatalf("missing name failure has no diagnostic: %#v", final)
	}
	for _, event := range names {
		if event == "response.function_call_arguments.done" {
			t.Fatal("nameless tool arguments were marked executable")
		}
	}
}

func TestResponsesOutputIncompleteNeverCompletesTools(t *testing.T) {
	for _, reason := range []string{"length", "content_filter"} {
		raw := outputIntegritySSE(
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_partial","type":"function","function":{"name":"lookup","arguments":"{\"invoice_id\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"`+reason+`"}]}`,
		)
		names, values, err := outputIntegrityStream(t, outputIntegrityRequest(t, false, true), raw)
		if err != nil {
			t.Fatal(err)
		}
		final := outputIntegrityFinal(t, names, values, "response.incomplete")
		if call := outputIntegrityCall(t, final); call["status"] != "incomplete" {
			t.Fatalf("partial tool lost incomplete status: %#v", call)
		}
		for i, event := range names {
			if event == "response.function_call_arguments.done" {
				t.Error("partial arguments emitted a done event")
			}
			if event == "response.output_item.done" {
				item := values[i]["item"].(map[string]any)
				if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
					t.Errorf("partial tool emitted execution-triggering done event: %#v", item)
				}
			}
		}
	}
}
