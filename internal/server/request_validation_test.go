// ═══ 更新日志 ═══
// 2026-09-16：覆盖入口坏类型与不支持能力，并保留空消息、历史配对兼容、custom 工具及三份真实 Codex 捕获。
// 2026-09-17：公开的 Codex 转换测试改用最小合成夹具，真实捕获不随源码发布。
package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/jsonutil"
)

func requestValidationResponses(raw []byte) error {
	var object map[string]json.RawMessage
	if err := jsonutil.Decode(raw, &object); err != nil {
		return err
	}
	var request responsesRequest
	if err := jsonutil.Decode(raw, &request); err != nil {
		return err
	}
	return validateResponsesOptions(object, &request)
}

func TestValidateChatRequestAcceptsCompatibleBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty_messages", `{"model":"m","messages":[]}`},
		{"empty_text", `{"model":"m","messages":[{"role":"user","content":""}]}`},
		{"developer_role", `{"model":"m","messages":[{"role":"developer","content":"keep me"}]}`},
		{"upstream_role_extension", `{"model":"m","messages":[{"role":"critic","content":"review"}]}`},
		{"nullable_fields", `{"model":"m","messages":[{"role":"assistant","content":null,"reasoning_content":null}],"stream":null,"parallel_tool_calls":null,"tools":null}`},
		{"text_and_image", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"high"}}]}]}`},
		{"upstream_modality_extension", `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}]}]}`},
		{"legacy_text_part", `{"model":"m","messages":[{"role":"user","content":[{"text":"text without an explicit part type"}]}]}`},
		{"function_definition", `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"namespace.lookup","description":"","parameters":{},"strict":false}}],"tool_choice":{"type":"function","function":{"name":"namespace.lookup"}}}`},
		{"legacy_functions", `{"model":"m","messages":[{"role":"function","name":"lookup","content":"result"}],"functions":[{"name":"lookup","parameters":null}],"function_call":{"name":"lookup"}}`},
		{"partial_historical_arguments", `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"orphan","type":"function","function":{"name":"lookup","arguments":"{"}}]}]}`},
		{"unmatched_tool_result", `{"model":"m","messages":[{"role":"tool","tool_call_id":"unmatched","content":"keep for the existing pairing adapter"}]}`},
		{"historical_pairing_not_enforced", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"call_a","function":{"name":"lookup","arguments":""}}]},{"role":"user","content":"interrupt"},{"role":"tool","tool_call_id":"call_b","content":"result"}]}`},
		{"legacy_history_without_id", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"function":{"name":"lookup","arguments":""}}]}]}`},
		{"old_function_call_history", `{"model":"m","messages":[{"role":"assistant","function_call":{"name":"lookup","arguments":"{"}}]}`},
		{"empty_call_list", `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[]}]}`},
		{"basic_scalar_options", `{"model":"m","messages":[],"stream":true,"parallel_tool_calls":false,"temperature":0.2,"top_p":1,"max_tokens":0,"unknown_extension":{"allowed":true}}`},
		{"large_schema_integer", `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"id":{"enum":[9007199254740993]}}}}}]}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(test.body)
			before := append([]byte(nil), raw...)
			if err := validateChatRequest(raw); err != nil {
				t.Fatalf("compatible request rejected: %v", err)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("validation mutated the caller's request bytes")
			}
		})
	}
}

