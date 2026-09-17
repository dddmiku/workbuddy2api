// ═══ 更新日志 ═══
// 2026-09-16：在选号前校验请求基础结构并拒绝不支持的 Responses 状态能力，避免坏参数被静默丢弃或触发换号。
// 2026-09-17：接受 Responses 的命名空间工具分组，并把命名空间名字写回函数调用历史。
// 2026-09-18：内置工具按前缀接受并丢弃（补齐 tool_search 等新类型），避免客户端升级即不可用。
// 2026-09-18：只对"能力/状态"类字段报错（background/store/previous_response_id/
//
//	服务端工具），风格与提示类字段（text.verbosity、truncation、allowed_tools、
//	未知历史项）一律接受并忽略，避免客户端升级反复炸在 400 上。
package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/jsonutil"
)

func requestValidationObject(value any, path string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("%s must be an object", path)
	}
	return object, nil
}

func requestValidationString(value any, path string, nonempty bool) error {
	text, ok := value.(string)
	if !ok || (nonempty && strings.TrimSpace(text) == "") {
		if nonempty {
			return fmt.Errorf("%s must be a nonempty string", path)
		}
		return fmt.Errorf("%s must be a string", path)
	}
	return nil
}

func requestValidationOptionalStrings(object map[string]any, path string, keys ...string) error {
	for _, key := range keys {
		if value, present := object[key]; present && value != nil {
			if err := requestValidationString(value, path+"."+key, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func requestValidationOptionalBools(object map[string]any, path string, keys ...string) error {
	for _, key := range keys {
		if value, present := object[key]; present && value != nil {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%s.%s must be a boolean", path, key)
			}
		}
	}
	return nil
}

func requestValidationOptionalNumbers(object map[string]any, path string, integers bool, keys ...string) error {
	for _, key := range keys {
		value, present := object[key]
		if !present || value == nil {
			continue
		}
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%s.%s must be a number", path, key)
		}
		if integers {
			n, err := number.Int64()
			if err != nil || n < 0 {
				return fmt.Errorf("%s.%s must be a nonnegative integer", path, key)
			}
		} else if _, err := number.Float64(); err != nil {
			return fmt.Errorf("%s.%s must be a finite number", path, key)
		}
	}
	return nil
}

func requestValidationStringArray(value any, path string) error {
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array of strings", path)
	}
	for i, item := range items {
		if err := requestValidationString(item, fmt.Sprintf("%s[%d]", path, i), false); err != nil {
			return err
		}
	}
	return nil
}

// Schema keywords and model-specific limits remain the downstream contract's
// responsibility. This guard rejects a non-object schema, without inventing one.
func requestValidationFunction(function map[string]any, path string) error {
	if err := requestValidationString(function["name"], path+".name", true); err != nil {
		return err
	}
	if err := requestValidationOptionalStrings(function, path, "description"); err != nil {
		return err
	}
	if err := requestValidationOptionalBools(function, path, "strict"); err != nil {
		return err
	}
	if parameters, present := function["parameters"]; present && parameters != nil {
		if _, err := requestValidationObject(parameters, path+".parameters"); err != nil {
			return err
		}
	}
	return nil
}

func requestValidationTools(value any, path string, responses bool) error {
	if value == nil {
		return nil
	}
	tools, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array", path)
	}
	for i, raw := range tools {
		toolPath := fmt.Sprintf("%s[%d]", path, i)
		tool, err := requestValidationObject(raw, toolPath)
		if err != nil {
			return err
		}
		if err := requestValidationToolSpec(tool, toolPath, responses, true); err != nil {
			return err
		}
	}
	return nil
}

// requestValidationToolSpec 校验单条工具定义。allowNamespace 为真时接受 Responses 的
// 命名空间分组（type=namespace，内含 function/custom 子工具，子层不再允许继续嵌套）。
// 网关无法实现的内置工具按声明接受、由 responsesTools 丢弃：官方 Codex 0.155 默认
// 就会带上 web_search，整条请求拒绝会让客户端完全不可用。
//
// 两类区别对待：
//   - 客户端自己执行、默认就会带上的（web_search / tool_search）：接受并丢弃，
//     拒绝等于整个会话不可用（Codex 0.156 起默认带 tool_search）。
//   - 需要服务端能力、用户显式声明的（file_search / mcp / image_generation 等）：
//     继续明确报错。静默丢弃会让用户以为文件检索/图片生成在生效，比报错更难排查。
var unimplementedBuiltinTools = map[string]bool{
	"web_search":         true,
	"web_search_preview": true,
	"tool_search":        true,
}

