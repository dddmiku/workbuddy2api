// ═══ 更新日志 ═══
// 2026-09-17：锁定密钥模型白名单：越界模型在选号前被拒，模型列表按绑定过滤。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func boundKeyHandler(t *testing.T, models []string) (*Handler, string, *int) {
	t.Helper()
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		*calls++
		return http.StatusOK, sseOK, true
	})
	handler := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "legacy-key",
		APIKeys:  store,
	})
	_, key, err := store.Create("bound", "", models)
	if err != nil {
		t.Fatal(err)
	}
	return handler, key, calls
}

func TestKeyModelBindingBlocksOtherModels(t *testing.T) {
	handler, key, calls := boundKeyHandler(t, []string{"cn:deepseek-v4.1-flash"})
	invoke := func(model string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+key)
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if recorder := invoke("cn:deepseek-v4.1-flash"); recorder.Code != http.StatusOK {
		t.Fatalf("bound model rejected: %d %s", recorder.Code, recorder.Body)
	}
	if *calls != 1 {
		t.Fatalf("upstream calls=%d want 1", *calls)
	}
	recorder := invoke("cn:glm-5.2")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("unbound model accepted: %d %s", recorder.Code, recorder.Body)
	}
	if !assertJSONErrorCode(t, recorder.Body.String(), "model_not_allowed") {
		t.Fatalf("wrong error envelope: %s", recorder.Body)
	}
	if *calls != 1 {
		t.Fatalf("blocked model reached upstream: calls=%d", *calls)
	}
}

func TestKeyModelBindingBareNameMatchesPrefixedRequest(t *testing.T) {
	handler, key, calls := boundKeyHandler(t, []string{"deepseek-v4.1-flash"})
	for _, model := range []string{"cn:deepseek-v4.1-flash", "deepseek-v4.1-flash"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+key)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("bare binding rejected %q: %d %s", model, recorder.Code, recorder.Body)
		}
	}
	// global 前缀的绑定项不覆盖 cn 请求。
	if *calls != 2 {
		t.Fatalf("upstream calls=%d", *calls)
	}
}

func TestKeyModelBindingFiltersModelList(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", ContextWindow: 131072, MaxTokens: 8192},
		{ID: "glm-5.2", ContextWindow: 131072, MaxTokens: 8192},
	}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	handler, key, _ := boundKeyHandler(t, []string{"deepseek-v4.1-flash"})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status=%d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "deepseek-v4.1-flash") {
		t.Fatalf("bound model missing from list: %s", recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "glm-5.2") {
		t.Fatalf("unbound model leaked into list: %s", recorder.Body)
	}
}

func TestKeyWithoutBindingKeepsFullAccess(t *testing.T) {
	handler, key, _ := boundKeyHandler(t, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unbound key restricted: %d %s", recorder.Code, recorder.Body)
	}
}
