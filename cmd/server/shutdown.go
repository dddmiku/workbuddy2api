// ═══ 更新日志 ═══
// 2026-09-18：先停止请求和后台任务，等待处理器退出后再关闭用量、账号池和 Redis，保留最后一笔状态。
// 2026-09-18：在落盘前停止会话 GC，热更新直接退出也不能绕过后台清理。
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
)

type runtimeShutdown struct {
	stopBackground  func()
	backgroundDone  <-chan struct{}
	stopSessions    func()
	waitConnections func()
	closePool       func()
	closeStore      func() error
	shutdownAdmin   func(context.Context) error
	shutdownHTTP    func(context.Context) error
	closeUsage      func() error
}

func drainRuntime(ctx context.Context, services runtimeShutdown) error {
	if services.stopBackground != nil {
		services.stopBackground()
	}
	// 两个入口同时停止接收，管理请求的收尾不能让公共监听器继续接入新任务。
	stopped := make(chan error, 2)
	go func() { stopped <- services.shutdownAdmin(ctx) }()
	go func() { stopped <- services.shutdownHTTP(ctx) }()
	firstErr, secondErr := <-stopped, <-stopped
	if services.waitConnections != nil {
		services.waitConnections()
	}
	if services.backgroundDone != nil {
		<-services.backgroundDone
	}
	if services.stopSessions != nil {
		services.stopSessions()
	}
	usageErr := services.closeUsage()
	services.closePool()
	storeErr := services.closeStore()
	return errors.Join(firstErr, secondErr, usageErr, storeErr)
}

func shutdownHTTPServer(ctx context.Context, server *http.Server) error {
	if err := server.Shutdown(ctx); err != nil {
		// 到达宽限期后关闭连接会取消处理器的请求上下文；调用者仍等待处理器结束再落盘。
		return errors.Join(err, server.Close())
	}
	return nil
}

func trackConnections(wg *sync.WaitGroup) func(net.Conn, http.ConnState) {
	return func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			wg.Add(1)
		case http.StateClosed, http.StateHijacked:
			wg.Done()
		}
	}
}
