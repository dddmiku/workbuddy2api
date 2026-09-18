// ═══ 更新日志 ═══
// 2026-09-18：以官方工具选择约束和已接受的内容形态复现白名单失效、严格参数失效及内容丢失。
// 2026-09-18：补充重复工具身份、未知历史类型隔离及严格/非严格模式的正向控制。
// 2026-09-18：验证命名空间分组的说明在扁平化后仍对模型可见。
// 2026-09-18：对照开源转换器验证64字节名字边界和声明/历史/选择/回程的一致映射。
// 2026-09-18：覆盖顶层公开长工具名，保证与命名空间工具使用同一套双向别名规则。
// 2026-09-18：CN兼容的裸字符串指名选择也必须使用相同的长名别名。
package server

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const protocolDiagnosisTools = `[
 {"type":"function","name":"safe","parameters":{"type":"object"}},
 {"type":"function","name":"unsafe","parameters":{"type":"object"}},
 {"type":"namespace","name":"ns","tools":[
  {"type":"function","name":"lookup","parameters":{"type":"object"}},
  {"type":"custom","name":"exec","description":"execute input"}]}]`

func protocolDiagnosisRequest(choice string) string {
	return `{"model":"m","input":"complete the task","stream":true,"tools":` + protocolDiagnosisTools + `,"tool_choice":` + choice + `}`
}

func protocolDiagnosisReply(t *testing.T, request string, message map[string]any, finish string, stream bool) (map[string]any, []string, []map[string]any, error) {
	t.Helper()
	_, req, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	req.Stream = stream
	choice := map[string]any{"index": 0, "finish_reason": finish}
	if !stream {
		choice["message"] = message
		chat := map[string]any{"choices": []any{choice}}
		rw := newResponsesWriter(nil, req)
		err := rw.ValidateCompletion(chat)
		return chatToResponses(chat, req.Model, req), nil, nil, err
	}
	choice["delta"] = message
	frame, err := json.Marshal(map[string]any{"choices": []any{choice}})
	if err != nil {
		t.Fatal(err)
	}
	names, values, err := outputIntegrityStream(t, req, outputIntegritySSE(string(frame)))
	if len(values) == 0 {
		t.Fatal("no response events")
	}
	response, _ := values[len(values)-1]["response"].(map[string]any)
	return response, names, values, err
}

