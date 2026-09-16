// ═══ 更新日志 ═══
// 2026-09-15: 新增。NarraFork / Codex 等客户端走 OpenAI Responses API（POST /v1/responses），
//   网关此前只实现 /v1/chat/completions，客户端拿到 Go 默认的 "404 page not found"。
//   本文件把 Responses 请求翻译成 chat completions 后复用 chatCompletions（轮转、租约、
//   粘性、错误策略全部沿用，零重复），再把输出翻译回 Responses 形状（对象 / SSE 事件）。
// 2026-09-16: 补 custom 型工具桥接（Codex 的 exec / apply_patch 走的正是 custom）。
//   此前 responsesTools 只留 type=="function"，custom 被静默丢弃 → 模型拿到的是「没有
//   这些工具」的世界，于是把补丁当正文吐出来、只叙述不调用（实测 10:16 会话 7 次空转收尾）。
//   对齐参考仓库 responses.js:102-129 的等价语义：入站 custom → function{input}，
//   出站按 customNames 还原 custom_tool_call（input 字段）+ 历史项互逆折回，
//   流式侧 custom 不发 arguments.delta / arguments.done（Responses 无对应事件）。

// 2026-09-16：保留真实 Codex 参数并校验结构化输出，错误与截断使用真实终态。
// 2026-09-16：缓存迟到工具元数据与参数，保留 refusal/legacy 调用，并在终态确定后收口输出。
// 2026-09-17：合并 fork 的 Responses/图片工具兼容，保留严格终态、schema控制与数字保真扩展。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"workbuddy2api/internal/jsonutil"
)

