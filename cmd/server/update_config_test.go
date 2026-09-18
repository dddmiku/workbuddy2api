// ═══ 更新日志 ═══
// 2026-09-18：锁定监督脚本与 Go 共用的更新目录，保留文件配置和状态目录的回退语义。
package main

import (
	"path/filepath"
	"testing"
)

func TestUpdateDirUsesSupervisorDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "supervisor-updates")
	t.Setenv("WB2API_UPDATE_DIR", dir)
	config := Default()
	config.Update.Dir = filepath.Join(t.TempDir(), "file-config-updates")
	if got := updateDir(config); got != dir {
		t.Fatalf("update directory = %q; supervisor restarts from %q", got, dir)
	}
}

func TestUpdateDirKeepsFileFallbacks(t *testing.T) {
	t.Setenv("WB2API_UPDATE_DIR", "")
	config := Default()
	custom := filepath.Join(t.TempDir(), "custom")
	config.Update.Dir = custom
	if got := updateDir(config); got != custom {
		t.Fatalf("explicit update directory = %q want %q", got, custom)
	}
	config.Update.Dir = ""
	config.StateFile = filepath.Join(t.TempDir(), "state.json")
	if got, want := updateDir(config), filepath.Join(filepath.Dir(config.StateFile), "updates"); got != want {
		t.Fatalf("state-derived update directory = %q want %q", got, want)
	}
	config.StateFile = "state.json"
	if got := updateDir(config); got != filepath.Join("data", "updates") {
		t.Fatalf("default update directory = %q", got)
	}
}
