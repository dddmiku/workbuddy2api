// ═══ 更新日志 ═══
// 2026-09-18：Windows 使用文件区域锁保护密钥读改写，保持多实例更新语义。
//go:build windows

package apikeys

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var keyKernel = syscall.NewLazyDLL("kernel32.dll")
var keyLockFile = keyKernel.NewProc("LockFileEx")
var keyUnlockFile = keyKernel.NewProc("UnlockFileEx")

func lockKeyStore(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	operation := &syscall.Overlapped{}
	result, _, failure := keyLockFile.Call(file.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(operation)))
	if result == 0 {
		file.Close()
		return nil, failure
	}
	return func() { keyUnlockFile.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(operation))); _ = file.Close() }, nil
}
