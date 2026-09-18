// ═══ 更新日志 ═══
// 2026-09-18：Windows 使用文件区域锁保护完整读改写，避免多实例开发与测试互相覆盖。
//go:build windows

package usage

import (
	"os"
	"syscall"
	"unsafe"
)

var ledgerKernel = syscall.NewLazyDLL("kernel32.dll")
var ledgerLockFile = ledgerKernel.NewProc("LockFileEx")
var ledgerUnlockFile = ledgerKernel.NewProc("UnlockFileEx")

func lockLedger(path string) (func(), error) {
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	operation := &syscall.Overlapped{}
	result, _, failure := ledgerLockFile.Call(file.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(operation)))
	if result == 0 {
		file.Close()
		return nil, failure
	}
	return func() {
		ledgerUnlockFile.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(operation)))
		_ = file.Close()
	}, nil
}