func protocolDiagnosisMessage(names []string, args string) map[string]any {
	message := map[string]any{"role": "assistant", "content": "done"}
	var calls []any
	for index, name := range names {
		calls = append(calls, map[string]any{"index": index, "id": "call_" + name + strings.Repeat("x", index), "type": "function", "function": map[string]any{"name": name, "arguments": args}})
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	return message
}

func TestProtocolDiagnosisAllowedToolsRestrictsAndPreservesMode(t *testing.T) {
	cases := []struct{ selection, upstream string }{
		{`{"type":"function","name":"safe"}`, "safe"},
		{`{"type":"function","namespace":"ns","name":"lookup"}`, "ns__lookup"},
		{`{"type":"custom","namespace":"ns","name":"exec"}`, "ns__exec"},
	}
	for _, mode := range []string{"auto", "required"} {
		for _, test := range cases {
			t.Run(mode+"/"+test.upstream, func(t *testing.T) {
				choice := `{"type":"allowed_tools","mode":"` + mode + `","tools":[` + test.selection + `]}`
				body, req, err := responsesToChat([]byte(protocolDiagnosisRequest(choice)))
				if err != nil {
					t.Fatal(err)
				}
				tools := chatToolsByName(t, body)
				if len(tools) != 1 || tools[test.upstream] == nil {
					t.Fatalf("tool allowlist was widened: got=%v want=%s", tools, test.upstream)
				}
				chat := decodeChat(t, body)
				if chat["tool_choice"] != mode {
					t.Fatalf("mode=%v want=%s", chat["tool_choice"], mode)
				}
				if len(req.Tools) != 3 || string(req.ToolChoice) != choice {
					t.Fatal("client tool declaration or tool_choice was rewritten")
				}
			})
		}
	}
}

func TestProtocolDiagnosisMalformedToolChoiceIsRejected(t *testing.T) {
	for _, choice := range []string{
		`{"type":"allowed_tools","mode":"future","tools":[{"type":"function","name":"safe"}]}`,
		`{"type":"allowed_tools","mode":false,"tools":[{"type":"function","name":"safe"}]}`,
		`{"type":"allowed_tools","mode":"auto","tools":"safe"}`,
		`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"missing"}]}`,
		`{"type":"allowed_tools","mode":"required","tools":[]}`,
		`{"type":"allowed_tools","tools":[{"type":"future","name":"safe"}]}`,
		`{"type":"function","name":"missing"}`,
	} {
		if _, _, err := responsesToChat([]byte(protocolDiagnosisRequest(choice))); err == nil {
			t.Errorf("tool constraint silently accepted: %s", choice)
		}
	}
}

func TestProtocolDiagnosisToolChoiceCannotCompleteWithWrongCalls(t *testing.T) {
	cases := []struct {
		name, choice string
		calls        []string
	}{
		{"none", `"none"`, []string{"safe"}},
		{"required", `"required"`, nil},
		{"forced_wrong", `{"type":"function","name":"safe"}`, []string{"unsafe"}},
		{"forced_multiple", `{"type":"function","name":"safe"}`, []string{"safe", "safe"}},
		{"allowed_wrong", `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"safe"}]}`, []string{"unsafe"}},
		{"allowed_required", `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"safe"}]}`, nil},
	}
	for _, test := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(test.name+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				finish := "stop"
				if len(test.calls) > 0 {
					finish = "tool_calls"
				}
				response, names, values, err := protocolDiagnosisReply(t, protocolDiagnosisRequest(test.choice), protocolDiagnosisMessage(test.calls, "{}"), finish, stream)
				if stream {
					if response["status"] != "failed" {
						t.Fatalf("violated tool_choice completed: status=%v", response["status"])
					}
					for index, event := range names {
						if event == "response.function_call_arguments.done" {
							t.Fatal("invalid tool was marked executable")
						}
						if event == "response.output_item.done" {
							item, _ := values[index]["item"].(map[string]any)
							if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
								t.Fatal("invalid tool item was marked executable")
							}
						}
					}
				} else if err == nil {
					t.Fatal("nonstream tool_choice violation was accepted")
				}
			})
		}
	}
}

func TestProtocolDiagnosisToolChoicePreservesValidOutcomes(t *testing.T) {
	for _, choice := range []string{`"auto"`, `"required"`, `{"type":"function","name":"safe"}`} {
		for _, stream := range []bool{false, true} {
			response, _, _, err := protocolDiagnosisReply(t, protocolDiagnosisRequest(choice), protocolDiagnosisMessage([]string{"safe"}, "{}"), "tool_calls", stream)
			if err != nil || response["status"] != "completed" {
				t.Fatalf("valid call rejected: choice=%s status=%v err=%v", choice, response["status"], err)
			}
			for _, reason := range []string{"length", "content_filter"} {
				response, _, _, err = protocolDiagnosisReply(t, protocolDiagnosisRequest(choice), protocolDiagnosisMessage(nil, ""), reason, stream)
				if err != nil || response["status"] != "incomplete" {
					t.Fatal("explicit incomplete outcome became a tool_choice error")
				}
			}
			response, _, _, err = protocolDiagnosisReply(t, protocolDiagnosisRequest(choice), map[string]any{"refusal": "Cannot fulfill this request."}, "stop", stream)
			if err != nil || response["status"] != "completed" {
				t.Fatal("refusal was overridden by a tool-choice requirement")
			}
		}
	}
}

func TestProtocolDiagnosisStrictFunctionArgumentsAreValidated(t *testing.T) {
	request := `{"model":"m","stream":true,"input":"lookup","tools":[{"type":"function","name":"lookup","strict":true,"parameters":{"type":"object","properties":{"invoice_id":{"type":"integer","enum":[9007199254740993]}},"required":["invoice_id"],"additionalProperties":false}}]}`
	for _, args := range []string{`{}`, `{"invoice_id":"9007199254740993"}`, `{"invoice_id":9007199254740992}`, `{"invoice_id":9007199254740993,"extra":true}`} {
		for _, stream := range []bool{false, true} {
			response, _, _, err := protocolDiagnosisReply(t, request, protocolDiagnosisMessage([]string{"lookup"}, args), "tool_calls", stream)
			if (stream && response["status"] != "failed") || (!stream && err == nil) {
				t.Errorf("strict tool schema ignored: args=%s stream=%t", args, stream)
			}
		}
	}
	for _, stream := range []bool{false, true} {
		response, _, _, err := protocolDiagnosisReply(t, request, protocolDiagnosisMessage([]string{"lookup"}, `{"invoice_id":9007199254740993}`), "tool_calls", stream)
		if err != nil || response["status"] != "completed" {
			t.Fatalf("valid large integer tool argument rejected: %v", err)
		}
	}
}

