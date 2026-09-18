// ═══ 更新日志 ═══
// 2026-09-18：用真实子进程验证交接提交失败、就绪协议与空启动参数，避免半成功切换。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func lifecycleHelper(t *testing.T, ready string) ([]byte, string) {
	t.Helper()
	requireListenerInheritance(t)
	root := t.TempDir()
	pidPath := filepath.Join(root, "child.pid")
	stopPath := filepath.Join(root, "stop")
	t.Setenv("WB2API_TEST_CHILD_PID", pidPath)
	t.Setenv("WB2API_TEST_CHILD_STOP", stopPath)
	t.Setenv("WB2API_TEST_EMPTY_ARGS", "")
	t.Cleanup(func() {
		_ = os.WriteFile(stopPath, []byte("stop"), 0o600)
		raw, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		for i := 0; i < 100 && IsProcessAlive(pid); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if IsProcessAlive(pid) {
			process, err := os.FindProcess(pid)
			if err == nil {
				_ = process.Kill()
			}
		}
	})
	script := "#!/bin/sh\n# ═══ 更新日志 ═══\n# 2026-09-18：隔离交接测试助手，只操作测试临时文件。\n" +
		"printf '%s\\n' \"$$\" > \"$WB2API_TEST_CHILD_PID\"\n" +
		"if [ \"$WB2API_TEST_EMPTY_ARGS\" = 1 ] && [ \"$#\" -ne 0 ]; then exit 11; fi\n" +
		"printf '__READY__\\n' >&4\n" +
		"attempt=0\n" +
		"while [ ! -e \"$WB2API_TEST_CHILD_STOP\" ] && [ \"$attempt\" -lt 400 ]; do\n" +
		"  attempt=$((attempt + 1))\n  sleep 0.05\ndone\n"
	return []byte(strings.ReplaceAll(script, "__READY__", ready)), pidPath
}

func managerForLifecycleAsset(t *testing.T, content []byte, args []string) *Manager {
	t.Helper()
	sum := sha256.Sum256(content)
	name, err := assetName()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var provider *httptest.Server
	provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			_, _ = w.Write(content)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v99.0.1",
			"assets": []map[string]any{{
				"name": name, "size": len(content),
				"browser_download_url": provider.URL + "/asset",
				"digest":               "sha256:" + hex.EncodeToString(sum[:]),
			}},
		})
	}))
	t.Cleanup(provider.Close)
	manager := NewManager(Options{Enabled: true, Dir: t.TempDir(), Listener: listener, Args: args})
	manager.client.APIBase = provider.URL
	return manager
}

func TestManagerKeepsEmptyStartupArguments(t *testing.T) {
	content, _ := lifecycleHelper(t, "ready")
	t.Setenv("WB2API_TEST_EMPTY_ARGS", "1")
	manager := managerForLifecycleAsset(t, content, nil)
	switched := false
	manager.opts.OnSwitched = func() {
		switched = true
		if CurrentBinary(manager.opts.Dir) == "" {
			t.Error("restart pointer must be committed before draining the old instance")
		}
	}
	if _, err := manager.Apply(context.Background(), ""); err != nil {
		t.Fatalf("empty startup arguments must remain empty: %v", err)
	}
	if !switched {
		t.Fatal("ready update did not switch")
	}
}

func TestManagerPointerCommitFailureStopsCandidate(t *testing.T) {
	content, pidPath := lifecycleHelper(t, "ready")
	manager := managerForLifecycleAsset(t, content, []string{"preserved-argument"})
	if err := os.Mkdir(filepath.Join(manager.opts.Dir, "current"), 0o700); err != nil {
		t.Fatal(err)
	}
	switched := false
	manager.opts.OnSwitched = func() { switched = true }
	status, err := manager.Apply(context.Background(), "")
	if err == nil || status.State != StateFailed || switched {
		t.Errorf("pointer commit failure must preserve the old instance: status=%+v err=%v switched=%v", status, err, switched)
	}
	raw, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatalf("candidate never reached its readiness handshake: %v", readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if parseErr != nil || IsProcessAlive(pid) {
		t.Errorf("uncommitted candidate must be stopped: pid=%d parse=%v", pid, parseErr)
	}
}

func TestHandoverRejectsUnexpectedReadinessMessage(t *testing.T) {
	content, _ := lifecycleHelper(t, "not-ready")
	path := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := Handover(path, nil, listener, nil, time.Second); err == nil {
		t.Fatal("unexpected readiness bytes must not commit a handover")
	}
}
