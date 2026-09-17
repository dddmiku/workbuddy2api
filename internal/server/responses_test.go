// ═══ 更新日志 ═══
// 2026-09-15: 新增。/v1/responses 兼容层单测：请求翻译、工具翻译、非流式对象翻译、
//   流式事件序列（含推理条目与工具调用）。
// 2026-09-16: 新增工具输出图片用例——含图保留 part 数组 + detail；纯文本仍退化字符串。
// 2026-09-16: 补 custom 工具桥接用例（入站折成 function{input}、出站还原 custom_tool_call、
//   历史项互逆折回、流式不发 arguments.delta、非流式按名还原），并更新 chatToResponses 签名。

// ═══ 更新日志 ═══
// 2026-09-16：补充已有部分输出后的 failed 终态及 length/content_filter 的 incomplete 与工具状态回归。
// 2026-09-17：合并两侧有效转换断言，未知工具由静默丢弃改为明确拒绝并保留相应用例。
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
		{"type":"function","name":"f1","description":"d1","parameters":{"type":"object"}}
	],"tool_choice":{"type":"function","name":"f1"}}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	chat := decodeChat(t, chatBody)
	tools, ok := chat["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("function 工具应完整保留，实际 %#v", chat["tools"])
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

// 官方 Codex 0.155 默认声明 web_search；网关不实现它，但必须接受整条请求并丢弃该工具。
func TestResponsesDeclaredBuiltinIsAcceptedAndDropped(t *testing.T) {
	body := []byte(`{"model":"cn:auto","input":"hi","tools":[{"type":"function","name":"f1","parameters":{"type":"object"}},{"type":"web_search"}]}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("declared builtin must not fail the request: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("web_search must not be forwarded upstream: %v", tools)
	}
	// 未知类型仍然明确拒绝，避免静默丢掉真正的工具。
	if _, _, err := responsesToChat([]byte(`{"model":"cn:auto","input":"hi","tools":[{"type":"future_builtin","name":"x"}]}`)); err == nil {
		t.Fatal("unknown tool type must stay rejected")
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
	obj := chatToResponses(chat, "cn:auto", nil)
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

// ─────────────────────── custom 型工具桥接（Codex exec / apply_patch）───────────────────────
//
// 这一组用例锁死的是 Codex 长任务中断的根因：custom 型工具被静默丢弃后，模型看到的
// 是个「没有 exec / apply_patch」的世界，于是只输出叙述句就结束回合。桥接必须双向互逆。

// 入站：custom 工具定义要折成 function，参数固定 {input:string}。
func TestResponsesCustomToolBridged(t *testing.T) {
	body := []byte(`{"model":"cn:auto","input":"hi","tools":[
		{"type":"function","name":"f1","description":"d1","parameters":{"type":"object"}},
		{"type":"custom","name":"apply_patch","description":"Apply a patch"},
		{"type":"custom","name":"exec","description":"Run a command"}
	]}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	chat := decodeChat(t, chatBody)
	tools, ok := chat["tools"].([]any)
	if !ok || len(tools) != 3 {
		t.Fatalf("function + 两个 custom 应共 3 条，实际 %#v", chat["tools"])
	}
	byName := map[string]map[string]any{}
	for _, tl := range tools {
		m := tl.(map[string]any)
		if m["type"] != "function" {
			t.Fatalf("所有出站工具都应是 function 形状: %#v", m)
		}
		fn := m["function"].(map[string]any)
		byName[fn["name"].(string)] = fn
	}
	for _, name := range []string{"apply_patch", "exec"} {
		fn, ok := byName[name]
		if !ok {
			t.Fatalf("custom 工具 %s 被丢弃了 —— 这正是 Codex 空转的根因", name)
		}
		params := fn["parameters"].(map[string]any)
		props := params["properties"].(map[string]any)
		if _, ok := props["input"]; !ok {
			t.Fatalf("%s 的参数应固定含 input 字段: %#v", name, params)
		}
		req := params["required"].([]any)
		if len(req) != 1 || req[0] != "input" {
			t.Fatalf("%s 的 required 应为 [input]: %#v", name, req)
		}
	}
	if _, ok := byName["f1"]; !ok {
		t.Fatal("原生 function 工具被误伤")
	}
}

// 入站历史：custom_tool_call / custom_tool_call_output 要折回 chat 的 tool 流量，
// 否则客户端把上一轮调用写回历史时会被整条丢掉，模型忘记自己刚做过什么。
func TestResponsesCustomToolCallHistoryFoldBack(t *testing.T) {
	body := []byte(`{"model":"cn:auto","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"改个文件"}]},
		{"type":"custom_tool_call","id":"ctc_1","status":"completed","call_id":"call_abc","name":"apply_patch","input":"*** Begin Patch\n*** End Patch\n"},
		{"type":"custom_tool_call_output","call_id":"call_abc","output":"Done!"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
	]}`)
	chatBody, _, err := responsesToChat(body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	chat := decodeChat(t, chatBody)
	msgs := chat["messages"].([]any)
	roles := rolesOf(t, chat)

	var assistant map[string]any
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			assistant = mm
		}
	}
	if assistant == nil {
		t.Fatalf("custom_tool_call 未折成 assistant 条目，roles=%v", roles)
	}
	tcs, ok := assistant["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("assistant 应带一条 tool_calls: %#v", assistant)
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "apply_patch" {
		t.Fatalf("工具名丢了: %#v", fn)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("参数不是合法 JSON: %v", fn["arguments"])
	}
	if args["input"] != "*** Begin Patch\n*** End Patch\n" {
		t.Fatalf("原始 input 未原样装回: %#v", args["input"])
	}
	if tcs[0].(map[string]any)["id"] != "call_abc" {
		t.Fatalf("call_id 未透传: %#v", tcs[0])
	}

	var toolMsg map[string]any
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			toolMsg = mm
		}
	}
	if toolMsg == nil || toolMsg["tool_call_id"] != "call_abc" || toolMsg["content"] != "Done!" {
		t.Fatalf("custom_tool_call_output 未折成 tool 消息: %#v", toolMsg)
	}
}

