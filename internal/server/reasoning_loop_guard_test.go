// ═══ 更新日志 ═══
// 2026-09-19：以合成上游覆盖重复推理保护的四种出口、失败用量及账号/粘性边界。
// 2026-09-19：按32字符短行规则补齐完整早期用量、关闭开关与正常进展边界，避免误截和用量推算。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/usage"
)

type reasoningGuardBody struct {
	*strings.Reader
	closed int
}

func (b *reasoningGuardBody) Close() error { b.closed++; return nil }

func reasoningGuardFrame(delta map[string]any) string {
	value := map[string]any{"id": "reasoning-guard-fixture", "choices": []any{map[string]any{"index": 0, "delta": delta}}}
	raw, _ := json.Marshal(value)
	return "data: " + string(raw) + "\n\n"
}

func reasoningGuardLines(count int) string {
	return strings.Repeat("checking the same step once more\n", count)
}

func reasoningGuardTool(index int, id, name, arguments string) string {
	return reasoningGuardFrame(map[string]any{"tool_calls": []any{map[string]any{
		"index": index, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": arguments},
	}}})
}

func reasoningGuardFinish(payload, reason string) string {
	return payload + reasoningGuardFrame(map[string]any{"content": "after-guard-marker"}) +
		postreleaseFinish(reason) + postreleaseUsageOnly + "data: [DONE]\n\n"
}

func reasoningGuardFixture(t *testing.T, payload, model string) (*Handler, *usage.Store, *reasoningGuardBody, *int, *bindStore) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	h, ledger := postreleaseUsageHandler(t, payload)
	domain := ""
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "global:") {
		domain = "www.workbuddy.ai"
	}
	h.cfg.Pool = testPoolWith(
		&auth.Auth{UID: "guard-first", Domain: domain, AccessToken: "fixture-first", ExpiresAt: 9999999999},
		&auth.Auth{UID: "guard-second", Domain: domain, AccessToken: "fixture-second", ExpiresAt: 9999999999},
	)
	h.cfg.MaxRotate = 3
	h.cfg.GlobalEnabled = true
	body := &reasoningGuardBody{Reader: strings.NewReader(payload)}
	calls := 0
	h.cfg.Upstream.GlobalEnabled = true
	h.cfg.Upstream.ChatBaseGlobal = "https://fixture.invalid"
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	})
	bindings := newBindStore()
	h.cfg.Session = session.New(session.Config{TTL: time.Hour, Store: bindings,
		Available: func() []string { return []string{"guard-first", "guard-second"} }})
	h.cfg.Session.Bind("reasoning-guard-session", "guard-first")
	return h, ledger, body, &calls, bindings
}

func reasoningGuardRequest(path, model string, streaming bool, tools bool) *http.Request {
	value := map[string]any{"model": model, "stream": streaming, "conversation_id": "reasoning-guard-session",
		"metadata": map[string]any{"conversation_id": "reasoning-guard-session"}}
	if path == "/v1/responses" {
		value["input"] = "continue the fixture task"
	} else {
		value["messages"] = []any{map[string]any{"role": "user", "content": "continue the fixture task"}}
	}
	if tools {
		function := map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
		if path == "/v1/responses" {
			function["type"] = "function"
			value["tools"] = []any{function}
		} else {
			value["tools"] = []any{map[string]any{"type": "function", "function": function}}
		}
	}
	raw, _ := json.Marshal(value)
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
}

func assertReasoningGuardFailure(t *testing.T, recorder *httptest.ResponseRecorder, path string, streaming bool) {
	t.Helper()
	wantStatus := http.StatusUnprocessableEntity
	if streaming {
		wantStatus = http.StatusOK
	}
	if recorder.Code != wantStatus {
		t.Errorf("status=%d want=%d", recorder.Code, wantStatus)
	}
	output := recorder.Body.String()
	if !strings.Contains(output, `"code":"upstream_reasoning_loop"`) {
		t.Errorf("missing stable loop error, output bytes=%d", len(output))
	}
	if strings.Contains(output, "after-guard-marker") {
		t.Error("stream continued after the guarded loop")
	}
	if streaming && path == "/v1/responses" {
		if strings.Count(output, "event: response.failed\n") != 1 {
			t.Error("Responses did not emit exactly one failed terminal")
		}
		if strings.Contains(output, "event: response.completed\n") {
			t.Error("guarded Responses emitted a success terminal")
		}
	} else if streaming && strings.Contains(output, `"finish_reason":"stop"`) {
		t.Error("guarded Chat emitted a successful finish")
	}
}

