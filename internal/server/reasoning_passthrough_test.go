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

// TestReasoningTextlessItemStillMarksTrace 客户端只剩加密推理（summary/content 都没有明文）
// 时也必须把「本对话走过思考」这件事带出去：上游按字段存在性校验，整条链缺字段就是 11155。
func TestReasoningTextlessItemStillMarksTrace(t *testing.T) {
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"读文件"},
	  {"type":"reasoning","id":"rs_1","summary":[],"content":null,"encrypted_content":"opaque-blob"},
	  {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_1","output":"结果"}],
	  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	messages := assistantMessages(t, body)
	if len(messages) != 1 {
		t.Fatalf("assistant 消息数 = %d want 1: %s", len(messages), body)
	}
	value, present := messages[0]["reasoning_content"]
	if !present {
		t.Fatalf("无明文推理时仍必须带 reasoning_content 字段: %s", body)
	}
	if text, _ := value.(string); text != "" {
		t.Errorf("无明文推理应补空串，得到 %q", text)
	}
}

// TestReasoningTrailingItemAttachesToLastAssistant 历史以推理项收尾（后面没有新的
// assistant 输出）时，这段推理要贴到最后一条 assistant 消息上，不能被静默丢弃。
func TestReasoningTrailingItemAttachesToLastAssistant(t *testing.T) {
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"第一步"},
	  {"role":"assistant","content":"第一步答案"},
	  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"收尾推理"}]}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	messages := assistantMessages(t, body)
	if len(messages) != 1 {
		t.Fatalf("assistant 消息数 = %d want 1: %s", len(messages), body)
	}
	if text, _ := messages[0]["reasoning_content"].(string); text != "收尾推理" {
		t.Fatalf("尾随推理项未贴到最后一条 assistant: %q (%s)", text, body)
	}
}

// TestReasoningFillSkippedForNonDeepSeek 非 deepseek 模型不补 reasoning_content，
// 避免给 glm/kimi 塞上游不认识的字段。
func TestReasoningFillSkippedForNonDeepSeek(t *testing.T) {
	request := `{"model":"global:glm-5.2","stream":false,"input":[
	  {"role":"user","content":"读文件"},
	  {"type":"reasoning","id":"rs_1","summary":[],"content":null,"encrypted_content":"opaque-blob"},
	  {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_1","output":"结果"}],
	  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if strings.Contains(string(body), "reasoning_content") {
		t.Fatalf("非 deepseek 模型不应补 reasoning_content: %s", body)
	}
}

// TestReasoningStatsFeedsDiagnostics 转换层要把推理项形态留在请求对象上，
// 供上游 11155 的归因日志使用。
func TestReasoningStatsFeedsDiagnostics(t *testing.T) {
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"hi"},
	  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"有明文"}]},
	  {"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"opaque"},
	  {"role":"assistant","content":"答案"}]}`
	_, req, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if req.reasoning.Items != 2 || req.reasoning.WithText != 1 {
		t.Fatalf("reasoning stats = %+v want {Items:2 WithText:1}", req.reasoning)
	}
}

// TestAdjacentAssistantMessagesAreMerged 国际版后端要求「一条 assistant 消息 = 一个回合」：
// 模型先写一句正文、再调工具时，Responses 侧是 message + function_call 两个 item，
// 翻译后会变成两条相邻 assistant，global 直接 400（11155）。转换层要并成一条。
func TestAdjacentAssistantMessagesAreMerged(t *testing.T) {
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"读文件再总结"},
	  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"先看一眼"}]},
	  {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_1","output":"内容"},
	  {"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"再核对一遍"}]},
	  {"role":"assistant","content":"我看一下目录"},
	  {"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_2","output":"内容"}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	chat := decodeChat(t, body)
	raw, _ := chat["messages"].([]any)
	roles := make([]string, 0, len(raw))
	for _, item := range raw {
		message, _ := item.(map[string]any)
		role, _ := message["role"].(string)
		roles = append(roles, role)
	}
	for index := 1; index < len(roles); index++ {
		if roles[index] == "assistant" && roles[index-1] == "assistant" {
			t.Fatalf("仍有相邻 assistant 消息（global 会 11155）: %v\n%s", roles, body)
		}
	}
	// 合并后的那一条要同时带正文与 tool_calls，并且正文顺序不乱。
	var mergedText, mergedCalls, mergedReasoning string
	calls := 0
	for _, item := range raw {
		message, _ := item.(map[string]any)
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		if text, _ := message["content"].(string); strings.Contains(text, "我看一下目录") {
			mergedText = text
			if list, ok := message["tool_calls"].([]any); ok {
				calls = len(list)
			}
			mergedReasoning, _ = message["reasoning_content"].(string)
			mergedCalls = "found"
		}
	}
	if mergedCalls == "" || calls != 1 {
		t.Fatalf("正文没有并进带 tool_calls 的那条 assistant: %s", body)
	}
	if mergedText != "我看一下目录" {
		t.Fatalf("合并后正文 = %q want 我看一下目录", mergedText)
	}
	if mergedReasoning != "再核对一遍" {
		t.Fatalf("合并后 reasoning_content = %q want 再核对一遍", mergedReasoning)
	}
}

// TestAssistantTurnFollowedByUserIsNotMerged 正文 assistant 后面紧跟 user（回合边界清晰）
// 时不该被合并——那边界正是上游接受的形状。
func TestAssistantTurnFollowedByUserIsNotMerged(t *testing.T) {
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"想想"}]},
	  {"role":"assistant","content":"第一次回答"},
	  {"role":"user","content":"继续"}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	chat := decodeChat(t, body)
	raw, _ := chat["messages"].([]any)
	if len(raw) != 2 {
		t.Fatalf("消息数 = %d want 2（assistant/user，未被合并）: %s", len(raw), body)
	}
	assistant, _ := raw[0].(map[string]any)
	if text, _ := assistant["content"].(string); text != "第一次回答" {
		t.Fatalf("正文被改动: %s", body)
	}
}

// TestAdjacentAssistantMergeKeepsToolPairing 合并后 tool 消息必须仍能配对上被并入的
// tool_call（上游按 id 配对，错位会变成另一种 400）。
func TestAdjacentAssistantMergeKeepsToolPairing(t *testing.T) {
	request := `{"model":"global:deepseek-v4.1-flash","stream":false,"input":[
	  {"role":"user","content":"跑两条命令"},
	  {"type":"function_call","call_id":"call_a","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_a","output":"甲"},
	  {"role":"assistant","content":"先跑第二条"},
	  {"type":"function_call","call_id":"call_b","name":"lookup","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_b","output":"乙"}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	chat := decodeChat(t, body)
	raw, _ := chat["messages"].([]any)
	declared := map[string]bool{}
	for _, item := range raw {
		message, _ := item.(map[string]any)
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			if id, _ := call["id"].(string); id != "" {
				declared[id] = true
			}
		}
	}
	for _, item := range raw {
		message, _ := item.(map[string]any)
		if role, _ := message["role"].(string); role != "tool" {
			continue
		}
		id, _ := message["tool_call_id"].(string)
		if !declared[id] {
			t.Fatalf("tool 结果 %q 找不到对应的 tool_call: %s", id, body)
		}
	}
}
