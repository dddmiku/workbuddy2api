// ═══ 更新日志 ═══
// 2026-09-17：新增版本元数据：构建时经 ldflags 注入，供 /healthz、/update/status
//
//	与自更新比对使用。
package version

import "strings"

// 构建期注入（Dockerfile / Makefile 的 -ldflags "-X ...=..."）。未注入时是开发态默认值。
var (
	// Version 语义化版本号，例如 v1.2.0。
	Version = "dev"
	// Commit 构建来源提交（短 SHA 或完整 SHA）。
	Commit = "unknown"
	// BuiltAt 构建时间（RFC3339 或任意可读字符串）。
	BuiltAt = "unknown"
)

// String 返回一行可读的版本描述。
func String() string {
	parts := []string{Version}
	if Commit != "" && Commit != "unknown" {
		commit := Commit
		if len(commit) > 8 {
			commit = commit[:8]
		}
		parts = append(parts, commit)
	}
	if BuiltAt != "" && BuiltAt != "unknown" {
		parts = append(parts, BuiltAt)
	}
	return strings.Join(parts, " ")
}

// IsDev 判断是否为未注入版本的开发构建（自更新对 dev 构建仍允许，但会提示）。
func IsDev() bool { return Version == "" || Version == "dev" }
