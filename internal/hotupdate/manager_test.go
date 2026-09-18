// ═══ 更新日志 ═══
// 2026-09-18：复现交接失败仍改写重启指针的问题，防止热更新失败后重启到坏版本。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestManagerFailedHandoverPreservesRestartBinary(t *testing.T) {
	requireListenerInheritance(t)
	dir := t.TempDir()
	oldBinary := filepath.Join(dir, "working-version")
	if err := os.WriteFile(oldBinary, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentPointer(dir, oldBinary); err != nil {
		t.Fatal(err)
	}
	running := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("old-ok"))
	}))
	defer running.Close()

	// 摘要正确但不可执行：实际下载链路成功，真实 exec 交接失败。
	content := []byte("not an executable image\n")
	sum := sha256.Sum256(content)
	name, err := assetName()
	if err != nil {
		t.Fatal(err)
	}
	var releaseServer *httptest.Server
	releaseServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			_, _ = w.Write(content)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v99.0.0",
			"assets": []map[string]any{{
				"name": name, "size": len(content),
				"browser_download_url": releaseServer.URL + "/asset",
				"digest":               "sha256:" + hex.EncodeToString(sum[:]),
			}},
		})
	}))
	defer releaseServer.Close()

	switched := false
	manager := NewManager(Options{
		Enabled: true, Dir: dir, Listener: running.Listener,
		OnSwitched: func() { switched = true },
	})
	manager.client.APIBase = releaseServer.URL
	status, err := manager.Apply(context.Background(), "")
	if err == nil || status.State != StateFailed {
		t.Fatalf("failed executable must fail the update: status=%+v err=%v", status, err)
	}
	if switched {
		t.Fatal("failed handover must not drain the running instance")
	}
	if got := CurrentBinary(dir); got != oldBinary {
		t.Fatalf("failed handover changed restart binary to %q; want %q", got, oldBinary)
	}
	if got := dialBody(t, running.Listener.Addr().String()); got != "old-ok" {
		t.Fatalf("old instance stopped serving after failed handover: %q", got)
	}
}
