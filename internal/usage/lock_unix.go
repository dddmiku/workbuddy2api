// ═══ 更新日志 ═══
// 2026-09-17：用量账本的跨进程文件锁（flock）。热更新期间新旧进程同时落盘时，
//             读-改-写必须互斥，否则一方的新增量会被另一方覆盖。
//go:build !windows

package usage

import (
	"os"
	"syscall"
)

// lockLedger 对账本旁路文件加排他锁；返回的解锁函数必须调用。
//
// 锁加在 <path>.lock 上而不是账本本身：账本是 tmp + rename 原子替换的，
// rename 之后 inode 会变，锁在被替换掉的旧 inode 上就失去意义。
func lockLedger(path string) (func(), error) {
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
