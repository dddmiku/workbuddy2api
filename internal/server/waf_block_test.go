// ═══ 更新日志 ═══
// 2026-09-17：锁定 WAF 拦截页对调用方的可见结果：单次调用、专用错误码、不回显 HTML。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestChatUpstreamWAFBlockPageIsTerminal 上游 WAF 按正文判定拦截，同域换号同样命中：
// handler 必须在首次响应就返回，不对第二个账号重复发送同一份被拦正文，
// 返回专用错误码与可读文案，而不是把 HTML 拦截页当成请求参数错误原样透传。
func TestChatUpstreamWAFBlockPageIsTerminal(t *testing.T) {
	const page = `<!DOCTYPE html><html lang="en"><head><title>WAF Block Page</title></head>` +
		`<body><p class="title">Your request has been interrupted</p></body></html>`
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusForbidden, page, false
	})
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("WAF block rotated: upstream calls=%d want=1", calls)
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