func assertReasoningGuardAccountUnchanged(t *testing.T, before, after pool.Status) {
	t.Helper()
	if after.ErrTotal != before.ErrTotal || after.BreakerFails != before.BreakerFails || after.Disabled || after.Cooling ||
		!after.BreakerUntil.Equal(before.BreakerUntil) || !after.LastErrTime.Equal(before.LastErrTime) ||
		after.SuccessCount != before.SuccessCount || after.InFlight != 0 {
		t.Errorf("loop guard changed account health/accounting: before=%+v after=%+v", before, after)
	}
}

func assertReasoningGuardSuccess(t *testing.T, recorder *httptest.ResponseRecorder, path string, streaming bool, ledger *usage.Store) {
	t.Helper()
	output := recorder.Body.String()
	if recorder.Code != http.StatusOK || strings.Contains(output, `"code":"upstream_reasoning_loop"`) ||
		!strings.Contains(output, "after-guard-marker") {
		t.Errorf("valid output was interrupted: status=%d bytes=%d", recorder.Code, len(output))
	}
	if path == "/v1/responses" && streaming && (strings.Count(output, "event: response.completed\n") != 1 ||
		strings.Contains(output, "event: response.failed\n")) {
		t.Error("valid Responses did not emit exactly one successful terminal")
	}
	want := usage.Totals{Requests: 1, PromptTokens: 5000, CompletionTokens: 120, CachedTokens: 4096, TotalTokens: 5120, Credit: 1.25}
	if got := ledger.Snapshot().Totals; got != want {
		t.Errorf("valid output changed raw usage: got=%+v want=%+v", got, want)
	}
}

func TestReasoningLoopGuardFourExitsAndUsage(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, earlyUsage := range []string{"none", "partial", "complete"} {
				t.Run(fmt.Sprintf("%s/stream=%t/earlyUsage=%s", path, streaming, earlyUsage), func(t *testing.T) {
					payload := reasoningGuardFrame(map[string]any{"role": "assistant"})
					if earlyUsage == "partial" {
						payload += "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"prompt_cache_hit_tokens\":64,\"credit\":0.5}}\n\n"
					} else if earlyUsage == "complete" {
						payload += "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":8,\"prompt_cache_hit_tokens\":64,\"credit\":0.5}}\n\n"
					}
					payload += reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)})
					payload += reasoningGuardFrame(map[string]any{"content": "after-guard-marker"}) + postreleaseFinish("stop") + "data: [DONE]\n\n"
					model := "global:deepseek-v4.1-flash"
					h, ledger, body, calls, bindings := reasoningGuardFixture(t, payload, model)
					beforeFirst, _ := h.cfg.Pool.Status("guard-first")
					beforeSecond, _ := h.cfg.Pool.Status("guard-second")
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
					assertReasoningGuardFailure(t, recorder, path, streaming)
					if *calls != 1 {
						t.Errorf("loop triggered upstream retries: calls=%d", *calls)
					}
					if body.closed == 0 {
						t.Error("upstream body was not closed")
					}
					afterFirst, firstPresent := h.cfg.Pool.Status("guard-first")
					afterSecond, secondPresent := h.cfg.Pool.Status("guard-second")
					if !firstPresent || !secondPresent {
						t.Fatal("loop guard removed an account from the pool")
					}
					assertReasoningGuardAccountUnchanged(t, beforeFirst, afterFirst)
					assertReasoningGuardAccountUnchanged(t, beforeSecond, afterSecond)
					if uid, ok := bindings.lastUID("reasoning-guard-session"); !ok || uid != "guard-first" {
						t.Error("loop guard invalidated the existing sticky binding")
					}
					want := usage.Totals{Requests: 1, FailedRequests: 1, UnreportedRequests: 1}
					if earlyUsage != "none" {
						want.PromptTokens = 120
						want.CachedTokens = 64
						want.TotalTokens = 120
						want.Credit = 0.5
					}
					if earlyUsage == "complete" {
						want.CompletionTokens = 8
						want.TotalTokens = 128
					}
					if got := ledger.Snapshot().Totals; got != want {
						t.Errorf("partial usage lost or fabricated: got=%+v want=%+v", got, want)
					}
				})
			}
		}
	}
}

