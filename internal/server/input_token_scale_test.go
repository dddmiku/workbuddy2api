// ═══ 更新日志 ═══
// 2026-09-19：新增。锁定「上报用量换算到上游上限口径」这一层：
// 关闭时零改动、开启时只动输入侧且保持 total 与 cached 的不变式，
// 流式与非流式两条出口都要换算。
// 2026-09-19：补充非有限配置、整数溢出及1.5倍正常输入的回归，保持估计用量自洽。
package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

func TestScaleContextUsageDisabledIsNoop(t *testing.T) {
	usage := map[string]any{
		"input_tokens":         1000,
		"output_tokens":        20,
		"total_tokens":         1020,
		"input_tokens_details": map[string]any{"cached_tokens": 900},
	}
	scaleContextUsage(usage, 1)
	if usage["input_tokens"] != 1000 || usage["total_tokens"] != 1020 {
		t.Fatalf("scale=1 不应改动 usage: %#v", usage)
	}
	if cached := usage["input_tokens_details"].(map[string]any)["cached_tokens"]; cached != 900 {
		t.Fatalf("scale=1 不应改动 cached: %v", cached)
	}
}

func TestScaleContextUsageInvalidScaleIsNoop(t *testing.T) {
	for _, scale := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 6} {
		usage := map[string]any{
			"input_tokens": 1000, "output_tokens": 20, "total_tokens": 1020,
			"input_tokens_details": map[string]any{"cached_tokens": 900},
		}
		want := map[string]any{
			"input_tokens": 1000, "output_tokens": 20, "total_tokens": 1020,
			"input_tokens_details": map[string]any{"cached_tokens": 900},
		}
		scaleContextUsage(usage, scale)
		if !reflect.DeepEqual(usage, want) {
			t.Fatalf("invalid scale=%v changed usage: %#v", scale, usage)
		}
	}
}

func TestScaleContextUsageSaturatesWithoutOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, input := range []int{maxInt / 2, maxInt - 20} {
		usage := map[string]any{
			"input_tokens": input, "output_tokens": 20, "total_tokens": input + 20,
			"input_tokens_details": map[string]any{"cached_tokens": input},
		}
		scaleContextUsage(usage, 5)
		gotInput := intOf(usage["input_tokens"])
		gotCached := intOf(usage["input_tokens_details"].(map[string]any)["cached_tokens"])
		if gotInput != maxInt-20 || gotCached != gotInput || intOf(usage["total_tokens"]) != maxInt {
			t.Fatalf("overflow or inconsistent saturation for input=%d: %#v", input, usage)
		}
		if usage["output_tokens"] != 20 {
			t.Fatalf("scale changed output tokens: %#v", usage)
		}
	}
}

func TestScaleContextUsageOnePointFive(t *testing.T) {
	usage := map[string]any{
		"input_tokens": 1001, "output_tokens": 20, "total_tokens": 1021,
		"input_tokens_details": map[string]any{"cached_tokens": 999},
	}
	scaleContextUsage(usage, 1.5)
	if usage["input_tokens"] != 1502 || usage["total_tokens"] != 1522 || usage["output_tokens"] != 20 ||
		usage["input_tokens_details"].(map[string]any)["cached_tokens"] != 1499 {
		t.Fatalf("1.5 scale rounding changed: %#v", usage)
	}
}

func TestScaleContextUsageKeepsInvariants(t *testing.T) {
	usage := map[string]any{
		"input_tokens":         1000,
		"output_tokens":        20,
		"total_tokens":         1020,
		"input_tokens_details": map[string]any{"cached_tokens": 900},
	}
	scaleContextUsage(usage, 1.4)
	if got := usage["input_tokens"]; got != 1400 {
		t.Fatalf("input_tokens=%v want 1400", got)
	}
	if got := usage["output_tokens"]; got != 20 {
		t.Fatalf("输出侧不应换算: %v", got)
	}
	if got := usage["total_tokens"]; got != 1420 {
		t.Fatalf("total_tokens=%v want 1420（input+output）", got)
	}
	cached := usage["input_tokens_details"].(map[string]any)["cached_tokens"]
	if cached != 1260 {
		t.Fatalf("cached_tokens=%v want 1260", cached)
	}
	if cached.(int) > usage["input_tokens"].(int) {
		t.Fatalf("cached 不能大于 input: %#v", usage)
	}
}

func TestScaleContextUsageClampsCachedAndSkipsEmptyInput(t *testing.T) {
	usage := map[string]any{
		"input_tokens":         10,
		"output_tokens":        0,
		"total_tokens":         10,
		"input_tokens_details": map[string]any{"cached_tokens": 10},
	}
	scaleContextUsage(usage, 5)
	if usage["input_tokens"] != 50 {
		t.Fatalf("input_tokens=%v want 50", usage["input_tokens"])
	}
	if cached := usage["input_tokens_details"].(map[string]any)["cached_tokens"]; cached != 50 {
		t.Fatalf("cached 应夹到 input 以内: %v", cached)
	}

	empty := map[string]any{"input_tokens": 0, "output_tokens": 7, "total_tokens": 7}
	scaleContextUsage(empty, 1.4)
	if empty["input_tokens"] != 0 || empty["total_tokens"] != 7 {
		t.Fatalf("缺 usage 时不应凭空造数: %#v", empty)
	}
}

