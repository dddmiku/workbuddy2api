// truncation.go 工具调用的残缺参数检测（吸收参考仓库 sse.ts:158-167
// isTruncatedArguments 语义）。
//
// 背景：SSE 流被截断（连接中断 / finish_reason==length）时，工具调用的 arguments
// 会只剩半截 JSON。此时网关若把脏参数原样交给客户端，客户端解析会报非法 JSON 并卡死会话。
// 参考仓库的处置是丢弃残缺调用并触发重试（报告 max-tokens），而非补成 {} 伪造合法外观。
//
// 关键区分：只把「非空但无法解析」视为截断。空串是合法的无参数工具；能解析但类型不对
// （标量 / 数组）属于模型输出错误，交给客户端 schema 校验回传即可，不在此判定。
// ═══ 更新日志 ═══
// 2026-09-16：结束校验覆盖所有成功终态；只检验 JSON 语法，避免把大数等合法参数误判为残缺。
// 2026-09-16：不完整终态也过滤完全缺失或类型错误的参数，显式空字符串继续保留。
package upstream

import (
	"encoding/json"
	"strings"
)

// isTruncatedArguments 判定工具参数字符串是否因分片丢失而残缺（区别于「该工具本就无参数」）。
//   - 空串 / 纯空白 → false（合法无参工具）；
//   - 非空但 JSON 解析失败 → true（截断）；
//   - 能解析（含 null/标量/数组等任何合法 JSON）→ false。
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	return !json.Valid([]byte(trimmed))
}

// dropTruncatedToolCalls 过滤出 arguments 完整的 tool_call（返回新 slice）。
// 只依据 isTruncatedArguments 判定，不改动任何保留的调用（正例零改动）。
func dropTruncatedToolCalls(calls []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		args, exists := fn["arguments"].(string)
		if !exists || isTruncatedArguments(args) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}