func TestProtocolDiagnosisAcceptedToolRefusalIsNotDropped(t *testing.T) {
	raw := `{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"refusal","refusal":"Permission denied for this file."}]}]}`
	body, _, err := responsesToChat([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	chat := decodeChat(t, body)
	messages := chat["messages"].([]any)
	if messages[1].(map[string]any)["content"] != "Permission denied for this file." {
		t.Fatal("accepted tool result refusal was converted to empty content")
	}
}

func TestProtocolDiagnosisAdjacentAssistantPartsArePreserved(t *testing.T) {
	image := []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,c2FmZQ=="}}
	for _, contents := range [][]any{{"before", image}, {image, "after"}, {image, image}} {
		raw, _ := json.Marshal(map[string]any{"model": "m", "input": []any{
			map[string]any{"role": "assistant", "content": contents[0]},
			map[string]any{"role": "assistant", "content": contents[1]},
		}})
		body, _, err := responsesToChat(raw)
		if err != nil {
			t.Fatal(err)
		}
		chat := decodeChat(t, body)
		messages := chat["messages"].([]any)
		if len(messages) != 1 {
			t.Fatal("assistant normalization no longer merges adjacent messages")
		}
		parts, _ := messages[0].(map[string]any)["content"].([]any)
		if len(parts) != 2 {
			t.Fatalf("accepted assistant parts were discarded: %s", body)
		}
	}
}

func TestProtocolDiagnosisCustomGrammarRemainsVisible(t *testing.T) {
	request := `{"model":"m","input":"run","tools":[{"type":"custom","name":"date","description":"Use a date","format":{"type":"grammar","syntax":"regex","definition":"[0-9]{4}-[0-9]{2}-[0-9]{2}"}}]}`
	body, req, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	tools := chatToolsByName(t, body)
	description, _ := tools["date"]["description"].(string)
	if !strings.Contains(description, "[0-9]{4}-[0-9]{2}-[0-9]{2}") || !strings.Contains(description, "regex") {
		t.Fatal("the custom grammar is completely absent from the model-facing declaration")
	}
	original := req.Tools[0].(map[string]any)
	if original["description"] != "Use a date" {
		t.Fatal("echoed original custom description was changed")
	}
}

func TestProtocolDiagnosisAllowedToolsDeduplicatesReferences(t *testing.T) {
	choice := `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"safe"},{"type":"function","name":"safe"},{"type":"function","name":"unsafe"}]}`
	body, _, err := responsesToChat([]byte(protocolDiagnosisRequest(choice)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range chatToolsByName(t, body) {
		names = append(names, name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"safe", "unsafe"}) {
		t.Fatalf("allowed selection changed: %v", names)
	}
}

func TestProtocolDiagnosisDuplicateToolIdentityIsRejected(t *testing.T) {
	for _, tools := range []string{
		`[{"type":"function","name":"same","parameters":{}},{"type":"function","name":"same","parameters":{}}]`,
		`[{"type":"function","name":"same","parameters":{}},{"type":"custom","name":"same"}]`,
		`[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"same","parameters":{}},{"type":"custom","name":"same"}]}]`,
	} {
		request := `{"model":"m","input":"run","tools":` + tools + `}`
		if _, _, err := responsesToChat([]byte(request)); err == nil {
			t.Errorf("ambiguous tool identity accepted: %s", tools)
		}
	}
}

func TestProtocolDiagnosisUnknownHistoryTypeIsNotAUserMessage(t *testing.T) {
	request := `{"model":"m","input":[{"role":"user","content":"task"},{"type":"future_status_item","content":[{"type":"output_text","text":"opaque metadata"}]},{"role":"user","content":"continue"}]}`
	body, _, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	chat := decodeChat(t, body)
	messages := chat["messages"].([]any)
	if len(messages) != 2 || strings.Contains(string(body), "opaque metadata") {
		t.Fatal("unsupported history item was promoted into a user message")
	}
}

func TestProtocolDiagnosisBestEffortAndSelectedCustomStillWork(t *testing.T) {
	for _, stream := range []bool{false, true} {
		request := `{"model":"m","input":"run","tools":[{"type":"function","name":"safe","strict":false,"parameters":{"type":"object","required":["missing"]}}]}`
		response, _, _, err := protocolDiagnosisReply(t, request, protocolDiagnosisMessage([]string{"safe"}, "{}"), "tool_calls", stream)
		if err != nil || response["status"] != "completed" {
			t.Fatal("best-effort schema was treated as strict")
		}
		choice := `{"type":"allowed_tools","mode":"required","tools":[{"type":"custom","namespace":"ns","name":"exec"}]}`
		response, _, _, err = protocolDiagnosisReply(t, protocolDiagnosisRequest(choice), protocolDiagnosisMessage([]string{"ns__exec"}, `{"input":"echo safe"}`), "tool_calls", stream)
		if err != nil || response["status"] != "completed" {
			t.Fatalf("selected namespace custom tool failed: %v", err)
		}
		calls := continuationIntegrityCalls(response)
		if len(calls) != 1 || calls[0]["type"] != "custom_tool_call" || calls[0]["name"] != "exec" || calls[0]["namespace"] != "ns" || calls[0]["input"] != "echo safe" {
			t.Fatal("selected custom tool lost its namespace or raw input")
		}
	}
}

func TestProtocolDiagnosisNamespaceDescriptionIsNotDiscarded(t *testing.T) {
	request := `{"model":"m","input":"lookup","tools":[{"type":"namespace","name":"billing","description":"Billing tools use amounts in cents.","tools":[{"type":"function","name":"lookup","description":"Read an invoice","parameters":{}}]}]}`
	body, req, err := responsesToChat([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	tool := chatToolsByName(t, body)["billing__lookup"]
	description, _ := tool["description"].(string)
	if !strings.Contains(description, "Read an invoice") || !strings.Contains(description, "amounts in cents") {
		t.Fatal("namespace usage instructions were lost while flattening its tool")
	}
	group := req.Tools[0].(map[string]any)
	child := group["tools"].([]any)[0].(map[string]any)
	if child["description"] != "Read an invoice" {
		t.Fatal("original child declaration was changed")
	}
}

func TestProtocolDiagnosisLongNamespaceAliasesRoundTrip(t *testing.T) {
	namespace := strings.Repeat("billing", 8)
	name := strings.Repeat("lookup", 9)
	for _, kind := range []string{"function", "custom"} {
		tool := map[string]any{"type": kind, "name": name, "parameters": map[string]any{"type": "object"}}
		request := map[string]any{"model": "m", "stream": true, "input": "run", "tools": []any{map[string]any{"type": "namespace", "name": namespace, "tools": []any{tool}}}, "tool_choice": map[string]any{"type": kind, "namespace": namespace, "name": name}}
		raw, _ := json.Marshal(request)
		body, req, err := responsesToChat(raw)
		if err != nil {
			t.Fatal(err)
		}
		tools := chatToolsByName(t, body)
		alias := ""
		for candidate := range tools {
			alias = candidate
		}
		if len(alias) == 0 || len(alias) > 64 {
			t.Fatalf("namespace expansion generated an invalid Chat name: bytes=%d", len(alias))
		}
		choice := decodeChat(t, body)["tool_choice"].(map[string]any)
		if choice["function"].(map[string]any)["name"] != alias {
			t.Fatal("named tool choice did not use the declaration alias")
		}
		arguments := "{}"
		if kind == "custom" {
			arguments = `{"input":"raw input"}`
		}
		encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": protocolDiagnosisMessage([]string{alias}, arguments), "finish_reason": "tool_calls"}}})
		names, values, err := outputIntegrityStream(t, req, outputIntegritySSE(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		response := outputIntegrityFinal(t, names, values, "response.completed")
		calls := continuationIntegrityCalls(response)
		if len(calls) != 1 || calls[0]["name"] != name || calls[0]["namespace"] != namespace {
			t.Fatal("short alias did not restore the original client identity")
		}
		if upstreamToolName(req.toolAliasIndex(), namespace, name) != alias {
			t.Fatal("replayed history would use a different alias")
		}
	}
}

func TestProtocolDiagnosisLongNamespaceAliasesDoNotCollide(t *testing.T) {
	used := map[string]bool{}
	first := flatToolName(strings.Repeat("n", 60), strings.Repeat("x", 55)+"A", used)
	used[first] = true
	second := flatToolName(strings.Repeat("n", 60), strings.Repeat("x", 55)+"B", used)
	used[second] = true
	third := flatToolName(strings.Repeat("n", 60), strings.Repeat("x", 55)+"A", used)
	if first == second || first == third || second == third || len(first) > 64 || len(second) > 64 || len(third) > 64 {
		t.Fatal("long tool names collide or exceed the Chat byte limit")
	}
}

func TestProtocolDiagnosisLongTopLevelAliasesRoundTrip(t *testing.T) {
	name := strings.Repeat("lookup_", 12)
	for _, kind := range []string{"function", "custom"} {
		tool := map[string]any{"type": kind, "name": name, "parameters": map[string]any{"type": "object"}}
		call := map[string]any{"type": "function_call", "name": name, "call_id": "previous", "arguments": "{}"}
		output := map[string]any{"type": "function_call_output", "call_id": "previous", "output": "done"}
		args := "{}"
		if kind == "custom" {
			call = map[string]any{"type": "custom_tool_call", "name": name, "call_id": "previous", "input": "old input"}
			output["type"] = "custom_tool_call_output"
			args = `{"input":"new input"}`
		}
		request := map[string]any{"model": "m", "stream": true, "input": []any{call, output, map[string]any{"role": "user", "content": "again"}}, "tools": []any{tool}, "tool_choice": map[string]any{"type": kind, "name": name}}
		raw, _ := json.Marshal(request)
		body, req, err := responsesToChat(raw)
		if err != nil {
			t.Fatal(err)
		}
		alias := ""
		for candidate := range chatToolsByName(t, body) {
			alias = candidate
		}
		if len(alias) == 0 || len(alias) > 64 {
			t.Fatalf("top-level name was not bounded: bytes=%d", len(alias))
		}
		chat := decodeChat(t, body)
		previous := chat["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
		choice := chat["tool_choice"].(map[string]any)["function"].(map[string]any)
		if previous["name"] != alias || choice["name"] != alias || req.Tools[0].(map[string]any)["name"] != name {
			t.Fatal("declaration/history/choice/echo identities differ")
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": protocolDiagnosisMessage([]string{alias}, args), "finish_reason": "tool_calls"}}})
		names, values, err := outputIntegrityStream(t, req, outputIntegritySSE(string(frame)))
		if err != nil {
			t.Fatal(err)
		}
		response := outputIntegrityFinal(t, names, values, "response.completed")
		calls := continuationIntegrityCalls(response)
		if len(calls) != 1 || calls[0]["name"] != name || calls[0]["namespace"] != nil {
			t.Fatal("top-level alias was not restored for the client")
		}
	}
}

func TestProtocolDiagnosisLongBareToolChoiceUsesAlias(t *testing.T) {
	name := strings.Repeat("named_tool_", 8)
	function := map[string]any{"name": name, "parameters": map[string]any{"type": "object"}}
	for _, chat := range []bool{false, true} {
		request := map[string]any{"model": "m", "input": "run", "tool_choice": name}
		if chat {
			request["tools"] = []any{map[string]any{"type": "function", "function": function}}
		} else {
			request["tools"] = []any{map[string]any{"type": "function", "name": name, "parameters": function["parameters"]}}
		}
		raw, _ := json.Marshal(request)
		var body []byte
		if chat {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			contract, err := newChatOutputContract(fields)
			if err != nil {
				t.Fatalf("bare Chat choice rejected: %v", err)
			}
			if _, err := normalizeChatToolDeclarations(fields, contract); err != nil {
				t.Fatal(err)
			}
			body, _ = json.Marshal(fields)
		} else {
			var err error
			body, _, err = responsesToChat(raw)
			if err != nil {
				t.Fatalf("bare Responses choice rejected: %v", err)
			}
		}
		value := decodeChat(t, body)
		alias := value["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
		if len(alias) > 64 || value["tool_choice"] != alias {
			t.Fatal("bare choice did not follow the public tool alias")
		}
	}
}
