//go:build !windows

// ═══ 更新日志 ═══
// 2026-09-18：锁定交接失败后的监听器非阻塞属性，防止旧进程 Accept 卡死导致关停挂起。
package hotupdate

import (
	"net"
	"syscall"
	"testing"
)

func TestHandoverFilePreservesParentNonblockingMode(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	raw, err := listener.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	// 旧实现会改变共享 open-file-description 的标志；恢复后再关闭以免测试自身挂住。
	defer raw.Control(func(fd uintptr) { _ = syscall.SetNonblock(int(fd), true) })
	file, err := listenerFile(listener)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	_ = file.Fd() // exec.Cmd 在传递 ExtraFiles 时实际调用同一路径。
	var flags uintptr
	var flagErr syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		flags, _, flagErr = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	}); err != nil || flagErr != 0 {
		t.Fatalf("get listener flags: %v %v", err, flagErr)
	}
	if flags&syscall.O_NONBLOCK == 0 {
		t.Fatal("handover cleared O_NONBLOCK on the running listener")
	}
}
