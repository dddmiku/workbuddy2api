// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
// ═══ 更新日志 ═══
// 2026-09-16：统一 SSE 事件解析与结束校验，保留上游错误并防止断流和残缺工具参数伪装成功。
// 2026-09-16：将完整消息快照转成缺失增量并核对已有输出，区分工具参数暂缺、显式空串和类型错误。
// 2026-09-17：合并 fork 的错误信封透传，保留完整诊断字段与数字字面量，同时维持 typed 失败终态。
// 2026-09-18：拒绝错误形状的工具列表，并在流结束时校验工具终态确有调用，避免预告正文静默收尾。
// 2026-09-18：逐 choice 隔离工具名称与聚合输出，拒绝被静默忽略的非法正文、delta 和 choices 形状。
// 2026-09-19：累计用量按已出现字段合并，末尾 credit-only 或详细字段不再抹掉前帧 token。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// StreamError 表示上游数据或传输失败；与客户端写出失败区分，供 handler 记录真实结果。
// Upstream 保留上游 error 对象（包括厂商 code/details）；Cause 可由 errors.Is/As 检查。
// Stream 已经发出 ErrorObject 及 [DONE] 时仍返回此错误，调用方不得再合成成功终态。
type StreamError struct {
	Code     string
	Message  string
	Upstream map[string]any
	Cause    error
	// 合法 error 对象的原始 JSON 信封，仅 Stream 使用；保留信封外的 requestId 等诊断字段。
	rawFrame json.RawMessage
}

func (e *StreamError) Error() string { return e.Message }
func (e *StreamError) Unwrap() error { return e.Cause }

// ErrorObject 返回 OpenAI SSE error 字段的值；上游对象不做字段删改。
func (e *StreamError) ErrorObject() map[string]any {
	if e.Upstream != nil {
		return e.Upstream
	}
	return map[string]any{"code": e.Code, "message": e.Message, "type": "upstream_error"}
}

type sseEvent struct {
	name string
	data string
}

// readSSE 按空行分隔事件，data: 可无空格、多行 data 按 SSE 规范用换行连接。
// 保留最后一个没有空行但数据完整的 EOF 事件，兼容只发 finish_reason 的上游。
// 非 EOF 读错误始终保留；done 由 consume 显式返回，之后不再消费任何数据。
func readSSE(r io.Reader, consume func(sseEvent) (bool, error), comment func(string) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data []string
	eventName := ""
	dispatch := func() (bool, error) {
		if len(data) == 0 {
			eventName = ""
			return false, nil
		}
		ev := sseEvent{name: eventName, data: strings.Join(data, "\n")}
		data = nil
		eventName = ""
		return consume(ev)
	}
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" && err == nil {
			if stop, consumeErr := dispatch(); consumeErr != nil || stop {
				return consumeErr
			}
		} else if strings.HasPrefix(line, ":") {
			if comment != nil {
				if writeErr := comment(line); writeErr != nil {
					return writeErr
				}
			}
		} else if line != "" {
			field, value, found := strings.Cut(line, ":")
			if found {
				value = strings.TrimPrefix(value, " ")
			}
			switch field {
			case "data":
				data = append(data, value)
			case "event":
				eventName = value
			}
		}
		if err == io.EOF {
			_, consumeErr := dispatch()
			return consumeErr
		}
		if err != nil {
			return &StreamError{Code: "upstream_read_error", Message: "upstream stream read failed", Cause: err}
		}
	}
}

func upstreamEventError(value any) *StreamError {
	obj, ok := value.(map[string]any)
	if !ok {
		message, _ := value.(string)
		if message == "" {
			message = "upstream returned an error"
		}
		obj = map[string]any{"message": message, "type": "upstream_error"}
	}
	message, _ := obj["message"].(string)
	if message == "" {
		message = "upstream returned an error"
	}
	return &StreamError{Code: "upstream_error", Message: message, Upstream: obj}
}

