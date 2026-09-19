// ═══ 更新日志 ═══
// 2026-09-19：锁定已有上游用量遇失败、取消、输出拒绝和迟到统计时仍被账本保留。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/usage"
)

func postreleaseUsageHandler(t *testing.T, payload string) (*Handler, *usage.Store) {
	t.Helper()
	ledger, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "audit-only", AccessToken: "fixture-only", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return http.StatusOK, payload, true }),
		Usage:    ledger,
	})
	return h, ledger
}

func postreleaseUsageRequest(path string, stream bool, extra string) *http.Request {
	input := `"messages":[{"role":"user","content":"audit"}]`
	if path == "/v1/responses" {
		input = `"input":"audit"`
	}
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(fmt.Sprintf(`{"model":"cn:deepseek-v4.1-flash","stream":%t,%s%s}`, stream, input, extra)))
}

const postreleaseUsageContent = "data: {\"id\":\"usage-fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\",\"reasoning_content\":\"thought\"}}]}\n\n"
const postreleaseUsageOnly = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5000,\"completion_tokens\":120,\"total_tokens\":5120,\"prompt_cache_hit_tokens\":4096,\"completion_thinking_tokens\":100,\"credit\":1.25}}\n\n"

func postreleaseFinish(reason string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\n", reason)
}

func TestPostreleaseUsageControls(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{true, false} {
			for _, reason := range []string{"stop", "length", "content_filter"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, reason), func(t *testing.T) {
					h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish(reason)+postreleaseUsageOnly+"data: [DONE]\n\n")
					rr := httptest.NewRecorder()
					h.ServeHTTP(rr, postreleaseUsageRequest(path, stream, ""))
					if rr.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
					}
					got := ledger.Snapshot().Totals
					if got.FailedRequests != 0 || got.UnreportedRequests != 0 {
						t.Fatalf("complete upstream usage was marked failed or unknown: %+v", got)
					}
					if got.Requests != 1 || got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.CachedTokens != 4096 || got.TotalTokens != 5120 || got.Credit != 1.25 {
						t.Fatalf("successful/incomplete terminal lost known usage: %+v", got)
					}
				})
			}
		}
	}
}

func TestPostreleaseUsageAlreadyObservedBeforeFailure(t *testing.T) {
	cases := []struct{ name, suffix string }{
		{"stream-error", "data: {\"error\":{\"code\":\"fixture_failure\",\"message\":\"failure after billed usage\"}}\n\n"},
		{"missing-terminal", ""},
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{true, false} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, tc.name), func(t *testing.T) {
					h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseUsageOnly+tc.suffix)
					rr := httptest.NewRecorder()
					h.ServeHTTP(rr, postreleaseUsageRequest(path, stream, ""))
					if !strings.Contains(rr.Body.String(), "error") {
						t.Fatalf("fixture did not fail: %s", rr.Body.String())
					}
					got := ledger.Snapshot().Totals
					if got.FailedRequests != 1 || got.UnreportedRequests != 0 {
						t.Fatalf("failed request lost its observed-usage outcome: %+v", got)
					}
					if got.Requests != 1 || got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.Credit != 1.25 {
						t.Fatalf("known billed usage discarded when request failed: got=%+v want=requests:1 prompt:5000 completion:120 credit:1.25 response=%s", got, rr.Body.String())
					}
				})
			}
		}
	}
}

func TestPostreleaseUsageAlreadyObservedBeforeContractFailure(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				extra := `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}],"tool_choice":"required"`
				if path == "/v1/responses" {
					extra = `,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}],"tool_choice":"required"`
				}
				h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+postreleaseUsageOnly+"data: [DONE]\n\n")
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, postreleaseUsageRequest(path, stream, extra))
				if !strings.Contains(rr.Body.String(), "error") {
					t.Fatalf("fixture did not reject tool contract: %s", rr.Body.String())
				}
				got := ledger.Snapshot().Totals
				if got.FailedRequests != 1 || got.UnreportedRequests != 0 {
					t.Fatalf("contract failure outcome=%+v", got)
				}
				if got.Requests != 1 || got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.Credit != 1.25 {
					t.Fatalf("known billed usage discarded when gateway rejected output: got=%+v want=requests:1 prompt:5000 completion:120 credit:1.25", got)
				}
			})
		}
	}
}

type postreleaseFailDoneWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	failed *bool
}

func (w postreleaseFailDoneWriter) Write(raw []byte) (int, error) {
	if strings.Contains(string(raw), "[DONE]") {
		*w.failed = true
		if w.cancel != nil {
			w.cancel()
		}
		return 0, errors.New("fixture client closed after receiving usage")
	}
	return w.ResponseRecorder.Write(raw)
}

func (w postreleaseFailDoneWriter) WriteString(raw string) (int, error) { return w.Write([]byte(raw)) }

func TestPostreleaseUsageAlreadySentBeforeClientDisconnect(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelContext=%t", cancelContext), func(t *testing.T) {
			h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+postreleaseUsageOnly+"data: [DONE]\n\n")
			rr := httptest.NewRecorder()
			req := postreleaseUsageRequest("/v1/chat/completions", true, "")
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			failed := false
			writer := postreleaseFailDoneWriter{ResponseRecorder: rr, failed: &failed}
			if cancelContext {
				writer.cancel = cancel
			}
			h.ServeHTTP(writer, req.WithContext(ctx))
			if !failed || !strings.Contains(rr.Body.String(), `"prompt_tokens":5000`) {
				t.Fatal("fixture did not receive usage then disconnect")
			}
			got := ledger.Snapshot().Totals
			if got.FailedRequests != 1 || got.UnreportedRequests != 0 {
				t.Fatalf("disconnect outcome=%+v", got)
			}
			if got.Requests != 1 || got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.Credit != 1.25 {
				t.Fatalf("usage already sent to client lost from ledger: %+v", got)
			}
		})
	}
}

func TestPostreleaseUsagePartialFramesPreserveObservedFields(t *testing.T) {
	sse := postreleaseUsageContent + postreleaseFinish("stop") + postreleaseUsageOnly + "data: {\"choices\":[],\"usage\":{\"credit\":1.25}}\n\ndata: [DONE]\n\n"
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h, ledger := postreleaseUsageHandler(t, sse)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, postreleaseUsageRequest("/v1/chat/completions", stream, ""))
			got := ledger.Snapshot().Totals
			if got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.CachedTokens != 4096 {
				t.Fatalf("later metadata-only usage zeroed prior counters: %+v", got)
			}
		})
	}
}

func TestPostreleaseResponsesCacheDetailsParity(t *testing.T) {
	sse := strings.ReplaceAll(postreleaseUsageContent+postreleaseFinish("stop")+postreleaseUsageOnly+"data: [DONE]\n\n", `"prompt_cache_hit_tokens":4096`, `"prompt_tokens_details":{"cached_tokens":4096}`)
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h, ledger := postreleaseUsageHandler(t, sse)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, postreleaseUsageRequest("/v1/responses", stream, ""))
			if got := ledger.Snapshot().Totals.CachedTokens; got != 4096 {
				t.Fatalf("ledger cached=%d", got)
			}
			var response map[string]any
			if stream {
				for _, line := range strings.Split(rr.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var frame map[string]any
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) == nil && frame["type"] == "response.completed" {
						response, _ = frame["response"].(map[string]any)
					}
				}
			} else {
				_ = json.Unmarshal(rr.Body.Bytes(), &response)
			}
			u, _ := response["usage"].(map[string]any)
			d, _ := u["input_tokens_details"].(map[string]any)
			if d["cached_tokens"] != float64(4096) {
				t.Fatalf("Responses cache detail discarded although ledger has it: %v", u)
			}
		})
	}
}

