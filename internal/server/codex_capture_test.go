// ═══ 更新日志 ═══
// 2026-09-16：增加 Codex 请求字段及工具文本经过 Responses、CN 适配与缓存键注入的保真回归。
// 2026-09-17：公开版本使用最小合成夹具，移除真实指令/会话/环境，保留调用配对、编号与历史失真边界。
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/upstream"
)

type codexFixtureCall struct {
	name      string
	arguments string
}

func codexFixtureObject(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s must be an object, got %T", label, value)
	}
	return object
}

func codexFixtureDecode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := jsonutil.Decode(raw, &object); err != nil || object == nil {
		t.Fatalf("invalid synthetic fixture or transformed JSON: %v", err)
	}
	return object
}

func codexFixtureHistory(t *testing.T, source map[string]any) (map[string]codexFixtureCall, map[string]string) {
	t.Helper()
	input, ok := source["input"].([]any)
	if !ok {
		t.Fatal("synthetic fixture input must be an array")
	}
	calls := make(map[string]codexFixtureCall)
	outputs := make(map[string]string)
	for _, raw := range input {
		item := codexFixtureObject(t, raw, "input item")
		switch item["type"] {
		case "function_call":
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, ok := item["arguments"].(string)
			if id == "" || name == "" || !ok || !json.Valid([]byte(arguments)) {
				t.Fatalf("invalid function call in synthetic fixture: %q", id)
			}
			if _, duplicate := calls[id]; duplicate {
				t.Fatalf("duplicate fixture function call: %q", id)
			}
			calls[id] = codexFixtureCall{name: name, arguments: arguments}
		case "function_call_output":
			id, _ := item["call_id"].(string)
			output, ok := item["output"].(string)
			if id == "" || !ok {
				t.Fatalf("invalid textual tool output in synthetic fixture: %q", id)
			}
			if _, duplicate := outputs[id]; duplicate {
				t.Fatalf("duplicate fixture tool output: %q", id)
			}
			outputs[id] = output
		}
	}
	return calls, outputs
}

func codexFixtureCheckHistory(t *testing.T, stage string, body map[string]any, wantCalls map[string]codexFixtureCall, wantOutputs map[string]string) {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("%s: transformed messages are missing", stage)
	}
	gotCalls := make(map[string]codexFixtureCall)
	gotOutputs := make(map[string]string)
	for _, raw := range messages {
		message := codexFixtureObject(t, raw, stage+" message")
		if message["role"] == "tool" {
			id, _ := message["tool_call_id"].(string)
			content, ok := message["content"].(string)
			if id == "" || !ok {
				t.Fatalf("%s: tool output changed type or lost its ID", stage)
			}
			if _, duplicate := gotOutputs[id]; duplicate {
				t.Fatalf("%s: duplicate tool result %q", stage, id)
			}
			gotOutputs[id] = content
		}
		if rawCalls, ok := message["tool_calls"].([]any); ok {
			for _, rawCall := range rawCalls {
				call := codexFixtureObject(t, rawCall, stage+" tool call")
				function := codexFixtureObject(t, call["function"], stage+" function")
				id, _ := call["id"].(string)
				name, _ := function["name"].(string)
				arguments, ok := function["arguments"].(string)
				if id == "" || !ok || call["type"] != "function" || !json.Valid([]byte(arguments)) {
					t.Fatalf("%s: function arguments or call identity became invalid: %q", stage, id)
				}
				if _, duplicate := gotCalls[id]; duplicate {
					t.Fatalf("%s: duplicate function call %q", stage, id)
				}
				gotCalls[id] = codexFixtureCall{name: name, arguments: arguments}
			}
		}
	}
	if len(gotCalls) != len(wantCalls) || len(gotOutputs) != len(wantOutputs) {
		t.Errorf("%s: call/result counts changed: got %d/%d, want %d/%d",
			stage, len(gotCalls), len(gotOutputs), len(wantCalls), len(wantOutputs))
	}
	for id, want := range wantCalls {
		if got, present := gotCalls[id]; !present || got != want {
			t.Errorf("%s: fixture function name/arguments changed or disappeared: %q", stage, id)
		}
	}
	for id, want := range wantOutputs {
		got, present := gotOutputs[id]
		if !present || got != want {
			t.Errorf("%s: fixture tool output changed or disappeared: %q; 11128 count got=%d want=%d",
				stage, id, strings.Count(got, "11128"), strings.Count(want, "11128"))
		}
	}
}

