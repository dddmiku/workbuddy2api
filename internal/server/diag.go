// diag.go 上游 400 的形状归因：只记录「消息形状」，不落任何对话正文。
//
// 背景：DeepSeek 思考模式的上游 11155（reasoning_content_missing）只在部分历史形态下
// 触发，客户端侧看不到区别。网关出站前把关键指标压成一行日志，下一次复现就能直接判定是
// 「客户端没带推理项」「带了但转换层丢了」还是「assistant 消息缺 reasoning_content 字段」。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// reasoningStatsContextKey 把 Responses 侧的推理项统计带到 chatCompletions 的错误分支。
type reasoningStatsContextKey struct{}

// withReasoningStats 把推理项统计挂进请求上下文（/v1/responses 路径专用）。
func withReasoningStats(ctx context.Context, stats reasoningStats) context.Context {
	return context.WithValue(ctx, reasoningStatsContextKey{}, stats)
}

// reasoningStatsFrom 取上下文里的推理项统计；/v1/chat/completions 路径没有 → ok=false。
func reasoningStatsFrom(ctx context.Context) (reasoningStats, bool) {
	stats, ok := ctx.Value(reasoningStatsContextKey{}).(reasoningStats)
	return stats, ok
}

// chatShapeSummary 用一行紧凑指标描述将要发给上游的 chat 消息形状（不含正文）。
// 关注点就是思考模式的匹配规则：assistant 消息有没有 reasoning_content（含空串），
// 以及最后一条消息是 assistant 还是 tool/user。
func chatShapeSummary(body []byte, stats reasoningStats, hasStats bool) string {
	var chat struct {
		Messages []map[string]any `json:"messages"`
		Model    string           `json:"model"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return "shape=unparsable error=" + strconv.Quote(err.Error())
	}
	assistant, withReasoning, emptyReasoning, toolCallMessages := 0, 0, 0, 0
	lastRole, lastHasReasoning, lastHasTools := "none", "false", "false"
	for index, message := range chat.Messages {
		role, _ := message["role"].(string)
		if index == len(chat.Messages)-1 {
			lastRole = role
			_, present := message["reasoning_content"]
			lastHasReasoning = strconv.FormatBool(present)
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				lastHasTools = "true"
			}
		}
		if role != "assistant" {
			continue
		}
		assistant++
		if value, present := message["reasoning_content"]; present {
			withReasoning++
			if text, _ := value.(string); strings.TrimSpace(text) == "" {
				emptyReasoning++
			}
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			toolCallMessages++
		}
	}
	summary := fmt.Sprintf(
		"model=%s messages=%d assistant=%d reasoning_content=%d empty=%d tool_call_msgs=%d last_role=%s last_reasoning_content=%s last_tool_calls=%s",
		chat.Model, len(chat.Messages), assistant, withReasoning, emptyReasoning,
		toolCallMessages, lastRole, lastHasReasoning, lastHasTools)
	if hasStats {
		summary += fmt.Sprintf(" history_reasoning_items=%d with_text=%d", stats.Items, stats.WithText)
	}
	return summary
}
