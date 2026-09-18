// ═══ 更新日志 ═══
// 2026-09-18：直接 Chat 复用工具/结构化输出契约；正文增量保留，成功 finish 与 DONE 在完整校验后发出。
// 2026-09-18：重复终态检查仍返回客户端写失败，避免清理路径把断开误报为成功。
// 2026-09-18：过滤无法执行的内置 Chat 工具声明，无可用工具时去掉仅用于工具的空控制字段。
// 2026-09-18：直接 Chat 的公开长名同步到声明/历史/choice，并在流与JSON响应中恢复原名。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/upstream"
)

func newChatOutputContract(fields map[string]json.RawMessage) (*responsesRequest, error) {
	req := &responsesRequest{ToolChoice: fields["tool_choice"]}
	for key, target := range map[string]any{"model": &req.Model, "tools": &req.Tools, "parallel_tool_calls": &req.ParallelToolCalls} {
		if raw := fields[key]; len(raw) > 0 {
			if err := jsonutil.Decode(raw, target); err != nil {
				return nil, fmt.Errorf("invalid %s: %w", key, err)
			}
		}
	}
	if len(req.Tools) == 0 && len(fields["functions"]) > 0 {
		var functions []any
		if err := jsonutil.Decode(fields["functions"], &functions); err != nil {
			return nil, err
		}
		for _, function := range functions {
			req.Tools = append(req.Tools, map[string]any{"type": "function", "function": function})
		}
	}
	if len(req.ToolChoice) == 0 {
		req.ToolChoice = fields["function_call"]
	}
	if len(req.ToolChoice) > 0 {
		var choice any
		if err := jsonutil.Decode(req.ToolChoice, &choice); err != nil {
			return nil, err
		}
		if object, ok := choice.(map[string]any); ok {
			switch object["type"] {
			case "auto", "none", "required":
				choice = object["type"]
			case "function":
				if function, ok := object["function"].(map[string]any); ok {
					choice = map[string]any{"type": "function", "name": function["name"]}
				}
			case nil:
				// Legacy function_call={name:...} has no type discriminator.
				choice = map[string]any{"type": "function", "name": object["name"]}
			}
		}
		req.ToolChoice, _ = json.Marshal(choice)
	}
	if _, _, err := req.prepareToolPolicy(responsesTools(req.Tools, req)); err != nil {
		return nil, err
	}
	if raw := fields["response_format"]; len(raw) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		var format map[string]any
		if err := jsonutil.Decode(raw, &format); err != nil || format == nil {
			return nil, fmt.Errorf("response_format must be an object")
		}
		if format["type"] == "json_schema" {
			nested, ok := format["json_schema"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("response_format.json_schema must be an object")
			}
			copy := make(map[string]any, len(nested)+1)
			for key, value := range nested {
				copy[key] = value
			}
			copy["type"] = "json_schema"
			format = copy
		}
		text, _ := json.Marshal(map[string]any{"format": format})
		var err error
		req.output, err = parseOutputContract(text)
		if err != nil {
			return nil, err
		}
	}
	active := req.toolPolicy.mode != "auto" || req.toolPolicy.allowed != nil || len(req.toolPolicy.schemas) > 0 || len(req.toolAliases) > 0
	active = active || (req.ParallelToolCalls != nil && !*req.ParallelToolCalls) || (req.output != nil && req.output.chatFormat != nil)
	if !active {
		return nil, nil
	}
	return req, nil
}

