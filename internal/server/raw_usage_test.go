// ═══ 更新日志 ═══
// 2026-09-19：锁定 Responses 和 Chat 的流式/非流式出口保留上游输入、缓存、输出与总量。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/usage"
)

func TestRawUsageAcrossProtocolsAndResponseModes(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions"} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s_stream_%t", endpoint, stream), func(t *testing.T) {
				ledger, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ledger.Close() })
				h := NewHandler(Config{
					Pool: testPoolWith(&auth.Auth{UID: "raw-usage-fixture", AccessToken: "fixture", ExpiresAt: 9999999999}),
					Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
						return http.StatusOK, sseCacheHit, true
					}),
					APIKey: "raw-usage-fixture", Usage: ledger,
				})
				body := fmt.Sprintf(`{"model":"cn:deepseek-v4.1-flash","stream":%t,"input":"hi"}`, stream)
				if endpoint == "/v1/chat/completions" {
					body = fmt.Sprintf(`{"model":"cn:deepseek-v4.1-flash","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
				}
				req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer raw-usage-fixture")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
				}
				var clientUsage map[string]any
				if stream {
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data:") {
							continue
						}
						var event map[string]any
						if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
							continue
						}
						if response, ok := event["response"].(map[string]any); ok {
							event = response
						}
						if value, ok := event["usage"].(map[string]any); ok {
							clientUsage = value
						}
					}
				} else {
					var result map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					clientUsage, _ = result["usage"].(map[string]any)
				}
				var input, output, cached int
				if endpoint == "/v1/responses" {
					input, output = intOf(clientUsage["input_tokens"]), intOf(clientUsage["output_tokens"])
					details, _ := clientUsage["input_tokens_details"].(map[string]any)
					cached = intOf(details["cached_tokens"])
				} else {
					input, output = intOf(clientUsage["prompt_tokens"]), intOf(clientUsage["completion_tokens"])
					cached = intOf(clientUsage["prompt_cache_hit_tokens"])
				}
				if input != 5000 || cached != 4096 || output != 120 || intOf(clientUsage["total_tokens"]) != 5120 {
					t.Fatalf("client usage differs from upstream 5000 input / 4096 cached / 120 output / 5120 total: %#v", clientUsage)
				}
				totals := ledger.Snapshot().Totals
				if totals.Requests != 1 || totals.PromptTokens != 5000 || totals.CompletionTokens != 120 ||
					totals.CachedTokens != 4096 || totals.TotalTokens != 5120 {
					t.Fatalf("ledger differs from observed upstream usage: %+v", totals)
				}
			})
		}
	}
}