func TestPostreleaseUsageMissingIsNotZero(t *testing.T) {
	r := newChatStatsReaderSince(strings.NewReader(postreleaseUsageContent+postreleaseFinish("stop")+"data: [DONE]\n\n"), time.Now())
	_, err := io.Copy(io.Discard, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Tokens(); ok {
		t.Fatal("fixture unexpectedly contains usage")
	}
	if r.PromptTokens() != -1 || r.CachedTokens() != -1 {
		t.Fatalf("unobserved metrics reported as explicit zero: prompt=%d cache=%d", r.PromptTokens(), r.CachedTokens())
	}
}

func TestUsageOutcomeUnknownAndZeroCounters(t *testing.T) {
	cases := []struct {
		name, fields                string
		prompt, completion, unknown int64
	}{
		{"missing", "", 0, 0, 1},
		{"prompt-only", `"prompt_tokens":5000`, 5000, 0, 1},
		{"completion-only", `"completion_tokens":120`, 0, 120, 1},
		{"explicit-zero", `"prompt_tokens":0,"completion_tokens":0`, 0, 0, 0},
		{"null-output", `"prompt_tokens":5000,"completion_tokens":null`, 5000, 0, 1},
		{"invalid-output", `"prompt_tokens":5000,"completion_tokens":"invalid"`, 5000, 0, 1},
		{"negative-output", `"prompt_tokens":5000,"completion_tokens":-1`, 5000, 0, 1},
	}
	for _, stream := range []bool{true, false} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				metrics := ""
				if tc.fields != "" {
					metrics = "data: {\"choices\":[],\"usage\":{" + tc.fields + "}}\n\n"
				}
				h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+metrics+"data: [DONE]\n\n")
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, postreleaseUsageRequest("/v1/chat/completions", stream, ""))
				got := ledger.Snapshot().Totals
				if got.Requests != 1 || got.FailedRequests != 0 || got.UnreportedRequests != tc.unknown || got.PromptTokens != tc.prompt || got.CompletionTokens != tc.completion || got.TotalTokens != tc.prompt+tc.completion {
					t.Fatalf("partial/zero observation was fabricated or dropped: %+v", got)
				}
			})
		}
	}
}

func TestUsageOutcomeLocalRejectionsDoNotCount(t *testing.T) {
	for _, reason := range []string{"authentication", "invalid-request", "no-account", "already-cancelled"} {
		t.Run(reason, func(t *testing.T) {
			h, ledger := postreleaseUsageHandler(t, sseOK)
			calls := 0
			h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })
			req := postreleaseUsageRequest("/v1/chat/completions", true, "")
			switch reason {
			case "authentication":
				h.cfg.APIKey = "fixture-auth-required"
			case "invalid-request":
				req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":false}`))
			case "no-account":
				h.cfg.Pool = testPoolWith()
			case "already-cancelled":
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got := ledger.Snapshot().Totals; calls != 0 || got.Requests != 0 {
				t.Fatalf("local rejection counted: upstream calls=%d totals=%+v", calls, got)
			}
		})
	}
}

