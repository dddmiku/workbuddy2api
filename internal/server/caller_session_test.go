// ═══ 更新日志 ═══
// 2026-09-19：通过真实鉴权及 Chat/Responses Handler 锁定跨调用密钥会话隔离、失败解绑和关联头隔离。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

func callerSessionFixture(t *testing.T, secondFails bool) (*Handler, []string, *[]http.Header, *bindStore) {
	t.Helper()
	h, _ := postreleaseUsageHandler(t, sseOK)
	h.cfg.Pool = testPoolWith(
		&auth.Auth{UID: "caller-pool-a", AccessToken: "synthetic-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "caller-pool-b", AccessToken: "synthetic-b", ExpiresAt: 9999999999},
	)
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "synthetic-keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.APIKeys = store
	var keys []string
	for _, label := range []string{"fixture-a", "fixture-b"} {
		_, key, err := store.Create(label, "session isolation regression", nil)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	bindings := newBindStore()
	h.cfg.Session = session.New(session.Config{TTL: time.Hour, Store: bindings, Available: h.cfg.Pool.AvailableUIDs})
	var headers []http.Header
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		headers = append(headers, req.Header.Clone())
		if secondFails && len(headers) == 2 {
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":11101,"msg":"Unmarshal chat params failed"}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})
	return h, keys, &headers, bindings
}

func callerSessionRequest(t *testing.T, path, scenario string, actor int, key string) *http.Request {
	t.Helper()
	body := map[string]any{"model": "cn:hy3", "stream": true}
	if path == "/v1/responses" {
		body["input"] = "continue"
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": "continue"}}
	}
	switch scenario {
	case "conversation", "headers":
		body["metadata"] = map[string]any{"conversation_id": "shared-session"}
	case "client-thread":
		body["client_metadata"] = map[string]any{"thread_id": "shared-thread"}
	case "explicit-over-cache":
		body["prompt_cache_key"] = "shared-cache"
		body["conversation_id"] = fmt.Sprintf("conversation-%d", actor)
	case "cache", "sticky-off":
		body["prompt_cache_key"] = "shared-cache"
	case "user":
		body["metadata"] = map[string]any{"user_id": "generic-client-user"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+key)
	if scenario == "headers" {
		req.Header.Set("X-Conversation-Request-ID", "0123456789abcdef0123456789abcdef")
		req.Header.Set("X-Trace-ID", "fedcba9876543210fedcba9876543210")
	}
	return req
}

func TestCallerSessionIsolation(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, scenario := range []string{"conversation", "client-thread", "explicit-over-cache", "cache", "user", "headers", "no-session", "sticky-off"} {
			t.Run(path+"/"+scenario, func(t *testing.T) {
				h, keys, captured, bindings := callerSessionFixture(t, false)
				if scenario == "sticky-off" {
					h.cfg.Session = nil
				}
				for _, actor := range []int{0, 1, 0} {
					rr := httptest.NewRecorder()
					h.ServeHTTP(rr, callerSessionRequest(t, path, scenario, actor, keys[actor]))
					if rr.Code != 200 {
						t.Fatalf("request status=%d", rr.Code)
					}
				}
				if len(*captured) != 3 {
					t.Fatalf("upstream calls=%d, want 3", len(*captured))
				}
				a, b, a2 := (*captured)[0], (*captured)[1], (*captured)[2]
				for _, header := range []string{"X-Conversation-Request-ID", "X-Conversation-ID", "X-Trace-ID"} {
					if a.Get(header) != "" && a.Get(header) == b.Get(header) {
						t.Errorf("different authenticated callers shared %s", header)
					}
					if a.Get(header) != a2.Get(header) {
						t.Errorf("same caller changed %s", header)
					}
				}
				if a.Get("X-Request-ID") == b.Get("X-Request-ID") {
					t.Error("request IDs must be unique")
				}
				wantBindings := 2
				if scenario == "no-session" || scenario == "sticky-off" {
					wantBindings = 0
				}
				if len(bindings.LoadBinds()) != wantBindings {
					t.Errorf("bindings=%d want=%d", len(bindings.LoadBinds()), wantBindings)
				}
				if wantBindings == 2 {
					if a.Get("X-User-Id") == b.Get("X-User-Id") {
						t.Error("different callers shared one binding despite an idle account")
					}
					if a.Get("X-User-Id") != a2.Get("X-User-Id") {
						t.Error("caller A lost its affinity")
					}
				}
				if snapshot := h.cfg.Usage.Snapshot(); len(snapshot.Keys) != 2 || snapshot.Totals.Requests != 3 {
					t.Fatal("usage must remain independently accounted")
				}
			})
		}
	}
}

func TestCallerFailureDoesNotUnbindAnotherCaller(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			h, keys, captured, bindings := callerSessionFixture(t, true)
			first := httptest.NewRecorder()
			h.ServeHTTP(first, callerSessionRequest(t, path, "conversation", 0, keys[0]))
			before := bindings.LoadBinds()
			second := httptest.NewRecorder()
			h.ServeHTTP(second, callerSessionRequest(t, path, "conversation", 1, keys[1]))
			after := bindings.LoadBinds()
			if first.Code != 200 || second.Code != 400 || len(before) != 1 || len(after) != 1 {
				t.Fatalf("statuses=%d/%d bindings=%d/%d; caller B must not unbind A", first.Code, second.Code, len(before), len(after))
			}
			for key, uid := range before {
				if after[key] != uid {
					t.Error("caller A's binding was changed by B's failure")
				}
			}
			third := httptest.NewRecorder()
			h.ServeHTTP(third, callerSessionRequest(t, path, "conversation", 0, keys[0]))
			if third.Code != 200 || (*captured)[0].Get("X-User-Id") != (*captured)[2].Get("X-User-Id") {
				t.Error("caller A did not retain its account")
			}
		})
	}
}

func TestExplicitConversationsOverrideSharedCacheForSameCaller(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			h, keys, captured, bindings := callerSessionFixture(t, false)
			for _, actor := range []int{0, 1, 0} {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, callerSessionRequest(t, path, "explicit-over-cache", actor, keys[0]))
				if rr.Code != 200 {
					t.Fatalf("status=%d", rr.Code)
				}
			}
			if len(bindings.LoadBinds()) != 2 || (*captured)[0].Get("X-User-Id") == (*captured)[1].Get("X-User-Id") {
				t.Error("shared cache key overrode two explicit conversations")
			}
		})
	}
}
