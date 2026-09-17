// ═══ 更新日志 ═══
// 2026-09-18：锁定上游 400 的形状归因日志：只统计消息形状，不含正文。
package server

import (
	"context"
	"strings"
	"testing"
)

func TestChatShapeSummaryCountsReasoningContent(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[
	  {"role":"system","content":"sys"},
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}],"reasoning_content":"想用工具"},
	  {"role":"tool","tool_call_id":"c1","content":"ok"},
	  {"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"lookup","arguments":"{}"}}],"reasoning_content":""},
	  {"role":"tool","tool_call_id":"c2","content":"ok"}]}`)
	summary := chatShapeSummary(body, reasoningStats{Items: 3, WithText: 2}, true)
	for _, want := range []string{
		"model=deepseek-v4.1-flash", "messages=6", "assistant=2",
		"reasoning_content=2", "empty=1", "tool_call_msgs=2",
		"last_role=tool", "history_reasoning_items=3", "with_text=2",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("形状摘要缺 %q: %s", want, summary)
		}
	}
}

func TestChatShapeSummarySurvivesBrokenBody(t *testing.T) {
	summary := chatShapeSummary([]byte("not json"), reasoningStats{}, false)
	if !strings.Contains(summary, "unparsable") {
		t.Fatalf("坏 body 应给出 unparsable 摘要，得到 %s", summary)
	}
	if strings.Contains(summary, "history_reasoning_items") {
		t.Fatalf("没有 Responses 侧统计时不应输出该项: %s", summary)
	}
}

func TestReasoningStatsContextRoundTrip(t *testing.T) {
	ctx := withReasoningStats(context.Background(), reasoningStats{Items: 2, WithText: 1})
	stats, ok := reasoningStatsFrom(ctx)
	if !ok || stats.Items != 2 || stats.WithText != 1 {
		t.Fatalf("上下文回读 = %+v ok=%v", stats, ok)
	}
	if _, ok := reasoningStatsFrom(context.Background()); ok {
		t.Fatalf("未写入时不应命中上下文")
	}
}