func TestUsageOutcomeHTTPErrorAndRetryCountOnce(t *testing.T) {
	for _, succeedAfterRetry := range []bool{false, true} {
		for _, firstKnown := range []bool{false, true} {
			t.Run(fmt.Sprintf("retry=%t/firstKnown=%t", succeedAfterRetry, firstKnown), func(t *testing.T) {
				h, ledger := postreleaseUsageHandler(t, sseOK)
				h.cfg.Pool = testPoolWith(&auth.Auth{UID: "audit-a", AccessToken: "fixture-a", ExpiresAt: 9999999999}, &auth.Auth{UID: "audit-b", AccessToken: "fixture-b", ExpiresAt: 9999999999})
				h.cfg.MaxRotate = 2
				calls := 0
				h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) {
					calls++
					if calls == 2 && succeedAfterRetry {
						return 200, postreleaseUsageContent + postreleaseFinish("stop") + postreleaseUsageOnly + "data: [DONE]\n\n", true
					}
					body := `{"error":{"message":"fixture server failure"}}`
					if firstKnown && calls == 1 {
						body = `{"error":{"message":"fixture server failure"},"usage":{"prompt_tokens":100,"completion_tokens":2,"credit":0.5}}`
					}
					return 500, body, false
				})
				h.ServeHTTP(httptest.NewRecorder(), postreleaseUsageRequest("/v1/chat/completions", true, ""))
				got := ledger.Snapshot().Totals
				var prompt, completion, failed, unknown int64
				credit := 0.0
				if firstKnown {
					prompt = 100
					completion = 2
					credit = 0.5
				}
				if succeedAfterRetry {
					prompt += 5000
					completion += 120
					credit += 1.25
				} else {
					failed = 1
				}
				if !firstKnown || !succeedAfterRetry {
					unknown = 1
				}
				if calls != 2 || got.Requests != 1 || got.FailedRequests != failed || got.UnreportedRequests != unknown || got.PromptTokens != prompt || got.CompletionTokens != completion || got.Credit != credit {
					t.Fatalf("retry counted twice or discarded known attempt: calls=%d totals=%+v", calls, got)
				}
			})
		}
	}
}

type usageFinalWriteFailure struct {
	*httptest.ResponseRecorder
	match  string
	failed bool
}

func (w *usageFinalWriteFailure) Write(raw []byte) (int, error) {
	if strings.Contains(string(raw), w.match) {
		w.failed = true
		return 0, errors.New("fixture disconnect on final payload")
	}
	return w.ResponseRecorder.Write(raw)
}

func (w *usageFinalWriteFailure) WriteString(raw string) (int, error) { return w.Write([]byte(raw)) }

func TestUsageOutcomeFinalPayloadWriteFailure(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+postreleaseUsageOnly+"data: [DONE]\n\n")
				match := `"object":"chat.completion"`
				if path == "/v1/responses" {
					match = `"object":"response"`
				}
				if stream {
					match = "[DONE]"
					if path == "/v1/responses" {
						match = "response.completed"
					}
				}
				w := &usageFinalWriteFailure{ResponseRecorder: httptest.NewRecorder(), match: match}
				h.ServeHTTP(w, postreleaseUsageRequest(path, stream, ""))
				got := ledger.Snapshot().Totals
				if !w.failed || got.Requests != 1 || got.FailedRequests != 1 || got.UnreportedRequests != 0 || got.TotalTokens != 5120 || got.Credit != 1.25 {
					t.Fatalf("final write failure accounting: hit=%t totals=%+v", w.failed, got)
				}
			})
		}
	}
}

func TestUsageOutcomeMissingUsageLogsDash(t *testing.T) {
	withChatLog(t)
	h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+"data: [DONE]\n\n")
	output := captureStdout(t, func() { h.ServeHTTP(httptest.NewRecorder(), postreleaseUsageRequest("/v1/chat/completions", true, "")) })
	for _, expected := range []string{"in=-", "hit=-", "tok=-"} {
		if !strings.Contains(output, expected) {
			t.Errorf("missing field shown as zero: %s", output)
		}
	}
	if got := ledger.Snapshot().Totals; got.UnreportedRequests != 1 || got.Requests != 1 {
		t.Fatalf("missing usage outcome=%+v", got)
	}
}

func TestUsageOutcomeCancelledUpstreamCountsOnce(t *testing.T) {
	h, ledger := postreleaseUsageHandler(t, sseOK)
	req := postreleaseUsageRequest("/v1/chat/completions", true, "")
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	calls := 0
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; cancel(); return nil, context.Canceled })
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	if got := ledger.Snapshot().Totals; calls != 1 || got.Requests != 1 || got.FailedRequests != 1 || got.UnreportedRequests != 1 || got.TotalTokens != 0 {
		t.Fatalf("cancelled upstream attempt lost or counted more than once: calls=%d totals=%+v", calls, got)
	}
}

