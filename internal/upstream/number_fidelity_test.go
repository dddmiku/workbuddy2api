// ═══ 更新日志 ═══
// 2026-09-16：跨请求重写链验证 schema、工具定义、大整数和负零保真，覆盖实际图片裁剪及 JSON 边界。
package upstream

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/prompt"
)

const numberFidelityFields = `
"model":"deepseek-v4.1-flash",
"stream":false,
"reasoning_effort":"low",
"parallel_tool_calls":false,
"temperature":0.125,
"top_p":0.90,
"max_tokens":4096,
"metadata":{"invoice_id":9007199254740993,"label":" keep whitespace ","unset":null},
"business":{"id":9007199254740993,"negative_id":-9007199254740993,"negative_zero":-0,"decimal":0.10000000000000001,"exponent":1.2300e+04,"enabled":false,"optional":null},
"response_format":{"type":"json_schema","json_schema":{"name":"invoice","strict":true,"schema":{"type":"object","properties":{"id":{"type":"integer","const":9007199254740993}},"required":["id"],"additionalProperties":false}}},
"tools":[{"type":"function","function":{"name":"lookup_invoice","parameters":{"type":"object","properties":{"id":{"type":"integer","const":9007199254740993,"enum":[9007199254740993,9007199254740995]}},"required":["id"],"additionalProperties":false},"strict":true}}],
"tool_choice":"auto"`

const numberFidelityImageBudget = 3000

func numberFidelityBody(responses bool) []byte {
	imageURL := "data:image/png;base64," + strings.Repeat("A", 8192)
	if responses {
		return []byte("{" + numberFidelityFields + `,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":" keep source text\n"},{"type":"input_image","image_url":"` + imageURL + `"}]}]}`)
	}
	return []byte("{" + numberFidelityFields + `,"messages":[{"role":"user","content":[{"type":"text","text":" keep source text\n"},{"type":"image_url","image_url":{"url":"` + imageURL + `"}}]}]}`)
}

func readNumberFidelityBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var obj map[string]any
	if err := decoder.Decode(&obj); err != nil {
		t.Fatalf("invalid request JSON: %v", err)
	}
	return obj
}

func assertNumberFidelityFields(t *testing.T, before, after []byte) map[string]any {
	t.Helper()
	want := readNumberFidelityBody(t, before)
	got := readNumberFidelityBody(t, after)
	for _, key := range []string{
		"model", "metadata", "business", "response_format", "tools", "tool_choice",
		"temperature", "top_p", "max_tokens", "parallel_tool_calls",
	} {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("%s changed\ngot:  %#v\nwant: %#v", key, got[key], want[key])
		}
	}
	return got
}

func numberFidelityTransforms() []struct {
	name   string
	run    func([]byte) []byte
	shrunk bool
} {
	return []struct {
		name   string
		run    func([]byte) []byte
		shrunk bool
	}{
		{"prepare_body", func(body []byte) []byte { return PrepareBodyOpt(body, true) }, false},
		{"cache_key", func(body []byte) []byte {
			return InjectPromptCacheKey(body, "number-test-user", "number-test-conversation")
		}, false},
		{"image_budget", func(body []byte) []byte { return ShrinkOutboundImages(body, numberFidelityImageBudget) }, true},
		{"console_system", ensureConsoleSystem, false},
		{"custom_prompt", func(body []byte) []byte { return prompt.Rewrite(body, "Explicit custom prompt.") }, false},
	}
}

func TestRequestRewriteNumberFidelity(t *testing.T) {
	body := numberFidelityBody(false)
	for _, step := range numberFidelityTransforms() {
		t.Run(step.name, func(t *testing.T) {
			out := step.run(body)
			got := assertNumberFidelityFields(t, body, out)
			if bytes.Equal(body, out) {
				t.Fatal("test did not exercise a request rewrite")
			}
			if step.shrunk && (len(out) > numberFidelityImageBudget || len(collectImageParts(got)) != 0) {
				t.Fatalf("test did not exercise image removal: size=%d", len(out))
			}
		})
	}
}

func TestClientRequestPipelineNumberFidelity(t *testing.T) {
	for _, c := range []struct {
		name          string
		realm         string
		customPrompt  bool
		existingCache bool
	}{
		{"cn_passthrough_generated_cache", "cn", false, false},
		{"cn_custom_existing_cache", "cn", true, true},
		{"global_console_generated_cache", "global", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			original := numberFidelityBody(false)
			body := original
			if c.existingCache {
				body = append([]byte(`{"prompt_cache_key":"client-cache",`), body[1:]...)
			}
			if c.customPrompt {
				body = prompt.Rewrite(body, "Explicit custom prompt.")
			}
			client := New()
			client.SanitizeFingerprints = true
			client.OutboundImageBudgetBytes = numberFidelityImageBudget
			out := client.prepareBody(body, c.realm, "number-test-user", "number-test-conversation")
			if c.realm == "global" {
				out = ensureConsoleSystem(out)
			}
			got := assertNumberFidelityFields(t, original, out)
			if got["stream"] != true {
				t.Error("stream compatibility lost")
			}
			if c.realm == "cn" && got["reasoning_effort"] != "low" {
				t.Errorf("supported explicit effort changed: %v", got["reasoning_effort"])
			}
			key, _ := got["prompt_cache_key"].(string)
			if c.existingCache {
				if key != "client-cache" {
					t.Errorf("explicit cache key changed: %q", key)
				}
			} else if key != buildCacheKey("number-test-user", "number-test-conversation") {
				t.Errorf("generated cache key changed: %q", key)
			}
			if len(out) > numberFidelityImageBudget || len(collectImageParts(got)) != 0 {
				t.Fatalf("pipeline did not exercise image removal: size=%d", len(out))
			}
			userFound := false
			for _, item := range got["messages"].([]any) {
				message := item.(map[string]any)
				if message["role"] != "user" {
					continue
				}
				userFound = true
				parts := message["content"].([]any)
				if parts[0].(map[string]any)["text"] != " keep source text\n" {
					t.Error("non-image business text changed")
				}
			}
			if !userFound {
				t.Error("user message lost")
			}
		})
	}
}

func TestResponsesImageBudgetNumberFidelity(t *testing.T) {
	body := numberFidelityBody(true)
	out := ShrinkOutboundImages(body, numberFidelityImageBudget)
	got := assertNumberFidelityFields(t, body, out)
	if len(out) > numberFidelityImageBudget || len(collectImageParts(got)) != 0 {
		t.Fatalf("Responses image path was not rewritten: size=%d", len(out))
	}
	parts := got["input"].([]any)[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != " keep source text\n" || parts[1].(map[string]any)["type"] != "input_text" {
		t.Fatalf("Responses content adaptation changed: %#v", parts)
	}
}

func TestRequestRewritesRejectTrailingJSON(t *testing.T) {
	for _, step := range numberFidelityTransforms() {
		t.Run(step.name, func(t *testing.T) {
			for _, suffix := range []string{" {}", " null", " false", " trailing", " {"} {
				body := append(numberFidelityBody(false), []byte(suffix)...)
				if out := step.run(body); !bytes.Equal(out, body) {
					t.Errorf("invalid JSON document was partially accepted (suffix %q)", suffix)
				}
			}
		})
	}
}
