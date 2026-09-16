// ═══ 更新日志 ═══
// 2026-09-16：从 HTTP 入口贯穿上游 SSE、Responses 终态与账号统计，防止失败/截断在适配层变成成功。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestGatewayIntegrityInvalidJSONDoesNotReachUpstream(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, body := range []string{"", "null", "[]", "{broken", `{"stream":"true"}`} {
			t.Run(route+" "+body, func(t *testing.T) {
				calls := 0
				up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })
				p := testPoolWith(&auth.Auth{UID: "integrity", AccessToken: "at-integrity", ExpiresAt: 9999999999})
				h := NewHandler(Config{Pool: p, Upstream: up})
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", route, strings.NewReader(body)))
				if rec.Code != 400 || calls != 0 {
					t.Fatalf("invalid input was dispatched: status=%d calls=%d body=%s", rec.Code, calls, rec.Body)
				}
				var envelope map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || envelope["error"] == nil {
					t.Fatalf("missing JSON error: %s", rec.Body)
				}
				state, _ := p.Status("integrity")
				if state.SuccessCount != 0 || state.ErrTotal != 0 || state.InFlight != 0 {
					t.Fatalf("invalid input changed account state: %+v", state)
				}
			})
		}
	}
}

func TestGatewayIntegrityClientErrorsDoNotRotate(t *testing.T) {
	for _, status := range []int{400, 422} {
		body := `{"error":"unsupported requested option"}`
		if kind := upstream.Classify(status, body); kind != upstream.ErrClient {
			t.Fatalf("fixture must exercise ErrClient, got %v", kind)
		}
		calls := 0
		up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return status, body, false })
		p := testPoolWith(
			&auth.Auth{UID: "first", AccessToken: "first-token", ExpiresAt: 9999999999},
			&auth.Auth{UID: "second", AccessToken: "second-token", ExpiresAt: 9999999999},
		)
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		if rec.Code != 400 || calls != 1 {
			t.Fatalf("client error retried or misclassified: upstream=%d status=%d calls=%d body=%s", status, rec.Code, calls, rec.Body)
		}
		for _, uid := range []string{"first", "second"} {
			state, _ := p.Status(uid)
			if state.SuccessCount != 0 || state.ErrTotal != 0 || state.Cooling || state.Disabled || state.InFlight != 0 || state.BreakerFails != 0 {
				t.Fatalf("client error penalized account %s: %+v", uid, state)
			}
		}
	}
}

func TestGatewayIntegrityResponsesTerminalsEndToEnd(t *testing.T) {
	const text = `{"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"partial 11128"}}]}`
	const partialTool = `{"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_partial","type":"function","function":{"name":"lookup","arguments":"{\"id\":"}}]}}]}`
	const terminalError = `{"error":{"code":"upstream_error","message":"upstream unavailable"}}`
	cases := []struct{ name, raw, status, detail string }{
		{"initial error", string(sseStream(terminalError)), "failed", ""},
		{"error after output", string(sseStream(text, terminalError)), "failed", ""},
		{"EOF after output", "data: " + text + "\n\n", "failed", ""},
		{"invalid SSE JSON", string(sseStream(text, "{broken")), "failed", ""},
		{"usage without output", string(sseStream(`{"choices":[],"usage":{"total_tokens":1}}`)), "failed", ""},
		{"invalid tool success", string(sseStream(partialTool, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)), "failed", ""},
		{"tool token limit", string(sseStream(partialTool, `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)), "incomplete", "max_output_tokens"},
		{"tool content filter", string(sseStream(partialTool, `{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`)), "incomplete", "content_filter"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			name := tc.name + " json"
			if stream {
				name = tc.name + " stream"
			}
			t.Run(name, func(t *testing.T) {
				calls := 0
				up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, tc.raw, true })
				p := testPoolWith(&auth.Auth{UID: "integrity", AccessToken: "at-integrity", ExpiresAt: 9999999999})
				h := NewHandler(Config{Pool: p, Upstream: up})
				request, _ := json.Marshal(map[string]any{"model": "cn:deepseek-v4.1-flash", "input": "lookup 11128", "stream": stream})
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(request))))
				if calls != 1 {
					t.Fatalf("bad stream was retried: %d calls", calls)
				}
				state, _ := p.Status("integrity")
				if state.InFlight != 0 {
					t.Fatalf("request leaked its account lease: %+v", state)
				}
				if tc.status == "failed" && (state.SuccessCount != 0 || !state.LastSuccessTime.IsZero()) {
					t.Fatalf("bad stream was counted as success: %+v", state)
				}
				if !stream && tc.status == "failed" {
					if rec.Code != 502 {
						t.Fatalf("failed aggregate must return 502: status=%d body=%s", rec.Code, rec.Body)
					}
					var envelope map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || envelope["error"] == nil {
						t.Fatalf("aggregate returned fake completion: %s", rec.Body)
					}
					return
				}
				if rec.Code != 200 {
					t.Fatalf("unexpected response status=%d body=%s", rec.Code, rec.Body)
				}
				var final map[string]any
				if stream {
					names, datas := eventsOf(t, rec.Body.String())
					wantEvent := evFailed
					if tc.status == "incomplete" {
						wantEvent = evIncomplete
					}
					if len(names) == 0 || names[len(names)-1] != wantEvent {
						t.Fatalf("wrong terminal event: %v body=%s", names, rec.Body)
					}
					for i, event := range names {
						if event == evCompleted || event == evArgsDone {
							t.Fatalf("invalid tool/response completed: %v", names)
						}
						if event == evItemDone {
							item, _ := datas[i]["item"].(map[string]any)
							if item["type"] == "function_call" && item["status"] == "completed" {
								t.Fatalf("invalid tool marked completed: %v", item)
							}
						}
					}
					final, _ = datas[len(datas)-1]["response"].(map[string]any)
				} else if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
					t.Fatal(err)
				}
				if final["status"] != tc.status {
					t.Fatalf("wrong response status: %v", final)
				}
				if tc.status == "failed" && final["error"] == nil {
					t.Fatalf("failure details lost: %v", final)
				}
				if tc.detail != "" && final["incomplete_details"].(map[string]any)["reason"] != tc.detail {
					t.Fatalf("incomplete reason changed: %v", final)
				}
				for _, rawItem := range final["output"].([]any) {
					item := rawItem.(map[string]any)
					if item["type"] == "function_call" && item["status"] == "completed" {
						t.Fatalf("partial output contains executable tool: %v", item)
					}
				}
			})
		}
	}
}

// 此路径经过 Aggregate，tool_calls 的实际 Go 类型为 []map[string]any；不能仅测 JSON 解码后的 []any。
func TestGatewayIntegrityAggregateToolsHonorParallelLimit(t *testing.T) {
	raw := string(sseStream(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}},{"index":1,"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	))
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	p := testPoolWith(&auth.Auth{UID: "integrity", AccessToken: "at-integrity", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","input":"lookup","stream":false,"parallel_tool_calls":false,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`)))
	if rec.Code != 502 {
		t.Fatalf("parallel-tool contract violation was accepted: status=%d body=%s", rec.Code, rec.Body)
	}
	state, _ := p.Status("integrity")
	if state.SuccessCount != 0 {
		t.Fatalf("contract violation counted as success: %+v", state)
	}
}