// builtinToolPrefixes 已知内置工具族：官方会发布带日期后缀的版本变体
// （例如 web_search_2025_08_26），按前缀一并接受，避免每次客户端升级都炸一次。
var builtinToolPrefixes = []string{
	"web_search", "tool_search",
}

// isUnimplementedBuiltinTool 判断是否为"接受但丢弃"的内置工具类型。
func isUnimplementedBuiltinTool(kind string) bool {
	if kind == "" {
		return false
	}
	if unimplementedBuiltinTools[kind] {
		return true
	}
	for _, prefix := range builtinToolPrefixes {
		if strings.HasPrefix(kind, prefix+"_") || strings.HasPrefix(kind, prefix+"-") {
			return true
		}
	}
	return false
}

func requestValidationToolSpec(tool map[string]any, toolPath string, responses, allowNamespace bool) error {
	if err := requestValidationString(tool["type"], toolPath+".type", true); err != nil {
		return err
	}
	if kind, _ := tool["type"].(string); isUnimplementedBuiltinTool(kind) {
		if !allowNamespace {
			return fmt.Errorf("%s.type %q is not supported inside a namespace; use function or custom tools", toolPath, kind)
		}
		// 声明本身可以出现，只是不会转发到上游；/v1/chat/completions 与 /v1/responses
		// 一致：客户端带上默认的内置工具不该让整条请求失败。额外字段不校验，避免绑定未来格式。
		return nil
	}
	switch tool["type"] {
	case "function":
		function := tool
		functionPath := toolPath
		if rawFunction, nested := tool["function"]; nested {
			object, err := requestValidationObject(rawFunction, toolPath+".function")
			if err != nil {
				return err
			}
			function, functionPath = object, toolPath+".function"
		} else if !responses {
			return fmt.Errorf("%s.function must be an object", toolPath)
		}
		return requestValidationFunction(function, functionPath)
	case "custom":
		if !responses {
			return fmt.Errorf("%s.type custom is supported through the Responses endpoint", toolPath)
		}
		if err := requestValidationString(tool["name"], toolPath+".name", true); err != nil {
			return err
		}
		if err := requestValidationOptionalStrings(tool, toolPath, "description"); err != nil {
			return err
		}
		if rawFormat := tool["format"]; rawFormat != nil {
			format, err := requestValidationObject(rawFormat, toolPath+".format")
			if err != nil {
				return err
			}
			return requestValidationOptionalStrings(format, toolPath+".format", "type", "syntax", "definition")
		}
		return nil
	case "namespace":
		if !responses {
			return fmt.Errorf("%s.type namespace is supported through the Responses endpoint", toolPath)
		}
		if !allowNamespace {
			return fmt.Errorf("%s.type namespace cannot be nested; use function or custom tools inside a namespace", toolPath)
		}
		if err := requestValidationString(tool["name"], toolPath+".name", true); err != nil {
			return err
		}
		if err := requestValidationOptionalStrings(tool, toolPath, "description"); err != nil {
			return err
		}
		children, ok := tool["tools"].([]any)
		if !ok || len(children) == 0 {
			return fmt.Errorf("%s.tools must be a non-empty array of function or custom tools", toolPath)
		}
		for i, raw := range children {
			childPath := fmt.Sprintf("%s.tools[%d]", toolPath, i)
			child, err := requestValidationObject(raw, childPath)
			if err != nil {
				return err
			}
			if err := requestValidationToolSpec(child, childPath, responses, false); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s.type %q is not supported; use function, custom, or namespace tools", toolPath, tool["type"])
	}
}

func requestValidationContent(value any, path string, responses bool) error {
	if value == nil {
		return nil
	}
	if _, ok := value.(string); ok {
		return nil
	}
	parts, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be a string, content-part array, or null", path)
	}
	for i, raw := range parts {
		partPath := fmt.Sprintf("%s[%d]", path, i)
		part, err := requestValidationObject(raw, partPath)
		if err != nil {
			return err
		}
		if err := requestValidationOptionalStrings(part, partPath, "type", "text", "refusal", "detail"); err != nil {
			return err
		}
		kind, _ := part["type"].(string)
		switch kind {
		case "", "text", "input_text", "output_text", "summary_text":
			if err := requestValidationString(part["text"], partPath+".text", false); err != nil {
				return err
			}
		case "refusal":
			if err := requestValidationString(part["refusal"], partPath+".refusal", false); err != nil {
				return err
			}
		case "image_url", "input_image":
			image := part["image_url"]
			if imageObject, ok := image.(map[string]any); ok {
				if err := requestValidationOptionalStrings(imageObject, partPath+".image_url", "detail"); err != nil {
					return err
				}
				image = imageObject["url"]
			}
			if err := requestValidationString(image, partPath+".image_url", true); err != nil {
				return err
			}
		default:
			if responses {
				return fmt.Errorf("%s.type %q is not supported by the Responses content adapter", partPath, kind)
			}
			// Chat content is passed through: leave other well-formed modalities
			// to the selected upstream instead of assuming a text-only model.
		}
	}
	return nil
}

