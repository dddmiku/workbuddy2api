package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
)

func TestManagedKeysAuthorizeAndRevokeAllPublicRoutes(t *testing.T) {
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "legacy-original-key")
	if err != nil {
		t.Fatal(err)
	}
	info, key, err := store.Create("test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "test", AccessToken: "test", ExpiresAt: 9999999999}), APIKey: "legacy-original-key", APIKeys: store})
	for _, token := range []string{key, "legacy-original-key"} {
		req := httptest.NewRequest("GET", "/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("valid key rejected: %d", rec.Code)
		}
	}
	enabled := false
	if _, err := store.Update(info.ID, nil, nil, &enabled, nil); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{{"GET", "/v1/models"}, {"POST", "/v1/responses"}, {"POST", "/v1/chat/completions"}, {"GET", "/status"}} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("revoked key accepted on %s: %d", route.path, rec.Code)
		}
	}
	if err := store.Delete("legacy"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer legacy-original-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("legacy fallback reopened access")
	}
	rec = httptest.NewRecorder()
	h.InternalHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatal("panel locked out after revoking all keys")
	}
	for _, path := range []string{"/keys", "/keys/legacy"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 404 {
			t.Fatal("key management exposed publicly")
		}
	}
}
