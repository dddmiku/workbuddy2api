// ═══ 更新日志 ═══
// 2026-09-17：锁定按调用密钥记账、请求行带 key= 列，以及 /usage 只走本机管理通道。
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
	"workbuddy2api/internal/usage"
)

// usageLedgerHandler 构造带密钥库 + 用量账本的 handler，返回 handler、新密钥、账本。
func usageLedgerHandler(t *testing.T) (*Handler, string, *usage.Store) {
	t.Helper()
	dir := t.TempDir()
	keyStore, err := apikeys.Open(filepath.Join(dir, "keys.json"), "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := usage.Open(filepath.Join(dir, "usage.json"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})
	handler := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "legacy-key",
		APIKeys:  keyStore,
		Usage:    ledger,
	})
	entry, key, err := keyStore.Create("团队 A", "用量测试", nil)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID == "" {
		t.Fatal("created key has no id")
	}
	return handler, key, ledger
}

func TestUsageCountsPerKeyFromUpstreamUsage(t *testing.T) {
	handler, key, ledger := usageLedgerHandler(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", recorder.Code, recorder.Body)
	}

	snapshot := ledger.Snapshot()
	if snapshot.Totals.Requests != 1 || snapshot.Totals.TotalTokens != 2 {
		t.Fatalf("totals = %+v want 1 request / 2 tokens (sseOK usage)", snapshot.Totals)
	}
	if len(snapshot.Keys) != 1 {
		t.Fatalf("keys = %d want 1", len(snapshot.Keys))
	}
	entry := snapshot.Keys[0]
	if entry.Name != "团队 A" || entry.MaskedKey == "" {
		t.Fatalf("key identity not recorded: %+v", entry)
	}
	if entry.Totals.PromptTokens != 1 || entry.Totals.CompletionTokens != 1 {
		t.Fatalf("prompt/completion split lost: %+v", entry.Totals)
	}
	if len(entry.Models) != 1 || entry.Models[0].Model != "cn:deepseek-v4.1-flash" {
		t.Fatalf("per-model breakdown missing: %+v", entry.Models)
	}
}

func TestUsageEndpointIsInternalOnly(t *testing.T) {
	handler, key, ledger := usageLedgerHandler(t)
	ledger.Record("key_a", "团队 A", "wb2a_ab…cd", "cn:deepseek-v4.1-flash", 10, 5, 0, false, time.Now())

	// 公开端口：即使带合法调用密钥也不放行全量用量。
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/usage", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("public /usage status=%d want 401 body=%s", recorder.Code, recorder.Body)
	}

	// 本机管理通道：InternalHandler 注入内部上下文后放行。
	internal := httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(internal, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if internal.Code != http.StatusOK {
		t.Fatalf("internal /usage status=%d body=%s", internal.Code, internal.Body)
	}
	body := internal.Body.String()
	for _, want := range []string{`"ok":true`, `"keys"`, `key_a`, `cn:deepseek-v4.1-flash`, `"totals"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("/usage payload missing %q:\n%s", want, body)
		}
	}
}

func TestUsageEndpointWithoutLedgerReportsDisabled(t *testing.T) {
	handler := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return http.StatusOK, sseOK, true }),
		APIKey:   "legacy-key",
	})
	recorder := httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"ok":false`) {
		t.Fatalf("disabled ledger should report ok=false: %s", recorder.Body)
	}
}

func TestChatStatKeyLabelFallsBackToMask(t *testing.T) {
	stat := &chatStat{keyName: "团队 A", keyMask: "wb2a_ab…cd"}
	if got := stat.keyLabel(); got != "团队 A" {
		t.Fatalf("label=%q want name", got)
	}
	stat.keyName = ""
	if got := stat.keyLabel(); got != "wb2a_ab…cd" {
		t.Fatalf("label=%q want masked key", got)
	}
	stat.keyMask = ""
	if got := stat.keyLabel(); got != "-" {
		t.Fatalf("label=%q want dash", got)
	}
}
