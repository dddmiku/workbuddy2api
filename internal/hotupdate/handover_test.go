// ═══ 更新日志 ═══
// 2026-09-17：锁定热更新交接的真实语义：监听套接字经 FD 传给新进程、新进程就绪后
//
//	父进程才停机、父进程手里的在途请求要跑完、失败路径不空等到超时。
package hotupdate

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	helperEnv     = "WB2API_HANDOVER_HELPER"
	helperStopEnv = "WB2API_HANDOVER_STOP"
	helperFailEnv = "WB2API_HANDOVER_FAIL"
)

// TestHandoverHelperProcess 是被 Handover 拉起的新实例，不在父进程正常测试里执行。
//
// 它继承监听套接字，开始服务后写 ready，然后一直服务到测试写入停止文件为止。
func TestHandoverHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process only")
	}
	if os.Getenv(helperFailEnv) == "1" {
		os.Exit(3) // 模拟新实例启动即失败：父进程必须立刻报错而不是等到超时
	}
	listener, inherited, err := Listen("127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper listen:", err)
		os.Exit(2)
	}
	if !inherited {
		fmt.Fprintln(os.Stderr, "helper did not inherit the listening socket")
		os.Exit(2)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "child-ok")
	})}
	go func() { _ = server.Serve(listener) }()
	if err := NotifyReady(); err != nil {
		fmt.Fprintln(os.Stderr, "helper notify ready:", err)
		os.Exit(2)
	}

	stop := os.Getenv(helperStopEnv)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if stop != "" {
			if _, err := os.Stat(stop); err == nil {
				// 删掉停止文件当作"我退出了"的回执：父测试据此确认管道已关闭。
				_ = os.Remove(stop)
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startHandover 在测试进程里发起一次交接，返回停止子进程的清理函数。
//
// 停止文件放在系统临时目录而不是 t.TempDir()：TempDir 在测试收尾时会被删掉，
// 子进程可能因此错过停止信号并一直握着 go test 的 stdout 管道，
// 让整个包以「Test I/O incomplete」失败。
func startHandover(t *testing.T, ln net.Listener) func() {
	t.Helper()
	requireListenerInheritance(t)
	stop := filepath.Join(os.TempDir(), fmt.Sprintf("wb2api-hotupdate-stop-%d-%d", os.Getpid(), time.Now().UnixNano()))
	t.Setenv(helperEnv, "1")
	t.Setenv(helperStopEnv, stop)
	if err := Handover(os.Args[0], []string{"-test.run=^TestHandoverHelperProcess$", "-test.timeout=90s"}, ln, 20*time.Second); err != nil {
		t.Fatalf("handover: %v", err)
	}
	return func() {
		_ = os.WriteFile(stop, []byte("stop\n"), 0o600)
		// 等子进程删回执：确认它已经退出，不留孤儿进程和未关闭的管道。
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(stop); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = os.Remove(stop)
		t.Errorf("helper process did not exit within 10s")
	}
}

// requireListenerInheritance fd 传递只在 Unix 上成立（Windows 的 ExtraFiles 不支持
// 监听套接字）。正式部署是 Linux 容器，本地 Windows 开发跳过即可。
func requireListenerInheritance(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("监听套接字继承是 Unix 语义；在 Linux 容器里跑这条用例")
	}
}

func dialBody(t *testing.T, addr string) string {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 50; attempt++ {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + addr + "/")
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return string(body)
		}
		lastErr = fmt.Errorf("status %d", resp.StatusCode)
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no response from the new instance: %v", lastErr)
	return ""
}

// TestHandoverPassesListenerToNewInstance 新实例必须能在同一个套接字上提供服务。
func TestHandoverPassesListenerToNewInstance(t *testing.T) {
	requireListenerInheritance(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	stopHelper := startHandover(t, ln)
	defer stopHelper()

	if body := dialBody(t, addr); body != "child-ok" {
		t.Fatalf("body = %q want child-ok", body)
	}
}

// TestHandoverKeepsInFlightRequestAlive 交接期间父进程手里的在途请求必须跑完——
// 这是「切版本不打断正在进行的对话」的最小可复现形态。
func TestHandoverKeepsInFlightRequestAlive(t *testing.T) {
	requireListenerInheritance(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	release := make(chan struct{})
	started := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "parent-done")
	})}
	go func() { _ = server.Serve(ln) }()

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + addr + "/")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		done <- result{body: string(body), err: err}
	}()
	<-started

	stopHelper := startHandover(t, ln)
	defer stopHelper()
	close(release)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("in-flight request failed during handover: %v", got.err)
		}
		if got.body != "parent-done" {
			t.Fatalf("in-flight body = %q want parent-done", got.body)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("in-flight request never finished")
	}

	// 旧进程按生产路径收尾：关掉自己的监听（套接字由新实例接着持有）。
	// Close/Shutdown 必须立刻返回；如果继承时把套接字切成阻塞模式，这里会卡住。
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownDone <- server.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("old process shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("old process shutdown blocked; inherited socket must stay non-blocking")
	}

	// 旧进程交出监听后，新连接只能落到新实例手里。
	if body := dialBody(t, addr); body != "child-ok" {
		t.Fatalf("post-handover body = %q want child-ok", body)
	}
}

