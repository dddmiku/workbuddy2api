// ═══ 更新日志 ═══
// 2026-09-16：把旧指纹清洗测试改为内容保真契约，覆盖所有角色、工具参数和真实出站边界。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"workbuddy2api/internal/auth"
)

const (
	ccIdentity        = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch          = "Main branch (you will usually use this for PRs)"
	ccHeader          = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
)

func TestPrepareBodyPreservesFormerFingerprintText(t *testing.T) {
	cases := []struct{ name, text string }{
		{"cli_identity", ccIdentity},
		{"desktop_identity", "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."},
		{"branch", ccBranch},
		{"codex_instructions", codexInstructions},
		{"feedback_link", "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},
		{"header_value", ccHeader},
		{"header_case", "X-Anthropic-Billing-Header: cc_version=1.0;"},
		{"bare_header", "引用 `x-anthropic-billing-header` 和 `X-Anthropic-Billing-Header` 键名"},
		{"key_values", "prefix; cc_version=2.0; cc_entrypoint=cli; cc_name=business; suffix"},
		{"invoice_number", "编号 11128，对照 11-128，原样保留二者。"},
		{"code", "result = {'invoice_id': 11128}\nassert result['invoice_id'] == 11128\n"},
		{"whitespace", " \t" + ccHeader + "\r\n11128\t \n"},
		{"ordinary_text", "please use main branch and see https://github.com/anthropics/anthropic-cookbook"},
	}
	for _, c := range cases {
		for _, role := range []string{"system", "developer", "user", "assistant", "tool"} {
			for _, legacySanitize := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/legacy_sanitize=%t", c.name, role, legacySanitize), func(t *testing.T) {
					messages := fidelityMessagesForRole(role, c.text)
					got := prepareFidelityMessages(t, "glm-5.2", legacySanitize, messages)
					if role == "developer" {
						messages[0].(map[string]any)["role"] = "system"
					}
					if !reflect.DeepEqual(got, messages) {
						t.Errorf("business content changed\ngot:  %#v\nwant: %#v", got, messages)
					}
				})
			}
		}
	}
}

func TestPrepareBodyPreservesMultimodalContent(t *testing.T) {
	for _, legacySanitize := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_sanitize=%t", legacySanitize), func(t *testing.T) {
			messages := []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": " \t" + ccIdentity + "\n11128\n"},
				map[string]any{"type": "text", "text": ccHeader},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "https://example.test/images/11128.png", "detail": "original",
				}},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": "QUJD",
				}},
			}}}
			got := prepareFidelityMessages(t, "glm-5.2", legacySanitize, messages)
			if !reflect.DeepEqual(got, messages) {
				t.Errorf("multimodal content changed\ngot:  %#v\nwant: %#v", got, messages)
			}
		})
	}
}

func TestPrepareBodyPreservesToolArgumentStrings(t *testing.T) {
	for _, arguments := range []string{
		`{"command":"echo 11128"}`,
		`{"header":"x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;","value":"keep"}`,
		`{"body":"You are Claude Code, Anthropic's official CLI for Claude.\nMain branch (you will usually use this for PRs)"}`,
	} {
		for _, legacySanitize := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy_sanitize=%t", arguments, legacySanitize), func(t *testing.T) {
				messages := fidelityMessagesForRole("tool", "done")
				assistant := messages[0].(map[string]any)
				fn := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
				fn["arguments"] = arguments
				for _, contentPresent := range []bool{true, false} {
					if contentPresent {
						assistant["content"] = nil
					} else {
						delete(assistant, "content")
					}
					got := prepareFidelityMessages(t, "glm-5.2", legacySanitize, messages)
					if !reflect.DeepEqual(got, messages) {
						t.Errorf("tool arguments changed (content present=%t)\ngot:  %#v\nwant: %#v", contentPresent, got, messages)
					}
				}
			})
		}
	}
}

func TestReasoningBackfillPreservesBusinessText(t *testing.T) {
	for _, legacySanitize := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_sanitize=%t", legacySanitize), func(t *testing.T) {
			copied := " \t11128 " + ccHeader + "\n"
			provided := codexInstructions + "\n" + ccBranch
			messages := []any{
				map[string]any{"role": "assistant", "content": "11128", "reasoning": copied},
				map[string]any{"role": "assistant", "content": ccIdentity, "reasoning_content": provided},
				map[string]any{"role": "assistant", "content": "done"},
			}
			got := prepareFidelityMessages(t, "deepseek-v4.1-flash", legacySanitize, messages)
			messages[0].(map[string]any)["reasoning_content"] = copied
			messages[2].(map[string]any)["reasoning_content"] = ""
			if !reflect.DeepEqual(got, messages) {
				t.Errorf("reasoning backfill altered business text\ngot:  %#v\nwant: %#v", got, messages)
			}
		})
	}
}

func TestChatStreamWireBodyPreservesBusinessContent(t *testing.T) {
	for _, legacySanitize := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_sanitize=%t", legacySanitize), func(t *testing.T) {
			wire := make(chan []byte, 1)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				wire <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer ts.Close()

			c := New()
			c.SanitizeFingerprints = legacySanitize
			c.ChatBaseCN = ts.URL
			acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}
			messages := []any{
				map[string]any{"role": "system", "content": ccIdentity + " " + ccHeader},
				map[string]any{"role": "developer", "content": codexInstructions},
				map[string]any{"role": "user", "content": ccBranch + "\n11128\n"},
			}
			messages = append(messages, fidelityMessagesForRole("tool", capturedCodexInvoiceOutput)...)
			body, err := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash", "messages": messages})
			if err != nil {
				t.Fatal(err)
			}
			rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
			if err != nil {
				t.Fatal(err)
			}
			if rc != nil {
				defer rc.Close()
			}
			if status != http.StatusOK {
				t.Fatalf("upstream status %d: %s", status, respBody)
			}
			var got map[string]any
			if err := json.Unmarshal(<-wire, &got); err != nil {
				t.Fatal(err)
			}
			messages[1].(map[string]any)["role"] = "system"
			if !reflect.DeepEqual(got["messages"], messages) {
				t.Errorf("business content changed on the wire\ngot:  %#v\nwant: %#v", got["messages"], messages)
			}
			if got["stream"] != true {
				t.Error("stream compatibility was lost")
			}
		})
	}
}

func fidelityMessagesForRole(role string, content any) []any {
	message := map[string]any{"role": role, "content": content}
	if role != "tool" {
		return []any{message}
	}
	message["tool_call_id"] = "business_call"
	return []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "business_call", "type": "function", "function": map[string]any{
				"name": "read_document", "arguments": `{"invoice_id":11128}`,
			}},
		}},
		message,
	}
}
