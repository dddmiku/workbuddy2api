// ═══ 更新日志 ═══
// 2026-09-17：Windows 上不做跨进程锁。网关的正式部署形态是 Linux 容器，
//             本地开发跑单个进程，不存在两进程共用账本的场景。
//go:build windows

package usage

// lockLedger 在 Windows 上是空实现（返回可直接调用的空解锁函数）。
func lockLedger(string) (func(), error) {
	return func() {}, nil
}
