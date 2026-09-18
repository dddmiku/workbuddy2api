// ═══ 更新日志 ═══
// 2026-09-18：通过真实 Chat handler 与假上游复现 required/forced/strict 违约，锁定失败前不泄漏成功终态。
// 2026-09-18：补流式正文即时输出、迟到错误、截断、多 choice 校验与重复调用写失败控制。
// 2026-09-18：对照开源转换器锁定未知内置声明的过滤与无工具时的控制字段清理。
// 2026-09-18：锁定顶层长名字的wire/历史/选择/回程一致性，以及纯builtin显式强制选择必须拒绝。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func protocolDiagnosisChatRequest(choice any, strict, stream bool) map[string]any {
	return map[string]any{
		"model": "glm-5.2", "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": "finish the task"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "safe", "strict": strict, "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []any{"value"}, "additionalProperties": false}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "unsafe", "parameters": map[string]any{"type": "object"}}},
		},
		"tool_choice": choice,
	}
}

func protocolDiagnosisChat(t *testing.T, request map[string]any, rawSSE string) *httptest.ResponseRecorder {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return http.StatusOK, rawSSE, true })
	account := &auth.Auth{UID: "protocol-only", AccessToken: "fixture-token", ExpiresAt: 9999999999}
	h := NewHandler(Config{Pool: testPoolWith(account), Upstream: up})
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(encoded))))
	return rec
}

func protocolDiagnosisChatFrame(t *testing.T, message map[string]any, finish string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"id": "chatcmpl_protocol", "model": "glm-5.2", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func protocolDiagnosisChatTerminal(t *testing.T, body string) (int, int) {
	t.Helper()
	errors, finishes := 0, 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if frame["error"] != nil {
			errors++
		}
		choices, _ := frame["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if finish, _ := choice["finish_reason"].(string); finish != "" {
				finishes++
			}
		}
	}
	return errors, finishes
}

func TestProtocolDiagnosisChatRejectsContractViolations(t *testing.T) {
	forced := map[string]any{"type": "function", "function": map[string]any{"name": "safe"}}
	cases := []struct {
		name, args string
		choice     any
		strict     bool
		calls      []string
	}{
		{"required", "{}", "required", false, nil},
		{"none", "{}", "none", false, []string{"safe"}},
		{"forced", "{}", forced, false, []string{"unsafe"}},
		{"strict", `{"value":"wrong type"}`, "auto", true, []string{"safe"}},
	}
	for _, test := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(test.name+map[bool]string{true: "/stream", false: "/json"}[stream], func(t *testing.T) {
				finish := "stop"
				if len(test.calls) > 0 {
					finish = "tool_calls"
				}
				raw := outputIntegritySSE(protocolDiagnosisChatFrame(t, protocolDiagnosisMessage(test.calls, test.args), finish))
				rec := protocolDiagnosisChat(t, protocolDiagnosisChatRequest(test.choice, test.strict, stream), raw)
				if stream {
					errors, finishes := protocolDiagnosisChatTerminal(t, rec.Body.String())
					if errors != 1 || finishes != 0 || strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
						t.Fatalf("violated Chat contract succeeded: errors=%d finish_frames=%d body=%s", errors, finishes, rec.Body)
					}
				} else if rec.Code != http.StatusBadGateway {
					t.Fatalf("violated Chat contract returned HTTP %d", rec.Code)
				}
			})
		}
	}
}

func TestProtocolDiagnosisChatChecksStructuredOutput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		request := protocolDiagnosisChatRequest("auto", false, stream)
		request["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "result", "strict": true, "schema": map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []any{"ok"}, "additionalProperties": false}}}
		rec := protocolDiagnosisChat(t, request, outputIntegritySSE(protocolDiagnosisChatFrame(t, map[string]any{"content": "plain paragraph"}, "stop")))
		if stream {
			errors, finishes := protocolDiagnosisChatTerminal(t, rec.Body.String())
			if errors != 1 || finishes != 0 {
				t.Fatal("Chat structured-output violation emitted success")
			}
		} else if rec.Code != http.StatusBadGateway {
			t.Fatal("Chat structured-output violation was accepted")
		}
	}
}

func TestProtocolDiagnosisChatValidCallAndLateUsageSurvive(t *testing.T) {
	for _, stream := range []bool{false, true} {
		raw := outputIntegritySSE(
			protocolDiagnosisChatFrame(t, map[string]any{"content": "checking"}, ""),
			protocolDiagnosisChatFrame(t, protocolDiagnosisMessage([]string{"safe"}, `{"value":1}`), "tool_calls"),
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
		)
		rec := protocolDiagnosisChat(t, protocolDiagnosisChatRequest("required", true, stream), raw)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "safe") || !strings.Contains(rec.Body.String(), `"total_tokens":9`) {
			t.Fatalf("valid Chat output changed: HTTP %d body=%s", rec.Code, rec.Body)
		}
		if stream {
			errors, finishes := protocolDiagnosisChatTerminal(t, rec.Body.String())
			if errors != 0 || finishes != 1 || strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
				t.Fatalf("invalid Chat terminal sequence: errors=%d finishes=%d", errors, finishes)
			}
		}
	}
}

