// ═══ 更新日志 ═══
// 2026-09-16：映射并校验 Responses 的输出格式，禁用外部 schema 加载，防止格式约束静默丢失。
// 2026-09-18：text.verbosity 改为接受并忽略：新版 Codex 默认携带，上游没有对应开关，
//
//	把它当错误回 400 会让整个会话不可用。
//
// 2026-09-18：把工具选择和 strict 函数参数纳入本地终态校验，防止不受支持的上游行为伪装契约成功。
// 2026-09-18：裸字符串指名选择同样解析公开长名别名，避免声明已缩短而选择仍指向原名。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"
)

type outputContract struct {
	format     map[string]any
	chatFormat map[string]any
	schema     *jsonschema.Schema
	jsonObject bool
}

type responseToolInvocation struct {
	name      string
	arguments string
}

type responseToolPolicy struct {
	mode    string
	exact   bool
	allowed map[string]bool
	schemas map[string]*jsonschema.Schema
}

func compileResponseSchema(schema map[string]any) (*jsonschema.Schema, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.LoadURL = func(string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("external schema references are not supported")
	}
	const schemaURL = "https://workbuddy2api.invalid/contract.schema.json"
	if err := compiler.AddResource(schemaURL, bytes.NewReader(encoded)); err != nil {
		return nil, err
	}
	return compiler.Compile(schemaURL)
}

// Keep the original Responses declarations intact for response echoes and alias
// lookup. Only the model-facing Chat tool set is narrowed by allowed_tools.
func (req *responsesRequest) prepareToolPolicy(tools []any) ([]any, any, error) {
	policy := &responseToolPolicy{mode: "auto", schemas: map[string]*jsonschema.Schema{}}
	req.toolPolicy = policy
	available := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		available[name] = true
		if strict, _ := function["strict"].(bool); strict {
			parameters, _ := function["parameters"].(map[string]any)
			if parameters == nil {
				parameters = map[string]any{"type": "object", "additionalProperties": false}
			}
			schema, err := compileResponseSchema(parameters)
			if err != nil {
				return nil, nil, fmt.Errorf("tool %q has an invalid or unsupported strict schema: %w", name, err)
			}
			policy.schemas[name] = schema
		}
	}
	resolve := func(reference map[string]any) (string, error) {
		name, _ := reference["name"].(string)
		name = upstreamToolName(req.toolAliasIndex(), namespaceOf(reference), name)
		if !available[name] {
			return "", fmt.Errorf("tool_choice references undeclared or unsupported tool %q", name)
		}
		if (reference["type"] == "custom") != req.customTools[name] {
			return "", fmt.Errorf("tool_choice type does not match declared tool %q", name)
		}
		return name, nil
	}
	var choice any
	if len(req.ToolChoice) > 0 {
		if err := json.Unmarshal(req.ToolChoice, &choice); err != nil {
			return nil, nil, err
		}
	}
	var wire any
	if mode, ok := choice.(string); ok {
		wire = mode
		switch mode {
		case "auto", "none", "required":
			policy.mode = mode
		default:
			// Existing CN clients may use the bare declared function name.
			name := upstreamToolName(req.toolAliasIndex(), "", mode)
			if !available[name] {
				return nil, nil, fmt.Errorf("tool_choice references undeclared tool %q", mode)
			}
			policy.mode, policy.exact = "required", true
			policy.allowed = map[string]bool{name: true}
			wire = name
		}
	} else if object, ok := choice.(map[string]any); ok {
		if object["type"] == "allowed_tools" {
			if mode, ok := object["mode"].(string); ok {
				policy.mode = mode
			}
			policy.allowed = map[string]bool{}
			references, _ := object["tools"].([]any)
			for _, raw := range references {
				reference, _ := raw.(map[string]any)
				name, err := resolve(reference)
				if err != nil {
					return nil, nil, err
				}
				policy.allowed[name] = true
			}
			selected := make([]any, 0, len(policy.allowed))
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				if policy.allowed[chatToolName(tool)] {
					selected = append(selected, raw)
				}
			}
			tools, wire = selected, policy.mode
		} else {
			name, err := resolve(object)
			if err != nil {
				return nil, nil, err
			}
			policy.mode, policy.exact = "required", true
			policy.allowed = map[string]bool{name: true}
			wire = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	if policy.mode == "required" && len(tools) == 0 {
		return nil, nil, fmt.Errorf("tool_choice=required needs at least one supported declared tool")
	}
	return tools, wire, nil
}