// TestHandoverFailsFastWhenChildDies 新实例启动即崩时不能干等到超时。
func TestHandoverFailsFastWhenChildDies(t *testing.T) {
	requireListenerInheritance(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	t.Setenv(helperEnv, "1")
	t.Setenv(helperFailEnv, "1")

	begin := time.Now()
	err = Handover(os.Args[0], []string{"-test.run=^TestHandoverHelperProcess$"}, ln, 20*time.Second)
	if err == nil {
		t.Fatal("handover must fail when the new instance exits before ready")
	}
	if !strings.Contains(err.Error(), "ready") {
		t.Fatalf("error = %v want a ready-report failure", err)
	}
	if elapsed := time.Since(begin); elapsed > 10*time.Second {
		t.Fatalf("handover waited %s; must fail as soon as the child exits", elapsed)
	}
}

func TestInheritedAndStripHandoverEnv(t *testing.T) {
	t.Setenv(EnvListenFD, "  ")
	if Inherited() {
		t.Fatal("blank env must not be treated as inherited")
	}
	t.Setenv(EnvListenFD, "3")
	if !Inherited() {
		t.Fatal("fd env must be detected as inherited")
	}
	env := stripHandoverEnv([]string{"PATH=/bin", EnvListenFD + "=3", EnvReadyFD + "=4", "HOME=/root"})
	want := []string{"PATH=/bin", "HOME=/root"}
	if len(env) != len(want) || env[0] != want[0] || env[1] != want[1] {
		t.Fatalf("stripHandoverEnv = %v want %v", env, want)
	}
}

func TestListenRejectsBrokenInheritedFD(t *testing.T) {
	t.Setenv(EnvListenFD, "not-a-number")
	if _, _, err := Listen("127.0.0.1:0"); err == nil {
		t.Fatal("broken fd env must fail instead of silently binding a new port")
	}
	t.Setenv(EnvListenFD, "")
	ln, inherited, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	if inherited {
		t.Fatal("plain listen must not report an inherited socket")
	}
}

func TestCurrentBinaryPointer(t *testing.T) {
	dir := t.TempDir()
	if got := CurrentBinary(dir); got != "" {
		t.Fatalf("missing pointer must be empty, got %q", got)
	}
	binary := filepath.Join(dir, "v1.3.0-wb2api-linux-amd64")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}
	if err := writeCurrentPointer(dir, binary); err != nil {
		t.Fatalf("write pointer: %v", err)
	}
	if got := CurrentBinary(dir); got != binary {
		t.Fatalf("pointer = %q want %q", got, binary)
	}
	// 指针指向的文件被删掉（比如手工清目录）时按「没有指针」处理，回落内置二进制。
	if err := os.Remove(binary); err != nil {
		t.Fatalf("remove binary: %v", err)
	}
	if got := CurrentBinary(dir); got != "" {
		t.Fatalf("stale pointer must be ignored, got %q", got)
	}
}

func TestShutdownTimeoutCoversLongStreams(t *testing.T) {
	// 上游 idle_timeout 默认 300s；交接后的收尾窗口必须显著大于它。
	if got := ShutdownTimeout(); got < 10*time.Minute {
		t.Fatalf("shutdown timeout = %s, too short for long SSE streams", got)
	}
}

// TestExitHandoverCodeIsDocumented 退出码是入口脚本与网关之间的约定，改动必须同步。
func TestExitHandoverCodeIsDocumented(t *testing.T) {
	if ExitHandover != 75 {
		t.Fatalf("ExitHandover = %d; docker-entrypoint.sh 依赖 75 这个约定", ExitHandover)
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "docker-entrypoint.sh"))
	if err != nil {
		t.Skipf("entrypoint not in this tree: %v", err)
	}
	if !strings.Contains(string(script), "75") {
		t.Fatal("docker-entrypoint.sh must keep the exit-code handshake")
	}
}