func TestProtocolDiagnosisChatLateCallCannotFollowAnExposedFinish(t *testing.T) {
	raw := outputIntegritySSE(
		protocolDiagnosisChatFrame(t, map[string]any{"content": "checking"}, "tool_calls"),
		protocolDiagnosisChatFrame(t, protocolDiagnosisMessage([]string{"safe"}, `{"value":1}`), "tool_calls"),
	)
	rec := protocolDiagnosisChat(t, protocolDiagnosisChatRequest("required", false, true), raw)
	errors, finishes := protocolDiagnosisChatTerminal(t, rec.Body.String())
	if errors != 0 || finishes != 1 {
		t.Fatalf("late tool left an earlier successful finish: errors=%d finishes=%d", errors, finishes)
	}
	if strings.Index(rec.Body.String(), `"name":"safe"`) > strings.Index(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatal("the successful finish was sent before its tool call")
	}
}

func protocolDiagnosisChatWriter(t *testing.T) (*chatContractWriter, *httptest.ResponseRecorder) {
	t.Helper()
	raw, _ := json.Marshal(protocolDiagnosisChatRequest("required", true, true))
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	contract, err := newChatOutputContract(fields)
	if err != nil || contract == nil {
		t.Fatalf("missing Chat contract: %v", err)
	}
	rec := httptest.NewRecorder()
	w := &chatContractWriter{inner: rec, req: contract}
	w.Header().Set("Content-Type", "text/event-stream")
	return w, rec
}

func TestProtocolDiagnosisChatStreamingTextIsNotBuffered(t *testing.T) {
	w, rec := protocolDiagnosisChatWriter(t)
	prefix := "data: " + protocolDiagnosisChatFrame(t, map[string]any{"content": "visible immediately"}, "") + "\n\n"
	if _, err := w.Write([]byte(prefix)); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if !strings.Contains(rec.Body.String(), "visible immediately") {
		t.Fatal("Chat wrapper buffered ordinary text until completion")
	}
	end := outputIntegritySSE(protocolDiagnosisChatFrame(t, protocolDiagnosisMessage([]string{"safe"}, `{"value":1}`), "tool_calls"))
	if _, err := w.Write([]byte(end)); err != nil {
		t.Fatal(err)
	}
	_, finishes := protocolDiagnosisChatTerminal(t, rec.Body.String())
	if finishes != 0 || strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatal("Chat terminal escaped before validation")
	}
	if err := w.CompletionError(); err != nil {
		t.Fatal(err)
	}
	w.finish()
	_, finishes = protocolDiagnosisChatTerminal(t, rec.Body.String())
	if finishes != 1 || strings.Count(rec.Body.String(), "[DONE]") != 1 {
		t.Fatal("Chat completion is not idempotent")
	}
}

func TestProtocolDiagnosisChatErrorAfterFinishKeepsOriginalError(t *testing.T) {
	w, rec := protocolDiagnosisChatWriter(t)
	raw := outputIntegritySSE(protocolDiagnosisChatFrame(t, protocolDiagnosisMessage([]string{"safe"}, `{"value":1}`), "tool_calls"), `{"error":{"code":"upstream_error","message":"disconnected","details":{"id":9007199254740993}}}`)
	if err := upstream.Stream(w, strings.NewReader(raw)); err == nil {
		t.Fatal("expected upstream error")
	}
	w.finish()
	errors, finishes := protocolDiagnosisChatTerminal(t, rec.Body.String())
	if errors != 1 || finishes != 0 || !strings.Contains(rec.Body.String(), "9007199254740993") {
		t.Fatalf("upstream error changed or followed success: %s", rec.Body)
	}
}

