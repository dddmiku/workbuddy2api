package apikeys

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAdminTransportAndSecretDisclosure(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	h := s.AdminHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/keys", strings.NewReader(`{"name":"client","note":"laptop"}`)))
	if rec.Code != 201 {
		t.Fatalf("create=%d %s", rec.Code, rec.Body)
	}
	var created struct {
		Key   string `json:"key"`
		Entry Info   `json:"entry"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(created.Key) {
		t.Fatal("create did not authorize key")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/keys", nil))
	if strings.Contains(rec.Body.String(), created.Key) || strings.Contains(rec.Body.String(), "sha256") {
		t.Fatal("list disclosed key material")
	}
	for _, body := range []string{`{"enabled":"false"}`, `{"enabled":false,"sha256":"overwrite"}`, `{} {}`, strings.Repeat(" ", 8193)} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("PATCH", "/keys/"+created.Entry.ID, strings.NewReader(body)))
		if rec.Code != 400 {
			t.Fatalf("invalid update code=%d", rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/keys/"+created.Entry.ID, nil))
	if rec.Code != 200 || s.Authenticate(created.Key) {
		t.Fatal("deleted key accepted")
	}
}

func TestAdminSocketPermissionAndConflict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket deployment check")
	}
	path := filepath.Join(t.TempDir(), "admin.sock")
	listener, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket permissions not private")
	}
	if duplicate, err := ListenUnix(path); err == nil {
		duplicate.Close()
		t.Fatal("live socket replaced")
	}
	file := filepath.Join(t.TempDir(), "keep.txt")
	_ = os.WriteFile(file, []byte("keep"), 0600)
	if bad, err := ListenUnix(file); err == nil {
		bad.Close()
		t.Fatal("regular file replaced")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "keep" {
		t.Fatal("regular file changed")
	}
}

// TestWaitUnixTakesOverAfterRelease 热更新兼容路径：旧实例（不会传管理 socket FD）
// 收尾释放路径后，新实例要能重新 bind 并继续提供管理接口。
func TestWaitUnixTakesOverAfterRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket deployment check")
	}
	path := filepath.Join(t.TempDir(), "admin.sock")
	previous, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := ListenUnix(path); !errors.Is(err, ErrSocketBusy) {
		if duplicate != nil {
			duplicate.Close()
		}
		t.Fatalf("err = %v want ErrSocketBusy", err)
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = previous.Close()
	}()
	deadline := time.Now().Add(10 * time.Second)
	current, err := WaitUnix(path, 8*time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	defer current.Close()
	if elapsed := time.Since(deadline.Add(-10 * time.Second)); elapsed < 200*time.Millisecond {
		t.Fatalf("wait returned after %s; must wait for the previous instance to exit", elapsed)
	}

	store, err := Open(filepath.Join(t.TempDir(), "keys.json"), "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: store.AdminHandler()}
	go func() { _ = server.Serve(current) }()
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}}
	response, err := client.Get("http://admin/keys")
	if err != nil {
		t.Fatalf("get over the re-bound socket: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status = %d want 200", response.StatusCode)
	}
}