func requestValidationHistoryCalls(value any, path string) error {
	if value == nil {
		return nil
	}
	calls, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array", path)
	}
	for i, raw := range calls {
		callPath := fmt.Sprintf("%s[%d]", path, i)
		call, err := requestValidationObject(raw, callPath)
		if err != nil {
			return err
		}
		if err := requestValidationOptionalStrings(call, callPath, "id", "type"); err != nil {
			return err
		}
		if kind, _ := call["type"].(string); kind != "" && kind != "function" {
			return fmt.Errorf("%s.type %q is not supported", callPath, kind)
		}
		function, err := requestValidationObject(call["function"], callPath+".function")
		if err != nil {
			return err
		}
		if err := requestValidationString(function["name"], callPath+".function.name", true); err != nil {
			return err
		}
		if err := requestValidationOptionalStrings(function, callPath+".function", "arguments"); err != nil {
			return err
		}
	}
	return nil
}

func requestValidationToolChoice(value any, path string, responses bool) error {
	if value == nil {
		return nil
	}
	if _, ok := value.(string); ok {
		// The CN adapter also accepts a bare function name as a string.
		return requestValidationString(value, path, true)
	}
	choice, err := requestValidationObject(value, path)
	if err != nil {
		return err
	}
	if err := requestValidationString(choice["type"], path+".type", true); err != nil {
		return err
	}
	kind := choice["type"].(string)
	if responses {
		if kind == "allowed_tools" {
			// 白名单式选择：responsesToolChoice 会折成 auto。这是"限制模型可调用的工具"
			// 的提示，网关没有等价物，接受声明但不收缩上游的可选工具集。
			return nil
		}
		if kind != "function" && kind != "custom" {
			return fmt.Errorf("%s.type %q is not supported; use auto/none/required strings or a named function/custom choice", path, kind)
		}
		// This is the shape that responsesToolChoice actually maps. A nested
		// chat-style choice would otherwise silently fall back to auto.
		// 命名空间内的工具额外带 namespace，映射时拼回出站扁平名。
		if err := requestValidationOptionalStrings(choice, path, "namespace"); err != nil {
			return err
		}
		return requestValidationString(choice["name"], path+".name", true)
	}
	switch kind {
	case "auto", "none", "required":
		return nil
	case "function":
		if rawFunction, present := choice["function"]; present {
			function, err := requestValidationObject(rawFunction, path+".function")
			if err != nil {
				return err
			}
			return requestValidationString(function["name"], path+".function.name", true)
		}
		return requestValidationString(choice["name"], path+".name", true)
	default:
		return fmt.Errorf("%s.type %q is not supported", path, kind)
	}
}

