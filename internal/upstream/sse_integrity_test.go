// ═══ 更新日志 ═══
// 2026-09-16：覆盖真实审计发现的错误吞没、异常 EOF 和工具残参假成功，并保留合法终态兼容。
// 2026-09-16：复核完整 message 快照去重/保真，以及新旧工具协议的参数缺失、null 与类型错误。
package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const integrityContent = `{"id":"integrity-1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"partial 11128"}}]}`

func integrityEvent(payload string) string { return "data: " + payload + "\n\n" }

func integrityFinish(reason string) string {
	return integrityEvent(`{"choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}`)
}

func integrityTool(args string) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_integrity", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": args},
		}}},
	}}})
	return integrityEvent(string(raw))
}

func integrityErrorFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatalf("output contained malformed JSON: %q: %v", line, err)
		}
		if e, ok := frame["error"].(map[string]any); ok {
			found = append(found, e)
		}
	}
	return found
}

func TestIntegrityFailuresAreNotCompletions(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"upstream error", integrityEvent(`{"error":{"code":"upstream_error","message":"quota service unavailable"}}`) + integrityEvent("[DONE]")},
		{"error after partial output", integrityEvent(integrityContent) + integrityEvent(`{"error":{"type":"server_error","message":"failed"}}`)},
		{"error after finish", integrityEvent(integrityContent) + integrityFinish("stop") + integrityEvent(`{"error":{"message":"failed"}}`)},
		{"error SSE event", "event: error\ndata: {\"code\":\"busy\",\"message\":\"service unavailable\"}\n\n"},
		{"abrupt EOF", integrityEvent(integrityContent)},
		{"empty stream", ""},
		{"DONE only", integrityEvent("[DONE]")},
		{"usage only", integrityEvent(`{"choices":[],"usage":{"total_tokens":2}}`) + integrityEvent("[DONE]")},
		{"role only", integrityEvent(`{"choices":[{"delta":{"role":"assistant"}}]}`) + integrityEvent("[DONE]")},
		{"malformed data", integrityEvent(integrityContent) + integrityEvent(`{"choices":`) + integrityEvent("[DONE]")},
		{"null payload", integrityEvent("null") + integrityEvent("[DONE]")},
		{"unknown JSON only", integrityEvent(`{"status":"ok"}`) + integrityEvent("[DONE]")},
		{"DONE prefix is not DONE", integrityEvent(integrityContent) + integrityEvent("[DONE]garbage")},
		{"unknown terminal reason", integrityEvent(integrityContent) + integrityFinish("failed") + integrityEvent("[DONE]")},
		{"tool arguments partial despite tool finish", integrityTool(`{"id":`) + integrityFinish("tool_calls") + integrityEvent("[DONE]")},
		{"tool arguments partial despite stop", integrityTool(`{"id":`) + integrityFinish("stop") + integrityEvent("[DONE]")},
		{"tool arguments partial despite DONE", integrityTool(`{"id":`) + integrityEvent("[DONE]")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("aggregate", func(t *testing.T) {
				got, err := Aggregate(strings.NewReader(tc.raw))
				if err == nil || got != nil {
					t.Fatalf("failure became a completion: got=%v err=%v", got, err)
				}
			})
			t.Run("stream", func(t *testing.T) {
				rec := httptest.NewRecorder()
				if err := Stream(rec, strings.NewReader(tc.raw)); err == nil {
					t.Fatalf("failure returned nil error: %s", rec.Body)
				}
				if got := integrityErrorFrames(t, rec.Body.String()); len(got) != 1 {
					t.Fatalf("want exactly one usable error frame, got=%v body=%s", got, rec.Body)
				}
				if got := strings.Count(rec.Body.String(), "data: [DONE]\n\n"); got != 1 {
					t.Fatalf("want one transport closing delimiter, got=%d body=%s", got, rec.Body)
				}
			})
		})
	}
}

func TestIntegrityUpstreamErrorDetailsSurvive(t *testing.T) {
	const rawError = `{"message":"quota service unavailable","code":"quota_exceeded","type":"server_error","retry_after":12,"details":{"scope":"model"}}`
	var want map[string]any
	if err := json.Unmarshal([]byte(rawError), &want); err != nil {
		t.Fatal(err)
	}
	frame := map[string]any{"error": want}
	normalized := normalizeFrame(frame)
	if !reflect.DeepEqual(normalized["error"], want) {
		t.Fatalf("normalization stripped or changed error: %#v", normalized)
	}
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(integrityEvent(`{"error":`+rawError+`}`)+integrityEvent("[DONE]"))); err == nil {
		t.Fatal("upstream error returned success")
	}
	got := integrityErrorFrames(t, rec.Body.String())
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("error details lost: got=%v want=%v", got, want)
	}
}

