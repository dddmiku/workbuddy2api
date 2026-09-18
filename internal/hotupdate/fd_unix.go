//go:build !windows

// ═══ 更新日志 ═══
// 2026-09-18：复制供 exec 继承的监听 FD，保持原监听器非阻塞且避免并发 fork 泄漏描述符。
package hotupdate

import (
	"os"
	"syscall"
)

func duplicateListenerFile(raw syscall.RawConn) (*os.File, error) {
	fd := -1
	var dupErr error
	err := raw.Control(func(original uintptr) {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()
		fd, dupErr = syscall.Dup(int(original))
		if dupErr == nil {
			syscall.CloseOnExec(fd)
		}
	})
	if err != nil {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		return nil, err
	}
	if dupErr != nil {
		return nil, dupErr
	}
	// net.Listener.File() 返回的包装器会在 Fd() 时切成阻塞；直接 NewFile 保留
	// 传入 FD 的非阻塞属性，exec 读取 ExtraFiles 时便不会修改旧进程的监听器。
	return os.NewFile(uintptr(fd), "handover-listener"), nil
}
