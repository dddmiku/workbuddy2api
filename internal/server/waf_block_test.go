// ═══ 更新日志 ═══
// 2026-09-17：WAF 拦截改为「同号断词重试」后，测试相应更新为锁定不换号与最终文案。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestChatUpstreamWAFBlockPageIsTerminal 上游 WAF 按正文判定拦截，同域换号同样命中。
// 期望：同一账号上做有限次断词重试（正文逐次改写），绝不换号；全部失败后
// 返回专用错误码与可读文案，而不是把 HTML 拦截页当成请求参数错误原样透传。
func TestChatUpstreamWAFBlockPageIsTerminal(t *testing.T) {
	const page = `<!DOCTYPE html><html lang="en"><head><title>WAF Block Page</title></head>` +
		`<body><p class="title">Your request has been interrupted</p></body></html>`
	var (
		auths  []string
		bodies []string
	)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			auths = append(auths, r.Header.Get("Authorization"))
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(raw))
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(page)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: up,
	})
	payload, err := json.Marshal(map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "user", "content": "please review the <script> snippet"},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(payload))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	// 不换号：所有出站请求用同一账号凭据。
	if len(auths) < 2 {
		t.Fatalf("expected retries on the same account, got %d call(s)", len(auths))
	}
	for i, got := range auths {
		if got != auths[0] {
			t.Fatalf("account rotated on retry %d: %q != %q", i, got, auths[0])
		}
	}
	// 有限次重试：初始 1 次 + 最多 3 次断词重发。
	if len(auths) > 4 {
		t.Fatalf("too many retries: %d", len(auths))
	}
	// 后续重试的正文必须比上一次多出零宽断词符。
	if !strings.Contains(bodies[len(bodies)-1], "\u200b") {
		t.Fatalf("retry body was not neutralised: %s", bodies[len(bodies)-1])
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "upstream_waf_blocked" {
		t.Errorf("error code=%q want upstream_waf_blocked", e.Error.Code)
	}
	if strings.Contains(e.Error.Message, "<!DOCTYPE") || strings.Contains(e.Error.Message, "<title>") {
		t.Errorf("message must not echo the WAF HTML page: %q", e.Error.Message)
	}
	if strings.Contains(e.Error.Message, "upstream rejected request params") {
		t.Errorf("WAF page must not be reported as a params error: %q", e.Error.Message)
	}
	if !strings.Contains(e.Error.Message, "WAF") {
		t.Errorf("message should name the upstream WAF: %q", e.Error.Message)
	}
}