func TestUsageObjectAppliesScale(t *testing.T) {
	rw := newResponsesWriter(httptest.NewRecorder(), nil)
	rw.scale = 1.4
	rw.usage = map[string]any{
		"prompt_tokens":           663178,
		"completion_tokens":       1000,
		"total_tokens":            664178,
		"prompt_cache_hit_tokens": 663040,
	}
	usage := rw.usageObject()
	if got := usage["input_tokens"]; got != 928449 {
		t.Fatalf("input_tokens=%v want 928449", got)
	}
	if got := usage["total_tokens"]; got != 929449 {
		t.Fatalf("total_tokens=%v want 929449", got)
	}
	if cached := usage["input_tokens_details"].(map[string]any)["cached_tokens"]; cached != 928256 {
		t.Fatalf("cached_tokens=%v want 928256", cached)
	}
}

func TestStreamingUsageIsScaled(t *testing.T) {
	req := &responsesRequest{Model: "global:deepseek-v4.1-flash", Stream: true}
	frames := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"好"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100000,"completion_tokens":10,"total_tokens":100010,"prompt_cache_hit_tokens":99000}}`,
	}
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, req)
	rw.scale = 1.4
	if err := upstream.Stream(rw, strings.NewReader(outputIntegritySSE(frames...))); err != nil {
		t.Fatalf("stream: %v", err)
	}
	rw.finish()
	_, values := eventsOf(t, rec.Body.String())
	if len(values) == 0 {
		t.Fatal("没有事件")
	}
	response := values[len(values)-1]["response"].(map[string]any)
	usage, _ := response["usage"].(map[string]any)
	// eventsOf 走标准库解码，数字是 float64。
	if got := usage["input_tokens"]; got != float64(140000) {
		t.Fatalf("流式 input_tokens=%v want 140000", got)
	}
	if got := usage["total_tokens"]; got != float64(140010) {
		t.Fatalf("流式 total_tokens=%v want 140010", got)
	}
}

func TestNonStreamingUsageIsScaled(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl-1", "model": "global:deepseek-v4.1-flash", "created": float64(1),
		"choices": []any{map[string]any{
			"index": float64(0), "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "好"}}},
		"usage": map[string]any{
			"prompt_tokens": float64(50000), "completion_tokens": float64(10),
			"total_tokens": float64(50010), "prompt_cache_hit_tokens": float64(40000)},
	}
	raw, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, nil)
	rw.scale = 1.4
	rw.buf = raw
	rw.finishJSON()

	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	usage, _ := result["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != float64(70000) {
		t.Fatalf("非流式 input_tokens=%v want 70000", got)
	}
	if got := usage["total_tokens"]; got != float64(70010) {
		t.Fatalf("非流式 total_tokens=%v want 70010", got)
	}
}

func TestInputTokenScaleHandlerKeepsChatAndLedgerRaw(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions"} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s_stream_%t", endpoint, stream), func(t *testing.T) {
				ledger, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ledger.Close() })
				h := NewHandler(Config{
					Pool: testPoolWith(&auth.Auth{UID: "scale-fixture", AccessToken: "fixture", ExpiresAt: 9999999999}),
					Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
						return http.StatusOK, sseCacheHit, true
					}),
					APIKey: "scale-fixture", Usage: ledger, InputTokenScale: 1.5,
				})
				body := fmt.Sprintf(`{"model":"cn:deepseek-v4.1-flash","stream":%t,"input":"hi"}`, stream)
				if endpoint == "/v1/chat/completions" {
					body = fmt.Sprintf(`{"model":"cn:deepseek-v4.1-flash","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
				}
				req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer scale-fixture")
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
				if endpoint == "/v1/responses" {
					if intOf(clientUsage["input_tokens"]) != 7500 || intOf(clientUsage["total_tokens"]) != 7620 {
						t.Fatalf("Responses scale wiring failed: %#v", clientUsage)
					}
				} else if intOf(clientUsage["prompt_tokens"]) != 5000 || intOf(clientUsage["total_tokens"]) != 5120 {
					t.Fatalf("Chat changed by scale: %#v", clientUsage)
				}
				totals := ledger.Snapshot().Totals
				if totals.Requests != 1 || totals.PromptTokens != 5000 || totals.CompletionTokens != 120 ||
					totals.CachedTokens != 4096 || totals.TotalTokens != 5120 {
					t.Fatalf("ledger changed by scale: %+v", totals)
				}
			})
		}
	}
}
