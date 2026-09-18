// ═══ 更新日志 ═══
// 2026-09-18：覆盖未经过 New 的 Run 初始化和手动任务退出等待，防止零值崩溃及上下文被重置。
package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSchedulerRunInitializesZeroLifecycleAndWaitsForManualTask(t *testing.T) {
	fake := &zeroLifecycleScript{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	original := newScriptCmd
	newScriptCmd = func(string, ...string) scriptRunner { return fake }
	defer func() { newScriptCmd = original }()
	scheduler := &Scheduler{}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &runtimeObservedContext{Context: base, entered: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fake.release) }) }
	defer release()
	stopped := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		release()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("scheduler cleanup did not complete")
		}
	}()
	// AfterFunc 首次读取 Done 时，Run 的锁内初始化必须已经完成。
	select {
	case <-ctx.entered:
	case <-time.After(time.Second):
		t.Fatal("Run did not register cancellation")
	}
	if err := scheduler.TriggerTask(string(TaskKeySchool)); err != nil {
		t.Fatalf("manual task after Run initialization: %v", err)
	}
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("manual task did not start")
	}
	cancel()
	select {
	case <-fake.canceled:
	case <-time.After(time.Second):
		t.Fatal("zero-value lifecycle did not receive cancellation")
	}
	select {
	case <-stopped:
		t.Fatal("Run returned before the accepted manual task finished cancellation cleanup")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Run did not wait for the canceled manual task")
	}
	for _, task := range scheduler.TaskSnapshot() {
		if task.Running {
			t.Fatal("Run returned while a manual task was still marked running")
		}
	}
	if err := scheduler.TriggerTask(string(TaskKeySchool)); err != ErrSchedulerStopped {
		t.Fatalf("stopped scheduler accepted a manual task: %v", err)
	}
}

type zeroLifecycleScript struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (*zeroLifecycleScript) SetDir(string) {}

func (s *zeroLifecycleScript) Run() error {
	close(s.started)
	<-s.release
	return nil
}

func (s *zeroLifecycleScript) RunContext(ctx context.Context) error {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	<-s.release
	return ctx.Err()
}

func TestSchedulerRunPreservesExistingLifecycle(t *testing.T) {
	scheduler := runtimeDisabledScheduler()
	original := scheduler.lifecycle
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scheduler.Run(ctx)
	if scheduler.lifecycle != original {
		t.Fatal("Run replaced the lifecycle used by previously accepted manual tasks")
	}
	if original.Err() != context.Canceled {
		t.Fatalf("existing lifecycle was not canceled: %v", original.Err())
	}
}