func TestValidateChatRequestRejectsBadStructure(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"invalid_json", `{`, "JSON"},
		{"null_root", `null`, "JSON object"},
		{"array_root", `[]`, "JSON object"},
		{"trailing_document", `{"model":"m","messages":[]} {}`, "JSON"},
		{"missing_model", `{"messages":[]}`, "model"},
		{"null_model", `{"model":null,"messages":[]}`, "model"},
		{"numeric_model", `{"model":1,"messages":[]}`, "model"},
		{"blank_model", `{"model":" \t","messages":[]}`, "model"},
		{"missing_messages", `{"model":"m"}`, "messages"},
		{"null_messages", `{"model":"m","messages":null}`, "messages"},
		{"object_messages", `{"model":"m","messages":{}}`, "messages"},
		{"string_message", `{"model":"m","messages":["hello"]}`, "messages[0]"},
		{"null_message", `{"model":"m","messages":[null]}`, "messages[0]"},
		{"missing_role", `{"model":"m","messages":[{"content":"hi"}]}`, "role"},
		{"bad_role_type", `{"model":"m","messages":[{"role":9,"content":"hi"}]}`, "role"},
		{"bad_content_type", `{"model":"m","messages":[{"role":"user","content":9}]}`, "content"},
		{"bad_content_object", `{"model":"m","messages":[{"role":"user","content":{}}]}`, "content"},
		{"bad_part", `{"model":"m","messages":[{"role":"user","content":[1]}]}`, "content[0]"},
		{"bad_part_type", `{"model":"m","messages":[{"role":"user","content":[{"type":1,"text":"hi"}]}]}`, "type"},
		{"bad_text_type", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":5}]}]}`, "text"},
		{"bad_image_url", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":false}}]}]}`, "image_url"},
		{"bad_message_name", `{"model":"m","messages":[{"role":"assistant","name":false}]}`, "name"},
		{"bad_tool_call_id", `{"model":"m","messages":[{"role":"tool","tool_call_id":2,"content":"x"}]}`, "tool_call_id"},
		{"object_calls", `{"model":"m","messages":[{"role":"assistant","tool_calls":{}}]}`, "tool_calls"},
		{"null_call", `{"model":"m","messages":[{"role":"assistant","tool_calls":[null]}]}`, "tool_calls[0]"},
		{"missing_function", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function"}]}]}`, "function"},
		{"bad_function_name", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"function":{"name":2,"arguments":""}}]}]}`, "name"},
		{"object_arguments", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"function":{"name":"f","arguments":{}}}]}]}`, "arguments"},
		{"bad_calls_type", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"type":"unknown","function":{"name":"f"}}]}]}`, "type"},
		{"object_tools", `{"model":"m","messages":[],"tools":{}}`, "tools"},
		{"null_tool_entry", `{"model":"m","messages":[],"tools":[null]}`, "tools[0]"},
		{"unknown_chat_tool", `{"model":"m","messages":[],"tools":[{"type":"web_search"}]}`, "not supported"},
		{"missing_chat_function", `{"model":"m","messages":[],"tools":[{"type":"function","name":"f"}]}`, "function"},
		{"bad_parameters", `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":[]}}]}`, "parameters"},
		{"bad_description", `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","description":3}}]}`, "description"},
		{"bad_strict", `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","strict":"true"}}]}`, "strict"},
		{"bad_stream", `{"model":"m","messages":[],"stream":"true"}`, "stream"},
		{"bad_temperature", `{"model":"m","messages":[],"temperature":"0.2"}`, "temperature"},
		{"negative_tokens", `{"model":"m","messages":[],"max_tokens":-1}`, "max_tokens"},
		{"fractional_tokens", `{"model":"m","messages":[],"max_tokens":1.5}`, "max_tokens"},
		{"bad_tool_choice", `{"model":"m","messages":[],"tool_choice":3}`, "tool_choice"},
		{"bad_choice_name", `{"model":"m","messages":[],"tool_choice":{"type":"function","function":{"name":3}}}`, "name"},
		{"bad_legacy_functions", `{"model":"m","messages":[],"functions":[{"name":false}]}`, "name"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := validateChatRequest([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want field/capability %q", err, test.want)
			}
		})
	}
}