func TestIntegrityAcceptsLegitimateTerminals(t *testing.T) {
	cases := []struct{ name, raw, finish string }{
		{"explicit DONE", integrityEvent(integrityContent) + integrityEvent("[DONE]"), "stop"},
		{"finish without DONE", integrityEvent(integrityContent) + integrityFinish("stop"), "stop"},
		{"finish at EOF without separator", integrityEvent(integrityContent) + strings.TrimRight(integrityFinish("stop"), "\n"), "stop"},
		{"empty but explicitly finished", integrityFinish("stop"), "stop"},
		{"length without DONE", integrityEvent(integrityContent) + integrityFinish("length"), "length"},
		{"content filter", integrityEvent(integrityContent) + integrityFinish("content_filter") + integrityEvent("[DONE]"), "content_filter"},
		{"arguments intact", integrityTool(`{"id":11128}`) + integrityFinish("tool_calls"), "tool_calls"},
		{"empty tool arguments", integrityTool("") + integrityFinish("tool_calls"), "tool_calls"},
		{"valid scalar arguments", integrityTool("123") + integrityFinish("tool_calls"), "tool_calls"},
		{"legacy function finish", integrityEvent(integrityContent) + integrityFinish("function_call"), "function_call"},
		{"ignore after DONE", integrityEvent(integrityContent) + integrityEvent("[DONE]") + integrityEvent(`{"error":{"message":"ignored"}}`), "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Aggregate(strings.NewReader(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if fr := got["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != tc.finish {
				t.Fatalf("finish_reason=%v want=%s", fr, tc.finish)
			}
			rec := httptest.NewRecorder()
			if err := Stream(rec, strings.NewReader(tc.raw)); err != nil {
				t.Fatal(err)
			}
			if got := integrityErrorFrames(t, rec.Body.String()); len(got) != 0 {
				t.Fatalf("unexpected error: %v", got)
			}
			if n := strings.Count(rec.Body.String(), "data: [DONE]\n\n"); n != 1 {
				t.Fatalf("DONE count=%d", n)
			}
		})
	}
}

func TestIntegritySSEFramingAndUsage(t *testing.T) {
	raw := ": heartbeat\r\n\r\nevent: message\r\ndata:{\"id\":\"integrity-1\",\r\ndata: \"model\":\"deepseek-v4.1-flash\",\r\ndata:\"choices\":[{\"index\":0,\"delta\":{\"content\":\"11128\",\"reasoning_content\":\"thinking\"}}]}\r\n\r\n" +
		"data:{\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
		"data: {\"choices\":[],\"usage\":{\"total_tokens\":17}}\r\n\r\ndata:[DONE]\r\n\r\n"
	got, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "11128" || msg["reasoning_content"] != "thinking" {
		t.Fatalf("lost content: %v", msg)
	}
	if got["usage"].(map[string]any)["total_tokens"] != float64(17) {
		t.Fatalf("lost usage: %v", got["usage"])
	}
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if strings.Count(rec.Body.String(), "data: [DONE]") != 1 || !strings.Contains(rec.Body.String(), `"total_tokens":17`) || !strings.Contains(rec.Body.String(), `"reasoning_content":"thinking"`) {
		t.Fatalf("normalized stream lost SSE data: %s", rec.Body)
	}
}