func normalizeChatToolDeclarations(fields map[string]json.RawMessage, req *responsesRequest) (bool, error) {
	changed := false
	names := req.toolAliasIndex()
	rename := func(function map[string]any, declaration bool) bool {
		name, _ := function["name"].(string)
		alias, found := names["\x00"+name]
		if !found || alias == name {
			return false
		}
		function["name"] = alias
		if declaration {
			describeToolAlias(function, name)
		}
		return true
	}
	var tools []any
	if raw := fields["tools"]; len(raw) > 0 {
		if err := jsonutil.Decode(raw, &tools); err != nil {
			return false, err
		}
	}
	kept := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		kind, _ := tool["type"].(string)
		if isUnimplementedBuiltinTool(kind) {
			changed = true
			continue
		}
		if function, ok := tool["function"].(map[string]any); ok && rename(function, true) {
			changed = true
		}
		kept = append(kept, raw)
	}
	if changed {
		if len(kept) == 0 {
			delete(fields, "tools")
		} else {
			fields["tools"], _ = json.Marshal(kept)
		}
	}
	var legacy []any
	if raw := fields["functions"]; len(raw) > 0 {
		if err := jsonutil.Decode(raw, &legacy); err != nil {
			return false, err
		}
	}
	legacyChanged := false
	for _, raw := range legacy {
		if function, ok := raw.(map[string]any); ok && rename(function, true) {
			legacyChanged = true
		}
	}
	if legacyChanged {
		fields["functions"], _ = json.Marshal(legacy)
		changed = true
	}
	if len(names) > 0 {
		for _, key := range []string{"tool_choice", "function_call"} {
			if raw := fields[key]; len(raw) > 0 {
				var choice any
				if err := jsonutil.Decode(raw, &choice); err != nil {
					return false, err
				}
				choiceChanged := false
				if name, ok := choice.(string); ok {
					if alias, found := names["\x00"+name]; found {
						choice, choiceChanged = alias, true
					}
				} else if object, ok := choice.(map[string]any); ok {
					if function, ok := object["function"].(map[string]any); ok {
						choiceChanged = rename(function, false)
					} else {
						choiceChanged = rename(object, false)
					}
				}
				if choiceChanged {
					fields[key], _ = json.Marshal(choice)
					changed = true
				}
			}
		}
		var messages []any
		if raw := fields["messages"]; len(raw) > 0 {
			if err := jsonutil.Decode(raw, &messages); err != nil {
				return false, err
			}
		}
		historyChanged := false
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			for _, call := range responseArray(message["tool_calls"]) {
				item, _ := call.(map[string]any)
				if function, ok := item["function"].(map[string]any); ok && rename(function, false) {
					historyChanged = true
				}
			}
			if function, ok := message["function_call"].(map[string]any); ok && rename(function, false) {
				historyChanged = true
			}
			if message["role"] == "function" && rename(message, false) {
				historyChanged = true
			}
		}
		if historyChanged {
			fields["messages"], _ = json.Marshal(messages)
			changed = true
		}
	}
	if len(kept) == 0 && len(legacy) == 0 {
		if _, exists := fields["parallel_tool_calls"]; exists {
			delete(fields, "parallel_tool_calls")
			changed = true
		}
		for _, key := range []string{"tool_choice", "function_call"} {
			if raw := fields[key]; len(raw) > 0 {
				var choice any
				if err := jsonutil.Decode(raw, &choice); err != nil {
					return false, err
				}
				if object, ok := choice.(map[string]any); ok {
					choice = object["type"]
				}
				if choice == nil || choice == "auto" || choice == "none" {
					delete(fields, key)
					changed = true
				}
			}
		}
	}
	return changed, nil
}

// Chat's successful finish marker is an execution boundary for clients. Retain
// it until every call/choice is validated, including metadata arriving late.
// Text and argument deltas can still stream; usage follows the final finish.
type chatContractWriter struct {
	inner       http.ResponseWriter
	req         *responsesRequest
	raw         bytes.Buffer
	buffer      []byte
	usageFrames [][]byte
	errorFrame  []byte
	streaming   bool
	checked     bool
	err         error
	writeErr    error
}

func (w *chatContractWriter) Header() http.Header    { return w.inner.Header() }
func (w *chatContractWriter) WriteHeader(status int) { w.inner.WriteHeader(status) }

func (w *chatContractWriter) writeRaw(raw []byte) {
	if w.writeErr == nil {
		_, w.writeErr = w.inner.Write(raw)
	}
}

func (w *chatContractWriter) Write(raw []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		return w.inner.Write(raw)
	}
	w.streaming = true
	w.raw.Write(raw)
	w.buffer = append(w.buffer, raw...)
	for {
		index := bytes.Index(w.buffer, []byte("\n\n"))
		if index < 0 {
			break
		}
		frame := append([]byte{}, w.buffer[:index+2]...)
		w.buffer = w.buffer[index+2:]
		w.frame(frame)
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(raw), nil
}

