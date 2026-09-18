// ═══ 更新日志 ═══
// 2026-09-19：新增。锁定「上报用量换算到上游上限口径」这一层：
// 关闭时零改动、开启时只动输入侧且保持 total 与 cached 的不变式，
// 流式与非流式两条出口都要换算。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
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