func decodeSSEEvent(ev sseEvent) (obj map[string]any, done bool, err error) {
	if ev.name != "error" && strings.TrimSpace(ev.data) == "[DONE]" {
		return nil, true, nil
	}
	decodeErr := json.Unmarshal([]byte(ev.data), &obj)
	if ev.name == "error" {
		if decodeErr == nil && obj != nil {
			if value, ok := obj["error"]; ok && value != nil {
				failure := upstreamEventError(value)
				if _, ok := value.(map[string]any); ok {
					failure.rawFrame = json.RawMessage(ev.data)
				}
				return nil, false, failure
			}
			return nil, false, upstreamEventError(obj)
		}
		return nil, false, upstreamEventError(ev.data)
	}
	if decodeErr != nil || obj == nil {
		return nil, false, &StreamError{Code: "upstream_parse", Message: "upstream stream contained invalid JSON data", Cause: decodeErr}
	}
	if value, ok := obj["error"]; ok && value != nil {
		failure := upstreamEventError(value)
		if _, ok := value.(map[string]any); ok {
			failure.rawFrame = json.RawMessage(ev.data)
		}
		return nil, false, failure
	}
	return obj, false, nil
}

type streamText struct {
	value strings.Builder
	seen  bool
}

// append 返回尚未发送的内容；完整快照不能覆盖已经发送过的不同前缀。
func (s *streamText) append(value string, snapshot bool) (string, error) {
	addition := value
	if snapshot {
		previous := s.value.String()
		if !strings.HasPrefix(value, previous) {
			return "", &StreamError{Code: "upstream_parse", Message: "upstream message snapshot conflicts with streamed data"}
		}
		addition = value[len(previous):]
	}
	s.value.WriteString(addition)
	s.seen = true
	return addition, nil
}

type streamFunction struct {
	name      string
	arguments streamText
}

func (f *streamFunction) observe(raw map[string]any, snapshot bool) (map[string]any, error) {
	out := map[string]any{}
	if name, ok := raw["name"].(string); ok && name != "" {
		if f.name != "" && name != f.name {
			return nil, &StreamError{Code: "upstream_parse", Message: "upstream changed a streamed function name"}
		}
		if !snapshot || f.name == "" {
			out["name"] = name
		}
		f.name = name
	}
	if value, exists := raw["arguments"]; exists {
		args, ok := value.(string)
		if !ok {
			return nil, &StreamError{Code: "invalid_tool_arguments", Message: "upstream tool arguments must be a JSON string"}
		}
		seen := f.arguments.seen
		addition, err := f.arguments.append(args, snapshot)
		if err != nil {
			return nil, err
		}
		if !snapshot || addition != "" || !seen {
			out["arguments"] = addition
		}
	}
	return out, nil
}

type streamToolCall struct {
	id       string
	typ      string
	function streamFunction
}

type streamChoice struct {
	output       bool
	finishReason string
	role         string
	text         map[string]*streamText
	tools        map[int]*streamToolCall
	function     *streamFunction
}

type streamState struct {
	choices map[int]*streamChoice
	done    bool
}

func validFinishReason(reason string) bool {
	switch reason {
	case "stop", "length", "tool_calls", "content_filter", "function_call":
		return true
	}
	return false
}

