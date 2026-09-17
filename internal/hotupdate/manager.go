// ═══ 更新日志 ═══
// 2026-09-17：新增热更新管理器：查版本、下载校验、监听套接字交接、优雅停机，
//
//	并把状态暴露给管理台。全过程不中断在途请求（含长 SSE 对话）。
package hotupdate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/version"
)

// ExitHandover 交接完成后旧进程的退出码。
//
// 约定：容器里的 PID 1（docker-entrypoint.sh）看到这个退出码就不再拉起新实例——
// 此刻监听套接字已经在新进程手里，再起一个只会撞端口；同时 PID 1 保持存活，
// 容器不会因为主进程退出而被销毁。
const ExitHandover = 75

// State 更新状态机。
type State string

const (
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StateDownloading State = "downloading"
	StateHandover    State = "handover"
	StateFailed      State = "failed"
)

// Status 对外暴露的更新状态。
type Status struct {
	Enabled     bool      `json:"enabled"`
	Current     string    `json:"current"`
	Commit      string    `json:"commit"`
	BuiltAt     string    `json:"built_at"`
	Repo        string    `json:"repo"`
	Dir         string    `json:"dir"`
	State       State     `json:"state"`
	LatestTag   string    `json:"latest_tag"`
	LatestAt    time.Time `json:"latest_at"`
	UpdateReady bool      `json:"update_ready"`
	AssetName   string    `json:"asset_name"`
	AssetSize   int64     `json:"asset_size"`
	CheckedAt   time.Time `json:"checked_at"`
	LastError   string    `json:"last_error"`
	InheritedFD bool      `json:"inherited_fd"`
}

// Options 构造参数。
type Options struct {
	Enabled    bool
	Repo       string
	Token      string
	Dir        string
	Binary     string   // 当前可执行文件路径（作为新实例的候选；实际用下载件）
	Args       []string // 新实例命令行参数（沿用本进程）
	Listener   net.Listener
	OnSwitched func() // 交接成功后的回调（main 用它触发优雅停机）
}

// Manager 热更新管理器。
type Manager struct {
	opts   Options
	client *Client

	mu        sync.Mutex
	state     State
	latest    Release
	checkedAt time.Time
	lastError string
	busy      bool
}

// NewManager 构造管理器；Dir 为空时回落到系统临时目录下的固定子目录。
func NewManager(opts Options) *Manager {
	if strings.TrimSpace(opts.Dir) == "" {
		opts.Dir = filepath.Join(os.TempDir(), "wb2api-updates")
	}
	return &Manager{
		opts:   opts,
		client: NewClient(opts.Repo, opts.Token),
		state:  StateIdle,
	}
}

