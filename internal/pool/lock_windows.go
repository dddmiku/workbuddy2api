//go:build windows

// ═══ 更新日志 ═══
// 2026-09-18：Windows 用文件区域锁保护状态读改写，与 Unix 端保持相同的跨实例合并边界。
package pool

import (
	"os"
	"syscall"
	"unsafe"
)

var poolKernel = syscall.NewLazyDLL("kernel32.dll")
var poolLockFile = poolKernel.NewProc("LockFileEx")
var poolUnlockFile = poolKernel.NewProc("UnlockFileEx")

func lockPoolState(path string) (func(), error) {
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	operation := &syscall.Overlapped{}
	result, _, failure := poolLockFile.Call(file.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(operation)))
	if result == 0 {
		_ = file.Close()
		return nil, failure
	}
	return func() {
		poolUnlockFile.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(operation)))
		_ = file.Close()
	}, nil
}