func TestIntegrityIncompleteTerminalsKeepTheirMeaning(t *testing.T) {
	for _, reason := range []string{"length", "content_filter"} {
		t.Run(reason, func(t *testing.T) {
			raw := integrityTool(`{"id":`) + integrityFinish(reason) + integrityEvent("[DONE]")
			got, err := Aggregate(strings.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			choice := got["choices"].([]any)[0].(map[string]any)
			if choice["finish_reason"] != reason {
				t.Fatalf("incomplete reason lost: %v", choice)
			}
			if _, exists := choice["message"].(map[string]any)["tool_calls"]; exists {
				t.Fatalf("incomplete aggregate exposed executable partial arguments: %v", choice)
			}
			rec := httptest.NewRecorder()
			if err := Stream(rec, strings.NewReader(raw)); err != nil {
				t.Fatal(err)
			}
			if got := integrityErrorFrames(t, rec.Body.String()); len(got) != 0 {
				t.Fatalf("explicit incomplete became transport error: %v", got)
			}
			if !strings.Contains(rec.Body.String(), `"finish_reason":"`+reason+`"`) {
				t.Fatalf("stream lost incomplete reason: %s", rec.Body)
			}
		})
	}
}

func TestIntegrityPartialToolNeverEmitsSuccessfulFinish(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(integrityTool(`{"id":`)+integrityFinish("tool_calls")+integrityEvent("[DONE]")))
	if err == nil {
		t.Fatal("partial tool call succeeded")
	}
	if strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("partial tool was marked executable before error: %s", rec.Body)
	}
	if i, j := strings.Index(rec.Body.String(), `"error"`), strings.Index(rec.Body.String(), "data: [DONE]"); i < 0 || j < i {
		t.Fatalf("failure must precede closing delimiter: %s", rec.Body)
	}
}

type integrityReadError struct{ err error }

func (r integrityReadError) Read([]byte) (int, error) { return 0, r.err }

func TestIntegrityReadFailureIsVisibleAndUnwraps(t *testing.T) {
	want := errors.New("synthetic transport reset")
	reader := func() io.Reader {
		return io.MultiReader(strings.NewReader(integrityEvent(integrityContent)), integrityReadError{want})
	}
	if got, err := Aggregate(reader()); got != nil || !errors.Is(err, want) {
		t.Fatalf("aggregate got=%v err=%v", got, err)
	}
	rec := httptest.NewRecorder()
	if err := Stream(rec, reader()); !errors.Is(err, want) {
		t.Fatalf("stream err=%v", err)
	}
	if got := integrityErrorFrames(t, rec.Body.String()); len(got) != 1 {
		t.Fatalf("missing read failure frame: %s", rec.Body)
	}
}

func TestIntegrityDONEStopsBeforeTrailingReadFailure(t *testing.T) {
	want := errors.New("must not read after DONE")
	reader := func() io.Reader {
		return io.MultiReader(strings.NewReader(integrityEvent(integrityContent)+integrityEvent("[DONE]")), integrityReadError{want})
	}
	if _, err := Aggregate(reader()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := Stream(rec, reader()); err != nil {
		t.Fatal(err)
	}
}

type integrityWriteError struct {
	h   http.Header
	err error
}

func (w *integrityWriteError) Header() http.Header       { return w.h }
func (w *integrityWriteError) WriteHeader(int)           {}
func (w *integrityWriteError) Write([]byte) (int, error) { return 0, w.err }

func TestIntegrityWriterFailurePropagates(t *testing.T) {
	want := errors.New("synthetic client disconnect")
	w := &integrityWriteError{h: make(http.Header), err: want}
	if err := Stream(w, strings.NewReader(integrityEvent(integrityContent)+integrityEvent("[DONE]"))); !errors.Is(err, want) {
		t.Fatalf("write error lost: %v", err)
	}
}

func integritySnapshot(fn map[string]any, legacy bool) string {
	message := map[string]any{"role": "assistant", "content": "11128", "reasoning_content": "thinking", "refusal": "refusal-text"}
	reason := "tool_calls"
	if legacy {
		message["function_call"] = fn
		reason = "function_call"
	} else {
		message["tool_calls"] = []any{map[string]any{"id": "call_integrity", "type": "function", "function": fn}}
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}}})
	return integrityEvent(string(raw))
}

func integrityMessageResult(t *testing.T, raw string) map[string]any {
	t.Helper()
	got, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
}

