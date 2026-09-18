// ═══ 更新日志 ═══
// 2026-09-18：发布名称编码为单个文件名，下载使用独占临时文件并在校验后设可执行位，防止越界和符号链接覆盖。
// 2026-09-17：新增自更新取件：查 GitHub Release 最新版本、按架构挑二进制并校验
//
//	SHA-256，供管理台一键热更新使用。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"workbuddy2api/internal/version"
)

const (
	// DefaultRepo 默认发布仓库（可用 config update.repo 覆盖）。
	DefaultRepo = "dddmiku/workbuddy2api"
	// defaultAPIBase GitHub API 根地址；测试用它指向本地假服务。
	defaultAPIBase = "https://api.github.com"
	// maxReleaseBytes 单个下载对象上限，防超大文件把磁盘写满。
	maxReleaseBytes = 128 << 20
	// httpTimeout 查版本/下载的超时。
	httpTimeout = 60 * time.Second
)

// Release 一个候选版本。
type Release struct {
	Tag         string    `json:"tag"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	Notes       string    `json:"notes"`
	AssetURL    string    `json:"asset_url"`
	AssetName   string    `json:"asset_name"`
	AssetSize   int64     `json:"asset_size"`
	Digest      string    `json:"digest"` // 形如 "sha256:xxxx"，发布端未提供时为空
}

// UpdateAvailable 判断候选版本是否比当前版本新（按标签名比较，非严格 semver）。
func (r Release) UpdateAvailable() bool {
	current := strings.TrimSpace(version.Version)
	if r.Tag == "" {
		return false
	}
	if current == "" || current == "dev" {
		// 开发构建：只要远端有 tag 就认为可更新，交由使用者判断。
		return true
	}
	return strings.TrimPrefix(r.Tag, "v") != strings.TrimPrefix(current, "v")
}

// Client 查版本 / 下载用。
type Client struct {
	Repo  string
	HTTP  *http.Client
	Token string // 私有仓库时用；公开仓库留空即可
	// APIBase 覆盖 GitHub API 根地址（测试用）；空 = 官方地址。
	APIBase string
}

// NewClient 构造取件客户端；repo 为空回落默认仓库。
func NewClient(repo, token string) *Client {
	if strings.TrimSpace(repo) == "" {
		repo = DefaultRepo
	}
	return &Client{Repo: repo, Token: token, HTTP: &http.Client{Timeout: httpTimeout}}
}

// apiBase 返回生效的 API 根地址。
func (c *Client) apiBase() string {
	if strings.TrimSpace(c.APIBase) == "" {
		return defaultAPIBase
	}
	return strings.TrimRight(c.APIBase, "/")
}

// assetName 本机架构对应的发布资产名。
func assetName() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "wb2api-linux-amd64", nil
	case "arm64":
		return "wb2api-linux-arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q for self-update", runtime.GOARCH)
	}
}

// Latest 查询最新发布版本（含本机架构的二进制资产）。
func (c *Client) Latest(ctx context.Context) (Release, error) {
	name, err := assetName()
	if err != nil {
		return Release{}, err
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.apiBase(), c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "workbuddy2api-self-update")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("query latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Release{}, fmt.Errorf("query latest release: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		PublishedAt time.Time `json:"published_at"`
		Body        string    `json:"body"`
		Assets      []struct {
			Name               string `json:"name"`
			Size               int64  `json:"size"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Digest             string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return Release{}, fmt.Errorf("decode release: %w", err)
	}
	release := Release{
		Tag:         payload.TagName,
		Name:        payload.Name,
		PublishedAt: payload.PublishedAt,
		Notes:       payload.Body,
	}
	for _, asset := range payload.Assets {
		if asset.Name != name {
			continue
		}
		release.AssetName = asset.Name
		release.AssetURL = asset.BrowserDownloadURL
		release.AssetSize = asset.Size
		release.Digest = asset.Digest
		break
	}
	return release, nil
}

// Download 把资产下载到 dir，返回文件路径与 SHA-256。
// 发布端提供 digest 时逐字节校验；未提供则只回报实测值，由调用方决定是否信任。
func (c *Client) Download(ctx context.Context, release Release, dir string) (string, string, error) {
	if release.AssetURL == "" {
		return "", "", errors.New("release has no asset for this architecture")
	}
	if release.AssetSize > maxReleaseBytes {
		return "", "", fmt.Errorf("asset too large: %d bytes", release.AssetSize)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.AssetURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "workbuddy2api-self-update")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("download asset: HTTP %d", resp.StatusCode)
	}

	target := filepath.Join(dir, url.PathEscape(release.Tag)+"-"+url.PathEscape(release.AssetName))
	file, err := os.CreateTemp(dir, ".download-*.part")
	if err != nil {
		return "", "", err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	defer file.Close()
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(resp.Body, maxReleaseBytes+1))
	if copyErr != nil {
		return "", "", fmt.Errorf("download asset: %w", copyErr)
	}
	if written > maxReleaseBytes {
		return "", "", fmt.Errorf("asset exceeds %d bytes", maxReleaseBytes)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if want := strings.TrimPrefix(release.Digest, "sha256:"); want != "" && !strings.EqualFold(want, sum) {
		return "", "", fmt.Errorf("sha256 mismatch: want %s got %s", want, sum)
	}
	if err := file.Chmod(0o755); err != nil {
		return "", "", err
	}
	if err := file.Sync(); err != nil {
		return "", "", err
	}
	if err := file.Close(); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", "", err
	}
	return target, sum, nil
}
