// ═══ 更新日志 ═══
// 2026-09-17：锁定自更新取件契约：按架构挑资产、下载逐字节校验 SHA-256、
//
//	摘要不符时不留残留文件、超大资产直接拒绝。
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
	"runtime"
	"strings"
	"testing"

	versionpkg "workbuddy2api/internal/version"
)

func TestLatestPicksAssetForThisArchitecture(t *testing.T) {
	want, err := assetName()
	if err != nil {
		t.Skipf("unsupported arch %s", runtime.GOARCH)
	}
	payload := map[string]any{
		"tag_name":     "v9.9.9",
		"name":         "v9.9.9",
		"published_at": "2026-09-17T10:00:00Z",
		"body":         "notes",
		"assets": []map[string]any{
			{"name": "wb2api-linux-amd64", "size": 11, "browser_download_url": "https://example.invalid/amd64",
				"digest": "sha256:aaaa"},
			{"name": "wb2api-linux-arm64", "size": 22, "browser_download_url": "https://example.invalid/arm64",
				"digest": "sha256:bbbb"},
			{"name": "SHA256SUMS.txt", "size": 33, "browser_download_url": "https://example.invalid/sums"},
		},
	}
	body, _ := json.Marshal(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/dddmiku/workbuddy2api/releases/latest" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "github") {
			t.Errorf("accept header = %q", got)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client := NewClient("", "")
	client.APIBase = server.URL
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if release.Tag != "v9.9.9" || release.AssetName != want {
		t.Fatalf("release = %+v want tag v9.9.9 asset %s", release, want)
	}
	if release.Digest == "" || release.AssetSize == 0 {
		t.Fatalf("digest/size must be carried over: %+v", release)
	}
	if release.PublishedAt.IsZero() || release.Notes != "notes" {
		t.Fatalf("metadata lost: %+v", release)
	}
}

func TestLatestRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer server.Close()
	client := NewClient("", "")
	client.APIBase = server.URL
	if _, err := client.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v want an HTTP 403 failure", err)
	}
}

func TestDownloadVerifiesDigest(t *testing.T) {
	content := []byte("#!/bin/sh\necho wb2api\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()
	dir := t.TempDir()
	client := NewClient("", "")
	release := Release{Tag: "v9.9.9", AssetName: "wb2api-linux-amd64", AssetURL: server.URL, AssetSize: int64(len(content))}

	// 摘要不符：必须失败并清掉临时文件，绝不留下一个未校验的二进制。
	bad := release
	bad.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, _, err := client.Download(context.Background(), bad, dir); err == nil {
		t.Fatal("digest mismatch must fail the download")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed download left files behind: %v", entries)
	}

	// 摘要正确：落盘且带上可执行位。
	good := release
	good.Digest = "sha256:" + digest
	path, got, err := client.Download(context.Background(), good, dir)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if got != digest {
		t.Fatalf("sum = %s want %s", got, digest)
	}
	if filepath.Dir(path) != dir || !strings.Contains(filepath.Base(path), "v9.9.9") {
		t.Fatalf("unexpected target path %s", path)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved binary: %v", err)
	}
	if string(saved) != string(content) {
		t.Fatalf("saved content mismatch")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows 没有可执行位，只在 Unix 上核对。
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("downloaded binary must be executable, mode = %v", info.Mode())
	}
}

func TestDownloadRejectsOversizeAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversize asset must be rejected before any request")
	}))
	defer server.Close()
	client := NewClient("", "")
	release := Release{Tag: "v9.9.9", AssetName: "wb2api-linux-amd64", AssetURL: server.URL,
		AssetSize: maxReleaseBytes + 1}
	if _, _, err := client.Download(context.Background(), release, t.TempDir()); err == nil {
		t.Fatal("oversize asset must fail")
	}
}

func TestDownloadRequiresAssetURL(t *testing.T) {
	client := NewClient("", "")
	if _, _, err := client.Download(context.Background(), Release{Tag: "v9.9.9"}, t.TempDir()); err == nil {
		t.Fatal("release without an asset must fail")
	}
}

func TestUpdateAvailableComparesVersions(t *testing.T) {
	original := versionpkg.Version
	defer func() { versionpkg.Version = original }()

	versionpkg.Version = "1.2.0"
	cases := []struct {
		tag  string
		want bool
	}{
		{"v1.2.0", false},
		{"1.2.0", false},
		{"v1.3.0", true},
		{"", false},
	}
	for _, item := range cases {
		if got := (Release{Tag: item.tag}).UpdateAvailable(); got != item.want {
			t.Fatalf("UpdateAvailable(%q) = %v want %v", item.tag, got, item.want)
		}
	}
	// 开发构建：远端只要有 tag 就提示可更新。
	versionpkg.Version = "dev"
	if !(Release{Tag: "v1.2.0"}).UpdateAvailable() {
		t.Fatal("dev builds must treat any release as an update")
	}
}

// TestReleaseAssetNamesMatchWorkflow 资产名是 release.go 与发布流水线之间的硬契约。
func TestReleaseAssetNamesMatchWorkflow(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "build.yml"))
	if err != nil {
		t.Skipf("workflow not in this tree: %v", err)
	}
	text := string(workflow)
	for _, name := range []string{"wb2api-linux-amd64", "wb2api-linux-arm64"} {
		if !strings.Contains(text, name) {
			t.Fatalf("build.yml must publish %s", name)
		}
	}
}

func TestNewClientDefaultsRepoAndTimeout(t *testing.T) {
	client := NewClient("  ", "token")
	if client.Repo != DefaultRepo {
		t.Fatalf("repo = %q want default", client.Repo)
	}
	if client.Token != "token" || client.HTTP == nil || client.HTTP.Timeout != httpTimeout {
		t.Fatalf("client = %+v", client)
	}
	if client.apiBase() != defaultAPIBase {
		t.Fatalf("api base = %q want %q", client.apiBase(), defaultAPIBase)
	}
	client.APIBase = "http://127.0.0.1:8080/"
	if client.apiBase() != "http://127.0.0.1:8080" {
		t.Fatalf("api base override = %q", client.apiBase())
	}
}

func TestAssetNameFollowsRuntimeArch(t *testing.T) {
	name, err := assetName()
	switch runtime.GOARCH {
	case "amd64", "arm64":
		if err != nil || name != "wb2api-linux-"+runtime.GOARCH {
			t.Fatalf("assetName() = %q, %v for %s", name, err, runtime.GOARCH)
		}
	default:
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("assetName() = %q, %v; unsupported arch must error", name, err)
		}
	}
}