func TestReasoningLoopGuardNormalOutputAndProgress(t *testing.T) {
	var unique strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&unique, "different reasoning item %04d\n", i)
	}
	longLine := strings.Repeat("推", 12000)
	before := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(200)})
	after := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(200)})
	cases := []struct {
		name, payload, finish, preserve string
		tools                           bool
	}{
		{name: "long-unbroken-reasoning", payload: reasoningGuardFrame(map[string]any{"reasoning_content": longLine}), preserve: longLine},
		{name: "repeated-lines-over-32-characters", payload: reasoningGuardFrame(map[string]any{"reasoning_content": strings.Repeat(strings.Repeat("m", 33)+"\n", 300)}), preserve: strings.Repeat("m", 33)},
		{name: "distinct-short-reasoning-lines", payload: reasoningGuardFrame(map[string]any{"reasoning_content": unique.String()})},
		{name: "repeated-visible-content", payload: reasoningGuardFrame(map[string]any{"content": reasoningGuardLines(300)})},
		{name: "content-resets-window", payload: before + reasoningGuardFrame(map[string]any{"content": "visible progress"}) + after},
		{name: "refusal-resets-window", payload: before + reasoningGuardFrame(map[string]any{"refusal": "refusal progress"}) + after},
		{name: "new-tool-resets-window", payload: before + reasoningGuardTool(0, "call_guard", "lookup", "{}") + after, finish: "tool_calls", tools: true},
		{name: "tool-arguments-reset-window", payload: reasoningGuardTool(0, "call_guard", "lookup", "{") + before + reasoningGuardTool(0, "", "", "}") + after, finish: "tool_calls", tools: true},
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, streaming, tc.name), func(t *testing.T) {
					finish := tc.finish
					if finish == "" {
						finish = "stop"
					}
					model := "global:deepseek-v4.1-flash"
					h, ledger, body, calls, _ := reasoningGuardFixture(t, reasoningGuardFinish(tc.payload, finish), model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, tc.tools))
					assertReasoningGuardSuccess(t, recorder, path, streaming, ledger)
					if tc.preserve != "" && !strings.Contains(recorder.Body.String(), tc.preserve) {
						t.Error("long reasoning text was truncated")
					}
					if *calls != 1 || body.closed == 0 {
						t.Errorf("unexpected upstream lifecycle: calls=%d closed=%d", *calls, body.closed)
					}
				})
			}
		}
	}
}

func TestReasoningLoopGuardMetadataDoesNotReset(t *testing.T) {
	before := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(200)})
	after := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(200)})
	cases := []struct {
		name, prefix, metadata string
		tools                  bool
	}{
		{name: "role", metadata: reasoningGuardFrame(map[string]any{"role": "assistant"})},
		{name: "empty-content", metadata: reasoningGuardFrame(map[string]any{"content": "", "refusal": ""})},
		{name: "whitespace-content", metadata: reasoningGuardFrame(map[string]any{"content": " \t\n", "refusal": " \r\n"})},
		{name: "usage-only", metadata: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":8}}\n\n"},
		{name: "empty-tool-fields", metadata: reasoningGuardTool(0, "", "", ""), tools: true},
		{name: "type-and-index", metadata: reasoningGuardFrame(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "type": "function"}}}), tools: true},
		{name: "repeated-tool-identity", prefix: reasoningGuardTool(0, "call_guard", "lookup", "{}"), metadata: reasoningGuardTool(0, "call_guard", "lookup", ""), tools: true},
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, streaming, tc.name), func(t *testing.T) {
					model := "global:deepseek-v4.1-flash"
					payload := reasoningGuardFinish(tc.prefix+before+tc.metadata+after, "stop")
					h, ledger, body, calls, _ := reasoningGuardFixture(t, payload, model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, tc.tools))
					assertReasoningGuardFailure(t, recorder, path, streaming)
					if *calls != 1 || body.closed == 0 {
						t.Errorf("guard retried or failed to close upstream: calls=%d closed=%d", *calls, body.closed)
					}
					got := ledger.Snapshot().Totals
					if got.Requests != 1 || got.FailedRequests != 1 || got.UnreportedRequests != 1 {
						t.Errorf("metadata hid guarded failure outcome: %+v", got)
					}
				})
			}
		}
	}
}

func TestReasoningLoopGuardModelScope(t *testing.T) {
	for _, model := range []string{
		"deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", "sg:deepseek-v4.1-flash",
		"cn:deepseek-v4.1-flash-extra", "cn:gpt-5.3-codex",
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model, path, streaming), func(t *testing.T) {
					payload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)}), "stop")
					h, ledger, _, calls, _ := reasoningGuardFixture(t, payload, model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
					if model == "cn:deepseek-v4.1-flash-extra" || model == "cn:gpt-5.3-codex" {
						assertReasoningGuardSuccess(t, recorder, path, streaming, ledger)
					} else {
						assertReasoningGuardFailure(t, recorder, path, streaming)
					}
					if *calls != 1 {
						t.Errorf("unexpected upstream calls=%d", *calls)
					}
				})
			}
		}
	}
}

func TestReasoningLoopGuardDisabled(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, streaming), func(t *testing.T) {
				model := "global:deepseek-v4.1-flash"
				payload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)}), "stop")
				h, ledger, body, calls, _ := reasoningGuardFixture(t, payload, model)
				disabled := false
				h.cfg.ReasoningLoopGuard = &disabled
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
				assertReasoningGuardSuccess(t, recorder, path, streaming, ledger)
				if *calls != 1 || body.closed == 0 {
					t.Errorf("disabled guard changed upstream lifecycle: calls=%d closed=%d", *calls, body.closed)
				}
			})
		}
	}
}