// responsesRequest 是 Responses API 请求体里网关需要理解的字段子集。
// 输出格式与推理、工具控制明确映射；不支持的服务端存储与续接返回错误。
// 客户端每轮自带完整 input，不依赖服务端会话。
type responsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	Stream             bool            `json:"stream"`
	MaxOutputTokens    *int            `json:"max_output_tokens"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	Tools              []any           `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	Metadata           json.RawMessage `json:"metadata"`
	Reasoning          map[string]any  `json:"reasoning"`
	Text               json.RawMessage `json:"text"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	PromptCacheKey     string          `json:"prompt_cache_key"`
	ConversationID     string          `json:"conversation_id"`
	ConversationCamel  string          `json:"conversationId"`
	PreviousResponseID string          `json:"previous_response_id"`
	Store              *bool           `json:"store"`
	output             *outputContract

	// customTools 记录被桥接成 function 的 custom 工具名（非 JSON 字段，翻译时填充）。
	// 出站还原 custom_tool_call 时按它判定，客户端才认得出这是自定义工具调用。
	customTools map[string]bool
}

// responsesToChat 把 Responses 请求体翻译成 chat completions 请求体。
func responsesToChat(body []byte) ([]byte, *responsesRequest, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, nil, fmt.Errorf("request body must be a JSON object")
	}
	var req responsesRequest
	if err := jsonutil.Decode(body, &req); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := validateResponsesOptions(object, &req); err != nil {
		return nil, nil, err
	}
	if req.PreviousResponseID != "" {
		return nil, nil, fmt.Errorf("previous_response_id is not supported; include the complete input history")
	}
	if req.Store != nil && *req.Store {
		return nil, nil, fmt.Errorf("store=true is not supported; responses are not stored on this gateway")
	}
	var outputErr error
	req.output, outputErr = parseOutputContract(req.Text)
	if outputErr != nil {
		return nil, nil, outputErr
	}
	msgs, err := responsesMessages(req.Input, req.Instructions)
	if err != nil {
		return nil, nil, err
	}
	if instruction := req.output.instruction(); instruction != "" {
		msgs = append([]any{map[string]any{"role": "system", "content": instruction}}, msgs...)
	}
	chat := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.output != nil && req.output.chatFormat != nil {
		chat["response_format"] = req.output.chatFormat
	}
	if req.Reasoning != nil {
		for source, target := range map[string]string{"effort": "reasoning_effort", "summary": "reasoning_summary"} {
			if value, present := req.Reasoning[source]; present {
				text, ok := value.(string)
				if !ok || strings.TrimSpace(text) == "" {
					return nil, nil, fmt.Errorf("reasoning.%s must be a nonempty string", source)
				}
				chat[target] = text
			}
		}
	}
	if req.ParallelToolCalls != nil {
		chat["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.PromptCacheKey != "" {
		chat["prompt_cache_key"] = req.PromptCacheKey
	}
	if req.ConversationID != "" {
		chat["conversation_id"] = req.ConversationID
	}
	if req.ConversationCamel != "" {
		chat["conversationId"] = req.ConversationCamel
	}
	if req.MaxOutputTokens != nil {
		chat["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	customNames := map[string]bool{}
	if tools := responsesTools(req.Tools, customNames); len(tools) > 0 {
		chat["tools"] = tools
		req.customTools = customNames
		if tc := responsesToolChoice(req.ToolChoice); tc != nil {
			chat["tool_choice"] = tc
		}
	}
	// metadata 透传：它不参与推理，但会话粘性（session.ExtractKey）会读它，
	// 丢掉会让多轮对话在网关侧退化成逐请求随机抽号。
	if len(req.Metadata) > 0 && string(req.Metadata) != "null" {
		chat["metadata"] = json.RawMessage(req.Metadata)
	}
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, nil, err
	}
	return out, &req, nil
}

// responsesMessages 把 Responses 的 input（字符串或 item 数组）+ instructions 折成 chat messages。
func responsesMessages(input json.RawMessage, instructions string) ([]any, error) {
	msgs := []any{}
	if strings.TrimSpace(instructions) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instructions})
	}
	raw := strings.TrimSpace(string(input))
	if raw == "" || raw == "null" {
		return msgs, nil
	}
	// input 为纯字符串：等价于一条 user 消息。
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal(input, &s); err != nil {
			return nil, fmt.Errorf("invalid input string: %w", err)
		}
		return append(msgs, map[string]any{"role": "user", "content": s}), nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or an array of items: %w", err)
	}
	// 同一轮里连续的 function_call 必须并进同一条 assistant 消息的 tool_calls，
	// 拆成多条 assistant 会被上游拒。
	var pending []any
	flush := func() {
		if len(pending) > 0 {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": pending})
			pending = nil
		}
	}
	for _, it := range items {
		var m map[string]any
		if jsonutil.Decode(it, &m) != nil {
			continue
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "function_call":
			name, _ := m["name"].(string)
			args, _ := m["arguments"].(string)
			callID, _ := m["call_id"].(string)
			if callID == "" {
				callID, _ = m["id"].(string)
			}
			pending = append(pending, map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
			continue
		case "function_call_output":
			flush()
			callID, _ := m["call_id"].(string)
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": callID, "content": responsesToolOutput(m["output"]),
			})
			continue
		case "custom_tool_call":
			// 桥接的反向：custom 调用在历史里带 input 字段，还原成 chat 的
			// function 调用 + {"input": "..."} 参数，与出站桥接严格互逆。
			// 不处理的话，客户端把上一轮的 custom 调用写回历史时会被整条丢掉，
			// 模型看不到自己刚做过什么，于是重复劳动或空转。
			name, _ := m["name"].(string)
			input, _ := m["input"].(string)
			callID, _ := m["call_id"].(string)
			if callID == "" {
				callID, _ = m["id"].(string)
			}
			args, err := json.Marshal(map[string]any{"input": input})
			if err != nil {
				args = []byte("{}")
			}
			pending = append(pending, map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": string(args)},
			})
			continue
		case "custom_tool_call_output":
			flush()
			callID, _ := m["call_id"].(string)
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": callID, "content": responsesToolOutput(m["output"]),
			})
			continue
		case "reasoning":
			// 历史推理内容不回灌上游：上游不接受 reasoning item，且不影响后续回答正确性。
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		content, ok := responsesContent(m["content"])
		if !ok {
			continue
		}
		flush()
		msgs = append(msgs, map[string]any{"role": role, "content": content})
	}
	flush()
	return msgs, nil
}

// responsesContent 把 Responses 的 content（字符串或 part 数组）折成 chat 的 content。
// 全为文本时退化成纯字符串（上游对纯文本消息最稳）；含图片时保留 part 数组。
func responsesContent(v any) (any, bool) {
	switch c := v.(type) {
	case nil:
		return nil, false
	case string:
		return c, true
	case []any:
		var texts []string
		var parts []any
		hasImage := false
		for _, e := range c {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := em["type"].(string); t {
			case "input_text", "output_text", "text", "summary_text", "":
				if s, ok := em["text"].(string); ok && s != "" {
					texts = append(texts, s)
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
			case "input_image", "image_url":
				if p := chatImagePart(em); p != nil {
					hasImage = true
					parts = append(parts, p)
				}
			case "refusal":
				if s, ok := em["refusal"].(string); ok && s != "" {
					texts = append(texts, s)
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
			}
		}
		if len(parts) == 0 {
			return "", true
		}
		if !hasImage {
			return strings.Join(texts, "\n"), true
		}
		return parts, true
	default:
		return nil, false
	}
}

// chatImagePart 把 Responses 的图片 part 转成 chat 的 image_url part；非图片返回 nil。
//
// detail 必须原样带上：Codex 的 view_image 发的是 detail=high，丢掉后上游按默认
// 分辨率处理，小字截图的识别质量会掉。url 兼容 data URI 与 http(s) 两种形态。
func chatImagePart(em map[string]any) map[string]any {
	url := em["image_url"]
	detail, _ := em["detail"].(string)
	if um, ok := url.(map[string]any); ok {
		if detail == "" {
			detail, _ = um["detail"].(string)
		}
		url = um["url"]
	}
	s, ok := url.(string)
	if !ok || s == "" {
		return nil
	}
	img := map[string]any{"url": s}
	if detail != "" {
		img["detail"] = detail
	}
	return map[string]any{"type": "image_url", "image_url": img}
}

// responsesToolOutput 把 function_call_output.output 折成 chat 的 tool content。
//
// 图片必须留成 part 数组。Codex 的 view_image 结果长这样：
//
//	[{"type":"input_image","image_url":"data:image/png;base64,...","detail":"high"}]
//
// 旧实现整段 json.Marshal 成字符串再塞进 tool content，上游于是按纯文本计费——
// 一张 1600x1000 截图（base64 约 500KB）吃掉约 10 万 token，十几张就把 1M 上下文
// 撑爆（实测 16 张截图 → 上游 11115 prompt is too long）。保留 part 数组后同一张
// 图只算约 1 千 token，与 OpenCode / 官方端点行为一致。
//
// 纯文本输出仍退化成字符串：上游对纯文本 tool 结果最稳，也是历史零回归路径。
func responsesToolOutput(v any) any {
	switch o := v.(type) {
	case nil:
		return ""
	case string:
		return o
	case []any:
		var texts []string
		var parts []any
		hasImage := false
		for _, e := range o {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := em["type"].(string); t {
			case "input_text", "output_text", "text", "summary_text", "":
				if s, ok := em["text"].(string); ok && s != "" {
					texts = append(texts, s)
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
			case "input_image", "image_url":
				if p := chatImagePart(em); p != nil {
					hasImage = true
					parts = append(parts, p)
				}
			}
		}
		if hasImage {
			return parts
		}
		if len(texts) == 0 {
			return ""
		}
		return strings.Join(texts, "\n")
	default:
		raw, err := json.Marshal(o)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// responsesTools 把 Responses 的扁平工具定义转成 chat 的嵌套定义。
//
// custom 型工具（Codex 的 exec / apply_patch）必须桥接成 function 而不是丢弃：
// 上游认不出 custom，丢掉等于把工具从模型视野里删掉——模型于是只能把补丁当正文吐出来，
// 表现为「说了要调用却不调用」的空转回合。桥接语义对齐参考仓库 responses.js:102-129：
// custom -> function，参数固定为 {input: string}，原始输入装在这个字段里往返。
// customNames 回填被桥接的工具名，供出站还原 custom_tool_call 时判定。
//
// 其余非 function 类型（web_search / file_search / mcp）网关侧确无对应实现，仍丢弃。
func responsesTools(tools []any, customNames map[string]bool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := tm["type"].(string)
		if typ == "custom" {
			if fn := customToolToFunction(tm); fn != nil {
				out = append(out, fn)
				if customNames != nil {
					if name, _ := fn["function"].(map[string]any)["name"].(string); name != "" {
						customNames[name] = true
					}
				}
			}
			continue
		}
		if typ != "function" {
			continue // web_search / file_search / mcp 等网关侧无对应实现，丢弃
		}
		if fn, ok := tm["function"].(map[string]any); ok && fn != nil {
			out = append(out, tm) // 已是 chat 形状，原样保留
			continue
		}
		fn := map[string]any{}
		for _, k := range []string{"name", "description", "parameters", "strict"} {
			if v, ok := tm[k]; ok && v != nil {
				fn[k] = v
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// customToolToFunction 把一条 custom 工具定义桥接成 chat 的 function 形状。
// 参数固定为单字段 input（原始自定义输入），与参考仓库 flattenResponseTool 一致。
func customToolToFunction(tm map[string]any) map[string]any {
	name, _ := tm["name"].(string)
	if name == "" {
		return nil
	}
	desc, _ := tm["description"].(string)
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "Raw custom tool input.",
					},
				},
				"required": []any{"input"},
			},
		},
	}
}

// customInputFromArgs 从桥接后的 {"input": "..."} 参数里取出原始自定义输入。
// 解析不出对象时原样返回参数字符串，绝不丢内容。
func customInputFromArgs(args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return ""
	}
	var m map[string]any
	if jsonutil.Decode([]byte(trimmed), &m) == nil {
		if v, ok := m["input"]; ok && v != nil {
			if s, ok := v.(string); ok {
				return s
			}
			if b, err := json.Marshal(v); err == nil {
				return string(b)
			}
		}
	}
	return args
}

// responsesToolChoice 把 Responses 的 tool_choice 转成 chat 形状。
func responsesToolChoice(raw json.RawMessage) any {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			return v
		}
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if name, ok := m["name"].(string); ok && name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	if t, _ := m["type"].(string); t == "allowed_tools" || t == "function" {
		return "auto"
	}
	return nil
}

// responses 处理 POST /v1/responses。
//
// 实现方式：翻译请求体后把工作整体交给 chatCompletions，用 responsesWriter 拦截它的写出来
// 做反向翻译。之所以不重新实现一遍轮转循环：那条路径里有账号租约、粘性绑定、失败轮转、
// 错误分类与冷却策略（约 340 行），复制一份必然与主路径漂移。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}

	chatBody, req, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 让 chatCompletions 从翻译后的 body 读；header/context/方法保持不变。
	sub := r.Clone(r.Context())
	sub.Body = io.NopCloser(bytes.NewReader(chatBody))
	sub.ContentLength = int64(len(chatBody))

	rw := newResponsesWriter(w, req)
	h.chatCompletions(rw, sub)
	rw.finish()
}

// ─────────────────────────── 写出口翻译 ───────────────────────────

const (
	evCreated      = "response.created"
	evInProgress   = "response.in_progress"
	evItemAdded    = "response.output_item.added"
	evItemDone     = "response.output_item.done"
	evPartAdded    = "response.content_part.added"
	evPartDone     = "response.content_part.done"
	evTextDelta    = "response.output_text.delta"
	evTextDone     = "response.output_text.done"
	evRefusalDelta = "response.refusal.delta"
	evRefusalDone  = "response.refusal.done"
	evRsPartAdded  = "response.reasoning_summary_part.added"
	evRsPartDone   = "response.reasoning_summary_part.done"
	evRsDelta      = "response.reasoning_summary_text.delta"
	evRsDone       = "response.reasoning_summary_text.done"
	evArgsDelta    = "response.function_call_arguments.delta"
	evArgsDone     = "response.function_call_arguments.done"
	evCompleted    = "response.completed"
	evIncomplete   = "response.incomplete"
	evFailed       = "response.failed"
)

// respToolCall 聚合一条流式 function_call。
type respToolCall struct {
	outIdx        int
	id            string
	callID        string
	name          string
	args          strings.Builder
	sentArgs      int
	argumentsSeen bool
	opened        bool
	custom        bool // 由 custom 工具桥接而来：出站还原成 custom_tool_call
}

// responsesWriter 拦截 chatCompletions 的写出并翻译成 Responses 形状。
// 模式：1=SSE 事件流，2=JSON 对象，3=错误原样透传（未翻译，保持原始状态码与 body）。
type responsesWriter struct {
	inner http.ResponseWriter
	req   *responsesRequest

	hdr    http.Header
	status int
	mode   int
	buf    []byte
	begun  bool
	closed bool

	respID    string
	msgID     string
	rsID      string
	modelName string
	created   int64
	seq       int
	nextIdx   int
	rsOutIdx  int
	msgOutIdx int

	text         strings.Builder
	refusal      strings.Builder
	messageParts []string
	legacyCallID string
	reason       strings.Builder
	calls        map[int]*respToolCall
	order        []int
	usage        map[string]any
	finishReason string

	msgOpen        bool
	rsOpen         bool
	rsPart         bool
	streamErr      map[string]any
	writeErr       error
	terminalStatus string
	sawDone        bool
}

func newResponsesWriter(w http.ResponseWriter, req *responsesRequest) *responsesWriter {
	mode := 2
	if req != nil && req.Stream {
		mode = 1
	}
	return &responsesWriter{
		inner: w, req: req, mode: mode,
		hdr: http.Header{}, calls: map[int]*respToolCall{}, status: http.StatusOK,
	}
}

func (rw *responsesWriter) Header() http.Header { return rw.hdr }

func (rw *responsesWriter) WriteHeader(code int) {
	rw.status = code
	if code >= 400 {
		rw.mode = 3
	}
}

func (rw *responsesWriter) Write(p []byte) (int, error) {
	if rw.writeErr != nil {
		return 0, rw.writeErr
	}
	switch rw.mode {
	case 3:
		rw.buf = append(rw.buf, p...)
	case 1:
		if !strings.HasPrefix(rw.hdr.Get("Content-Type"), "text/event-stream") {
			// 流式请求但上游走了错误 JSON：改为原样透传，由 finish 写回真实状态码与 body。
			rw.mode = 3
			rw.buf = append(rw.buf, p...)
			return len(p), nil
		}
		if !rw.begun {
			rw.beginStream()
		}
		rw.feed(p)
	default:
		if strings.HasPrefix(rw.hdr.Get("Content-Type"), "text/event-stream") {
			rw.mode = 1
			if !rw.begun {
				rw.beginStream()
			}
			rw.feed(p)
			return len(p), nil
		}
		rw.buf = append(rw.buf, p...)
	}
	if rw.writeErr != nil {
		return 0, rw.writeErr
	}
	return len(p), nil
}

func (rw *responsesWriter) Flush() {
	if rw.mode == 1 {
		if fl, ok := rw.inner.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// finish 在 chatCompletions 返回后收尾。
func (rw *responsesWriter) finish() {
	if rw.closed {
		return
	}
	rw.closed = true
	switch rw.mode {
	case 3:
		ct := rw.hdr.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		rw.inner.Header().Set("Content-Type", ct)
		rw.inner.WriteHeader(rw.status)
		_, _ = rw.inner.Write(rw.buf)
	case 2:
		rw.finishJSON()
	default:
		if !rw.begun {
			rw.beginStream()
		}
		rw.finishStream()
	}
}

// finishJSON 把缓冲的 chat completion 翻成 Responses 对象。
func (rw *responsesWriter) finishJSON() {
	var chat map[string]any
	if json.Unmarshal(rw.buf, &chat) != nil {
		// 解析不了就原样透传，别把本来能用的响应弄坏。
		writeOpenAIError(rw.inner, http.StatusBadGateway, "upstream_parse", "upstream response is not valid JSON")
		return
	}
	var customNames map[string]bool
	if rw.req != nil {
		customNames = rw.req.customTools
	}
	result := chatToResponses(chat, rw.resolvedModel(), customNames)
	if rw.req != nil {
		rw.req.applyEcho(result)
		if err := rw.validateJSONCompletion(chat, result); err != nil {
			writeOpenAIError(rw.inner, http.StatusBadGateway, "response_contract_violation", err.Error())
			return
		}
	}
	raw, _ := json.Marshal(result)
	rw.inner.Header().Set("Content-Type", "application/json")
	rw.inner.WriteHeader(http.StatusOK)
	_, _ = rw.inner.Write(raw)
}

func (rw *responsesWriter) resolvedModel() string {
	if rw.modelName != "" {
		return rw.modelName
	}
	if rw.req != nil && rw.req.Model != "" {
		return rw.req.Model
	}
	return "unknown"
}

// ─────────────────────────── 流式事件 ───────────────────────────

func (rw *responsesWriter) beginStream() {
	rw.begun = true
	h := rw.inner.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	rw.inner.WriteHeader(http.StatusOK)
	rw.created = time.Now().Unix()
	rw.respID = newRespID("resp_")
	rw.msgID = newRespID("msg_")
	rw.rsID = newRespID("rs_")
	rw.emit(evCreated, map[string]any{"response": rw.responseObject("in_progress")})
	rw.emit(evInProgress, map[string]any{"response": rw.responseObject("in_progress")})
}

// produced 判断是否已经产出过实质内容（用于区分「真失败」与「只是没内容」）。
func (rw *responsesWriter) produced() bool {
	return rw.text.Len() > 0 || rw.refusal.Len() > 0 || rw.reason.Len() > 0 || len(rw.order) > 0
}

func (rw *responsesWriter) emit(evType string, payload map[string]any) {
	payload["type"] = evType
	payload["sequence_number"] = rw.seq
	rw.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if rw.writeErr != nil {
		return
	}
	_, rw.writeErr = fmt.Fprintf(rw.inner, "event: %s\ndata: %s\n\n", evType, raw)
	if fl, ok := rw.inner.(http.Flusher); ok {
		fl.Flush()
	}
}

// feed 累积字节并按空行切帧。
func (rw *responsesWriter) feed(p []byte) {
	rw.buf = append(rw.buf, p...)
	for {
		i := bytes.Index(rw.buf, []byte("\n\n"))
		if i < 0 {
			return
		}
		frame := string(rw.buf[:i])
		rw.buf = rw.buf[i+2:]
		rw.handleFrame(frame)
	}
}

func (rw *responsesWriter) handleFrame(frame string) {
	if rw.sawDone {
		return
	}
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			rw.sawDone = true
			return // 收尾统一在 finishStream 做，避免与 finish 重复
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			rw.streamErr = map[string]any{"code": "upstream_parse", "message": "invalid upstream event"}
			continue
		}
		if e, ok := chunk["error"].(map[string]any); ok {
			rw.streamErr = e
			continue
		}
		rw.handleChunk(chunk)
	}
}

func (rw *responsesWriter) handleChunk(chunk map[string]any) {
	if rw.streamErr != nil {
		return
	}
	if v, ok := chunk["id"].(string); ok && v != "" && rw.respID == "" {
		rw.respID = "resp_" + v
	}
	if v, ok := chunk["model"].(string); ok && v != "" && rw.modelName == "" {
		rw.modelName = v
	}
	if u, ok := chunk["usage"].(map[string]any); ok {
		rw.usage = u
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return
	}
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		if idx, ok := c["index"].(float64); ok && idx != 0 {
			continue // Responses 的单次输出只对应一条 choice
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			rw.finishReason = fr
		}
		delta, ok := c["delta"].(map[string]any)
		if !ok {
			continue
		}
		if s, ok := delta["reasoning_content"].(string); ok && s != "" {
			rw.reasoningDelta(s)
		}
		if s, ok := delta["content"].(string); ok && s != "" {
			rw.textDelta(s)
		}
		if s, ok := delta["refusal"].(string); ok && s != "" {
			rw.refusalDelta(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			if rw.legacyCallID != "" {
				rw.failOutput("upstream_parse", "upstream mixed legacy and modern tool calls")
				return
			}
			rw.toolCallDelta(tcs)
		}
		if fn, ok := delta["function_call"].(map[string]any); ok {
			rw.legacyFunctionDelta(fn)
		}
	}
}

func (rw *responsesWriter) closeReasoning() {
	if !rw.rsOpen {
		return
	}
	if rw.rsPart {
		txt := rw.reason.String()
		rw.emit(evRsDone, map[string]any{
			"item_id": rw.rsID, "output_index": rw.rsOutIdx, "summary_index": 0, "text": txt,
		})
		rw.emit(evRsPartDone, map[string]any{
			"item_id": rw.rsID, "output_index": rw.rsOutIdx, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": txt},
		})
		rw.rsPart = false
	}
	rw.emit(evItemDone, map[string]any{
		"output_index": rw.rsOutIdx, "item": rw.reasoningItem(rw.itemStatus()),
	})
	rw.rsOpen = false
}

func (rw *responsesWriter) reasoningDelta(s string) {
	if !rw.rsOpen {
		rw.rsOutIdx = rw.nextIdx
		rw.nextIdx++
		rw.emit(evItemAdded, map[string]any{
			"output_index": rw.rsOutIdx, "item": rw.reasoningItem("in_progress"),
		})
		rw.rsOpen = true
	}
	if !rw.rsPart {
		rw.emit(evRsPartAdded, map[string]any{
			"item_id": rw.rsID, "output_index": rw.rsOutIdx, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
		rw.rsPart = true
	}
	rw.reason.WriteString(s)
	rw.emit(evRsDelta, map[string]any{
		"item_id": rw.rsID, "output_index": rw.rsOutIdx, "summary_index": 0, "delta": s,
	})
}

// openMessage 打开正文条目；正文与拒绝共享消息，在响应终态确定后统一收口。
func (rw *responsesWriter) openMessage() {
	if rw.msgOpen {
		return
	}
	rw.closeReasoning()
	rw.msgOutIdx = rw.nextIdx
	rw.nextIdx++
	rw.emit(evItemAdded, map[string]any{
		"output_index": rw.msgOutIdx, "item": rw.messageItem("in_progress"),
	})
	rw.msgOpen = true
}

func (rw *responsesWriter) ensureMessagePart(kind string) int {
	rw.openMessage()
	for index, existing := range rw.messageParts {
		if existing == kind {
			return index
		}
	}
	index := len(rw.messageParts)
	rw.messageParts = append(rw.messageParts, kind)
	part := map[string]any{"type": "refusal", "refusal": ""}
	if kind == "output_text" {
		part = map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	}
	rw.emit(evPartAdded, map[string]any{
		"item_id": rw.msgID, "output_index": rw.msgOutIdx, "content_index": index, "part": part,
	})
	return index
}

func (rw *responsesWriter) messageContent() []any {
	content := []any{}
	for _, kind := range rw.messageParts {
		if kind == "refusal" {
			content = append(content, map[string]any{"type": "refusal", "refusal": rw.refusal.String()})
		} else {
			content = append(content, map[string]any{"type": "output_text", "text": rw.text.String(), "annotations": []any{}})
		}
	}
	return content
}

func (rw *responsesWriter) closeMessage() {
	if !rw.msgOpen {
		return
	}
	for index, part := range rw.messageContent() {
		p := part.(map[string]any)
		if p["type"] == "refusal" {
			rw.emit(evRefusalDone, map[string]any{"item_id": rw.msgID, "output_index": rw.msgOutIdx, "content_index": index, "refusal": p["refusal"]})
		} else {
			rw.emit(evTextDone, map[string]any{"item_id": rw.msgID, "output_index": rw.msgOutIdx, "content_index": index, "text": p["text"]})
		}
		rw.emit(evPartDone, map[string]any{"item_id": rw.msgID, "output_index": rw.msgOutIdx, "content_index": index, "part": part})
	}
	rw.emit(evItemDone, map[string]any{"output_index": rw.msgOutIdx, "item": rw.messageItem(rw.itemStatus())})
	rw.msgOpen = false
}

func (rw *responsesWriter) textDelta(s string) {
	index := rw.ensureMessagePart("output_text")
	rw.text.WriteString(s)
	rw.emit(evTextDelta, map[string]any{"item_id": rw.msgID, "output_index": rw.msgOutIdx, "content_index": index, "delta": s})
}

func (rw *responsesWriter) refusalDelta(s string) {
	index := rw.ensureMessagePart("refusal")
	rw.refusal.WriteString(s)
	rw.emit(evRefusalDelta, map[string]any{"item_id": rw.msgID, "output_index": rw.msgOutIdx, "content_index": index, "delta": s})
}

// openCall 等名称与 call_id 确定后开出条目，避免 custom 工具在 added 后才改变类型。
func (rw *responsesWriter) openCall(call *respToolCall) {
	if call.opened || call.name == "" || call.callID == "" {
		return
	}
	call.opened = true
	prefix := "fc_"
	if call.custom {
		prefix = "ctc_"
	}
	call.id = newRespID(prefix)
	rw.emit(evItemAdded, map[string]any{"output_index": call.outIdx, "item": rw.callItem(call, "in_progress")})
}

func (rw *responsesWriter) failOutput(code, message string) {
	if rw.streamErr == nil {
		rw.streamErr = map[string]any{"code": code, "message": message}
	}
}

// flushReadyCalls 按首次出现顺序开出工具；早到的参数仅发送一次，custom 参数留到 input 收尾。
func (rw *responsesWriter) flushReadyCalls() {
	if rw.streamErr != nil {
		return
	}
	for _, index := range rw.order {
		call := rw.calls[index]
		if !call.opened {
			if call.name == "" || call.callID == "" {
				return
			}
			rw.openCall(call)
		}
		if call.custom {
			continue
		}
		args := call.args.String()
		if call.sentArgs < len(args) {
			rw.emit(evArgsDelta, map[string]any{"item_id": call.id, "output_index": call.outIdx, "delta": args[call.sentArgs:]})
			call.sentArgs = len(args)
		}
	}
}

func (rw *responsesWriter) toolCallDelta(tcs []any) {
	if rw.streamErr != nil {
		return
	}
	for _, item := range tcs {
		tm, ok := item.(map[string]any)
		if !ok {
			rw.failOutput("upstream_parse", "upstream contained an invalid tool call")
			return
		}
		index := 0
		if value, ok := tm["index"].(float64); ok {
			index = int(value)
		}
		call := rw.calls[index]
		if call == nil {
			rw.closeReasoning()
			call = &respToolCall{outIdx: rw.nextIdx}
			rw.nextIdx++
			rw.calls[index] = call
			rw.order = append(rw.order, index)
		}
		if value, ok := tm["id"].(string); ok && value != "" {
			if call.callID != "" && call.callID != value {
				rw.failOutput("upstream_parse", "upstream changed a streamed tool call identity")
				return
			}
			call.callID = value
		}
		if fn, ok := tm["function"].(map[string]any); ok {
			if value, ok := fn["name"].(string); ok && value != "" {
				if call.name != "" && call.name != value {
					rw.failOutput("upstream_parse", "upstream changed a streamed tool name")
					return
				}
				call.name = value
				call.custom = rw.req != nil && rw.req.customTools[value]
			}
			if value, present := fn["arguments"]; present {
				args, ok := value.(string)
				if !ok {
					rw.failOutput("invalid_tool_arguments", "upstream tool arguments must be a JSON string")
					return
				}
				call.argumentsSeen = true
				call.args.WriteString(args)
			}
		}
	}
	rw.flushReadyCalls()
}

func (rw *responsesWriter) legacyFunctionDelta(fn map[string]any) {
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	if rw.legacyCallID == "" && name == "" && args == "" {
		if value, present := fn["arguments"]; !present || value == "" {
			return
		}
	}
	if rw.legacyCallID == "" {
		if len(rw.order) > 0 {
			rw.failOutput("upstream_parse", "upstream mixed legacy and modern tool calls")
			return
		}
		rw.legacyCallID = newRespID("call_")
	}
	rw.toolCallDelta([]any{map[string]any{"index": float64(-1), "id": rw.legacyCallID, "type": "function", "function": fn}})
}

func (rw *responsesWriter) closeCalls() {
	rw.closeReasoning()
	rw.closeMessage()
	for _, idx := range rw.order {
		call := rw.calls[idx]
		if !call.opened {
			continue
		}
		// Codex 可把 output_item.done 当作工具执行信号；失败/截断只在最终响应保留 incomplete 项。
		if rw.itemStatus() != "completed" {
			call.opened = false
			continue
		}
		if !call.custom {
			rw.emit(evArgsDone, map[string]any{
				"item_id": call.id, "output_index": call.outIdx, "arguments": call.args.String(),
			})
		}
		rw.emit(evItemDone, map[string]any{
			"output_index": call.outIdx, "item": rw.callItem(call, rw.itemStatus()),
		})
		call.opened = false
	}
}

func (rw *responsesWriter) finishStream() {
	_ = rw.CompletionError()
	status := "completed"
	if rw.finishReason == "length" || rw.finishReason == "content_filter" {
		status = "incomplete"
	}
	if rw.streamErr != nil {
		status = "failed"
	}
	rw.terminalStatus = status
	rw.closeCalls()
	event := evCompleted
	if status == "failed" {
		event = evFailed
	} else if status == "incomplete" {
		event = evIncomplete
	}
	rw.emit(event, map[string]any{"response": rw.responseObject(status)})
}

func (rw *responsesWriter) itemStatus() string {
	if rw.terminalStatus == "failed" || rw.terminalStatus == "incomplete" {
		return "incomplete"
	}
	return "completed"
}

// CompletionError 可在 handler 计成功前调用；工具与格式校验先于任何 completed 工具事件。
func (rw *responsesWriter) CompletionError() error {
	if rw.writeErr != nil {
		return rw.writeErr
	}
	if rw.streamErr == nil && !rw.sawDone && rw.finishReason == "" {
		rw.failOutput("upstream_truncated", "upstream stream ended without a completion marker")
	}
	if rw.streamErr == nil && rw.finishReason != "length" && rw.finishReason != "content_filter" {
		if rw.req != nil && rw.req.ParallelToolCalls != nil && !*rw.req.ParallelToolCalls && len(rw.order) > 1 {
			rw.failOutput("parallel_tool_calls_violation", "model returned parallel tool calls despite parallel_tool_calls=false")
		}
		for _, index := range rw.order {
			call := rw.calls[index]
			if err := validateResponseToolCall(call.name, call.args.String(), call.argumentsSeen); err != nil {
				rw.failOutput("invalid_tool_call", err.Error())
				break
			}
		}
		if rw.streamErr == nil && len(rw.order) == 0 && rw.refusal.Len() == 0 && rw.req != nil {
			if err := rw.req.output.validate(rw.text.String()); err != nil {
				rw.failOutput("response_format_violation", err.Error())
			}
		}
		if rw.streamErr == nil {
			for _, index := range rw.order {
				if call := rw.calls[index]; call.callID == "" {
					call.callID = newRespID("call_")
				}
			}
			rw.flushReadyCalls()
		}
	}
	if rw.streamErr != nil {
		return fmt.Errorf("%v: %v", rw.streamErr["code"], rw.streamErr["message"])
	}
	return rw.writeErr
}

func validateResponseToolCall(name, args string, argumentsSeen bool) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("upstream ended with a tool call without a name")
	}
	if !argumentsSeen {
		return fmt.Errorf("upstream tool arguments must be a JSON string")
	}
	if strings.TrimSpace(args) != "" && !json.Valid([]byte(args)) {
		return fmt.Errorf("upstream ended with incomplete or invalid tool arguments")
	}
	return nil
}

func (rw *responsesWriter) ValidateCompletion(chat map[string]any) error {
	if rw.req == nil {
		return nil
	}
	return rw.validateJSONCompletion(chat, chatToResponses(chat, rw.resolvedModel(), rw.req.customTools))
}

func (rw *responsesWriter) validateJSONCompletion(chat, result map[string]any) error {
	if result["status"] != "completed" {
		return nil
	}
	choices := responseArray(chat["choices"])
	if len(choices) == 0 {
		return fmt.Errorf("upstream response contains no choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if len(responseArray(message["tool_calls"])) > 0 && legacyResponseFunction(message) != nil {
		return fmt.Errorf("upstream mixed legacy and modern tool calls")
	}
	calls := responseToolCalls(message)
	if rw.req.ParallelToolCalls != nil && !*rw.req.ParallelToolCalls && len(calls) > 1 {
		return fmt.Errorf("model returned parallel tool calls despite parallel_tool_calls=false")
	}
	for _, value := range calls {
		call, _ := value.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, ok := fn["arguments"].(string)
		if err := validateResponseToolCall(name, args, ok); err != nil {
			return err
		}
	}
	if len(calls) > 0 {
		return nil
	}
	if refusal, _ := message["refusal"].(string); refusal != "" {
		return nil
	}
	text, _ := message["content"].(string)
	return rw.req.output.validate(text)
}

func legacyResponseFunction(message map[string]any) map[string]any {
	fn, _ := message["function_call"].(map[string]any)
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	if name == "" && args == "" {
		return nil
	}
	return fn
}

func responseToolCalls(message map[string]any) []any {
	if calls := responseArray(message["tool_calls"]); len(calls) > 0 {
		return calls
	}
	if fn := legacyResponseFunction(message); fn != nil {
		return []any{map[string]any{"type": "function", "function": fn}}
	}
	return nil
}

// ─────────────────────────── 对象构造 ───────────────────────────

func (rw *responsesWriter) reasoningItem(status string) map[string]any {
	summary := []any{}
	if t := rw.reason.String(); t != "" {
		summary = []any{map[string]any{"type": "summary_text", "text": t}}
	}
	return map[string]any{
		"id": rw.rsID, "type": "reasoning", "summary": summary, "status": status,
	}
}

func (rw *responsesWriter) messageItem(status string) map[string]any {
	content := []any{}
	if status != "in_progress" {
		content = rw.messageContent()
	}
	return map[string]any{
		"id": rw.msgID, "type": "message", "status": status,
		"role": "assistant", "content": content,
	}
}

func (rw *responsesWriter) callItem(call *respToolCall, status string) map[string]any {
	if call.custom {
		input := ""
		if status != "in_progress" {
			input = customInputFromArgs(call.args.String())
		}
		return map[string]any{
			"id": call.id, "type": "custom_tool_call", "status": status,
			"call_id": call.callID, "name": call.name, "input": input,
		}
	}
	args := ""
	if status != "in_progress" {
		args = call.args.String()
	}
	return map[string]any{
		"id": call.id, "type": "function_call", "status": status,
		"call_id": call.callID, "name": call.name, "arguments": args,
	}
}

// outputItems 按已分配的 output_index 排列；尚无名称、从未开出的工具不能伪装成输出项。
func (rw *responsesWriter) outputItems() []any {
	type indexedItem struct {
		index int
		value any
	}
	ordered := []indexedItem{}
	if rw.rsOpen || rw.reason.Len() > 0 {
		ordered = append(ordered, indexedItem{rw.rsOutIdx, rw.reasoningItem(rw.itemStatus())})
	}
	if rw.msgOpen || rw.text.Len() > 0 || rw.refusal.Len() > 0 {
		ordered = append(ordered, indexedItem{rw.msgOutIdx, rw.messageItem(rw.itemStatus())})
	}
	for _, index := range rw.order {
		call := rw.calls[index]
		if call.id != "" {
			ordered = append(ordered, indexedItem{call.outIdx, rw.callItem(call, rw.itemStatus())})
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	items := make([]any, 0, len(ordered))
	for _, item := range ordered {
		items = append(items, item.value)
	}
	return items
}

func (rw *responsesWriter) responseObject(status string) map[string]any {
	obj := map[string]any{
		"id":                   rw.respID,
		"object":               "response",
		"created_at":           rw.created,
		"status":               status,
		"model":                rw.resolvedModel(),
		"output":               rw.outputItems(),
		"parallel_tool_calls":  true,
		"tool_choice":          "auto",
		"tools":                []any{},
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"metadata":             map[string]any{},
		"previous_response_id": nil,
		"reasoning":            nil,
		"store":                false,
		"temperature":          nil,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"top_p":                nil,
		"truncation":           "disabled",
		"user":                 nil,
		"usage":                rw.usageObject(),
	}
	if status == "failed" && rw.streamErr != nil {
		obj["error"] = map[string]any{
			"code":    rw.streamErr["code"],
			"message": rw.streamErr["message"],
		}
	}
	if status == "incomplete" {
		reason := "max_output_tokens"
		if rw.finishReason == "content_filter" {
			reason = "content_filter"
		}
		obj["incomplete_details"] = map[string]any{"reason": reason}
	}
	if rw.req != nil {
		rw.req.applyEcho(obj)
	}
	return obj
}

func (rw *responsesWriter) usageObject() map[string]any {
	in, out, total, cached, reason := 0, 0, 0, 0, 0
	if rw.usage != nil {
		in = intOf(rw.usage["prompt_tokens"])
		out = intOf(rw.usage["completion_tokens"])
		total = intOf(rw.usage["total_tokens"])
		cached = intOf(rw.usage["prompt_cache_hit_tokens"])
		reason = intOf(rw.usage["completion_thinking_tokens"])
		if reason == 0 {
			if d, ok := rw.usage["completion_tokens_details"].(map[string]any); ok {
				reason = intOf(d["reasoning_tokens"])
			}
		}
	}
	if total == 0 {
		total = in + out
	}
	return map[string]any{
		"input_tokens":          in,
		"output_tokens":         out,
		"total_tokens":          total,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens_details": map[string]any{"reasoning_tokens": reason},
	}
}

// chatToResponses 把一次完整的 chat completion 翻成 Responses 对象（非流式路径）。
func chatToResponses(chat map[string]any, model string, customNames map[string]bool) map[string]any {
	respID := newRespID("resp_")
	created := time.Now().Unix()
	if v, ok := chat["created"].(float64); ok && v > 0 {
		created = int64(v)
	}
	if v, ok := chat["id"].(string); ok && v != "" {
		respID = "resp_" + v
	}
	if v, ok := chat["model"].(string); ok && v != "" {
		model = v
	}
	items := []any{}
	var msg map[string]any
	status := "completed"
	var incomplete any
	if chs, ok := chat["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			msg, _ = c["message"].(map[string]any)
			if c["finish_reason"] == "length" {
				status = "incomplete"
				incomplete = map[string]any{"reason": "max_output_tokens"}
			} else if c["finish_reason"] == "content_filter" {
				status = "incomplete"
				incomplete = map[string]any{"reason": "content_filter"}
			}
		}
	}
	if msg != nil {
		if r, ok := msg["reasoning_content"].(string); ok && r != "" {
			items = append(items, map[string]any{
				"id": newRespID("rs_"), "type": "reasoning", "status": status,
				"summary": []any{map[string]any{"type": "summary_text", "text": r}},
			})
		}
		txt, _ := msg["content"].(string)
		refusal, _ := msg["refusal"].(string)
		content := []any{}
		if txt != "" || refusal == "" {
			content = append(content, map[string]any{"type": "output_text", "text": txt, "annotations": []any{}})
		}
		if refusal != "" {
			content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
		}
		items = append(items, map[string]any{
			"id": newRespID("msg_"), "type": "message", "status": status,
			"role": "assistant", "content": content,
		})
		if tcs := responseToolCalls(msg); len(tcs) > 0 {
			for _, t := range tcs {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				callID, _ := tm["id"].(string)
				if callID == "" {
					callID = newRespID("call_")
				}
				name, args := "", ""
				if fn, ok := tm["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
					args, _ = fn["arguments"].(string)
				}
				if customNames[name] {
					items = append(items, map[string]any{
						"id": newRespID("ctc_"), "type": "custom_tool_call", "status": status,
						"call_id": callID, "name": name, "input": customInputFromArgs(args),
					})
					continue
				}
				items = append(items, map[string]any{
					"id": newRespID("fc_"), "type": "function_call", "status": status,
					"call_id": callID, "name": name, "arguments": args,
				})
			}
		}
	}
	usage := map[string]any{
		"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
	if u, ok := chat["usage"].(map[string]any); ok {
		in := intOf(u["prompt_tokens"])
		out := intOf(u["completion_tokens"])
		total := intOf(u["total_tokens"])
		if total == 0 {
			total = in + out
		}
		reason := intOf(u["completion_thinking_tokens"])
		if reason == 0 {
			if d, ok := u["completion_tokens_details"].(map[string]any); ok {
				reason = intOf(d["reasoning_tokens"])
			}
		}
		usage = map[string]any{
			"input_tokens": in, "output_tokens": out, "total_tokens": total,
			"input_tokens_details":  map[string]any{"cached_tokens": intOf(u["prompt_cache_hit_tokens"])},
			"output_tokens_details": map[string]any{"reasoning_tokens": reason},
		}
	}
	return map[string]any{
		"id": respID, "object": "response", "created_at": created,
		"status": status, "model": model, "output": items,
		"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{},
		"error": nil, "incomplete_details": incomplete, "instructions": nil,
		"max_output_tokens": nil, "metadata": map[string]any{},
		"previous_response_id": nil, "reasoning": nil, "store": false,
		"temperature": nil, "text": map[string]any{"format": map[string]any{"type": "text"}},
		"top_p": nil, "truncation": "disabled", "user": nil, "usage": usage,
	}
}

// ─────────────────────────── 小工具 ───────────────────────────

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

func responseArray(v any) []any {
	switch values := v.(type) {
	case []any:
		return values
	case []map[string]any:
		result := make([]any, len(values))
		for i, value := range values {
			result[i] = value
		}
		return result
	default:
		return nil
	}
}

func newRespID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}
