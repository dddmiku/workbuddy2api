// ═══ 更新日志 ═══
// 2026-09-17：锁定热更新端点的边界：只走本机管理通道、未启用时报未启用、
//
//	/healthz 透出当前版本，供部署脚本核对切换是否生效。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/hotupdate"
	"workbuddy2api/internal/version"
)

func updateTestHandler(t *testing.T, manager *hotupdate.Manager) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return http.StatusOK, sseOK, true }),
		APIKey:   "legacy-key",
		Update:   manager,
	})
}

func TestUpdateEndpointsAreInternalOnly(t *testing.T) {
	handler := updateTestHandler(t, hotupdate.NewManager(hotupdate.Options{Enabled: true}))
	for _, item := range []struct{ method, path string }{
		{http.MethodGet, "/update"},
		{http.MethodPost, "/update/check"},
		{http.MethodPost, "/update/apply"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(item.method, item.path, strings.NewReader("{}"))
		request.Header.Set("Authorization", "Bearer legacy-key")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d want 401 (管理通道之外的调用密钥不能碰热更新)", item.method, item.path, recorder.Code)
		}
	}
}

func TestUpdateStatusWithoutManagerReportsDisabled(t *testing.T) {
	handler := updateTestHandler(t, nil)
	recorder := httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/update", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"ok":false`, `"enabled":false`, "update.enabled"} {
		if !strings.Contains(body, want) {
			t.Fatalf("disabled manager payload missing %q: %s", want, body)
		}
	}
}

func TestUpdateStatusCarriesVersionAndHandoverFlags(t *testing.T) {
	original := version.Version
	version.Version = "1.3.0"
	defer func() { version.Version = original }()

	handler := updateTestHandler(t, hotupdate.NewManager(hotupdate.Options{Enabled: true, Dir: t.TempDir()}))
	recorder := httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/update", nil))
	body := recorder.Body.String()
	for _, want := range []string{`"ok":true`, `"current":"1.3.0"`, `"state":"idle"`, `"inherited_fd":false`, hotupdate.DefaultRepo} {
		if !strings.Contains(body, want) {
			t.Fatalf("/update payload missing %q: %s", want, body)
		}
	}
}

// TestUpdateApplyWithoutPriorCheckStartsAnUpdate 未先点「检查更新」时，触发动作要自己去查
// 远端版本（否则管理台的「立即更新」会先回一句莫名其妙的"已经是最新版本"）。
func TestUpdateApplyWithoutPriorCheckStartsAnUpdate(t *testing.T) {
	handler := updateTestHandler(t, hotupdate.NewManager(hotupdate.Options{Enabled: true, Dir: t.TempDir()}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/update/apply", strings.NewReader("{}"))
	handler.InternalHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("apply must be accepted and let the manager query the release: %s", recorder.Body)
	}
}

func TestUpdateApplyRejectsBadTagPayload(t *testing.T) {
	handler := updateTestHandler(t, hotupdate.NewManager(hotupdate.Options{Enabled: true, Dir: t.TempDir()}))
	// 载荷一旦带了内容就必须是合法的小 JSON：否则一次手滑的请求会静默升到最新版。
	cases := []string{strings.Repeat("x", 1<<13), "{not json"}
	names := []string{"oversize", "broken-json"}
	for index, payload := range cases {
		withSubtest(t, names[index], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/update/apply", strings.NewReader(payload))
			handler.InternalHandler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
			}
			if !strings.Contains(recorder.Body.String(), `"ok":false`) {
				t.Fatalf("非法载荷必须被拒绝: %s", recorder.Body)
			}
		})
	}
}

func withSubtest(t *testing.T, name string, fn func(*testing.T)) {
	t.Helper()
	t.Run(name, fn)
}

func TestHealthzReportsVersion(t *testing.T) {
	original := version.Version
	version.Version = "1.3.0"
	version.Commit = "abcdef1234567890"
	defer func() {
		version.Version = original
		version.Commit = ""
	}()

	handler := updateTestHandler(t, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz status=%d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"version":"1.3.0"`, `"commit":"abcdef1234567890"`, ServiceName} {
		if !strings.Contains(body, want) {
			t.Fatalf("healthz payload missing %q: %s", want, body)
		}
	}
}
