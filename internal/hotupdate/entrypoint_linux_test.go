// ═══ 更新日志 ═══
// 2026-09-18：以隔离子进程模拟容器 PID 1，复现热更新后 TERM 不转发和新实例退出无人监督。
// 2026-09-18：覆盖配置自定义更新目录、状态目录及环境覆盖，保证重启仍选择已更新的实例。
package hotupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type entrypointProcess struct {
	command *exec.Cmd
	child   int
	stopped string
	done    <-chan struct{}
	waitErr *error
}

func startEntrypointFixture(t *testing.T) entrypointProcess {
	return startConfiguredEntrypoint(t, "")
}

func startConfiguredEntrypoint(t *testing.T, mode string) entrypointProcess {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is required to establish an isolated Linux subreaper")
	}
	root := t.TempDir()
	body, err := os.ReadFile(filepath.Join("..", "..", "docker-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "entrypoint.sh")
	fallback := filepath.Join(root, "unused-image-binary")
	if err := os.WriteFile(fallback, []byte("#!/bin/sh\n# ═══ 更新日志 ═══\n# 2026-09-18：测试意外回退时只退出，绝不启动真实服务。\nexit 64\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	entryBody := strings.ReplaceAll(string(body), "\r\n", "\n")
	entryBody = strings.ReplaceAll(entryBody, "IMAGE_BIN=\"/app/wb2api\"", "IMAGE_BIN=\""+fallback+"\"")
	if err := os.WriteFile(entry, []byte(entryBody), 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "updated-instance")
	pidFile := filepath.Join(root, "updated.pid")
	stopFile := filepath.Join(root, "updated.stopped")
	header := "#!/bin/sh\n# ═══ 更新日志 ═══\n# 2026-09-18：隔离监督测试，仅操作本用例子进程与临时文件。\n"
	childBody := header +
		"trap 'printf stopped > \"$WB2API_TEST_EXIT\"; exit 0' TERM\n" +
		"printf '%s\\n' \"$$\" > \"$WB2API_TEST_PID\"\n" +
		"while :; do sleep 0.05; done\n"
	if err := os.WriteFile(child, []byte(childBody), 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "old-instance")
	if err := os.WriteFile(launcher, []byte(header+"\"$WB2API_TEST_NEXT\" &\nexit 75\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	updateDir := root
	var serverArgs []string
	var directoryEnv []string
	if mode == "" {
		directoryEnv = append(directoryEnv, "WB2API_UPDATE_DIR="+root)
	} else {
		configPath := filepath.Join(root, "config.json")
		config := map[string]any{}
		switch mode {
		case "update-dir":
			updateDir = filepath.Join(root, "custom-updates")
			config["update"] = map[string]any{"dir": updateDir}
			serverArgs = []string{"-config=" + configPath}
		case "state-dir", "state-env", "state-relative":
			statePath := filepath.Join(root, "custom-state", "state.json")
			updateDir = filepath.Join(filepath.Dir(statePath), "updates")
			config["state_file"] = statePath
			if mode == "state-env" {
				config["state_file"] = filepath.Join(root, "ignored-state", "state.json")
				directoryEnv = append(directoryEnv, "WB2A_STATE_FILE="+statePath)
			}
			if mode == "state-relative" {
				config["state_file"] = "nested/../state.json"
				updateDir = filepath.Join(root, "data", "updates")
			}
			serverArgs = []string{"-config", configPath}
		default:
			t.Fatalf("unknown fixture mode %q", mode)
		}
		raw, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(updateDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(updateDir, "current"), []byte(launcher+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// PR_SET_CHILD_SUBREAPER 让孤儿后继归当前脚本，重现容器 PID 1 的收养语义；
	// 独立进程组确保测试清理只能触达自己创建的进程。
	code := "import ctypes,os,sys; assert ctypes.CDLL(None).prctl(36,1,0,0,0)==0; os.execv('/bin/sh',['sh']+sys.argv[1:])"
	cmd := exec.Command(python, append([]string{"-c", code, entry}, serverArgs...)...)
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "WB2API_UPDATE_DIR=") || strings.HasPrefix(item, "WB2A_STATE_FILE=") {
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	cmd.Env = append(cmd.Env, directoryEnv...)
	cmd.Env = append(cmd.Env, "WB2API_TEST_NEXT="+child,
		"WB2API_TEST_PID="+pidFile, "WB2API_TEST_EXIT="+stopFile)
	logFile, err := os.Create(filepath.Join(root, "supervisor.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		_ = logFile.Close()
	})
	var childPID int
	for i := 0; i < 200; i++ {
		select {
		case <-done:
			t.Fatalf("supervisor exited before selecting the configured update: %v", waitErr)
		default:
		}
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", childPID))
			for _, line := range strings.Split(string(status), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[0] == "PPid:" && fields[1] == strconv.Itoa(cmd.Process.Pid) {
					return entrypointProcess{cmd, childPID, stopFile, done, &waitErr}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("updated process was not adopted by the supervisor: child=%d", childPID)
	return entrypointProcess{}
}

func TestEntrypointUsesConfiguredUpdateDirectory(t *testing.T) {
	for _, mode := range []string{"update-dir", "state-dir", "state-env", "state-relative"} {
		t.Run(mode, func(t *testing.T) {
			startConfiguredEntrypoint(t, mode)
		})
	}
}

func TestEntrypointForwardsTERMToUpdatedInstance(t *testing.T) {
	process := startEntrypointFixture(t)
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(process.stopped); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("updated instance never received TERM from the supervisor")
}

func TestEntrypointExitsWhenUpdatedInstanceDies(t *testing.T) {
	process := startEntrypointFixture(t)
	if err := syscall.Kill(process.child, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.done:
		if *process.waitErr == nil {
			t.Fatal("unexpected successor exit must fail the supervisor so container restart policy applies")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor stayed alive after the updated instance exited")
	}
}
