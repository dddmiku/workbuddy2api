// ═══ 更新日志 ═══
// 2026-09-17：新增热更新的监听套接字交接：父进程把 listener 以 FD 传给新实例，
//
//	新实例 ready 后父进程再优雅停机，在途请求（含长 SSE）不受影响。
package hotupdate

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// EnvListenFD 新实例从该 FD 继承监听套接字（热更新切换）。
	EnvListenFD = "WB2API_LISTEN_FD"
	// EnvReadyFD 新实例就绪后往该 FD 写一行，父进程据此判断可以停旧实例。
	EnvReadyFD = "WB2API_READY_FD"
	// EnvAdminFD 新实例从该 FD 继承管理 socket（Unix domain）。老进程还在跑的时候
	// 新进程没法重新 bind 同一个路径，只能继承；不配管理 socket 时该变量不存在。
	EnvAdminFD = "WB2API_ADMIN_FD"
	// inheritedFDBase 传给子进程的第一个额外 FD 编号（0/1/2 是标准流）。
	inheritedFDBase = 3
	// adminFDNumber 管理 socket 占用的额外 FD 编号（listener=3、ready=4 之后）。
	adminFDNumber = inheritedFDBase + 2
	// readyTimeout 新实例从启动到开始服务的等待上限。
	readyTimeout = 30 * time.Second
)

// Inherited 判断本进程是否由热更新启动（继承了监听套接字）。
func Inherited() bool { return strings.TrimSpace(os.Getenv(EnvListenFD)) != "" }

// InheritedAdminFD 返回继承来的管理 socket FD；没有继承时第二个返回值为 false。
func InheritedAdminFD() (int, bool) {
	raw := strings.TrimSpace(os.Getenv(EnvAdminFD))
	if raw == "" {
		return 0, false
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 0 {
		return 0, false
	}
	return fd, true
}

// ListenerFromFD 把继承的 FD 还原成监听器（listener 与管理 socket 通用）。
func ListenerFromFD(fd int, name string) (net.Listener, error) {
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, fmt.Errorf("invalid inherited fd %d", fd)
	}
	// net.FileListener 会 dup 一份，随后关掉原 file 不影响监听。
	ln, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("inherit fd %d: %w", fd, err)
	}
	return ln, nil
}

// Listen 返回监听器：有继承 FD 就用它（热更新切换后的新实例），否则正常监听。
// 第二个返回值表示是否来自继承。
func Listen(addr string) (net.Listener, bool, error) {
	raw := strings.TrimSpace(os.Getenv(EnvListenFD))
	if raw == "" {
		ln, err := net.Listen("tcp", addr)
		return ln, false, err
	}
	fd, err := strconv.Atoi(raw)
	if err != nil {
		return nil, false, fmt.Errorf("invalid %s=%q: %w", EnvListenFD, raw, err)
	}
	ln, err := ListenerFromFD(fd, "inherited-listener")
	if err != nil {
		return nil, false, err
	}
	return ln, true, nil
}

// NotifyReady 通知父进程本实例已开始服务；未处于交接流程时是空操作。
func NotifyReady() error {
	raw := strings.TrimSpace(os.Getenv(EnvReadyFD))
	if raw == "" {
		return nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("invalid %s=%q: %w", EnvReadyFD, raw, err)
	}
	file := os.NewFile(uintptr(fd), "ready-pipe")
	if file == nil {
		return nil
	}
	defer file.Close()
	if _, err := file.Write([]byte("ready\n")); err != nil {
		return fmt.Errorf("notify ready: %w", err)
	}
	return nil
}