func TestValidateResponsesOptionsAcceptsCompatibleBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty_input", `{"model":"m","input":[]}`},
		{"null_input", `{"model":"m","input":null}`},
		{"instructions_only", `{"model":"m","instructions":"hello"}`},
		{"codex_optional_metadata", `{"model":"m","input":"hi","include":["reasoning.encrypted_content"],"client_metadata":{"session_id":"s","arbitrary":true},"unknown_extension":{"future":1}}`},
		{"stateless_options", `{"model":"m","input":"hi","background":false,"store":false,"conversation":null,"prompt":{},"truncation":"disabled"}`},
		{"empty_state_markers", `{"model":"m","input":"hi","conversation":"","prompt":null,"previous_response_id":"","conversation_id":"client-conv","conversationId":"client-camel","prompt_cache_key":"client-cache"}`},
		{"function_and_custom", `{"model":"m","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{},"strict":false},{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}],"tool_choice":{"type":"custom","name":"apply_patch"}}`},
		{"declared_builtin_tools", `{"model":"m","input":"hi","tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{}}]}`},
		{"nested_chat_function", `{"model":"m","input":"hi","tools":[{"type":"function","function":{"name":"lookup","parameters":null}}]}`},
		{"partial_function_history", `{"model":"m","input":[{"type":"function_call","call_id":"orphan","name":"lookup","arguments":"{"},{"role":"user","content":"interrupt"},{"type":"function_call_output","call_id":"unmatched","output":{"invoice_id":11128}}]}`},
		{"custom_history", `{"model":"m","input":[{"type":"custom_tool_call","name":"apply_patch","call_id":"c","input":""},{"type":"custom_tool_call_output","call_id":"c","output":"done"}]}`},
		{"tool_image_parts", `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"high"},{"type":"input_text","text":"loaded"}]}]}`},
		{"reasoning_history", `{"model":"m","input":[{"type":"reasoning","id":"r","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"optional"}],"reasoning":{"effort":"low","summary":"concise"}}`},
		{"strict_output_schema", `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object","properties":{"id":{"type":"integer"}}}}}}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := requestValidationResponses([]byte(test.body)); err != nil {
				t.Fatalf("compatible options rejected: %v", err)
			}
		})
	}
}

func TestValidateResponsesOptionsRejectsUnsupportedOrMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"background_work", `{"model":"m","input":"hi","background":true}`, "background=true"},
		{"background_wrong_type", `{"model":"m","input":"hi","background":"true"}`, "background"},
		{"stored_response", `{"model":"m","input":"hi","store":true}`, "store=true"},
		{"previous_response", `{"model":"m","input":"hi","previous_response_id":"resp_old"}`, "previous_response_id"},
		{"conversation_id_reference", `{"model":"m","input":"hi","conversation":"conv_old"}`, "conversation"},
		{"conversation_object_reference", `{"model":"m","input":"hi","conversation":{"id":"conv_old"}}`, "conversation"},
		{"conversation_wrong_type", `{"model":"m","input":"hi","conversation":false}`, "conversation"},
		{"prompt_template", `{"model":"m","input":"hi","prompt":{"id":"pmpt_old","variables":{"x":"y"}}}`, "prompt"},
		{"prompt_string_reference", `{"model":"m","input":"hi","prompt":"pmpt_old"}`, "prompt"},
		{"prompt_wrong_type", `{"model":"m","input":"hi","prompt":[]}`, "prompt"},
		{"automatic_truncation", `{"model":"m","input":"hi","truncation":"auto"}`, "truncation"},
		{"empty_truncation", `{"model":"m","input":"hi","truncation":""}`, "truncation"},
		{"numeric_truncation", `{"model":"m","input":"hi","truncation":1}`, "truncation"},
		{"unimplemented_without_declaration", `{"model":"m","input":"hi","tools":[{"type":"file_search"}]}`, "not supported"},
		{"mcp_tool", `{"model":"m","input":"hi","tools":[{"type":"mcp","server_url":"https://example.invalid"}]}`, "not supported"},
		{"unknown_tool_with_name", `{"model":"m","input":"hi","tools":[{"type":"future_builtin","name":"misleading"}]}`, "not supported"},
		{"missing_tool_type", `{"model":"m","input":"hi","tools":[{"name":"lookup"}]}`, "type"},
		{"null_tool", `{"model":"m","input":"hi","tools":[null]}`, "tools[0]"},
		{"missing_function_name", `{"model":"m","input":"hi","tools":[{"type":"function","parameters":{}}]}`, "name"},
		{"blank_custom_name", `{"model":"m","input":"hi","tools":[{"type":"custom","name":" "}]}`, "name"},
		{"bad_parameter_schema", `{"model":"m","input":"hi","tools":[{"type":"function","name":"lookup","parameters":"not-a-schema"}]}`, "parameters"},
		{"bad_tool_strict", `{"model":"m","input":"hi","tools":[{"type":"function","name":"lookup","strict":1}]}`, "strict"},
		{"bad_custom_format", `{"model":"m","input":"hi","tools":[{"type":"custom","name":"patch","format":{"definition":42}}]}`, "definition"},
		{"builtin_tool_choice", `{"model":"m","input":"hi","tool_choice":{"type":"web_search","name":"fake-function"}}`, "tool_choice"},
		{"unsupported_allowed_tools", `{"model":"m","input":"hi","tool_choice":{"type":"allowed_tools","tools":[]}}`, "tool_choice"},
		{"nested_choice_cannot_silently_become_auto", `{"model":"m","input":"hi","tool_choice":{"type":"function","function":{"name":"lookup"}}}`, "tool_choice.name"},
		{"bad_metadata", `{"model":"m","input":"hi","client_metadata":[]}`, "client_metadata"},
		{"bad_include_type", `{"model":"m","input":"hi","include":"reasoning.encrypted_content"}`, "include"},
		{"bad_include_entry", `{"model":"m","input":"hi","include":[{"path":"reasoning.encrypted_content"}]}`, "include[0]"},
		{"bad_reasoning_value", `{"model":"m","input":"hi","reasoning":{"effort":false}}`, "reasoning.effort"},
		{"bad_schema_type", `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"a","schema":[]}}}`, "text.format.schema"},
		{"bad_schema_name_type", `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":42,"schema":{}}}}`, "text.format.name"},
		{"bad_schema_strict_type", `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"a","strict":1,"schema":{}}}}`, "text.format.strict"},
		{"bad_input_item", `{"model":"m","input":[42]}`, "input[0]"},
		{"server_item_reference", `{"model":"m","input":[{"type":"item_reference","id":"msg_old"}]}`, "not supported"},
		{"bad_function_arguments_type", `{"model":"m","input":[{"type":"function_call","name":"lookup","arguments":{}}]}`, "arguments"},
		{"bad_custom_input_type", `{"model":"m","input":[{"type":"custom_tool_call","name":"patch","input":{}}]}`, "input"},
		{"unsupported_response_part", `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_old"}]}]}`, "not supported"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := requestValidationResponses([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want field/capability %q", err, test.want)
			}
		})
	}
}