func codexFixtureCheckFields(t *testing.T, stage string, source, body map[string]any, model string, hasSchema bool) {
	t.Helper()
	if body["model"] != model {
		t.Errorf("%s: model=%v, want %s", stage, body["model"], model)
	}
	for _, key := range []string{"stream", "parallel_tool_calls", "prompt_cache_key", "tool_choice"} {
		want, present := source[key]
		if !present {
			t.Fatalf("expected synthetic fixture field %s is absent", key)
		}
		if got, retained := body[key]; !retained || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: fixture %s was not preserved: got=%v want=%v", stage, key, got, want)
		}
	}
	reasoning := codexFixtureObject(t, source["reasoning"], "fixture reasoning")
	for sourceKey, targetKey := range map[string]string{"effort": "reasoning_effort", "summary": "reasoning_summary"} {
		if want, present := reasoning[sourceKey]; present {
			if got, retained := body[targetKey]; !retained || !reflect.DeepEqual(got, want) {
				t.Errorf("%s: reasoning.%s was not preserved: got=%v want=%v", stage, sourceKey, got, want)
			}
		}
	}
	// 历史答复里的错误编号也属于原文；适配层不能擅自纠正或丢弃已有对话。
	input, _ := source["input"].([]any)
	messages, _ := body["messages"].([]any)
	for _, raw := range input {
		item := codexFixtureObject(t, raw, "synthetic input item")
		text, _ := item["content"].(string)
		if item["role"] != "assistant" || !strings.Contains(text, "11-128") {
			continue
		}
		found := false
		for _, messageRaw := range messages {
			message := codexFixtureObject(t, messageRaw, stage+" message")
			if message["role"] == "assistant" && message["content"] == text {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: historical assistant text was changed or removed", stage)
		}
	}
	if !hasSchema {
		if _, invented := body["response_format"]; invented {
			t.Errorf("%s: response_format was invented for a fixture without text.format", stage)
		}
		return
	}
	text := codexFixtureObject(t, source["text"], "fixture text")
	format := codexFixtureObject(t, text["format"], "fixture text.format")
	spec := make(map[string]any)
	for key, value := range format {
		if key != "type" {
			spec[key] = value
		}
	}
	want := map[string]any{"type": format["type"], "json_schema": spec}
	if !reflect.DeepEqual(body["response_format"], want) {
		t.Errorf("%s: fixture JSON Schema/name/strict/properties/required/additionalProperties changed", stage)
	}
}