// Handover 把监听套接字交给 binary 的新实例，等它报告就绪后返回。
//
// 调用方在返回后应当停止接受新连接并等待在途请求收尾（srv.Shutdown）：
// 此刻新实例已经在同一个套接字上 accept，旧连接继续由本进程服务到结束。
func Handover(binary string, args []string, ln net.Listener, adminLn net.Listener, wait time.Duration) error {
	if _, ok := ln.(*net.TCPListener); !ok {
		return fmt.Errorf("listener is %T, need *net.TCPListener", ln)
	}
	file, err := listenerFile(ln)
	if err != nil {
		return fmt.Errorf("dup listener: %w", err)
	}
	defer file.Close()

	// 管理 socket（Unix domain）也要交给新实例：旧进程还在跑的时候，同一个路径没法
	// 重新 bind（apikeys.ListenUnix 会判定 already in use），只能继承 FD。
	var adminFile *os.File
	if adminLn != nil {
		if unixLn, ok := adminLn.(*net.UnixListener); ok {
			// 关掉"关闭时删除 socket 文件"：老进程收尾时不能把新进程正服务的路径删掉。
			unixLn.SetUnlinkOnClose(false)
		}
		adminFile, err = listenerFile(adminLn)
		if err != nil {
			return fmt.Errorf("dup admin listener: %w", err)
		}
		defer adminFile.Close()
	}

	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("ready pipe: %w", err)
	}
	defer readPipe.Close()

	cmd := exec.Command(binary, args...)
	env := append(stripHandoverEnv(os.Environ()),
		fmt.Sprintf("%s=%d", EnvListenFD, inheritedFDBase),
		fmt.Sprintf("%s=%d", EnvReadyFD, inheritedFDBase+1),
	)
	// ExtraFiles[0] → fd 3（listener），[1] → fd 4（ready 管道写端），[2] → fd 5（管理 socket）。
	extra := []*os.File{file, writePipe}
	if adminFile != nil {
		env = append(env, fmt.Sprintf("%s=%d", EnvAdminFD, adminFDNumber))
		extra = append(extra, adminFile)
	}
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	cmd.ExtraFiles = extra
	if err := cmd.Start(); err != nil {
		_ = writePipe.Close()
		return fmt.Errorf("start new instance: %w", err)
	}
	// 父进程只保留读端：写端若仍打开，读端永远等不到 EOF。
	_ = writePipe.Close()

	if wait <= 0 {
		wait = readyTimeout
	}
	ready := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		_, readErr := readPipe.Read(buf)
		ready <- readErr
	}()

	select {
	case readErr := <-ready:
		if readErr != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return fmt.Errorf("new instance exited before reporting ready: %w", readErr)
		}
	case <-time.After(wait):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("new instance did not report ready within %s", wait)
	}

	// 新实例已在服务：后台回收它，父进程继续把手上的在途请求跑完。
	go func() { _ = cmd.Wait() }()
	return nil
}

// listenerFile 取监听器的可传递副本（TCP 与 Unix domain 都支持）。
func listenerFile(ln net.Listener) (*os.File, error) {
	switch item := ln.(type) {
	case *net.TCPListener:
		return item.File()
	case *net.UnixListener:
		return item.File()
	default:
		return nil, fmt.Errorf("listener is %T, need *net.TCPListener or *net.UnixListener", ln)
	}
}

// stripHandoverEnv 去掉继承来的交接环境变量，避免子进程里出现重复键
// （重复时取值行为依赖具体实现，显式清理更可控）。
func stripHandoverEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		if strings.HasPrefix(item, EnvListenFD+"=") || strings.HasPrefix(item, EnvReadyFD+"=") ||
			strings.HasPrefix(item, EnvAdminFD+"=") {
			continue
		}
		out = append(out, item)
	}
	return out
}

// gracefulShutdownTimeout 父进程等待在途请求收尾的上限。
//
// 取值要覆盖合法的长流式生成：上游 idle_timeout 默认 300s，加上首字节前的
// 等待与收尾，15 分钟足够；超时则强退，不再无限等待。
const gracefulShutdownTimeout = 15 * time.Minute

// ShutdownTimeout 暴露给调用方，避免各处硬编码不一致。
func ShutdownTimeout() time.Duration { return gracefulShutdownTimeout }

// IsProcessAlive 判断 pid 对应进程是否还在（用于自检与诊断）。
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}