func (policy *responseToolPolicy) validate(calls []responseToolInvocation, refusal bool) error {
	if policy == nil || (refusal && len(calls) == 0) {
		return nil
	}
	if policy.mode == "none" && len(calls) > 0 {
		return fmt.Errorf("model returned a tool call despite tool_choice=none")
	}
	if policy.mode == "required" && len(calls) == 0 {
		return fmt.Errorf("model returned no tool call despite a required tool choice")
	}
	if policy.exact && len(calls) != 1 {
		return fmt.Errorf("model must return exactly one call for a named tool choice")
	}
	for _, call := range calls {
		if policy.allowed != nil && !policy.allowed[call.name] {
			return fmt.Errorf("model called tool %q outside tool_choice", call.name)
		}
		if schema := policy.schemas[call.name]; schema != nil {
			decoder := json.NewDecoder(strings.NewReader(call.arguments))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("model returned invalid JSON arguments for strict tool %q", call.name)
			}
			if err := schema.Validate(value); err != nil {
				return fmt.Errorf("model arguments do not match strict tool %q schema", call.name)
			}
		}
	}
	return nil
}

func parseOutputContract(raw json.RawMessage) (*outputContract, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var text struct {
		Format    json.RawMessage `json:"format"`
		Verbosity string          `json:"verbosity"`
	}
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("invalid text options: %w", err)
	}
	// text.verbosity 是"输出详略"的风格提示，上游 chat 协议没有对应字段。保留解析、
	// 不转发也不报错：客户端升级后默认带这个字段，拒绝等于整条请求 400。
	if len(text.Format) == 0 {
		return nil, nil
	}
	if len(text.Format) > 64<<10 {
		return nil, fmt.Errorf("text.format exceeds 64 KiB")
	}
	var format map[string]any
	decoder := json.NewDecoder(bytes.NewReader(text.Format))
	decoder.UseNumber()
	if err := decoder.Decode(&format); err != nil || format == nil {
		return nil, fmt.Errorf("text.format must be an object")
	}
	kind, _ := format["type"].(string)
	switch kind {
	case "text":
		return &outputContract{format: format}, nil
	case "json_object":
		return &outputContract{format: format, chatFormat: map[string]any{"type": "json_object"}, jsonObject: true}, nil
	case "json_schema":
		name, ok := format["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("text.format.name is required for json_schema")
		}
		if strict, present := format["strict"]; present {
			if _, ok := strict.(bool); !ok {
				return nil, fmt.Errorf("text.format.strict must be a boolean")
			}
		}
		rawSchema, ok := format["schema"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("text.format.schema must be an object")
		}
		schema, err := compileResponseSchema(rawSchema)
		if err != nil {
			return nil, fmt.Errorf("invalid output schema: %w", err)
		}
		spec := map[string]any{"name": name, "schema": rawSchema}
		for _, key := range []string{"description", "strict"} {
			if value, ok := format[key]; ok {
				spec[key] = value
			}
		}
		return &outputContract{format: format, chatFormat: map[string]any{"type": "json_schema", "json_schema": spec}, schema: schema}, nil
	default:
		return nil, fmt.Errorf("unsupported text.format.type %q", kind)
	}
}

func (contract *outputContract) instruction() string {
	if contract == nil || contract.chatFormat == nil {
		return ""
	}
	if contract.schema == nil {
		return "For the final assistant answer, return a JSON object without Markdown fences or surrounding text. Tool calls may be used to complete the task before the final answer."
	}
	schema, _ := json.Marshal(contract.format["schema"])
	return "For the final assistant answer, return only a JSON value matching the following JSON Schema, without Markdown fences or surrounding text. Tool calls may be used to complete the task before the final answer. JSON Schema: " + string(schema)
}

func (contract *outputContract) validate(text string) error {
	if contract == nil || contract.chatFormat == nil {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("model output is not valid JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fmt.Errorf("model output contains content after the JSON value")
	}
	if contract.jsonObject {
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("model output must be a JSON object")
		}
	}
	if contract.schema != nil {
		if err := contract.schema.Validate(value); err != nil {
			return fmt.Errorf("model output does not match the requested JSON schema")
		}
	}
	return nil
}

func (req *responsesRequest) applyEcho(obj map[string]any) {
	if req == nil {
		return
	}
	if req.ParallelToolCalls != nil {
		obj["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.Reasoning != nil {
		obj["reasoning"] = req.Reasoning
	}
	if req.output != nil {
		obj["text"] = map[string]any{"format": req.output.format}
	}
	if req.PromptCacheKey != "" {
		obj["prompt_cache_key"] = req.PromptCacheKey
	}
	if req.Tools != nil {
		obj["tools"] = req.Tools
	}
	if len(req.ToolChoice) > 0 {
		obj["tool_choice"] = req.ToolChoice
	}
	if len(req.Metadata) > 0 {
		obj["metadata"] = req.Metadata
	}
	if req.Instructions != "" {
		obj["instructions"] = req.Instructions
	}
	if req.MaxOutputTokens != nil {
		obj["max_output_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		obj["top_p"] = *req.TopP
	}
}
