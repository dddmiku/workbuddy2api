// ═══ 更新日志 ═══
// 2026-09-19：按字段合并上游累计用量，保留未知与显式零的区别，统一缓存与数值校验。
// 2026-09-19：提供请求局部的内部重试观测回调，公开调用签名和返回的最终响应保持兼容。
package upstream

import (
	"context"
	"encoding/json"
	"math"
)

type chatRetryObserverKey struct{}

// WithChatRetryObserver observes only responses discarded by an internal chat
// retry. The final response is still returned normally and is not observed here,
// avoiding duplicate accounting. Calls are synchronous within ChatStreamContext.
func WithChatRetryObserver(ctx context.Context, observe func([]byte)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if observe == nil {
		return ctx
	}
	return context.WithValue(ctx, chatRetryObserverKey{}, observe)
}

func observeChatRetry(ctx context.Context, body []byte) {
	if observe, ok := ctx.Value(chatRetryObserverKey{}).(func([]byte)); ok && observe != nil {
		observe(body)
	}
}

// UsageCount accepts an explicitly reported non-negative integer. Missing,
// fractional and out-of-range values are unknown, not measured zeroes.
func UsageCount(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, v >= 0
	case int64:
		if v >= 0 && uint64(v) <= uint64(^uint(0)>>1) {
			return int(v), true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return UsageCount(n)
		}
		if n, err := v.Float64(); err == nil {
			return UsageCount(n)
		}
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < float64(^uint(0)>>1) && math.Trunc(v) == v {
			return int(v), true
		}
	}
	return 0, false
}

// UsageCredit distinguishes a missing charge from an explicit free observation.
func UsageCredit(value any) (float64, bool) {
	var credit float64
	switch v := value.(type) {
	case float64:
		credit = v
	case json.Number:
		var err error
		credit, err = v.Float64()
		if err != nil {
			return 0, false
		}
	case int:
		credit = float64(v)
	case int64:
		credit = float64(v)
	default:
		return 0, false
	}
	return credit, credit >= 0 && !math.IsNaN(credit) && !math.IsInf(credit, 0)
}

// CachedInputTokens supports both the upstream and OpenAI detail forms. Cache
// hits are a subset of prompt_tokens and must not be added to the total again.
func CachedInputTokens(values map[string]any) (int, bool) {
	if cached, ok := UsageCount(values["prompt_cache_hit_tokens"]); ok {
		return cached, true
	}
	if details, ok := values["prompt_tokens_details"].(map[string]any); ok {
		return UsageCount(details["cached_tokens"])
	}
	return 0, false
}

// MergeUsage merges cumulative snapshots by field, never by summing frames.
// Metadata-only frames and null values cannot erase already observed counters;
// explicit zero remains a valid replacement. The returned maps do not alias the
// input, so a later client-facing conversion cannot change billing observations.
func MergeUsage(previous, next map[string]any) map[string]any {
	if previous == nil && next == nil {
		return nil
	}
	merged := make(map[string]any, len(previous)+len(next))
	for key, value := range previous {
		if nested, ok := value.(map[string]any); ok {
			value = MergeUsage(nil, nested)
		}
		merged[key] = value
	}
	for key, value := range next {
		if value == nil {
			continue
		}
		switch key {
		case "prompt_tokens", "completion_tokens", "total_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens", "completion_thinking_tokens", "cached_tokens", "reasoning_tokens":
			if _, ok := UsageCount(value); !ok {
				continue
			}
		case "credit":
			if _, ok := UsageCredit(value); !ok {
				continue
			}
		}
		if nested, ok := value.(map[string]any); ok {
			current, _ := merged[key].(map[string]any)
			value = MergeUsage(current, nested)
		}
		merged[key] = value
	}
	if cached, ok := CachedInputTokens(next); ok {
		// Either spelling can arrive later. Keep existing aliases synchronized
		// so a previous vendor field cannot shadow newer OpenAI detail values.
		if _, exists := merged["prompt_cache_hit_tokens"]; exists {
			merged["prompt_cache_hit_tokens"] = cached
		}
		if details, ok := merged["prompt_tokens_details"].(map[string]any); ok {
			if _, exists := details["cached_tokens"]; exists {
				details["cached_tokens"] = cached
			}
		}
	}
	return merged
}
