// ═══ 更新日志 ═══
// 2026-09-16：映射并校验 Responses 的输出格式，禁用外部 schema 加载，防止格式约束静默丢失。
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
	if text.Verbosity != "" {
		return nil, fmt.Errorf("text.verbosity is not supported by this gateway")
	}
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
		encoded, err := json.Marshal(rawSchema)
		if err != nil {
			return nil, err
		}
		compiler := jsonschema.NewCompiler()
		compiler.LoadURL = func(string) (io.ReadCloser, error) {
			return nil, fmt.Errorf("external schema references are not supported")
		}
		const schemaURL = "https://workbuddy2api.invalid/output.schema.json"
		if err := compiler.AddResource(schemaURL, bytes.NewReader(encoded)); err != nil {
			return nil, fmt.Errorf("invalid output schema: %w", err)
		}
		schema, err := compiler.Compile(schemaURL)
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
