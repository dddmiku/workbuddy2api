// ═══ 更新日志 ═══
// 2026-09-18：复现 GC 启动后立即停止仍继续删除绑定，锁定关闭后的后台生命周期。
package session

import (
	"runtime"
	"testing"
	"time"
)

func TestRuntimeGCDoesNotRunAfterImmediateStop(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	r := New(Config{TTL: time.Millisecond, GCInterval: time.Millisecond})
	r.StartGC()
	r.StopGC()
	r.Bind("after-stop", "account-runtime")
	time.Sleep(20 * time.Millisecond)
	if r.Count() != 1 {
		t.Fatal("GC kept running and removed a binding after StopGC returned")
	}
}
