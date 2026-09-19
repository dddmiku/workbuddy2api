// ═══ 更新日志 ═══
// 2026-09-19：拒绝非有限或格式错误的显式倍率配置，防止产生负用量或悄悄关闭估计。
// 2026-09-19：退役倍率配置仍兼容合法旧值，仅在显式配置时告警，默认加载保持安静。
package main

import (
	"bytes"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredInputTokenScaleWarnsOnlyWhenConfigured(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		env         string
		wantWarning bool
	}{
		{"defaults", `{}`, "", false},
		{"other_server_settings", `{"server":{"max_body_mb":16}}`, "", false},
		{"legacy_one", `{"server":{"input_token_scale":1}}`, "", true},
		{"legacy_one_point_five", `{"server":{"input_token_scale":1.5}}`, "", true},
		{"legacy_environment", `{}`, "1.5", true},
		{"legacy_both", `{"server":{"input_token_scale":1.5}}`, "2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WB2A_INPUT_TOKEN_SCALE", tc.env)
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var captured bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&captured)
			t.Cleanup(func() { log.SetOutput(previous) })
			if _, err := Load(path); err != nil {
				t.Fatalf("valid legacy configuration rejected: %v", err)
			}
			if !tc.wantWarning {
				if strings.Contains(captured.String(), "input_token_scale") {
					t.Fatalf("unset legacy option warned: %s", captured.String())
				}
				return
			}
			if strings.Count(captured.String(), "已退役") != 1 || !strings.Contains(captured.String(), "忽略") || !strings.Contains(captured.String(), "上游原值") {
				t.Fatalf("missing single explicit retirement warning: %s", captured.String())
			}
		})
	}
}

func TestInputTokenScaleConfigDefaultsAndOverrides(t *testing.T) {
	t.Setenv("WB2A_INPUT_TOKEN_SCALE", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.InputTokenScale != 1 {
		t.Fatalf("default input_token_scale=%v want 1", cfg.Server.InputTokenScale)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server":{"input_token_scale":1.5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.InputTokenScale != 1.5 {
		t.Fatalf("file input_token_scale=%v want 1.5", cfg.Server.InputTokenScale)
	}
	t.Setenv("WB2A_INPUT_TOKEN_SCALE", "2")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.InputTokenScale != 2 {
		t.Fatalf("env input_token_scale=%v want 2", cfg.Server.InputTokenScale)
	}
}

func TestInputTokenScaleRejectsInvalidEnvironment(t *testing.T) {
	for _, value := range []string{"NaN", "nan", "+Inf", "-Inf", "Infinity", "garbage", "1e999", "0", "0.9", "5.1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WB2A_INPUT_TOKEN_SCALE", value)
			if _, err := Load(""); err == nil || !strings.Contains(strings.ToLower(err.Error()), "input_token_scale") {
				t.Fatalf("expected named validation error for %q, got %v", value, err)
			}
		})
	}
}

func TestRetiredInputTokenScaleRejectsInvalidJSON(t *testing.T) {
	t.Setenv("WB2A_INPUT_TOKEN_SCALE", "")
	for _, value := range []string{"0", "-1", "0.9", "5.1", `"1.5"`, "true", "null", "[]", "{}", "1e999"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(`{"server":{"input_token_scale":`+value+`}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("invalid retired JSON value %s was silently accepted", value)
			}
		})
	}
}

func TestInputTokenScaleRejectsNonFiniteValue(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := Default()
		cfg.Server.InputTokenScale = value
		if err := cfg.normalize(); err == nil {
			t.Fatalf("accepted non-finite scale %v", value)
		}
	}
}

func TestInputTokenScaleValidBoundaries(t *testing.T) {
	for _, value := range []string{"1", "1.5", "5"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WB2A_INPUT_TOKEN_SCALE", value)
			if _, err := Load(""); err != nil {
				t.Fatalf("valid scale %q rejected: %v", value, err)
			}
		})
	}
}