func TestUsageOutcomeDisconnectBeforeUsageStaysUnknown(t *testing.T) {
	h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+postreleaseUsageOnly+"data: [DONE]\n\n")
	w := &usageFinalWriteFailure{ResponseRecorder: httptest.NewRecorder(), match: "answer"}
	h.ServeHTTP(w, postreleaseUsageRequest("/v1/chat/completions", true, ""))
	if got := ledger.Snapshot().Totals; !w.failed || got.Requests != 1 || got.FailedRequests != 1 || got.UnreportedRequests != 1 || got.TotalTokens != 0 || got.Credit != 0 {
		t.Fatalf("unread usage was fabricated after disconnect: hit=%t totals=%+v", w.failed, got)
	}
}

func TestUsageOutcomeRejectsMalformedMeasurementEvent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			invalid := strings.TrimSuffix(postreleaseUsageOnly, "\n") + "data: invalid trailing JSON\n\n"
			h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+invalid)
			h.ServeHTTP(httptest.NewRecorder(), postreleaseUsageRequest("/v1/chat/completions", stream, ""))
			if got := ledger.Snapshot().Totals; got.Requests != 1 || got.FailedRequests != 1 || got.UnreportedRequests != 1 || got.TotalTokens != 0 || got.Credit != 0 {
				t.Fatalf("malformed SSE event was treated as a valid billed measurement: %+v", got)
			}
		})
	}
}

