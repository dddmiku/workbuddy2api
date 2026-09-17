// ═══ 更新日志 ═══
// 2026-09-17：锁定运行约定的注入位置与开关：只在带工具的请求上追加到 system 末尾，
//
//	原 system 内容不改，关闭时不注入。
package server

import (
	"strings"
	"testing"

	"workbuddy2api/internal/prompt"
)

// firstSystem 取首条 system 消息的文本（decodeChat 复用 responses_test.go 里的同名助手）。
func firstSystem(t *testing.T, obj map[string]any) string {
	t.Helper()
	msgs, _ := obj["messages"].([]any)
	for _, item := range msgs {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			content, _ := msg["content"].(string)
			return content
		}
	}
	return ""
}

func TestApplyActNoteAppendsToSystemWhenToolsPresent(t *testing.T) {
	body := []byte(`{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"system","content":"You are Codex."},{"role":"user","content":"hi"}]}`)
	out := applyActNote(body, prompt.ActNote, true)
	obj := decodeChat(t, out)
	system := firstSystem(t, obj)
	if !strings.HasPrefix(system, "You are Codex.") {
		t.Fatalf("原 system 被改写: %q", system)
	}
	if !strings.Contains(system, prompt.ActNote) {
		t.Fatalf("运行约定未追加: %q", system)
	}
	// 用户消息与其余字段一字不动。
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2", len(msgs))
	}
	if user := msgs[1].(map[string]any); user["content"] != "hi" {
		t.Fatalf("用户消息被改动: %v", user["content"])
	}
}

func TestApplyActNoteSkippedWithoutTools(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are Codex."}]}`)
	out := applyActNote(body, prompt.ActNote, false)
	if strings.Contains(string(out), "act instead of narrating") {
		t.Fatalf("无工具请求不应注入运行约定: %s", out)
	}
}

func TestApplyActNoteCreatesSystemWhenMissing(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := applyActNote(body, prompt.ActNote, true)
	obj := decodeChat(t, out)
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2", len(msgs))
	}
	if role, _ := msgs[0].(map[string]any)["role"].(string); role != "system" {
		t.Fatalf("首条应为 system，实际 %q", role)
	}
}

func TestApplyActNoteEmptyNoteIsNoop(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are Codex."}]}`)
	out := applyActNote(body, "", true)
	if string(out) != string(body) {
		t.Fatalf("note 为空时不应改动 body")
	}
}

func TestActNoteForResolvesConfig(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		{"", prompt.ActNote},
		{"off", ""},
		{"none", ""},
		{"false", ""},
		{"  ", prompt.ActNote},
		{"自定义约定", "自定义约定"},
	}
	for _, c := range cases {
		if got := ActNoteFor(c.configured); got != c.want {
			t.Errorf("ActNoteFor(%q)=%q want %q", c.configured, got, c.want)
		}
	}
}
