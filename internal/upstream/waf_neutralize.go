// ═══ 更新日志 ═══
// 2026-09-17：新增上游 WAF 触发模式的中性化，供被拦截请求断词重试。
package upstream

import (
	"encoding/json"
	"regexp"

	"workbuddy2api/internal/jsonutil"
)

// wafBreakMarker 断词符：零宽空格。
//
// 它不参与词义、渲染不可见，但会切断上游 WAF 的字面匹配。选择它而不是删除或
// 改写字符，是为了让模型侧仍能读到同一段文本。
const wafBreakMarker = "\u200b"

// wafTextPatterns 已实测会让上游 WAF 返回 403 拦截页的文本形态。
//
// 依据：2026-09-17 对国际版上游（www.workbuddy.ai）逐条对照探测。命中的是脚本
// 注入、事件处理器、SQL 注入式表达式、命令注入片段与 `%3C`/`\x3C` 编码变体；
// 普通正文、纯标签结构、Markdown 代码块、XML 声明、`information_schema`
// 本身都不触发。CN 链路上同一批正文全部 200，说明拦截来自国际版前置 WAF，
// 而不是模型侧内容审核。
var wafTextPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bscript\b`),
	regexp.MustCompile(`(?i)%3c\s*/?\s*script`),
	regexp.MustCompile(`(?i)\\x3c\s*/?\s*script`),
	regexp.MustCompile(`(?i)\balert\b`),
	regexp.MustCompile(`(?i)\bjavascript\b`),
	regexp.MustCompile(`(?i)\bon(error|load|focus|click|submit|mouseover)\b`),
	regexp.MustCompile(`(?i)\beval\b`),
	regexp.MustCompile(`(?i)\bunion\b`),
	regexp.MustCompile(`(?i)\bjndi\b`),
	regexp.MustCompile(`(?i)\bxp_cmdshell\b`),
	regexp.MustCompile(`(?i)\bbenchmark\b`),
	regexp.MustCompile(`(?i)\bsleep\b`),
	regexp.MustCompile(`(?i)\bexec\b`),
	regexp.MustCompile(`(?i)\bcurl\s+https?://`),
	regexp.MustCompile(`(?i)\bdrop\s+(table|procedure)\b`),
	regexp.MustCompile(`(?i)\b(or|and)\s+['"]?\d+\s*['"]?\s*=\s*['"]?\d+`),
	regexp.MustCompile(`(?i)<\s*\?\s*php`),
}

// NeutralizeWAFTriggers 在「已确认被上游 WAF 拦截」的请求体上断开触发模式。
//
// 语义约定：
//   - 只在每个命中片段的第一个 ASCII 字母之后插入一个零宽空格；不删除、不改写
//     任何字符，模型侧读到的是同一段文本。
//   - 调用点仅限原样请求已被 403 拦截之后（见 ChatStreamContext 的重试分支），
//     正常请求的正文不经过这里。
//   - body 不是 JSON 对象、或没有命中任何模式时原样返回，第二个返回值为 false，
//     调用方据此决定是否重发。
//
// 之所以先解析 JSON 再改：出站 body 已由 prepareBody 重排过一次，这里再走一遍
// jsonutil（保留数字原值）不引入新的结构变化；而在 JSON 文本层直接插入字节会
// 有踩到 `\u003c` 这类转义序列的风险。
func NeutralizeWAFTriggers(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body, false
	}
	changed := false
	obj = neutralizeJSONValue(obj, &changed)
	if !changed {
		return body, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// neutralizeJSONValue 递归遍历解码后的 JSON，对每个字符串做断词。
func neutralizeJSONValue(value any, changed *bool) any {
	switch node := value.(type) {
	case string:
		return neutralizeWAFText(node, changed)
	case []any:
		for i, item := range node {
			node[i] = neutralizeJSONValue(item, changed)
		}
		return node
	case map[string]any:
		for key, item := range node {
			node[key] = neutralizeJSONValue(item, changed)
		}
		return node
	default:
		return value
	}
}

// neutralizeWAFText 在单个字符串内断开全部命中片段，报告是否发生改动。
func neutralizeWAFText(text string, changed *bool) string {
	if text == "" {
		return text
	}
	// 插入点用「原字符串中的字节偏移」表示，先收集再统一重建，避免边插边扫。
	insertAt := map[int]struct{}{}
	for _, pattern := range wafTextPatterns {
		for _, match := range pattern.FindAllStringIndex(text, -1) {
			offset := firstLetterOffset(text[match[0]:match[1]])
			if offset < 0 {
				continue
			}
			insertAt[match[0]+offset+1] = struct{}{}
		}
	}
	if len(insertAt) == 0 {
		return text
	}
	points := make([]int, 0, len(insertAt))
	for pos := range insertAt {
		points = append(points, pos)
	}
	sortInts(points)
	out := make([]byte, 0, len(text)+len(points)*len(wafBreakMarker))
	prev := 0
	for _, pos := range points {
		if pos <= prev || pos >= len(text) {
			continue
		}
		out = append(out, text[prev:pos]...)
		out = append(out, wafBreakMarker...)
		prev = pos
	}
	out = append(out, text[prev:]...)
	*changed = true
	return string(out)
}

// firstLetterOffset 返回片段中第一个 ASCII 字母的字节偏移；没有字母时返回 -1。
func firstLetterOffset(fragment string) int {
	for i := 0; i < len(fragment); i++ {
		c := fragment[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return i
		}
	}
	return -1
}

// sortInts 小整数切片升序（避免为一处排序引入新依赖与通用排序开销）。
func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		value := values[i]
		j := i - 1
		for j >= 0 && values[j] > value {
			values[j+1] = values[j]
			j--
		}
		values[j+1] = value
	}
}
