// ═══ 更新日志 ═══
// 2026-09-16：废弃会改写业务文本与数字的指纹清洗；旧配置仅保留兼容提示。
package upstream

import (
	"log"
	"sync"
)

var legacySanitizeWarning sync.Once

// warnDeprecatedSanitization 兼容旧配置，不提供内容清洗。
// 上游拒绝原始内容时应返回错误，不能以改写指令、代码或工具数据换取成功。
func warnDeprecatedSanitization(enabled bool) {
	if !enabled {
		return
	}
	legacySanitizeWarning.Do(func() {
		log.Print("WARN: [upstream] sanitize_blacklist_fingerprints is deprecated and ignored; request content is preserved verbatim")
	})
}