func (c *streamChoice) observeOutput(output map[string]any, wholeMessage bool) (map[string]any, error) {
	normalized := map[string]any{}
	if role, _ := output["role"].(string); role != "" {
		if !wholeMessage || c.role == "" {
			normalized["role"] = role
		}
		c.role = role
	}
	for _, key := range []string{"content", "reasoning_content", "refusal"} {
		if value := output[key]; value != nil {
			if _, ok := value.(string); !ok {
				return nil, &StreamError{Code: "upstream_parse", Message: "upstream " + key + " must be a string or null"}
			}
		}
		if value, ok := output[key].(string); ok {
			if c.text == nil {
				c.text = map[string]*streamText{}
			}
			if c.text[key] == nil {
				c.text[key] = &streamText{}
			}
			addition, err := c.text[key].append(value, wholeMessage)
			if err != nil {
				return nil, err
			}
			normalized[key] = addition
			if value != "" {
				c.output = true
			}
		}
	}
	if calls, ok := output["tool_calls"].([]any); ok {
		var normalizedCalls []any
		for i, item := range calls {
			call, ok := item.(map[string]any)
			if !ok {
				return nil, &StreamError{Code: "upstream_parse", Message: "upstream stream contained an invalid tool call"}
			}
			c.output = true
			idx := 0
			if wholeMessage {
				idx = i
			}
			if value, ok := call["index"].(float64); ok {
				idx = int(value)
			}
			if c.tools == nil {
				c.tools = map[int]*streamToolCall{}
			}
			if wholeMessage {
				if id, _ := call["id"].(string); id != "" {
					for candidate, previous := range c.tools {
						if previous.id == id {
							idx = candidate
							break
						}
					}
				}
			}
			state := c.tools[idx]
			if state == nil {
				state = &streamToolCall{}
				c.tools[idx] = state
			}
			next := map[string]any{"index": float64(idx)}
			for _, field := range []struct {
				key      string
				previous *string
			}{{"id", &state.id}, {"type", &state.typ}} {
				if value, _ := call[field.key].(string); value != "" {
					if *field.previous != "" && value != *field.previous {
						return nil, &StreamError{Code: "upstream_parse", Message: "upstream changed a streamed tool call identity"}
					}
					if !wholeMessage || *field.previous == "" {
						next[field.key] = value
					}
					*field.previous = value
				}
			}
			if fn, ok := call["function"].(map[string]any); ok {
				nextFn, err := state.function.observe(fn, wholeMessage)
				if err != nil {
					return nil, err
				}
				if len(nextFn) > 0 {
					next["function"] = nextFn
				}
			} else if value, exists := call["function"]; exists && value != nil {
				return nil, &StreamError{Code: "upstream_parse", Message: "upstream stream contained an invalid tool function"}
			}
			if len(next) > 1 {
				normalizedCalls = append(normalizedCalls, next)
			}
		}
		if len(normalizedCalls) > 0 {
			normalized["tool_calls"] = normalizedCalls
		}
	} else if value, exists := output["tool_calls"]; exists && value != nil {
		return nil, &StreamError{Code: "upstream_parse", Message: "upstream tool_calls must be an array"}
	}
	if fn, ok := output["function_call"].(map[string]any); ok {
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		if value, exists := fn["arguments"]; exists {
			if _, ok := value.(string); !ok {
				return nil, &StreamError{Code: "invalid_tool_arguments", Message: "upstream function arguments must be a JSON string"}
			}
		}
		// WorkBuddy 的空占位 function_call 不是一次工具调用，沿用原有剥除语义。
		if c.function != nil || name != "" || args != "" {
			if c.function == nil {
				c.function = &streamFunction{}
			}
			c.output = true
			next, err := c.function.observe(fn, wholeMessage)
			if err != nil {
				return nil, err
			}
			if len(next) > 0 {
				normalized["function_call"] = next
			}
		}
	} else if value, exists := output["function_call"]; exists && value != nil {
		return nil, &StreamError{Code: "upstream_parse", Message: "upstream stream contained an invalid function call"}
	}
	return normalized, nil
}

func (c *streamChoice) validateArguments() error {
	// length/content_filter 是明确的不完整响应，由协议转换层保留 incomplete 语义。
	if c.finishReason == "length" || c.finishReason == "content_filter" {
		return nil
	}
	for _, call := range c.tools {
		if !call.function.arguments.seen || isTruncatedArguments(call.function.arguments.value.String()) {
			return &StreamError{Code: "invalid_tool_arguments", Message: "upstream ended with incomplete or invalid tool arguments"}
		}
	}
	if c.function != nil && (!c.function.arguments.seen || isTruncatedArguments(c.function.arguments.value.String())) {
		return &StreamError{Code: "invalid_tool_arguments", Message: "upstream ended with incomplete or invalid function arguments"}
	}
	return nil
}

