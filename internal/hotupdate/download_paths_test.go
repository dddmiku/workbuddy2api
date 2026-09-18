// ═══ 更新日志 ═══
// 2026-09-18：复现发布标签越界写入和固定临时文件跟随符号链接，锁定下载目录边界。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func downloadFixture(t *testing.T, tag, asset string) (*Client, Release, []byte) {
	t.Helper()
	content := []byte("verified update payload")
	sum := sha256.Sum256(content)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)
	return NewClient("", ""), Release{
		Tag: tag, AssetName: asset, AssetURL: server.URL,
		AssetSize: int64(len(content)), Digest: "sha256:" + hex.EncodeToString(sum[:]),
	}, content
}

func TestDownloadKeepsReleaseNamesInsideUpdateDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "updates")
	client, release, _ := downloadFixture(t, "../escaped", "wb2api")
	path, _, err := client.Download(context.Background(), release, dir)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	relative, err := filepath.Rel(dir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("release tag escaped update directory: path=%q relative=%q err=%v", path, relative, err)
	}
}

func TestDownloadDoesNotFollowPreexistingPartSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink semantics")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "updates")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "unrelated-file")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, release, content := downloadFixture(t, "v99.0.0", "wb2api")
	oldPart := filepath.Join(dir, release.Tag+"-"+release.AssetName+".part")
	if err := os.Symlink(victim, oldPart); err != nil {
		t.Fatal(err)
	}
	path, _, err := client.Download(context.Background(), release, dir)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	saved, err := os.ReadFile(victim)
	if err != nil || string(saved) != "keep me" {
		t.Fatalf("download overwrote an unrelated file through .part symlink: %q, %v", saved, err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("downloaded binary must be a regular file: info=%v err=%v", info, err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(content) {
		t.Fatalf("downloaded binary mismatch: %q, %v", actual, err)
	}
}
