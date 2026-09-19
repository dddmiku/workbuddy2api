// ═══ 更新日志 ═══
// 2026-09-19：拒绝非有限或格式错误的显式倍率配置，防止产生负用量或悄悄关闭估计。
package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
