// ═══ 更新日志 ═══
// 2026-09-16：固定真实 Codex 渠道拒绝的诊断，防止误报违规词或换号放大请求。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestChannelRejectionIsExplicitAndDoesNotRotate(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	requests := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		requests++
		return http.StatusBadRequest, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","displayMsg":{"en":"The request was blocked by security policy."}}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","input":"fixture","stream":true}`)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"upstream_channel_rejected"`) {
		t.Fatalf("channel rejection lost: %d %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "content_blocked") || strings.Contains(recorder.Body.String(), "违规词") || requests != 1 {
		t.Fatalf("wrong diagnosis or rotation: requests=%d body=%s", requests, recorder.Body)
	}
	for _, uid := range []string{"u1", "u2"} {
		state, _ := p.Status(uid)
		if state.Cooling || state.ErrTotal != 0 || state.InFlight != 0 {
			t.Fatalf("request error damaged account %s: %+v", uid, state)
		}
	}
}