func (s *streamState) observe(obj map[string]any) error {
	if value := obj["choices"]; value != nil {
		if _, ok := value.([]any); !ok {
			return &StreamError{Code: "upstream_parse", Message: "upstream choices must be an array"}
		}
	}
	choices, _ := obj["choices"].([]any)
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			return &StreamError{Code: "upstream_parse", Message: "upstream stream contained an invalid choice"}
		}
		idx := 0
		if value, ok := choice["index"].(float64); ok {
			idx = int(value)
		}
		if s.choices == nil {
			s.choices = map[int]*streamChoice{}
		}
		state := s.choices[idx]
		if state == nil {
			state = &streamChoice{}
			s.choices[idx] = state
		}
		normalized := map[string]any{}
		for _, key := range []string{"delta", "message"} {
			if value := choice[key]; value != nil {
				if _, ok := value.(map[string]any); !ok {
					return &StreamError{Code: "upstream_parse", Message: "upstream " + key + " must be an object or null"}
				}
			}
			if output, ok := choice[key].(map[string]any); ok {
				delta, err := state.observeOutput(output, key == "message")
				if err != nil {
					return err
				}
				mergeOutputDelta(normalized, delta)
			}
		}
		// 下游只处理一次统一增量，完整 message 不再走另一个丢字段/重复输出的旁路。
		choice["delta"] = normalized
		delete(choice, "message")
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			if !validFinishReason(reason) {
				return &StreamError{Code: "upstream_parse", Message: "upstream stream contained an unknown finish_reason"}
			}
			state.finishReason = reason
			// 成功 finish 帧写出之前就校验，不能先允许客户端执行工具、随后再报残参错误。
			if err := state.validateArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamState) end() error {
	hasResponse := false
	for _, choice := range s.choices {
		if choice.output || choice.finishReason != "" {
			hasResponse = true
		}
		if choice.output && !s.done && choice.finishReason == "" {
			return &StreamError{Code: "upstream_truncated", Message: "upstream stream ended before a terminal marker"}
		}
		if err := choice.validateArguments(); err != nil {
			return err
		}
		// A finish marker can precede late metadata. Enforce presence only at the
		// response boundary, after all frames before DONE have been consumed.
		if (choice.finishReason == "tool_calls" || choice.finishReason == "function_call") && len(choice.tools) == 0 && choice.function == nil {
			return &StreamError{Code: "missing_tool_call", Message: "upstream ended with a tool finish reason but no tool call"}
		}
	}
	if !hasResponse {
		return &StreamError{Code: "empty_upstream", Message: "upstream stream contained no valid data events"}
	}
	return nil
}

func (c *streamChoice) aggregate(index int) map[string]any {
	role := c.role
	if role == "" {
		role = "assistant"
	}
	message := map[string]any{"role": role, "content": ""}
	for _, key := range []string{"content", "reasoning_content", "refusal"} {
		if text := c.text[key]; text != nil && text.value.Len() > 0 {
			message[key] = text.value.String()
		}
	}
	incomplete := c.finishReason == "length" || c.finishReason == "content_filter"
	function := func(value *streamFunction) map[string]any {
		if value == nil || (incomplete && (!value.arguments.seen || isTruncatedArguments(value.arguments.value.String()))) {
			return nil
		}
		out := map[string]any{}
		if value.name != "" {
			out["name"] = value.name
		}
		if value.arguments.seen {
			out["arguments"] = value.arguments.value.String()
		}
		return out
	}
	if fn := function(c.function); fn != nil {
		message["function_call"] = fn
	}
	indexes := make([]int, 0, len(c.tools))
	for index := range c.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	calls := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		call := c.tools[index]
		fn := function(&call.function)
		if fn == nil {
			continue
		}
		out := map[string]any{"index": index, "function": fn}
		if call.id != "" {
			out["id"] = call.id
		}
		if call.typ != "" {
			out["type"] = call.typ
		}
		calls = append(calls, out)
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	finish := c.finishReason
	if finish == "" {
		finish = "stop"
	}
	return map[string]any{"index": index, "message": message, "finish_reason": finish}
}