// Status 返回当前状态快照。
func (m *Manager) Status() Status {
	if m == nil {
		return Status{State: StateIdle}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

// Check 查询最新版本并更新状态。
func (m *Manager) Check(ctx context.Context) (Status, error) {
	if m == nil || !m.opts.Enabled {
		return m.Status(), errors.New("自更新未启用（config update.enabled=false）")
	}
	m.mu.Lock()
	if m.busy {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, errors.New("已有更新任务在进行中")
	}
	m.busy = true
	m.state = StateChecking
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()

	release, err := m.client.Latest(ctx)
	if err != nil {
		m.fail(err)
		return m.Status(), err
	}
	m.mu.Lock()
	m.latest = release
	m.checkedAt = time.Now().UTC()
	m.lastError = ""
	m.state = StateIdle
	status := m.statusLocked()
	m.mu.Unlock()
	return status, nil
}

// Apply 执行更新：下载校验后把监听套接字交给新实例，成功时回调 OnSwitched。
//
// 调用方应异步调用（下载与交接耗时以秒计）。返回前的最后一步是交接：
// 新实例报告就绪后本进程才开始优雅停机，在途请求继续跑完。
func (m *Manager) Apply(ctx context.Context, target string) (Status, error) {
	if m == nil || !m.opts.Enabled {
		return m.Status(), errors.New("自更新未启用（config update.enabled=false）")
	}
	m.mu.Lock()
	if m.busy {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, errors.New("已有更新任务在进行中")
	}
	if m.opts.Listener == nil {
		m.mu.Unlock()
		return m.Status(), errors.New("没有可交接的监听套接字（热更新需要以正常启动方式运行）")
	}
	m.busy = true
	m.state = StateChecking
	m.lastError = ""
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()

	release := Release{Tag: target}
	if strings.TrimSpace(target) == "" {
		found, err := m.client.Latest(ctx)
		if err != nil {
			m.fail(err)
			return m.Status(), err
		}
		release = found
	} else {
		// 指定版本：先查最新确认资产可用，再按需比对。
		found, err := m.client.Latest(ctx)
		if err != nil {
			m.fail(err)
			return m.Status(), err
		}
		if found.Tag != target {
			err = fmt.Errorf("指定版本 %s 不是最新发布（最新为 %s）；本功能只支持升级到最新版", target, found.Tag)
			m.fail(err)
			return m.Status(), err
		}
		release = found
	}
	if release.Tag == "" {
		err := errors.New("远端没有可用发布")
		m.fail(err)
		return m.Status(), err
	}
	if !release.UpdateAvailable() {
		err := fmt.Errorf("当前已是 %s，无需更新", version.Version)
		m.fail(err)
		return m.Status(), err
	}
	if release.AssetURL == "" {
		err := fmt.Errorf("发布 %s 没有本机架构的二进制资产（%s）", release.Tag, release.AssetName)
		m.fail(err)
		return m.Status(), err
	}
	if strings.TrimSpace(release.Digest) == "" {
		err := fmt.Errorf("发布 %s 的资产没有 sha256 摘要，拒绝执行未校验的二进制", release.Tag)
		m.fail(err)
		return m.Status(), err
	}

	m.mu.Lock()
	m.latest = release
	m.state = StateDownloading
	m.mu.Unlock()

	path, sum, err := m.client.Download(ctx, release, m.opts.Dir)
	if err != nil {
		m.fail(err)
		return m.Status(), err
	}
	log.Printf("[update] downloaded %s (%s, sha256=%s)", release.Tag, path, sum[:12])

	// current 指针：容器 entrypoint 优先执行它，重启后仍是新版本。
	if err := writeCurrentPointer(m.opts.Dir, path); err != nil {
		log.Printf("WARN: [update] write current pointer: %v", err)
	}

	m.mu.Lock()
	m.state = StateHandover
	m.mu.Unlock()

	args := m.opts.Args
	if len(args) == 0 {
		args = []string{"-config", "/app/config.json"}
	}
	if err := Handover(path, args, m.opts.Listener, readyTimeout); err != nil {
		m.fail(err)
		return m.Status(), err
	}
	log.Printf("[update] handover to %s done; draining in-flight requests", release.Tag)
	m.mu.Lock()
	m.state = StateIdle
	m.checkedAt = time.Now().UTC()
	m.mu.Unlock()
	if m.opts.OnSwitched != nil {
		m.opts.OnSwitched()
	}
	return m.Status(), nil
}

// writeCurrentPointer 记录当前生效的二进制路径（原子替换）。
func writeCurrentPointer(dir, binaryPath string) error {
	pointer := filepath.Join(dir, "current")
	tmp := pointer + ".tmp"
	if err := os.WriteFile(tmp, []byte(binaryPath+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, pointer)
}

// CurrentBinary 返回 current 指针指向的二进制（不存在时返回空串）。
func CurrentBinary(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "current"))
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(raw))
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

func (m *Manager) statusLocked() Status {
	return Status{
		Enabled:     m.opts.Enabled,
		Current:     version.Version,
		Commit:      version.Commit,
		BuiltAt:     version.BuiltAt,
		Repo:        m.client.Repo,
		Dir:         m.opts.Dir,
		State:       m.state,
		LatestTag:   m.latest.Tag,
		LatestAt:    m.latest.PublishedAt,
		UpdateReady: m.opts.Enabled && m.latest.Tag != "" && m.latest.UpdateAvailable(),
		AssetName:   m.latest.AssetName,
		AssetSize:   m.latest.AssetSize,
		CheckedAt:   m.checkedAt,
		LastError:   m.lastError,
		InheritedFD: Inherited(),
	}
}

func (m *Manager) fail(err error) {
	m.mu.Lock()
	m.state = StateFailed
	m.lastError = err.Error()
	m.checkedAt = time.Now().UTC()
	m.mu.Unlock()
	log.Printf("ERROR: [update] %v", err)
}