func (w *chatContractWriter) frame(raw []byte) {
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "data: ") {
		w.writeRaw(raw)
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	var frame map[string]any
	if err := jsonutil.Decode([]byte(payload), &frame); err != nil {
		w.err = err
		return
	}
	if frame["error"] != nil {
		w.errorFrame = raw
		return
	}
	changed := false
	if frame["usage"] != nil {
		usage := make(map[string]any, len(frame))
		for key, value := range frame {
			usage[key] = value
		}
		usage["choices"] = []any{}
		encoded, _ := json.Marshal(usage)
		w.usageFrames = append(w.usageFrames, append(append([]byte("data: "), encoded...), '\n', '\n'))
		frame["usage"] = nil
		changed = true
	}
	hasDelta := false
	choices, _ := frame["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if finish, _ := choice["finish_reason"].(string); finish != "" {
			choice["finish_reason"] = nil
			changed = true
		}
		if delta, _ := choice["delta"].(map[string]any); len(delta) > 0 {
			hasDelta = true
			if w.restoreToolNames(delta) {
				changed = true
			}
		}
	}
	if !hasDelta {
		return
	}
	if changed {
		encoded, _ := json.Marshal(frame)
		raw = append(append([]byte("data: "), encoded...), '\n', '\n')
	}
	w.writeRaw(raw)
}

func (w *chatContractWriter) restoreToolNames(message map[string]any) bool {
	changed := false
	restore := func(function map[string]any) {
		name, _ := function["name"].(string)
		original, _, _ := w.req.responsesToolName(name)
		if original != name {
			function["name"] = original
			changed = true
		}
	}
	for _, raw := range responseArray(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		if function, ok := call["function"].(map[string]any); ok {
			restore(function)
		}
	}
	if function, ok := message["function_call"].(map[string]any); ok {
		restore(function)
	}
	return changed
}

func (w *chatContractWriter) PrepareCompletion(chat map[string]any) {
	for _, raw := range responseArray(chat["choices"]) {
		choice, _ := raw.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		w.restoreToolNames(message)
	}
}

func (w *chatContractWriter) Flush() {
	if flush, ok := w.inner.(http.Flusher); ok {
		flush.Flush()
	}
}

func (w *chatContractWriter) ValidateCompletion(chat map[string]any) error {
	choices := responseArray(chat["choices"])
	if len(choices) == 0 {
		return fmt.Errorf("upstream response contains no choices")
	}
	checker := newResponsesWriter(nil, w.req)
	for _, choice := range choices {
		if err := checker.ValidateCompletion(map[string]any{"choices": []any{choice}}); err != nil {
			return err
		}
	}
	return nil
}

func (w *chatContractWriter) CompletionError() error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.checked || !w.streaming {
		return w.err
	}
	w.checked = true
	chat, err := upstream.Aggregate(bytes.NewReader(w.raw.Bytes()))
	if err == nil && w.err == nil {
		err = w.ValidateCompletion(chat)
	}
	if w.err == nil {
		w.err = err
	}
	if w.err != nil {
		if len(w.errorFrame) > 0 {
			w.writeRaw(w.errorFrame)
		} else {
			encoded, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "response_contract_violation", "message": w.err.Error(), "type": "upstream_error"}})
			w.writeRaw(append(append([]byte("data: "), encoded...), '\n', '\n'))
		}
	} else {
		choices := []any{}
		for _, raw := range responseArray(chat["choices"]) {
			choice, _ := raw.(map[string]any)
			choices = append(choices, map[string]any{"index": choice["index"], "delta": map[string]any{}, "finish_reason": choice["finish_reason"]})
		}
		terminal := map[string]any{"id": chat["id"], "object": "chat.completion.chunk", "created": chat["created"], "model": chat["model"], "choices": choices, "usage": nil}
		encoded, _ := json.Marshal(terminal)
		w.writeRaw(append(append([]byte("data: "), encoded...), '\n', '\n'))
		for _, usage := range w.usageFrames {
			w.writeRaw(usage)
		}
	}
	w.writeRaw([]byte("data: [DONE]\n\n"))
	w.Flush()
	if w.writeErr != nil {
		return w.writeErr
	}
	return w.err
}

func (w *chatContractWriter) finish() { _ = w.CompletionError() }
