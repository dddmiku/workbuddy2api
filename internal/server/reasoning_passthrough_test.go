// ═══ 更新日志 ═══
// 2026-09-18：锁定历史推理内容的回灌：DeepSeek 思考模式要求把上一轮 reasoning_content
// 原样带回（上游 11155 reasoning_content_missing），Conversions 不能把 reasoning 项丢掉。
package server

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

// assistantMessages 取 chat 体里所有 assistant 消息。
func assistantMessages(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	chat := decodeChat(t, body)
	raw, _ := chat["messages"].([]any)
	out := []map[string]any{}
	for _, item := range raw {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "assistant" {
			out = append(out, message)
		}
	}
	return out
}

func TestReasoningHistoryFeedsAssistantReasoningContent(t *testing.T) {
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"查一下天气"},
	  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"先看工具"}]},
	  {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_1","output":"晴天"},
	  {"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"拿到结果了"}],"content":[{"type":"reasoning_text","text":"整理答案"}]},
	  {"role":"assistant","content":"今天晴"}
	],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`

	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	messages := assistantMessages(t, body)
	if len(messages) != 2 {
		t.Fatalf("assistant 消息数 = %d want 2: %s", len(messages), body)
	}
	first, _ := messages[0]["reasoning_content"].(string)
	if first != "先看工具" {
		t.Errorf("tool_calls 那条 assistant 的 reasoning_content = %q want 先看工具", first)
	}
	second, _ := messages[1]["reasoning_content"].(string)
	if !strings.Contains(second, "拿到结果了") || !strings.Contains(second, "整理答案") {
		t.Errorf("第二条 assistant 的 reasoning_content = %q，应包含 summary+content 两段", second)
	}
	if content, _ := messages[1]["content"].(string); content != "今天晴" {
		t.Errorf("assistant 正文被改动: %q", content)
	}
}

func TestReasoningWithoutTraceStaysAbsent(t *testing.T) {
	// 没有推理项时不额外增加字段（避免给非思考模型塞无用参数）。
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if strings.Contains(string(body), "reasoning_content") {
		t.Fatalf("无推理痕迹时不应出现 reasoning_content: %s", body)
	}
}

// TestReasoningStringContentIsAccepted 有的客户端把推理正文直接给字符串 content。
func TestReasoningStringContentIsAccepted(t *testing.T) {
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"hi"},
	  {"type":"reasoning","id":"rs_1","content":"字符串形态的推理"},
	  {"role":"assistant","content":"答案"}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	messages := assistantMessages(t, body)
	if len(messages) != 1 {
		t.Fatalf("assistant 消息数 = %d want 1: %s", len(messages), body)
	}
	if text, _ := messages[0]["reasoning_content"].(string); text != "字符串形态的推理" {
		t.Fatalf("reasoning_content = %q want 字符串形态的推理", text)
	}
}

func TestReasoningPassthroughSurvivesUpstreamBackfill(t *testing.T) {
	// 端到端：转换后再经上游 payload 处理，所有 assistant 消息都必须带 reasoning_content。
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"第一步"},
	  {"role":"assistant","content":"第一步的答案"},
	  {"role":"user","content":"第二步"},
	  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"这次要查工具"}]},
	  {"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":"结果"}],
	  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	// 生产路径：handler 先把 model 改写成裸名，再由上游层做推理回填。
	out := upstream.PrepareBodyOpt(rewriteModel(body, "deepseek-v4.1-flash"), false)
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("回填后的 body 不是 JSON: %v", err)
	}
	raw, _ := chat["messages"].([]any)
	assistants := 0
	for _, item := range raw {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		assistants++
		if _, present := message["reasoning_content"]; !present {
			t.Errorf("assistant 消息缺 reasoning_content: %v", message)
		}
	}
	if assistants == 0 {
		t.Fatalf("没有 assistant 消息: %s", out)
	}
}
