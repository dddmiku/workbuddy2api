// ═══ 更新日志 ═══
// 2026-09-15: 新增。/v1/responses 兼容层单测：请求翻译、工具翻译、非流式对象翻译、
//   流式事件序列（含推理条目与工具调用）。
// 2026-09-16: 新增工具输出图片用例——含图保留 part 数组 + detail；纯文本仍退化字符串。

package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeChat(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("翻译后的 body 不是合法 JSON: %v", err)
	}
	return got
}

func rolesOf(t *testing.T, chat map[string]any) []string {
	t.Helper()
	raw, ok := chat["messages"].([]any)
	if !ok {
		t.Fatalf("messages 不是数组: %#v", chat["messages"])
	}
	out := make([]string, 0, len(raw))
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

func TestResponsesStringInput(t *testing.T) {
	body := []byte(`{"model":"cn:auto","input":"你好","instructions":"你是助手","stream":false}`)
	chatBody, req, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if req.Stream {
		t.Fatal("stream 应为 false")
	}
	chat := decodeChat(t, chatBody)
	if chat["model"] != "cn:auto" {
		t.Fatalf("model 未透传: %v", chat["model"])
	}
	if got := rolesOf(t, chat); len(got) != 2 || got[0] != "system" || got[1] != "user" {
		t.Fatalf("instructions 应折成首条 system，消息序列为 %v", got)
	}
	msgs := chat["messages"].([]any)
	if c := msgs[1].(map[string]any)["content"]; c != "你好" {
		t.Fatalf("user 内容不对: %v", c)
	}
}

func TestResponsesArrayInputAndParams(t *testing.T) {
	body := []byte(`{"model":"cn:auto","input":[{"role":"user","content":[{"type":"input_text","text":"第一段"},{"type":"input_text","text":"第二段"}]}],"max_output_tokens":512,"temperature":0.3,"top_p":0.9}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	chat := decodeChat(t, chatBody)
	if chat["max_tokens"] != float64(512) {
		t.Fatalf("max_output_tokens 未映射到 max_tokens: %v", chat["max_tokens"])
	}
	if chat["temperature"] != 0.3 || chat["top_p"] != 0.9 {
		t.Fatalf("采样参数未透传: %v / %v", chat["temperature"], chat["top_p"])
	}
	// 纯文本 part 应合并成单条字符串，而不是留 part 数组。
	msgs := chat["messages"].([]any)
	if c, ok := msgs[0].(map[string]any)["content"].(string); !ok || c != "第一段\n第二段" {
		t.Fatalf("纯文本 part 未合并: %#v", msgs[0].(map[string]any)["content"])
	}
}

func TestResponsesFunctionCallPairing(t *testing.T) {
	body := []byte(`{"model":"cn:auto","input":[
		{"role":"user","content":"北京天气"},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
		{"type":"function_call","call_id":"call_2","name":"get_time","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"晴 26 度"}
	]}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	chat := decodeChat(t, chatBody)
	roles := rolesOf(t, chat)
	want := []string{"user", "assistant", "tool"}
	if len(roles) != len(want) {
		t.Fatalf("消息数应为 %d，实际 %v", len(want), roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("第 %d 条角色应为 %s，实际 %v", i, want[i], roles)
		}
	}
	// 连续两个 function_call 必须并进同一条 assistant.tool_calls（拆开会被上游拒）。
	tc := chat["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)
	if len(tc) != 2 {
		t.Fatalf("tool_calls 应合并为 1 条 assistant 上的 2 个调用，实际 %d", len(tc))
	}
	toolMsg := chat["messages"].([]any)[2].(map[string]any)
	if toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "晴 26 度" {
		t.Fatalf("tool 结果映射不对: %#v", toolMsg)
	}
}

func TestResponsesToolsAndChoice(t *testing.T) {
	body := []byte(`{"model":"cn:auto","input":"hi","tools":[
		{"type":"function","name":"f1","description":"d1","parameters":{"type":"object"}},
		{"type":"web_search"}
	],"tool_choice":{"type":"function","name":"f1"}}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	chat := decodeChat(t, chatBody)
	tools, ok := chat["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("非 function 工具应被丢弃，实际 %#v", chat["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "f1" || fn["description"] != "d1" {
		t.Fatalf("工具未转成 chat 嵌套形状: %#v", tools[0])
	}
	tc := chat["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "f1" {
		t.Fatalf("tool_choice 未转换: %#v", chat["tool_choice"])
	}
}

func sseStream(chunks ...string) []byte {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}

// eventsOf 把写出的 SSE 文本解析成 (事件名, data) 序列。
func eventsOf(t *testing.T, raw string) ([]string, []map[string]any) {
	t.Helper()
	var names []string
	var datas []map[string]any
	for _, block := range strings.Split(raw, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		name, payload := "", ""
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				payload = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			continue
		}
		names = append(names, name)
		var m map[string]any
		_ = json.Unmarshal([]byte(payload), &m)
		datas = append(datas, m)
	}
	return names, datas
}

func TestResponsesWriterStreamWithReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, &responsesRequest{Model: "cn:auto", Stream: true})
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.WriteHeader(200)
	_, _ = rw.Write(sseStream(
		`{"id":"c1","model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","model":"glm-5.3","choices":[{"index":0,"delta":{"reasoning_content":"先想想"},"finish_reason":null}]}`,
		`{"id":"c1","model":"glm-5.3","choices":[{"index":0,"delta":{"content":"收到"},"finish_reason":null}]}`,
		`{"id":"c1","model":"glm-5.3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"completion_thinking_tokens":3}}`,
	))
	rw.finish()

	names, datas := eventsOf(t, rec.Body.String())
	want := []string{
		evCreated, evInProgress,
		evItemAdded, evRsPartAdded, evRsDelta,
		evRsDone, evRsPartDone, evItemDone,
		evItemAdded, evPartAdded, evTextDelta,
		evTextDone, evPartDone, evItemDone,
		evCompleted,
	}
	if len(names) != len(want) {
		t.Fatalf("事件数应为 %d，实际 %d：%v", len(want), len(names), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("第 %d 个事件应为 %s，实际 %s（完整序列 %v）", i, want[i], names[i], names)
		}
	}
	// 推理条目必须排在正文之前：Responses 的 output 顺序即 output_index 顺序。
	if datas[2]["output_index"] != float64(0) || datas[8]["output_index"] != float64(1) {
		t.Fatalf("output_index 分配不对: 推理=%v 正文=%v", datas[2]["output_index"], datas[8]["output_index"])
	}
	final := datas[len(datas)-1]["response"].(map[string]any)
	if final["status"] != "completed" {
		t.Fatalf("终态应为 completed: %v", final["status"])
	}
	out := final["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output 应有推理+正文两条，实际 %d", len(out))
	}
	if out[0].(map[string]any)["type"] != "reasoning" || out[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output 条目类型不对: %v / %v",
			out[0].(map[string]any)["type"], out[1].(map[string]any)["type"])
	}
	msg := out[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if msg["text"] != "收到" {
		t.Fatalf("正文内容不对: %v", msg["text"])
	}
	if rs := out[0].(map[string]any)["summary"].([]any)[0].(map[string]any)["text"]; rs != "先想想" {
		t.Fatalf("推理内容不对: %v", rs)
	}
	u := final["usage"].(map[string]any)
	if u["input_tokens"] != float64(10) || u["output_tokens"] != float64(5) || u["total_tokens"] != float64(15) {
		t.Fatalf("usage 映射不对: %#v", u)
	}
	if u["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(3) {
		t.Fatalf("推理 token 未映射: %#v", u["output_tokens_details"])
	}
}

func TestResponsesWriterStreamToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, &responsesRequest{Model: "cn:auto", Stream: true})
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.WriteHeader(200)
	_, _ = rw.Write(sseStream(
		`{"id":"c2","model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`{"id":"c2","model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c2","model":"glm-5.3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	))
	rw.finish()

	names, datas := eventsOf(t, rec.Body.String())
	if names[len(names)-1] != evCompleted {
		t.Fatalf("末事件应为 completed: %v", names)
	}
	sawDelta := false
	for i, n := range names {
		if n == evArgsDelta && !sawDelta {
			sawDelta = true
			if datas[i]["delta"] != `{"city":` {
				t.Fatalf("首个参数分片不对: %v", datas[i]["delta"])
			}
		}
	}
	if !sawDelta {
		t.Fatal("未产出 function_call_arguments.delta")
	}
	final := datas[len(datas)-1]["response"].(map[string]any)
	out := final["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output 应只有一条 function_call，实际 %d", len(out))
	}
	call := out[0].(map[string]any)
	if call["type"] != "function_call" || call["name"] != "get_weather" {
		t.Fatalf("function_call 条目不对: %#v", call)
	}
	if call["arguments"] != `{"city":"北京"}` {
		t.Fatalf("参数未按分片拼齐: %v", call["arguments"])
	}
	if call["call_id"] != "call_1" {
		t.Fatalf("call_id 未透传: %v", call["call_id"])
	}
}

func TestResponsesWriterPassthroughError(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, &responsesRequest{Model: "cn:auto", Stream: true})
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(503)
	_, _ = rw.Write([]byte(`{"error":{"message":"busy","type":"api_error","code":"no_healthy_account"}}`))
	rw.finish()
	if rec.Code != 503 {
		t.Fatalf("错误状态码应原样透传，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Fatalf("错误体应原样透传: %s", rec.Body.String())
	}
}

func TestChatToResponsesNonStream(t *testing.T) {
	chat := map[string]any{
		"id": "abc123", "object": "chat.completion", "created": float64(1700000000),
		"model": "glm-5.3",
		"choices": []any{map[string]any{
			"index": float64(0), "finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant", "content": "查一下",
				"reasoning_content": "需要天气",
				"tool_calls": []any{map[string]any{
					"id": "call_9", "type": "function",
					"function": map[string]any{"name": "get_weather", "arguments": `{"city":"上海"}`},
				}},
			},
		}},
		"usage": map[string]any{
			"prompt_tokens": float64(20), "completion_tokens": float64(9),
			"total_tokens": float64(29), "completion_thinking_tokens": float64(4),
			"prompt_cache_hit_tokens": float64(6),
		},
	}
	obj := chatToResponses(chat, "cn:auto")
	if obj["id"] != "resp_abc123" || obj["object"] != "response" {
		t.Fatalf("id/object 不对: %v / %v", obj["id"], obj["object"])
	}
	if obj["created_at"] != int64(1700000000) {
		t.Fatalf("created_at 未沿用上游: %v", obj["created_at"])
	}
	out := obj["output"].([]any)
	if len(out) != 3 {
		t.Fatalf("output 应为 推理+正文+工具调用 三条，实际 %d", len(out))
	}
	if out[0].(map[string]any)["type"] != "reasoning" ||
		out[1].(map[string]any)["type"] != "message" ||
		out[2].(map[string]any)["type"] != "function_call" {
		t.Fatal("output 条目类型顺序不对")
	}
	if out[2].(map[string]any)["arguments"] != `{"city":"上海"}` {
		t.Fatalf("工具参数未透传: %v", out[2].(map[string]any)["arguments"])
	}
	u := obj["usage"].(map[string]any)
	// 非流式路径的 usage 是 Go 侧直接构造的 int（不是 JSON 反序列化来的 float64）。
	if u["input_tokens"] != 20 || u["output_tokens"] != 9 || u["total_tokens"] != 29 {
		t.Fatalf("usage 映射不对: %#v", u)
	}
	if u["input_tokens_details"].(map[string]any)["cached_tokens"] != 6 {
		t.Fatalf("缓存 token 未映射: %#v", u["input_tokens_details"])
	}
	if u["output_tokens_details"].(map[string]any)["reasoning_tokens"] != 4 {
		t.Fatalf("推理 token 未映射: %#v", u["output_tokens_details"])
	}
}

// 含图的工具输出（view_image）必须留成 part 数组：折成字符串后上游按纯文本计费，
// 一张截图约 10 万 token，十几张就撑爆上下文。同时 detail 必须原样带上。
func TestResponsesToolOutputImageKeepsParts(t *testing.T) {
	uri := "data:image/png;base64,iVBORw0KGgo="
	body := []byte(`{"model":"cn:auto","input":[
		{"role":"user","content":"看图"},
		{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"input_image","image_url":"` + uri + `","detail":"high"},
			{"type":"input_text","text":"loaded"}
		]}
	]}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	chat := decodeChat(t, chatBody)
	msgs := chat["messages"].([]any)
	if len(msgs) < 3 {
		t.Fatalf("消息数不足: %#v", msgs)
	}
	toolMsg := msgs[2].(map[string]any)
	parts, ok := toolMsg["content"].([]any)
	if !ok {
		t.Fatalf("含图的 tool 结果必须保留 part 数组，实际 %#v", toolMsg["content"])
	}
	var sawImage, sawDetail, sawText bool
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		switch pm["type"] {
		case "image_url":
			img, _ := pm["image_url"].(map[string]any)
			if img["url"] == uri {
				sawImage = true
			}
			if img["detail"] == "high" {
				sawDetail = true
			}
		case "text":
			if pm["text"] == "loaded" {
				sawText = true
			}
		}
	}
	if !sawImage {
		t.Fatalf("图片 part 丢了: %#v", parts)
	}
	if !sawDetail {
		t.Fatalf("detail 必须原样带上: %#v", parts)
	}
	if !sawText {
		t.Fatalf("同批文本 part 丢了: %#v", parts)
	}
}

// 纯文本工具输出仍退化成字符串：上游对纯文本 tool 结果最稳，也是历史零回归路径。
func TestResponsesToolOutputTextStaysString(t *testing.T) {
	body := []byte(`{"model":"cn:auto","input":[
		{"role":"user","content":"北京天气"},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":[
			{"type":"input_text","text":"晴 26 度"}
		]}
	]}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	chat := decodeChat(t, chatBody)
	toolMsg := chat["messages"].([]any)[2].(map[string]any)
	if toolMsg["content"] != "晴 26 度" {
		t.Fatalf("纯文本 tool 结果应退化成字符串，实际 %#v", toolMsg["content"])
	}
}
