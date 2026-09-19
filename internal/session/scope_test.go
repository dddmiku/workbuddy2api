// ═══ 更新日志 ═══
// 2026-09-19：绑定键分域保持重启稳定、长度有界及旧单密钥模式兼容。
package session

import "testing"

func TestScopeKey(t *testing.T) {
	a := ScopeKey("caller-a", "shared-conversation")
	if len(a) != 32 || a != ScopeKey("caller-a", "shared-conversation") {
		t.Fatal("scoped key must be stable and bounded")
	}
	if a == ScopeKey("caller-b", "shared-conversation") {
		t.Fatal("callers must be isolated")
	}
	if ScopeKey("ab", "c") == ScopeKey("a", "bc") {
		t.Fatal("caller/key framing is ambiguous")
	}
	if ScopeKey("caller-a", "") != "" || ScopeKey("", "legacy") != "legacy" {
		t.Fatal("missing and legacy identities must be preserved")
	}
}