func TestValidateResponsesOptionsNilInputs(t *testing.T) {
	if err := validateResponsesOptions(nil, &responsesRequest{}); err == nil {
		t.Fatal("nil object accepted")
	}
	if err := validateResponsesOptions(map[string]json.RawMessage{"model": json.RawMessage(`"m"`)}, nil); err == nil {
		t.Fatal("nil parsed request accepted")
	}
}

func TestValidateResponsesOptionsKeepsFunctionAndCustomBridge(t *testing.T) {
	raw := []byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{}},{"type":"custom","name":"apply_patch"}]}`)
	if err := requestValidationResponses(raw); err != nil {
		t.Fatal(err)
	}
	body, request, err := responsesToChat(raw)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := jsonutil.Decode(body, &chat); err != nil {
		t.Fatal(err)
	}
	tools, ok := chat["tools"].([]any)
	if !ok || len(tools) != 2 || !request.customTools["apply_patch"] {
		t.Fatal("valid function/custom tools did not retain the existing bridge")
	}
	if err := validateChatRequest(body); err != nil {
		t.Fatalf("valid bridged chat request rejected: %v", err)
	}
}

// These are synthetic protocol examples, not evidence of live Codex requests.
func TestValidateRequestsAcceptSyntheticCodexFixtures(t *testing.T) {
	for _, name := range []string{
		"synthetic-initial.request.json",
		"synthetic-four-tools.request.json",
		"synthetic-fifteen-tools.request.json",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "codex", name))
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			var request responsesRequest
			if err := jsonutil.Decode(raw, &object); err != nil {
				t.Fatal(err)
			}
			if err := jsonutil.Decode(raw, &request); err != nil {
				t.Fatal(err)
			}
			beforeObject, _ := json.Marshal(object)
			beforeRequest, _ := json.Marshal(request)
			if err := validateResponsesOptions(object, &request); err != nil {
				t.Fatalf("synthetic Codex options rejected: %v", err)
			}
			afterObject, _ := json.Marshal(object)
			afterRequest, _ := json.Marshal(request)
			if !bytes.Equal(beforeObject, afterObject) || !bytes.Equal(beforeRequest, afterRequest) {
				t.Fatal("option validation mutated the synthetic fixture or parsed request")
			}
			chatBody, _, err := responsesToChat(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateChatRequest(chatBody); err != nil {
				t.Fatalf("translated synthetic Codex request rejected: %v", err)
			}
		})
	}
}
