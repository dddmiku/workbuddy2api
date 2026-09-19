// ═══ 更新日志 ═══
// 2026-09-19：锁定稀疏用量合并、显式零与非法数值，以及缓存明细副本隔离。
package upstream

import (
	"encoding/json"
	"math"
	"testing"
)

func TestMergeUsagePreservesSparseAndNestedMeasurements(t *testing.T) {
	first := map[string]any{"prompt_tokens": 100.0, "completion_tokens": 20.0, "prompt_tokens_details": map[string]any{"cached_tokens": 80.0, "audio_tokens": 3.0}, "credit": 0.5}
	merged := MergeUsage(first, map[string]any{"credit": 0.0, "completion_tokens": nil, "prompt_tokens_details": map[string]any{"audio_tokens": 5.0}})
	if prompt, _ := UsageCount(merged["prompt_tokens"]); prompt != 100 {
		t.Fatalf("prompt lost: %v", merged)
	}
	if output, _ := UsageCount(merged["completion_tokens"]); output != 20 {
		t.Fatalf("output lost: %v", merged)
	}
	if cache, _ := CachedInputTokens(merged); cache != 80 {
		t.Fatalf("nested cache lost: %v", merged)
	}
	if credit, ok := UsageCredit(merged["credit"]); !ok || credit != 0 {
		t.Fatalf("explicit free credit lost: %v", merged)
	}
	merged["prompt_tokens_details"].(map[string]any)["cached_tokens"] = 12
	if cached, _ := CachedInputTokens(first); cached != 80 {
		t.Fatal("merge aliased original billing details")
	}
	updated := MergeUsage(first, map[string]any{"prompt_tokens": 0.0, "completion_tokens": -2.0, "credit": "bad"})
	if prompt, _ := UsageCount(updated["prompt_tokens"]); prompt != 0 {
		t.Fatal("explicit zero must replace previous value")
	}
	if output, _ := UsageCount(updated["completion_tokens"]); output != 20 {
		t.Fatal("invalid output erased previous value")
	}
}

func TestUsageMeasurementsRejectMissingOrInvalidValues(t *testing.T) {
	for _, value := range []any{nil, "12", -1, 1.5, math.NaN(), math.Inf(1), json.Number("9223372036854775808")} {
		if n, ok := UsageCount(value); ok {
			t.Errorf("invalid token %v accepted as %d", value, n)
		}
	}
	for _, value := range []any{0, 0.0, json.Number("0"), json.Number("1e3")} {
		if _, ok := UsageCount(value); !ok {
			t.Errorf("valid token rejected: %v", value)
		}
	}
	for _, value := range []any{nil, "0", -1.0, math.NaN(), math.Inf(1)} {
		if n, ok := UsageCredit(value); ok {
			t.Errorf("invalid credit %v accepted as %v", value, n)
		}
	}
}

func TestMergeUsageCacheAliasesFollowLastObservation(t *testing.T) {
	merged := MergeUsage(map[string]any{"prompt_cache_hit_tokens": 100.0}, map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": 120.0}})
	if cached, _ := CachedInputTokens(merged); cached != 120 {
		t.Fatalf("stale vendor alias hid the later standard cache observation: %v", merged)
	}
	merged = MergeUsage(merged, map[string]any{"prompt_cache_hit_tokens": 140.0})
	details := merged["prompt_tokens_details"].(map[string]any)
	if cached, _ := UsageCount(details["cached_tokens"]); cached != 140 {
		t.Fatalf("stale standard detail hid the later vendor observation: %v", merged)
	}
}
