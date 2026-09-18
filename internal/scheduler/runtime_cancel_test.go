// ═══ 更新日志 ═══
// 2026-09-18：验证关停取消能阻止排队任务及终止活动脚本，避免旧进程继续执行后台写操作。
package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestRuntimeCanceledSchedulerDoesNotStartScript(t *testing.T) {
	f := installFakeExec(t)
	s := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.dispatch(ctx, taskSchool)
	if f.runN != 0 {
		t.Fatal("an already canceled scheduler started a script task")
	}
}

type runtimeObservedContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *runtimeObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func runtimeDisabledScheduler() *Scheduler {
	return New(Config{
		CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
		RedeemDisabled: true, LotteryDisabled: true, MakeupDisabled: true,
	})
}

func TestRuntimeSchedulerShutdownWaitsForManualTask(t *testing.T) {
	f := &runtimeCancelableScript{started: make(chan struct{}), unblock: make(chan struct{})}
	original := newScriptCmd
	newScriptCmd = func(string, ...string) scriptRunner { return f }
	defer func() { newScriptCmd = original }()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &runtimeObservedContext{Context: base, entered: make(chan struct{})}
	s := runtimeDisabledScheduler()
	stopped := make(chan struct{})
	go func() { s.Run(ctx); close(stopped) }()
	<-ctx.entered
	if err := s.TriggerTask(string(TaskKeySchool)); err != nil {
		t.Fatal(err)
	}
	<-f.started
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		close(f.unblock)
		t.Fatal("scheduler shutdown did not complete")
	}
	running := false
	for _, state := range s.TaskSnapshot() {
		if state.Key == string(TaskKeySchool) {
			running = state.Running
		}
	}
	close(f.unblock)
	if running {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			busy := false
			for _, state := range s.TaskSnapshot() {
				busy = busy || state.Running
			}
			if !busy {
				break
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("scheduler exited while a manually triggered task was still running")
	}
	if err := s.TriggerTask(string(TaskKeySchool)); err == nil {
		t.Fatal("stopped scheduler accepted another manual task")
	}
}

func TestRuntimeAccountTasksStopAfterCancellation(t *testing.T) {
	for _, kind := range []taskKind{taskCheckin, taskKeepalive, taskTravel, taskActivity} {
		t.Run(string(kind.key()), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				cancel()
				if kind == taskKeepalive {
					fmt.Fprint(w, `{"code":0,"data":{"accessToken":"fixture-access","refreshToken":"fixture-refresh","expiresIn":3600}}`)
				} else if kind == taskCheckin {
					fmt.Fprint(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`)
				} else {
					fmt.Fprint(w, `{"code":0,"data":{"buddy":null,"streak":{"days":0}}}`)
				}
			}))
			defer server.Close()
			p := pool.New("")
			for _, uid := range []string{"account-a", "account-b"} {
				p.Add(&auth.Auth{UID: uid, AccessToken: "fixture-access", RefreshToken: "fixture-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()})
			}
			s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: server.Client(), ChatBaseCN: server.URL, BillingBaseCN: server.URL}})
			s.dispatch(ctx, kind)
			if calls.Load() != 1 {
				t.Fatalf("canceled account task kept issuing requests: calls=%d", calls.Load())
			}
		})
	}
}

func TestRuntimeScriptChild(t *testing.T) {
	if os.Getenv("WB2A_RUNTIME_TEST_CHILD") != "1" {
		return
	}
	time.Sleep(10 * time.Second)
}

func TestRuntimeScriptProcessTerminatesOnCancellation(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeScriptChild$")
	cmd.Env = append(os.Environ(), "WB2A_RUNTIME_TEST_CHILD=1")
	runner := &scriptCmd{cmd: cmd}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := runner.RunContext(ctx); err == nil || ctx.Err() == nil {
		t.Fatal("script process did not stop because of cancellation")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("canceled script process continued running")
	}
}

type runtimeCancelableScript struct {
	started chan struct{}
	unblock chan struct{}
}

func (*runtimeCancelableScript) SetDir(string) {}
func (s *runtimeCancelableScript) Run() error  { close(s.started); <-s.unblock; return nil }
func (s *runtimeCancelableScript) RunContext(ctx context.Context) error {
	close(s.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.unblock:
		return nil
	}
}

func TestRuntimeSchedulerCancelsActiveScript(t *testing.T) {
	f := &runtimeCancelableScript{started: make(chan struct{}), unblock: make(chan struct{})}
	original := newScriptCmd
	newScriptCmd = func(string, ...string) scriptRunner { return f }
	defer func() { newScriptCmd = original }()
	s := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.dispatch(ctx, taskSchool); close(done) }()
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("script did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		close(f.unblock)
		<-done
		t.Fatal("scheduler cancellation was not passed to the active script")
	}
}