// These public fixtures are newly authored synthetic examples, distilled from
// the conversion boundaries found during the 2026-09-16 isolated Codex tests.
// They are not network captures and do not prove a live client/model run.
// All identifiers and workspace values are artificial. The final example
// deliberately includes a historical "11-128" reply that must remain unchanged.
func TestCodexSyntheticRequestsPreserveFields(t *testing.T) {
	cases := []struct {
		name                string
		sha256              string
		hasSchema           bool
		toolCount           int
		identifierToolCount int
		historicalTextCount int
	}{
		{
			name:      "synthetic-initial.request.json",
			sha256:    "78793a32e79570bd8a3fe9251f6fc63e997caf02136611cbcf195144c30f9e7f",
			hasSchema: true, toolCount: 0, identifierToolCount: 0,
		},
		{
			name:      "synthetic-four-tools.request.json",
			sha256:    "eb10b2446d75f76558c8fb940cefb0e96a1be825e505d2604b39d62074b4dcd2",
			hasSchema: true, toolCount: 4, identifierToolCount: 2,
		},
		{
			name:      "synthetic-fifteen-tools.request.json",
			sha256:    "1e6182087e65e043d572acd1fe394dd5970e4456b326074705ed50ff740c7242",
			hasSchema: false, toolCount: 15, identifierToolCount: 7, historicalTextCount: 1,
		},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "codex", fixture.name))
			if err != nil {
				t.Fatalf("required synthetic fixture is missing: %v", err)
			}
			// 固定公开文件的 LF 版本校验值，允许 Git 在 Windows 检出时转换行尾。
			canonical := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
			if got := fmt.Sprintf("%x", sha256.Sum256(canonical)); got != fixture.sha256 {
				t.Fatalf("synthetic fixture bytes changed: sha256=%s, want %s", got, fixture.sha256)
			}
			source := codexFixtureDecode(t, raw)
			const model = "cn:deepseek-v4.1-flash"
			const bareModel = "deepseek-v4.1-flash"
			if source["model"] != model || source["stream"] != true || source["parallel_tool_calls"] != true {
				t.Fatal("synthetic fixture no longer represents the specified streaming Codex/model request")
			}
			reasoning := codexFixtureObject(t, source["reasoning"], "fixture reasoning")
			if reasoning["effort"] != "low" || reasoning["summary"] != "concise" {
				t.Fatal("synthetic fixture lost its explicit low/concise reasoning request")
			}
			if key, ok := source["prompt_cache_key"].(string); !ok || key == "" {
				t.Fatal("synthetic fixture has no client-supplied prompt_cache_key")
			}
			text, hasText := source["text"]
			if hasText != fixture.hasSchema {
				t.Fatal("fixture schema coverage changed")
			}
			if fixture.hasSchema {
				format := codexFixtureObject(t, codexFixtureObject(t, text, "fixture text")["format"], "fixture format")
				if format["type"] != "json_schema" || format["strict"] != true {
					t.Fatal("the fixture format is not a strict JSON Schema")
				}
			}
			calls, outputs := codexFixtureHistory(t, source)
			identifierOutputs := 0
			for _, output := range outputs {
				if strings.Contains(output, "11128") {
					identifierOutputs++
				}
			}
			if len(calls) != fixture.toolCount || len(outputs) != fixture.toolCount || identifierOutputs != fixture.identifierToolCount {
				t.Fatalf("synthetic history coverage changed: calls=%d outputs=%d identifier_outputs=%d",
					len(calls), len(outputs), identifierOutputs)
			}
			historicalTexts := 0
			for _, rawItem := range source["input"].([]any) {
				item := codexFixtureObject(t, rawItem, "synthetic input item")
				text, _ := item["content"].(string)
				if item["role"] == "assistant" && strings.Contains(text, "11-128") {
					historicalTexts++
				}
			}
			if historicalTexts != fixture.historicalTextCount {
				t.Fatalf("historical text coverage changed: got %d want %d", historicalTexts, fixture.historicalTextCount)
			}
			chatBody, request, err := responsesToChat(raw)
			if err != nil || request == nil {
				t.Fatalf("responsesToChat failed for a synthetic request: %v", err)
			}
			chat := codexFixtureDecode(t, chatBody)
			codexFixtureCheckFields(t, "responsesToChat", source, chat, model, fixture.hasSchema)
			codexFixtureCheckHistory(t, "responsesToChat", chat, calls, outputs)
			bareBody := rewriteModel(chatBody, bareModel)
			bare := codexFixtureDecode(t, bareBody)
			codexFixtureCheckFields(t, "rewriteModel", source, bare, bareModel, fixture.hasSchema)
			codexFixtureCheckHistory(t, "rewriteModel", bare, calls, outputs)

			// Use the current public CN catalogue, without fetching remote metadata
			// or calling Client.ChatStreamContext. This exercises the local adapter.
			supported, defaultEffort := upstream.EffortListing("cn", bareModel, nil, "")
			efforts := map[string][]string{bareModel: supported}
			defaults := map[string]string{bareModel: defaultEffort}
			for _, legacySanitize := range []bool{false, true} {
				t.Run(fmt.Sprintf("cn_legacy_sanitize_%t", legacySanitize), func(t *testing.T) {
					prepared := upstream.PrepareBodyOptWithEffortsAndDefault(bareBody, legacySanitize, efforts, defaults)
					out := codexFixtureDecode(t, prepared)
					codexFixtureCheckFields(t, "CN adapter", source, out, bareModel, fixture.hasSchema)
					codexFixtureCheckHistory(t, "CN adapter", out, calls, outputs)

					// Competing fallback values must not replace the key that was
					// explicitly supplied by the synthetic client request.
					cached := upstream.InjectPromptCacheKey(prepared, "synthetic-test-account", "different-fallback-conversation")
					final := codexFixtureDecode(t, cached)
					codexFixtureCheckFields(t, "CN cache injection", source, final, bareModel, fixture.hasSchema)
					codexFixtureCheckHistory(t, "CN cache injection", final, calls, outputs)
				})
			}
			t.Logf("synthetic fixture: %d paired calls/results, %d outputs containing 11128, strict_schema=%t; CN catalogue default=%s",
				fixture.toolCount, fixture.identifierToolCount, fixture.hasSchema, defaultEffort)
		})
	}
}