func TestUsageOutcomeInternalGlobalFallback(t *testing.T) {
	for _, firstStatus := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		for _, firstKnown := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("status=%d/known=%t/stream=%t", firstStatus, firstKnown, stream), func(t *testing.T) {
					auth.SetGlobalEnabled(true)
					t.Cleanup(func() { auth.SetGlobalEnabled(true) })
					var mu sync.Mutex
					paths := []string{}
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						paths = append(paths, r.URL.Path)
						mu.Unlock()
						if r.URL.Path == "/v2/chat/completions" {
							body := `{"code":404,"msg":"fixture fallback"}`
							if firstKnown {
								body = `{"code":404,"msg":"fixture fallback","usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_cache_hit_tokens":80,"credit":0.5}}`
							}
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(firstStatus)
							_, _ = w.Write([]byte(body))
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte(postreleaseUsageContent + postreleaseFinish("stop") + postreleaseUsageOnly + "data: [DONE]\n\n"))
					}))
					defer srv.Close()
					h, ledger := postreleaseUsageHandler(t, sseOK)
					h.cfg.Pool = testPoolWith(&auth.Auth{UID: "audit-global", Domain: "www.workbuddy.ai", AccessToken: "fixture-global", ExpiresAt: 9999999999})
					h.cfg.GlobalEnabled = true
					h.cfg.Upstream.GlobalEnabled = true
					h.cfg.Upstream.ChatBaseGlobal = srv.URL
					h.cfg.Upstream.HTTP = srv.Client()
					req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"global:deepseek-v4.1-flash","stream":%t,"messages":[{"role":"user","content":"audit"}]}`, stream)))
					rr := httptest.NewRecorder()
					h.ServeHTTP(rr, req)
					mu.Lock()
					gotPaths := strings.Join(paths, ",")
					mu.Unlock()
					if rr.Code != 200 || gotPaths != "/v2/chat/completions,/console/chat/completions" {
						t.Fatalf("fixture did not exercise internal fallback: code=%d paths=%s body=%s", rr.Code, gotPaths, rr.Body.String())
					}
					want := usage.Totals{Requests: 1, PromptTokens: 5000, CompletionTokens: 120, CachedTokens: 4096, TotalTokens: 5120, Credit: 1.25, UnreportedRequests: 1}
					if firstKnown {
						want.PromptTokens += 100
						want.CompletionTokens += 2
						want.CachedTokens += 80
						want.TotalTokens += 102
						want.Credit += 0.5
						want.UnreportedRequests = 0
					}
					if got := ledger.Snapshot().Totals; got != want {
						t.Fatalf("internal fallback swallowed an attempt: got=%+v want=%+v", got, want)
					}
				})
			}
		}
	}
}

func TestUsageOutcomeInternalSamePathRetries(t *testing.T) {
	for _, kind := range []string{"channel", "waf"} {
		for _, firstKnown := range []bool{false, true} {
			if kind == "waf" && firstKnown {
				continue
			} // The observed WAF response is HTML, without JSON usage.
			for _, finalSuccess := range []bool{false, true} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/known=%t/success=%t/stream=%t", kind, firstKnown, finalSuccess, stream), func(t *testing.T) {
						h, ledger := postreleaseUsageHandler(t, sseOK)
						calls := 0
						h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) {
							calls++
							if calls == 1 {
								if kind == "waf" {
									return 403, `<html><head><title>WAF Block Page</title></head><body>request blocked by WAF</body></html>`, false
								}
								body := `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`
								if firstKnown {
									body = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","usage":{"prompt_tokens":100,"completion_tokens":2,"credit":0.5}}`
								}
								return 400, body, false
							}
							if finalSuccess {
								return 200, postreleaseUsageContent + postreleaseFinish("stop") + postreleaseUsageOnly + "data: [DONE]\n\n", true
							}
							return 400, `{"code":11101,"msg":"Unmarshal chat params failed","usage":{"prompt_tokens":200,"completion_tokens":3,"credit":0.75}}`, false
						})
						input := "audit"
						if kind == "waf" {
							input = "<script>fixture</script>"
						}
						body := fmt.Sprintf(`{"model":"cn:deepseek-v4.1-flash","stream":%t,"messages":[{"role":"system","content":"Codex CLI is an open source project led by OpenAI"},{"role":"user","content":%q}]}`, stream, input)
						h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
						want := usage.Totals{Requests: 1, FailedRequests: 1, UnreportedRequests: 1, PromptTokens: 200, CompletionTokens: 3, TotalTokens: 203, Credit: 0.75}
						if finalSuccess {
							want.FailedRequests = 0
							want.PromptTokens = 5000
							want.CompletionTokens = 120
							want.TotalTokens = 5120
							want.CachedTokens = 4096
							want.Credit = 1.25
						}
						if firstKnown {
							want.PromptTokens += 100
							want.CompletionTokens += 2
							want.TotalTokens += 102
							want.Credit += 0.5
							want.UnreportedRequests = 0
						}
						if got := ledger.Snapshot().Totals; calls != 2 || got != want {
							t.Fatalf("same-path retry usage missing or counted twice: calls=%d got=%+v want=%+v", calls, got, want)
						}
					})
				}
			}
		}
	}
}

func TestUsageOutcomeInternalRetryThenTransportFailure(t *testing.T) {
	h, ledger := postreleaseUsageHandler(t, sseOK)
	calls := 0
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":11128,"msg":"Illegal API invocation from an unapproved channel","usage":{"prompt_tokens":100,"completion_tokens":2,"credit":0.5}}`))}, nil
		}
		return nil, errors.New("fixture retry transport failure")
	})
	body := `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"system","content":"Codex CLI is an open source project led by OpenAI"},{"role":"user","content":"audit"}]}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	want := usage.Totals{Requests: 1, FailedRequests: 1, UnreportedRequests: 1, PromptTokens: 100, CompletionTokens: 2, TotalTokens: 102, Credit: 0.5}
	if got := ledger.Snapshot().Totals; calls != 2 || got != want {
		t.Fatalf("transport failure lost earlier internal attempt: calls=%d got=%+v want=%+v", calls, got, want)
	}
}