func TestProtocolDiagnosisChatIncompleteIsNotContractFailure(t *testing.T) {
	for _, finish := range []string{"length", "content_filter"} {
		for _, stream := range []bool{false, true} {
			raw := outputIntegritySSE(protocolDiagnosisChatFrame(t, protocolDiagnosisMessage([]string{"safe"}, `{"value":`), finish))
			rec := protocolDiagnosisChat(t, protocolDiagnosisChatRequest("required", true, stream), raw)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"finish_reason":"`+finish+`"`) {
				t.Fatalf("incomplete Chat response changed: %s", rec.Body)
			}
			if strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatal("length/filter was replaced by a tool contract error")
			}
		}
	}
}

func TestProtocolDiagnosisChatAllChoicesAreValidated(t *testing.T) {
	for _, stream := range []bool{false, true} {
		encoded, _ := json.Marshal(map[string]any{"choices": []any{
			map[string]any{"index": 0, "delta": protocolDiagnosisMessage([]string{"safe"}, `{"value":1}`), "finish_reason": "tool_calls"},
			map[string]any{"index": 1, "delta": protocolDiagnosisMessage([]string{"safe"}, `{"value":"bad"}`), "finish_reason": "tool_calls"},
		}})
		rec := protocolDiagnosisChat(t, protocolDiagnosisChatRequest("auto", true, stream), outputIntegritySSE(string(encoded)))
		if stream {
			errors, finishes := protocolDiagnosisChatTerminal(t, rec.Body.String())
			if errors != 1 || finishes != 0 {
				t.Fatal("invalid second choice escaped Chat validation")
			}
		} else if rec.Code != http.StatusBadGateway {
			t.Fatal("invalid second JSON choice escaped validation")
		}
	}
}

func TestProtocolDiagnosisChatWriteErrorRemainsVisible(t *testing.T) {
	w, _ := protocolDiagnosisChatWriter(t)
	w.streaming = true
	want := errors.New("client is disconnected")
	w.writeErr = want
	for call := 0; call < 2; call++ {
		if err := w.CompletionError(); !errors.Is(err, want) {
			t.Fatalf("completion check %d lost client write failure: %v", call, err)
		}
	}
}

func TestProtocolDiagnosisChatBuiltinDeclarationsDoNotLeakUpstream(t *testing.T) {
	var sent map[string]any
	up := newFakeUpstream(t, func(string) (int, string, bool) { return http.StatusOK, sseOK, true })
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &sent)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "wire-only", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: up})
	for _, tools := range []string{
		`[{"type":"function","function":{"name":"safe","parameters":{}}},{"type":"web_search"},{"type":"tool_search"}]`,
		`[{"type":"tool_search"}]`,
	} {
		raw := `{"model":"glm-5.2","messages":[],"tools":` + tools + `,"tool_choice":"auto","parallel_tool_calls":false}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw)))
		if rec.Code != http.StatusOK {
			t.Fatalf("compatible builtins caused HTTP %d", rec.Code)
		}
		forwarded, _ := sent["tools"].([]any)
		for _, raw := range forwarded {
			tool := raw.(map[string]any)
			if tool["type"] != "function" {
				t.Fatal("unimplemented builtin was still sent to the upstream")
			}
		}
		if len(forwarded) == 0 && (sent["tool_choice"] != nil || sent["parallel_tool_calls"] != nil) {
			t.Fatal("tool controls remained after all declarations were filtered")
		}
	}
}

func TestProtocolDiagnosisBuiltinRequiredOrForcedCannotDowngrade(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, forced := range []bool{false, true} {
			calls := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return http.StatusOK, sseOK, true })
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "unavailable", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: up})
			request := map[string]any{"model": "glm-5.2", "input": "run", "messages": []any{}, "tools": []any{map[string]any{"type": "tool_search"}}, "tool_choice": "required"}
			if forced {
				choice := map[string]any{"type": "function", "name": "tool_search"}
				if path == "/v1/chat/completions" {
					choice = map[string]any{"type": "function", "function": map[string]any{"name": "tool_search"}}
				}
				request["tool_choice"] = choice
			}
			raw, _ := json.Marshal(request)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw))))
			if rec.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("explicit tool requirement was downgraded: path=%s forced=%t status=%d upstream=%d", path, forced, rec.Code, calls)
			}
		}
	}
}

func TestProtocolDiagnosisChatLongNamesRoundTrip(t *testing.T) {
	name := strings.Repeat("tool_name_", 9)
	for _, stream := range []bool{false, true} {
		var sent map[string]any
		up := newFakeUpstream(t, func(string) (int, string, bool) { return http.StatusOK, sseOK, true })
		up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &sent)
			tools := sent["tools"].([]any)
			alias := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
			body := outputIntegritySSE(protocolDiagnosisChatFrame(t, protocolDiagnosisMessage([]string{alias}, "{}"), "tool_calls"))
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "names", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: up})
		request := map[string]any{"model": "glm-5.2", "stream": stream,
			"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": name, "parameters": map[string]any{"type": "object"}}}},
			"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": name}},
			"messages":    []any{protocolDiagnosisMessage([]string{name}, "{}"), map[string]any{"role": "tool", "tool_call_id": "call_" + name, "content": "done"}, map[string]any{"role": "user", "content": "again"}},
		}
		raw, _ := json.Marshal(request)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw))))
		if rec.Code != http.StatusOK {
			t.Fatalf("long-name Chat request failed: HTTP %d", rec.Code)
		}
		alias := sent["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
		if len(alias) > 64 || sent["tool_choice"] != alias {
			t.Fatalf("Chat declaration/choice alias differs or is overlong: %v", sent["tool_choice"])
		}
		messages := sent["messages"].([]any)
		previous := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
		if previous["name"] != alias {
			t.Fatal("Chat history used a different name")
		}
		if !strings.Contains(rec.Body.String(), `"name":"`+name+`"`) {
			t.Fatal("Chat result did not restore the original public tool name")
		}
	}
}
