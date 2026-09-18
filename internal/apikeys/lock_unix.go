// ═══ 更新日志 ═══
// 2026-09-18：通过旁路文件锁串行化并存网关实例的密钥修改，原子替换不影响锁身份。
//go:build !windows

package apikeys

import (
	"os"
	"path/filepath"
	"syscall"
)

func lockKeyStore(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
