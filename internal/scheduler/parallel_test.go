// ═══ 更新日志 ═══
// 2026-09-17：保留取消等待回归，并按既有任务日志隔离契约验证批量任务全部完成。
package scheduler

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// waitCalls 轮询等待计数达到 want（deadline 内），超时返回 false。
func waitCalls(c *atomic.Int32, want int32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for c.Load() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return c.Load() >= want
}

// ---------------------------------------------------------------------------
// sleepCtx 可取消等待
// ---------------------------------------------------------------------------

// TestSleepCtxZeroImmediate d<=0 立即放行（兼容测试把延迟置 0 的用法）。
func TestSleepCtxZeroImmediate(t *testing.T) {
	start := time.Now()
	if !sleepCtx(context.Background(), 0) {
		t.Error("d=0 应立即返回 true")
	}
	if e := time.Since(start); e > 50*time.Millisecond {
		t.Errorf("d=0 不应等待，耗时 %v", e)
	}
}

// TestSleepCtxElapsed 等满 d 后返回 true。
func TestSleepCtxElapsed(t *testing.T) {
	start := time.Now()
	if !sleepCtx(context.Background(), 50*time.Millisecond) {
		t.Error("等满应返回 true")
	}
	if e := time.Since(start); e < 40*time.Millisecond {
		t.Errorf("应等满约 50ms，实际 %v", e)
	}
}

// TestSleepCtxCancelled 等待期间取消 ctx：立即返回 false（不等 d 醒来）。
func TestSleepCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if sleepCtx(ctx, 2*time.Second) {
		t.Error("取消后应返回 false")
	}
	if e := time.Since(start); e > 1*time.Second {
		t.Errorf("取消后应快速返回（远小于 2s），实际 %v", e)
	}
}

// ---------------------------------------------------------------------------
// 账号间限速可取消：正在 sleep 的遍历随 ctx 取消立即退出
// ---------------------------------------------------------------------------

// TestRunActivityCtxCancelsDuringAccountDelay 活跃上报账号间限速（2s）中取消
// ctx：runActivity 立即退出，第 2 号不再上报。串行 time.Sleep 版本要等满 2s。
func TestRunActivityCtxCancelsDuringAccountDelay(t *testing.T) {
	oldDelay, oldGap := activityAccountDelay, activityReportGap
	activityAccountDelay, activityReportGap = 2*time.Second, 0
	t.Cleanup(func() {
		activityAccountDelay, activityReportGap = oldDelay, oldGap
	})

	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.runActivity(ctx)
		close(done)
	}()

	// 等第 1 号上报完成（随后的账号间限速等待中取消）。
	if !waitCalls(&stub.calls, 1, 3*time.Second) {
		t.Fatal("3s 内未观察到第 1 号上报")
	}
	cancel()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Error("runActivity 未在取消后 1.5s 内退出（账号间限速 sleep 不可取消）")
		select { // 排干 goroutine，避免泄漏与后续断言竞争
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	if e := time.Since(start); e > 1200*time.Millisecond {
		t.Errorf("取消后退出耗时 %v（应远小于 2s 的账号间限速）", e)
	}
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("report calls=%d want 1（取消后 u2 不应上报）", n)
	}
}

// TestRunTravelCtxCancelsDuringAccountDelay 旅行账号间限速（2s）中取消 ctx：
// runTravel 立即退出，第 2 号不再巡检。
func TestRunTravelCtxCancelsDuringAccountDelay(t *testing.T) {
	old := travelAccountDelay
	travelAccountDelay = 2 * time.Second
	t.Cleanup(func() { travelAccountDelay = old })

	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1", "u2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.runTravel(ctx)
		close(done)
	}()

	if !waitCalls(&stub.infoCalls, 1, 3*time.Second) {
		t.Fatal("3s 内未观察到第 1 号巡检")
	}
	cancel()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Error("runTravel 未在取消后 1.5s 内退出（账号间限速 sleep 不可取消）")
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	if e := time.Since(start); e > 1200*time.Millisecond {
		t.Errorf("取消后退出耗时 %v（应远小于 2s 的账号间限速）", e)
	}
	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1（取消后 u2 不应巡检）", n)
	}
}

// 同时到期的任务都要执行；当前日志接收器需要串行执行，避免任务日志互相覆盖。
func TestRunBatchCompletesKindsWithIsolatedLogs(t *testing.T) {
	fastActivity(t)
	fastTravel(t)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	var entries, active, maximum atomic.Int32
	var checkinCalls, reportCalls atomic.Int32
	enter := func(marker string) {
		now := active.Add(1)
		for old := maximum.Load(); now > old && !maximum.CompareAndSwap(old, now); old = maximum.Load() {
		}
		if entries.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
		} else {
			close(secondStarted)
		}
		log.Print(marker)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			checkinCalls.Add(1)
			enter("fixture_checkin_only")
			defer active.Add(-1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/v2/report"):
			reportCalls.Add(1)
			enter("fixture_activity_only")
			defer active.Add(-1)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()
	// 必须先释放握手，再等待HTTP服务退出，避免失败路径挂住测试。
	defer release()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(Config{Pool: p, Upstream: up})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { scheduler.runBatch(ctx, []taskKind{taskCheckin, taskActivity}); close(done) }()
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first task did not start")
	}
	select {
	case <-secondStarted:
		t.Error("task operations overlapped while the first task held its log context")
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("batch did not complete")
	}
	if checkinCalls.Load() != 1 || reportCalls.Load() != 1 || maximum.Load() != 1 {
		t.Fatalf("checkin=%d activity=%d concurrent=%d", checkinCalls.Load(), reportCalls.Load(), maximum.Load())
	}
	for _, test := range []struct{ key, own, other string }{{"checkin", "fixture_checkin_only", "fixture_activity_only"}, {"activity", "fixture_activity_only", "fixture_checkin_only"}} {
		lines, err := scheduler.TaskLog(test.key)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(lines, "\n")
		if !strings.Contains(text, test.own) || strings.Contains(text, test.other) {
			t.Errorf("%s task log mixed or lost operation output: %s", test.key, text)
		}
	}
}
