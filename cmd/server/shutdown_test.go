// ═══ 更新日志 ═══
// 2026-09-18：以真实 HTTP 在途请求断言落盘必须晚于最后一次状态更新，覆盖热更新和信号收尾共用路径。
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownFlushesStateAfterLastInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	requestDone := make(chan struct{})
	var requestFinished atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		requestFinished.Store(true)
		_, _ = io.WriteString(w, "completed")
	}))
	defer server.Close()
	go func() {
		defer close(requestDone)
		resp, err := http.Get(server.URL)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach the handler")
	}
	draining := make(chan struct{})
	done := make(chan error, 1)
	var poolClosed atomic.Bool
	var sessionsStopped atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		done <- drainRuntime(ctx, runtimeShutdown{
			stopSessions: func() { sessionsStopped.Store(true) },
			closePool: func() {
				if !requestFinished.Load() {
					t.Error("pool state flushed before the in-flight request completed")
				}
				poolClosed.Store(true)
			},
			closeStore: func() error {
				if !requestFinished.Load() {
					t.Error("Redis store closed before the in-flight request completed")
				}
				if !poolClosed.Load() {
					t.Error("Redis store closed before the final pool snapshot")
				}
				if !sessionsStopped.Load() {
					t.Error("Redis store closed before session GC stopped")
				}
				return nil
			},
			shutdownAdmin: func(context.Context) error { return nil },
			shutdownHTTP: func(ctx context.Context) error {
				close(draining)
				return server.Config.Shutdown(ctx)
			},
			closeUsage: func() error {
				if !requestFinished.Load() {
					t.Error("usage store closed before the in-flight request completed")
				}
				return nil
			},
		})
	}()
	<-draining
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("drain: %v", err)
	}
	<-requestDone
	if !poolClosed.Load() {
		t.Fatal("final pool state was not persisted")
	}
}

func TestShutdownWaitsForCanceledHandlerAndBackgroundWork(t *testing.T) {
	var connections sync.WaitGroup
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	requestDone := make(chan struct{})
	workerDone := make(chan struct{})
	workerStopped := make(chan struct{})
	var requestFinished atomic.Bool
	var workerFinished atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
		<-release
		requestFinished.Store(true)
	}))
	server.Config.ConnState = trackConnections(&connections)
	server.Start()
	defer server.Close()
	go func() {
		defer close(requestDone)
		resp, err := http.Get(server.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- drainRuntime(ctx, runtimeShutdown{
			stopBackground: func() { close(workerStopped) },
			backgroundDone: workerDone, waitConnections: connections.Wait,
			closePool: func() {
				if !requestFinished.Load() || !workerFinished.Load() {
					t.Error("pool closed before canceled handler and worker exited")
				}
			},
			closeStore:    func() error { return nil },
			shutdownAdmin: func(context.Context) error { return nil },
			shutdownHTTP: func(ctx context.Context) error {
				return shutdownHTTPServer(ctx, server.Config)
			},
			closeUsage: func() error {
				if !requestFinished.Load() || !workerFinished.Load() {
					t.Error("usage closed before canceled handler and worker exited")
				}
				return nil
			},
		})
	}()
	<-workerStopped
	<-canceled
	close(release)
	workerFinished.Store(true)
	close(workerDone)
	if err := <-done; err == nil {
		t.Fatal("forced HTTP shutdown must preserve its timeout error")
	}
	<-requestDone
}
