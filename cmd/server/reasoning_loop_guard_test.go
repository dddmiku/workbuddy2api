// ═══ 更新日志 ═══
// 2026-09-19：确认旧配置默认开启重复推理保护、明确false可关闭，非法值不被静默接受。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReasoningLoopGuardConfiguration(t *testing.T) {
	if !Default().Features.ReasoningLoopGuard {
		t.Fatal("default reasoning loop guard is disabled")
	}
	for _, tc := range []struct {
		name, body string
		want       bool
		invalid    bool
	}{
		{name: "old_config", body: `{}`, want: true},
		{name: "old_features", body: `{"features":{"sanitize_blacklist_fingerprints":false}}`, want: true},
		{name: "old_null_features", body: `{"features":null}`, want: true},
		{name: "enabled", body: `{"features":{"reasoning_loop_guard":true}}`, want: true},
		{name: "disabled", body: `{"features":{"reasoning_loop_guard":false}}`},
		{name: "invalid_string", body: `{"features":{"reasoning_loop_guard":"false"}}`, invalid: true},
		{name: "invalid_number", body: `{"features":{"reasoning_loop_guard":0}}`, invalid: true},
		{name: "invalid_null", body: `{"features":{"reasoning_loop_guard":null}}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid guard option was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Features.ReasoningLoopGuard != tc.want {
				t.Fatalf("guard=%t want %t", cfg.Features.ReasoningLoopGuard, tc.want)
			}
		})
	}
}
