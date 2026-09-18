//go:build !windows

// ═══ 更新日志 ═══
// 2026-09-18：状态文件读改写共用旁路文件锁，避免热交接及多实例并发写入互相覆盖。
package pool

import (
	"os"
	"syscall"
)

func lockPoolState(path string) (func(), error) {
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