// 出站流式：custom 工具要产出 custom_tool_call（带 input），且不发 arguments.delta/done。
func TestResponsesWriterStreamCustomToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, &responsesRequest{
		Model: "cn:auto", Stream: true,
		customTools: map[string]bool{"apply_patch": true},
	})
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.WriteHeader(200)
	_, _ = rw.Write(sseStream(
		`{"id":"c3","model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin"}}]},"finish_reason":null}]}`,
		`{"id":"c3","model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" Patch\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c3","model":"glm-5.3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	))
	rw.finish()

	names, datas := eventsOf(t, rec.Body.String())
	for i, n := range names {
		if n == evArgsDelta || n == evArgsDone {
			t.Fatalf("custom 工具不该发 %s（Responses 侧无对应事件）: %#v", n, datas[i])
		}
	}
	final := datas[len(datas)-1]["response"].(map[string]any)
	out := final["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output 应只有一条，实际 %d", len(out))
	}
	call := out[0].(map[string]any)
	if call["type"] != "custom_tool_call" {
		t.Fatalf("应还原成 custom_tool_call，实际 %v", call["type"])
	}
	if call["name"] != "apply_patch" || call["call_id"] != "call_9" {
		t.Fatalf("name/call_id 不对: %#v", call)
	}
	if call["input"] != "*** Begin Patch" {
		t.Fatalf("input 未从 {input:...} 参数里取出: %#v", call["input"])
	}
	if _, hasArgs := call["arguments"]; hasArgs {
		t.Fatalf("custom_tool_call 不该带 arguments 字段: %#v", call)
	}
	if !strings.HasPrefix(call["id"].(string), "ctc_") {
		t.Fatalf("custom 条目 id 应用 ctc_ 前缀（真实 API 约定）: %#v", call["id"])
	}

	// 进行中的条目 input 应为空串（与参考仓库一致），完成时才填。
	for i, n := range names {
		if n == evItemAdded {
			item := datas[i]["item"].(map[string]any)
			if item["type"] != "custom_tool_call" || item["input"] != "" {
				t.Fatalf("in_progress 条目应为空 input 的 custom_tool_call: %#v", item)
			}
		}
	}
}

// 出站非流式：同样的还原逻辑走 chatToResponses。
func TestChatToResponsesCustomToolNonStream(t *testing.T) {
	chat := map[string]any{
		"id": "abc999", "object": "chat.completion", "created": float64(1700000000),
		"model": "glm-5.3",
		"choices": []any{map[string]any{
			"index": float64(0), "finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{
					map[string]any{
						"id": "call_p", "type": "function",
						"function": map[string]any{"name": "apply_patch", "arguments": `{"input":"*** Patch ***"}`},
					},
					map[string]any{
						"id": "call_f", "type": "function",
						"function": map[string]any{"name": "f1", "arguments": `{"a":1}`},
					},
				},
			},
		}},
	}
	obj := chatToResponses(chat, "cn:auto", &responsesRequest{customTools: map[string]bool{"apply_patch": true}})
	// 按类型取，不按序号：message 条目在非流式路径恒产出（既有行为），
	// 序号断言会被它带偏。
	var patch, plain map[string]any
	for _, it := range obj["output"].([]any) {
		item := it.(map[string]any)
		switch item["type"] {
		case "custom_tool_call":
			patch = item
		case "function_call":
			plain = item
		}
	}
	if patch == nil {
		t.Fatalf("custom 名未还原成 custom_tool_call: %#v", obj["output"])
	}
	if patch["name"] != "apply_patch" {
		t.Fatalf("custom 名丢了: %#v", patch)
	}
	if patch["input"] != "*** Patch ***" {
		t.Fatalf("input 提取错误: %#v", patch["input"])
	}
	if patch["call_id"] != "call_p" {
		t.Fatalf("call_id 未透传: %#v", patch["call_id"])
	}
	if !strings.HasPrefix(patch["id"].(string), "ctc_") {
		t.Fatalf("custom 条目 id 应用 ctc_ 前缀: %#v", patch["id"])
	}
	if _, hasArgs := patch["arguments"]; hasArgs {
		t.Fatalf("custom_tool_call 不该带 arguments 字段: %#v", patch)
	}
	if plain == nil {
		t.Fatal("非 custom 工具丢了")
	}
	if plain["arguments"] != `{"a":1}` {
		t.Fatalf("非 custom 工具应保持 function_call 原样: %#v", plain)
	}
	if !strings.HasPrefix(plain["id"].(string), "fc_") {
		t.Fatalf("普通条目 id 应用 fc_ 前缀: %#v", plain["id"])
	}
}

// 参数不是预期形状时原样返回，绝不丢内容。
func TestCustomInputFromArgsNeverDrops(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"input":"hello"}`, "hello"},
		{`{"input":""}`, ""},
		{`not json at all`, "not json at all"},
		{`{"other":1}`, `{"other":1}`},
		{`  {"input":"trimmed"}  `, "trimmed"},
		{``, ""},
		{`{"input":123}`, "123"},
	}
	for _, c := range cases {
		if got := customInputFromArgs(c.in); got != c.want {
			t.Fatalf("customInputFromArgs(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestResponsesWriterPartialErrorIsFailed(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newResponsesWriter(rec, &responsesRequest{Model: "cn:deepseek-v4.1-flash", Stream: true})
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.WriteHeader(200)
	_, _ = rw.Write(sseStream(
		`{"choices":[{"index":0,"delta":{"content":"partial 11128"}}]}`,
		`{"error":{"code":"upstream_error","message":"upstream failed after partial output"}}`,
	))
	rw.finish()
	names, datas := eventsOf(t, rec.Body.String())
	if len(names) == 0 || names[len(names)-1] != evFailed {
		t.Fatalf("partial error must end with response.failed: %v", names)
	}
	for _, name := range names {
		if name == evCompleted {
			t.Fatalf("failed response also emitted completed: %v", names)
		}
	}
	final := datas[len(datas)-1]["response"].(map[string]any)
	if final["status"] != "failed" {
		t.Fatalf("partial output hid failure: %v", final)
	}
	errObj, _ := final["error"].(map[string]any)
	if errObj["code"] != "upstream_error" || errObj["message"] != "upstream failed after partial output" {
		t.Fatalf("error details lost: %v", errObj)
	}
	output, _ := final["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "partial 11128" {
		t.Fatalf("failure discarded existing output: %v", output)
	}
}

func TestResponsesWriterIncompleteDoesNotCompleteTools(t *testing.T) {
	for _, tc := range []struct{ reason, detail string }{{"length", "max_output_tokens"}, {"content_filter", "content_filter"}} {
		t.Run(tc.reason, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rw := newResponsesWriter(rec, &responsesRequest{Model: "cn:deepseek-v4.1-flash", Stream: true})
			rw.Header().Set("Content-Type", "text/event-stream")
			rw.WriteHeader(200)
			_, _ = rw.Write(sseStream(
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_incomplete","type":"function","function":{"name":"lookup","arguments":"{\"id\":"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"`+tc.reason+`"}]}`,
			))
			rw.finish()
			names, datas := eventsOf(t, rec.Body.String())
			if len(names) == 0 || names[len(names)-1] != evIncomplete {
				t.Fatalf("incomplete terminal lost: %v", names)
			}
			sawArgs := false
			for i, name := range names {
				if name == evCompleted || name == evArgsDone {
					t.Fatalf("partial tool was marked complete: %v", names)
				}
				if name == evArgsDelta {
					sawArgs = true
				}
				if name == evItemDone && datas[i]["item"].(map[string]any)["status"] == "completed" {
					t.Fatalf("partial tool item completed: %v", datas[i])
				}
			}
			if !sawArgs {
				t.Fatal("partial tool arguments disappeared")
			}
			final := datas[len(datas)-1]["response"].(map[string]any)
			if final["status"] != "incomplete" || final["incomplete_details"].(map[string]any)["reason"] != tc.detail {
				t.Fatalf("incomplete details wrong: %v", final)
			}
			out := final["output"].([]any)
			if len(out) != 1 {
				t.Fatalf("tool item lost/duplicated: %v", out)
			}
			call := out[0].(map[string]any)
			if call["status"] != "incomplete" || call["arguments"] != `{"id":` {
				t.Fatalf("partial arguments/status changed: %v", call)
			}
		})
	}
}