// Aggregate 读取完整 SSE 流，保留每个 choice 的正文、工具与终态。
// 分片/多行 SSE 由 readSSE 处理；只有合法结束才返回成功，异常流返回 *StreamError。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	var (
		id, model string
		created   float64
		usage     map[string]any
	)
	state := &streamState{}
	err := readSSE(r, func(ev sseEvent) (bool, error) {
		chunk, done, err := decodeSSEEvent(ev)
		if err != nil || done {
			state.done = done
			return true, err
		}
		if err := state.observe(chunk); err != nil {
			return true, err
		}
		if value, ok := chunk["id"].(string); ok && id == "" {
			id = value
		}
		if value, ok := chunk["model"].(string); ok && model == "" {
			model = value
		}
		if value, ok := chunk["created"].(float64); ok && created == 0 {
			created = value
		}
		if value, ok := chunk["usage"].(map[string]any); ok {
			usage = MergeUsage(usage, value)
		}
		return false, nil
	}, nil)
	if err != nil {
		return nil, err
	}
	if err := state.end(); err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	indexes := make([]int, 0, len(state.choices))
	for index := range state.choices {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	choices := make([]any, 0, len(indexes))
	for _, index := range indexes {
		choices = append(choices, state.choices[index].aggregate(index))
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": choices,
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	mergeFunctionDelta(mf, df)
}

func mergeFunctionDelta(merged, delta map[string]any) {
	if value, ok := delta["name"].(string); ok && value != "" {
		merged["name"] = value
	}
	if value, ok := delta["arguments"].(string); ok {
		previous, _ := merged["arguments"].(string)
		merged["arguments"] = previous + value
	}
}

func mergeOutputDelta(merged, delta map[string]any) {
	for key, value := range delta {
		switch key {
		case "content", "reasoning_content", "refusal":
			previous, _ := merged[key].(string)
			addition, _ := value.(string)
			merged[key] = previous + addition
		case "tool_calls":
			previous, _ := merged[key].([]any)
			addition, _ := value.([]any)
			merged[key] = append(previous, addition...)
		case "function_call":
			fn, _ := merged[key].(map[string]any)
			if fn == nil {
				fn = map[string]any{}
				merged[key] = fn
			}
			addition, _ := value.(map[string]any)
			mergeFunctionDelta(fn, addition)
		default:
			merged[key] = value
		}
	}
}

// stripToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除（无论上游是
// 空串还是重复非空串）。这是 OpenAI 官方流的真实形态——首帧带 name，后续帧只带
// arguments 片段、不再出现 name 键——因此是累加型与覆盖型客户端的共同祖先行为。
//
// 两类消费模型在该形态下同时正确：
//   - 累加型（官方 WorkBuddy/CodeBuddy `name += tc_function?.name || ""`）：
//     后续分片 name 键缺失 → 追加空串，累积 name 保持唯一，不再拼成 Bash×帧数（issue #82）。
//   - 覆盖型（hawklithm#2 / Grok Build `name ?? state.name` 或 `if (name) state.name = name`）：
//     后续分片 name 键缺失 → 保留已建好的首帧 name，不被空串意外清空。
//     键缺失是比空串更安全的形态：`??` 与 truthy 守卫对缺失键必然保留旧值，
//     而对空串，`??` 会误判为重设并清空工具名。
//
// seen 记录每个 index 是否已实际发过非空 name；仅收到 id/arguments 不能挡住后补的 name。
// 只动 function.name 键，id/type/arguments 原样透传。
func stripToolCallNames(obj map[string]any, seen map[[2]int]bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		choiceIndex := 0
		if value, ok := c["index"].(float64); ok {
			choiceIndex = int(value)
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			key := [2]int{choiceIndex, idx}
			if seen[key] {
				// 已发过首片：删除本分片的 name 键（存在即删，幂等）。
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			if fn, _ := tc["function"].(map[string]any); fn != nil {
				if name, _ := fn["name"].(string); name != "" {
					seen[key] = true
				} else {
					delete(fn, "name")
				}
			}
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	if value, exists := obj["error"]; exists && value != nil {
		return map[string]any{"error": value}
	}
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			d, _ := c["delta"].(map[string]any)
			if d == nil {
				d, _ = c["message"].(map[string]any)
			}
			if d != nil {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 逐事件规范化并 flush；合法终态写唯一 [DONE]。
// 上游错误/读错误/截断先发 error 再发 [DONE] 并返回 *StreamError。
// [DONE] 只关闭传输，不覆盖已有 error；客户端写失败直接返回原始错误。
func Stream(w http.ResponseWriter, r io.Reader) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// toolCallSeen 跨帧记录 delta.tool_calls 里已发过首片的 index，
	// 供逐 chunk 透传时收敛 name 为「每 index 一次」（对齐 OpenAI 官方流）。
	toolCallSeen := map[[2]int]bool{}

	// firstID 透传流的消息级 id 基准：缓存首个非空上游 id，后续帧缺失/空串时复用
	// （issue #35：同一条 SSE 消息所有帧共用一个真实 id，后台按 id 归并；此前中间帧
	// 一律补 chatcmpl-wb2api 哨兵，造成同流 id 分裂）。全流无真实 id → 才出现哨兵。
	firstID := ""
	var usage map[string]any

	// 正常帧和错误帧共享一个写出口，任何客户端断开均向上传递。
	writeRaw := func(payload string) error {
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	state := &streamState{}
	err := readSSE(r, func(ev sseEvent) (bool, error) {
		obj, done, err := decodeSSEEvent(ev)
		if err != nil || done {
			state.done = done
			return true, err
		}
		if err := state.observe(obj); err != nil {
			return true, err
		}
		if value, ok := obj["usage"].(map[string]any); ok {
			usage = MergeUsage(usage, value)
			obj["usage"] = usage
		}
		stripToolCallNames(obj, toolCallSeen)
		if firstID == "" {
			if value, ok := obj["id"].(string); ok && value != "" {
				firstID = value
			}
		} else if value, ok := obj["id"].(string); !ok || value == "" {
			obj["id"] = firstID
		}
		raw, err := json.Marshal(normalizeFrame(obj))
		if err != nil {
			return true, &StreamError{Code: "upstream_parse", Message: "upstream frame could not be encoded", Cause: err}
		}
		return false, writeRaw(string(raw))
	}, func(line string) error {
		if _, err := io.WriteString(w, line+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	})
	if err == nil {
		err = state.end()
	}
	if err != nil {
		var streamErr *StreamError
		if !errors.As(err, &streamErr) {
			return err
		}
		var raw []byte
		var marshalErr error
		if len(streamErr.rawFrame) > 0 {
			// 多行 SSE 的 JSON 收成一行，仅去格式空白，不重新解析数字或删诊断字段。
			var compact bytes.Buffer
			marshalErr = json.Compact(&compact, streamErr.rawFrame)
			raw = compact.Bytes()
		} else {
			raw, marshalErr = json.Marshal(map[string]any{"error": streamErr.ErrorObject()})
		}
		if marshalErr != nil {
			return errors.Join(err, marshalErr)
		}
		if writeErr := writeRaw(string(raw)); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	if writeErr := writeRaw("[DONE]"); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return err
}