func TestIntegrityMessageSnapshotsAreLosslessAndNotDuplicated(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "tool_calls"
		if legacy {
			name = "function_call"
		}
		t.Run(name, func(t *testing.T) {
			snapshot := integritySnapshot(map[string]any{"name": "lookup", "arguments": "{}"}, legacy)
			prefix := integrityTool("{}")
			if legacy {
				prefix = integrityEvent(`{"choices":[{"delta":{"function_call":{"name":"lookup","arguments":"{}"}}}]}`)
			}
			prefix = integrityEvent(`{"choices":[{"delta":{"content":"111","reasoning_content":"think","refusal":"refusal-"}}]}`) + prefix
			for _, raw := range []string{snapshot + integrityEvent("[DONE]"), prefix + snapshot + snapshot + integrityEvent("[DONE]")} {
				check := func(got map[string]any) {
					if got["content"] != "11128" || got["reasoning_content"] != "thinking" || got["refusal"] != "refusal-text" {
						t.Fatalf("snapshot message lost or duplicated: %v", got)
					}
					var fn map[string]any
					if legacy {
						fn, _ = got["function_call"].(map[string]any)
					} else {
						calls, _ := got["tool_calls"].([]map[string]any)
						if len(calls) != 1 {
							t.Fatalf("snapshot tools lost or duplicated: %v", got)
						}
						fn, _ = calls[0]["function"].(map[string]any)
					}
					if fn["name"] != "lookup" || fn["arguments"] != "{}" {
						t.Fatalf("snapshot arguments lost/duplicated: %v", fn)
					}
				}
				check(integrityMessageResult(t, raw))
				rec := httptest.NewRecorder()
				if err := Stream(rec, strings.NewReader(raw)); err != nil {
					t.Fatal(err)
				}
				check(integrityMessageResult(t, rec.Body.String()))
			}
		})
	}
}

func TestIntegrityToolArgumentsPresenceAndType(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			fn    map[string]any
			valid bool
		}{
			{"missing", map[string]any{"name": "lookup"}, false},
			{"null", map[string]any{"name": "lookup", "arguments": nil}, false},
			{"object", map[string]any{"name": "lookup", "arguments": map[string]any{}}, false},
			{"empty string", map[string]any{"name": "lookup", "arguments": ""}, true},
		} {
			t.Run(tc.name+map[bool]string{false: " tool", true: " legacy"}[legacy], func(t *testing.T) {
				raw := integritySnapshot(tc.fn, legacy) + integrityEvent("[DONE]")
				resp, aggErr := Aggregate(strings.NewReader(raw))
				rec := httptest.NewRecorder()
				streamErr := Stream(rec, strings.NewReader(raw))
				if tc.valid {
					if aggErr != nil || streamErr != nil {
						t.Fatalf("explicit empty string rejected: %v / %v", aggErr, streamErr)
					}
					msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
					var fn map[string]any
					if legacy {
						fn, _ = msg["function_call"].(map[string]any)
					} else {
						calls, _ := msg["tool_calls"].([]map[string]any)
						if len(calls) != 1 {
							t.Fatalf("empty-argument tool missing: %v", msg)
						}
						fn, _ = calls[0]["function"].(map[string]any)
					}
					if value, exists := fn["arguments"]; !exists || value != "" {
						t.Fatalf("empty string was erased: %v", fn)
					}
				} else if aggErr == nil || streamErr == nil {
					t.Fatalf("invalid tool arguments became success: aggregate=%v stream=%v resp=%v", aggErr, streamErr, resp)
				}
			})
		}
	}
}

func TestIntegrityIntermediateMissingArgumentsCanComplete(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		prefix := integrityEvent(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_integrity","type":"function","function":{"name":"lookup"}}]}}]}`)
		if legacy {
			prefix = integrityEvent(`{"choices":[{"delta":{"function_call":{"name":"lookup"}}}]}`)
		}
		raw := prefix + integritySnapshot(map[string]any{"name": "lookup", "arguments": "{}"}, legacy) + integrityEvent("[DONE]")
		_ = integrityMessageResult(t, raw)
		rec := httptest.NewRecorder()
		if err := Stream(rec, strings.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		_ = integrityMessageResult(t, rec.Body.String())
	}
}

func TestIntegrityLateToolNameFromSnapshotSurvives(t *testing.T) {
	raw := integrityEvent(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_integrity","type":"function","function":{"arguments":"{}"}}]}}]}`) +
		integritySnapshot(map[string]any{"name": "lookup", "arguments": "{}"}, false) + integrityEvent("[DONE]")
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	msg := integrityMessageResult(t, rec.Body.String())
	calls, _ := msg["tool_calls"].([]map[string]any)
	if len(calls) != 1 || calls[0]["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("late tool name lost: %v", msg)
	}
}

func TestIntegrityConflictingSnapshotFails(t *testing.T) {
	raw := integrityTool(`{"id":1}`) + integritySnapshot(map[string]any{"name": "lookup", "arguments": `{"id":2}`}, false) + integrityEvent("[DONE]")
	if got, err := Aggregate(strings.NewReader(raw)); err == nil || got != nil {
		t.Fatalf("conflicting snapshot accepted: %v %v", got, err)
	}
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err == nil {
		t.Fatalf("conflicting snapshot accepted: %s", rec.Body)
	}
}
