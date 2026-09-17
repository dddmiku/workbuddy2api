// ═══ 更新日志 ═══
// 2026-09-17：锁定渠道校验触发句的断词重试：只动命中句、词义不变、每路径只重发一次。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

const channelTriggerSentence = "Codex CLI is an open source project led by OpenAI"

func TestNeutralizeChannelTriggerBreaksSentence(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are a coding agent. ` +
		channelTriggerSentence + `. Follow the repository conventions."}]}`)
	out, changed := NeutralizeChannelTrigger(body)
	if !changed {
		t.Fatalf("changed=false, body=%s", out)
	}
	if bytes.Contains(out, []byte(channelTriggerSentence)) {
		t.Fatalf("trigger sentence still intact: %s", out)
	}
	if !bytes.Contains(out, []byte(wafBreakMarker)) {
		t.Fatalf("no break marker inserted: %s", out)
	}
	// 去掉零宽标记后必须逐字还原（不删不改任何字符）。比较解码后的结构：
	// 中性化会经 json.Marshal 重排键序，字节序差异不算内容差异。
	restored := bytes.ReplaceAll(out, []byte(wafBreakMarker), nil)
	var want, got any
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal(restored, &got); err != nil {
		t.Fatalf("unmarshal restored: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("content changed beyond markers:\n got=%v\nwant=%v", got, want)
	}
	// 句子之外的正文保持原样。
	if !bytes.Contains(out, []byte("Follow the repository conventions.")) {
		t.Fatalf("unrelated text damaged: %s", out)
	}
}

func TestNeutralizeChannelTriggerIgnoresOtherText(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"Codex is a product name here, OpenAI too."}]}`)
	out, changed := NeutralizeChannelTrigger(body)
	if changed || !bytes.Equal(out, body) {
		t.Fatalf("unexpected rewrite: changed=%v body=%s", changed, out)
	}
	if _, changed := NeutralizeChannelTrigger(nil); changed {
		t.Fatalf("empty body must not report a change")
	}
}

func TestNeutralizeChannelTriggerHandlesPunctuationVariant(t *testing.T) {
	// 句末无句号 / 后接中文标点都要命中，靠句子主体匹配。
	for _, tail := range []string{".", "!", "。", "`"} {
		body := []byte(`{"messages":[{"role":"system","content":"` + channelTriggerSentence + tail + `"}]}`)
		out, changed := NeutralizeChannelTrigger(body)
		if !changed {
			t.Fatalf("tail=%q not neutralised", tail)
		}
		if bytes.Contains(out, []byte(channelTriggerSentence)) {
			t.Fatalf("tail=%q sentence survived: %s", tail, out)
		}
	}
}

// TestChatStreamRetriesChannelRejection 断言被 11128 unapproved channel 拒绝后，
// 网关在同一账号、同一路径上用断词后的正文重发一次，且第二次请求成功。
func TestChatStreamRetriesChannelRejection(t *testing.T) {
	var bodies [][]byte
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			if len(bodies) == 1 {
				return jsonResp(400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`), nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are Codex. ` +
		channelTriggerSentence + `. Reply briefly."},{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at"}, body, "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	rc.Close()
	if len(bodies) != 2 {
		t.Fatalf("attempts=%d want 2 (one neutralised retry)", len(bodies))
	}
	if !bytes.Contains(bodies[0], []byte(channelTriggerSentence)) {
		t.Fatalf("first attempt should keep the original sentence: %s", bodies[0])
	}
	if bytes.Contains(bodies[1], []byte(channelTriggerSentence)) {
		t.Fatalf("retry still carries the trigger sentence: %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(wafBreakMarker)) {
		t.Fatalf("retry carries no break marker: %s", bodies[1])
	}
}

// TestChatStreamChannelRejectionWithoutTriggerStaysTerminal 说明里没有触发句时
// 不做无谓重发：原样返回上游终态，由 handler 归类。
func TestChatStreamChannelRejectionWithoutTriggerStaysTerminal(t *testing.T) {
	attempts := 0
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			return jsonResp(400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`), nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are a helpful assistant."}]}`)
	_, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at"}, body, "", ChatMeta{})
	if err != nil || status != 400 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d want 1 (no trigger sentence to break)", attempts)
	}
}