// validateChatRequest is structural only: it never rewrites the request, checks
// tool/result pairing, or tries to parse possibly partial historical arguments.
func validateChatRequest(body []byte) error {
	var object map[string]any
	if err := jsonutil.Decode(body, &object); err != nil {
		return fmt.Errorf("request body must be a JSON object: %w", err)
	}
	if object == nil {
		return fmt.Errorf("request body must be a JSON object")
	}
	if err := requestValidationString(object["model"], "model", true); err != nil {
		return err
	}
	messages, ok := object["messages"].([]any)
	if !ok {
		return fmt.Errorf("messages must be an array")
	}
	for i, raw := range messages {
		path := fmt.Sprintf("messages[%d]", i)
		message, err := requestValidationObject(raw, path)
		if err != nil {
			return err
		}
		if err := requestValidationString(message["role"], path+".role", true); err != nil {
			return err
		}
		if err := requestValidationOptionalStrings(message, path, "name", "tool_call_id", "reasoning", "reasoning_content", "refusal"); err != nil {
			return err
		}
		if err := requestValidationContent(message["content"], path+".content", false); err != nil {
			return err
		}
		if err := requestValidationHistoryCalls(message["tool_calls"], path+".tool_calls"); err != nil {
			return err
		}
		if rawCall := message["function_call"]; rawCall != nil {
			call, err := requestValidationObject(rawCall, path+".function_call")
			if err != nil {
				return err
			}
			if err := requestValidationString(call["name"], path+".function_call.name", true); err != nil {
				return err
			}
			if err := requestValidationOptionalStrings(call, path+".function_call", "arguments"); err != nil {
				return err
			}
		}
	}
	if err := requestValidationOptionalBools(object, "request", "stream", "parallel_tool_calls", "logprobs"); err != nil {
		return err
	}
	if err := requestValidationOptionalNumbers(object, "request", false, "temperature", "top_p", "frequency_penalty", "presence_penalty"); err != nil {
		return err
	}
	if err := requestValidationOptionalNumbers(object, "request", true, "max_tokens", "max_completion_tokens", "n", "top_logprobs"); err != nil {
		return err
	}
	if err := requestValidationTools(object["tools"], "tools", false); err != nil {
		return err
	}
	if err := requestValidationToolChoice(object["tool_choice"], "tool_choice", false); err != nil {
		return err
	}
	if rawFunctions := object["functions"]; rawFunctions != nil {
		functions, ok := rawFunctions.([]any)
		if !ok {
			return fmt.Errorf("functions must be an array")
		}
		for i, raw := range functions {
			path := fmt.Sprintf("functions[%d]", i)
			function, err := requestValidationObject(raw, path)
			if err != nil {
				return err
			}
			if err := requestValidationFunction(function, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func requestValidationResponsesInput(value any) error {
	if value == nil {
		return nil
	}
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("input must be a string, array, or null")
	}
	for i, raw := range items {
		path := fmt.Sprintf("input[%d]", i)
		item, err := requestValidationObject(raw, path)
		if err != nil {
			return err
		}
		if err := requestValidationOptionalStrings(item, path, "type", "id", "call_id", "status", "role"); err != nil {
			return err
		}
		kind, _ := item["type"].(string)
		switch kind {
		case "", "message":
			if err := requestValidationContent(item["content"], path+".content", true); err != nil {
				return err
			}
		case "function_call", "custom_tool_call":
			if err := requestValidationString(item["name"], path+".name", true); err != nil {
				return err
			}
			if err := requestValidationOptionalStrings(item, path, "arguments", "input"); err != nil {
				return err
			}
			// 命名空间工具的调用项带 namespace；缺失表示顶层工具。
			if err := requestValidationOptionalStrings(item, path, "namespace"); err != nil {
				return err
			}
		case "function_call_output", "custom_tool_call_output":
			if output, ok := item["output"].([]any); ok {
				if err := requestValidationContent(output, path+".output", true); err != nil {
					return err
				}
			}
			// Other JSON outputs remain compatible: responsesToolOutput
			// serializes maps/numbers/booleans without dropping their value.
		case "reasoning":
			// Optional reasoning/encrypted history is not required to answer
			// the full message history supplied by current Codex clients.
		case "item_reference":
			// 指向服务端保存的内容项：网关无状态，取不到内容，必须让客户端改传完整历史。
			return fmt.Errorf("%s.type %q is not supported; include full message and function/custom tool history", path, kind)
		default:
			// 其它历史项（tool_search_call / web_search_call / mcp_call 等）本网关不会产生，
			// 但客户端切换过上游时可能带上。历史是信息性的，忽略比整条请求 400 好；
			// 与之配对的 output 项同样会被忽略，模型看到的历史保持一致。
		}
	}
	return nil
}

func requestValidationStateOption(value any, path string) error {
	if value == nil {
		return nil
	}
	switch state := value.(type) {
	case string:
		if strings.TrimSpace(state) == "" {
			return nil
		}
	case map[string]any:
		if len(state) == 0 {
			return nil
		}
	default:
		return fmt.Errorf("%s must be a string, object, or null", path)
	}
	return fmt.Errorf("%s is not supported; send complete input/instructions instead of server-managed state", path)
}

// validateResponsesOptions checks capabilities that the compatibility adapter
// cannot silently fulfil. Unknown top-level extension fields are not rejected.
// The supplied request is read-only; parsing/output-schema enforcement stays in
// responsesToChat and its output contract.
func validateResponsesOptions(object map[string]json.RawMessage, req *responsesRequest) error {
	if object == nil || req == nil {
		return fmt.Errorf("request body must be a JSON object")
	}
	fields := make(map[string]any, len(object))
	for key, raw := range object {
		var value any
		if err := jsonutil.Decode(raw, &value); err != nil {
			return fmt.Errorf("%s contains invalid JSON: %w", key, err)
		}
		fields[key] = value
	}
	if err := requestValidationString(fields["model"], "model", true); err != nil {
		return err
	}
	if err := requestValidationOptionalBools(fields, "request", "stream", "parallel_tool_calls", "background", "store"); err != nil {
		return err
	}
	if fields["background"] == true {
		return fmt.Errorf("background=true is not supported; use a foreground request")
	}
	if fields["store"] == true {
		return fmt.Errorf("store=true is not supported; responses are not stored on this gateway")
	}
	if err := requestValidationOptionalStrings(fields, "request", "instructions", "prompt_cache_key", "conversation_id", "conversationId", "previous_response_id", "truncation"); err != nil {
		return err
	}
	if previous, _ := fields["previous_response_id"].(string); strings.TrimSpace(previous) != "" {
		return fmt.Errorf("previous_response_id is not supported; include complete input history")
	}
	for _, key := range []string{"conversation", "prompt"} {
		if err := requestValidationStateOption(fields[key], key); err != nil {
			return err
		}
	}
	// truncation 是"上下文超限时怎么办"的策略提示。网关本身不保存会话，auto 与 disabled
	// 在行为上只差上游报错时机，因此按声明接受、不转发，避免客户端带上默认值时整条 400。
	for _, key := range []string{"metadata", "client_metadata", "reasoning", "text"} {
		if value := fields[key]; value != nil {
			if _, err := requestValidationObject(value, key); err != nil {
				return err
			}
		}
	}
	if reasoning, ok := fields["reasoning"].(map[string]any); ok {
		for _, key := range []string{"effort", "summary"} {
			if value, present := reasoning[key]; present {
				if err := requestValidationString(value, "reasoning."+key, true); err != nil {
					return err
				}
			}
		}
	}
	if text, ok := fields["text"].(map[string]any); ok {
		if err := requestValidationOptionalStrings(text, "text", "verbosity"); err != nil {
			return err
		}
		if rawFormat := text["format"]; rawFormat != nil {
			format, err := requestValidationObject(rawFormat, "text.format")
			if err != nil {
				return err
			}
			if err := requestValidationString(format["type"], "text.format.type", true); err != nil {
				return err
			}
			if err := requestValidationOptionalStrings(format, "text.format", "name", "description"); err != nil {
				return err
			}
			if err := requestValidationOptionalBools(format, "text.format", "strict"); err != nil {
				return err
			}
			if schema, present := format["schema"]; present {
				if _, err := requestValidationObject(schema, "text.format.schema"); err != nil {
					return err
				}
			}
		}
	}
	if include := fields["include"]; include != nil {
		// include paths are optional response expansions, not required server
		// capabilities. In particular, reasoning.encrypted_content is allowed.
		if err := requestValidationStringArray(include, "include"); err != nil {
			return err
		}
	}
	if err := requestValidationOptionalNumbers(fields, "request", true, "max_output_tokens"); err != nil {
		return err
	}
	if err := requestValidationOptionalNumbers(fields, "request", false, "temperature", "top_p"); err != nil {
		return err
	}
	if err := requestValidationTools(fields["tools"], "tools", true); err != nil {
		return err
	}
	if err := requestValidationToolChoice(fields["tool_choice"], "tool_choice", true); err != nil {
		return err
	}
	return requestValidationResponsesInput(fields["input"])
}
