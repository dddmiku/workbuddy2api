// ═══ 更新日志 ═══
// 2026-09-18：Windows 明确拒绝 Unix 监听 FD 交接，避免进入不可用的 ExtraFiles 路径。
package hotupdate

import (
	"errors"
	"os"
	"syscall"
)

func duplicateListenerFile(raw syscall.RawConn) (*os.File, error) {
	return nil, errors.New("listener inheritance is unsupported on Windows")
}
