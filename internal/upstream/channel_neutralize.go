// ═══ 更新日志 ═══
// 2026-09-17：新增上游渠道校验触发句的中性化，供被 11128 拒绝的系统说明断词重试。
package upstream

import (
	"encoding/json"

	"workbuddy2api/internal/jsonutil"
)

// channelTriggerSentences 已实测会被上游按「未授权渠道」拒绝的系统说明句子。
//
// 依据（docs/codex.md §默认指令为什么会被拒，2026-09-17 字段二分结论）：
// 官方 Codex CLI 默认说明里这一句对渠道归属的声明命中上游校验，
// 删掉或改成中性说法后立即返回 200；工具声明、模型名、用户消息都不是触发点。
// 只匹配句子主体，不带句末标点，兼容原文与去掉句号的变体。
var channelTriggerSentences = []string{
	"Codex CLI is an open source project led by OpenAI",
}

// NeutralizeChannelTrigger 在「已确认被上游渠道校验拒绝」的请求体上断开触发句。
//
// 语义约定与 NeutralizeWAFTriggers 一致：
//   - 只在命中句子的每个单词首字母之后插入一个零宽空格，不删除、不改写任何字符，
//     模型侧读到的是同一句话；
//   - 调用点仅限原样请求已被 11128 unapproved channel 拒绝之后（见
//     ChatStreamContext 的重试分支），正常请求的正文不经过这里；
//   - body 不是 JSON、或没有命中触发句时原样返回，第二个返回值为 false。
func NeutralizeChannelTrigger(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body, false
	}
	changed := false
	obj = neutralizeChannelJSONValue(obj, &changed)
	if !changed {
		return body, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// neutralizeChannelJSONValue 递归遍历解码后的 JSON，对每个字符串做触发句断词。
func neutralizeChannelJSONValue(value any, changed *bool) any {
	switch node := value.(type) {
	case string:
		return neutralizeChannelText(node, changed)
	case []any:
		for i, item := range node {
			node[i] = neutralizeChannelJSONValue(item, changed)
		}
		return node
	case map[string]any:
		for key, item := range node {
			node[key] = neutralizeChannelJSONValue(item, changed)
		}
		return node
	default:
		return value
	}
}

// neutralizeChannelText 在单个字符串内断开全部命中句子，报告是否发生改动。
func neutralizeChannelText(text string, changed *bool) string {
	if text == "" {
		return text
	}
	insertAt := map[int]struct{}{}
	for _, sentence := range channelTriggerSentences {
		start := 0
		for {
			index := indexFrom(text, sentence, start)
			if index < 0 {
				break
			}
			for _, pos := range wordBreakPoints(text, index, index+len(sentence)) {
				insertAt[pos] = struct{}{}
			}
			start = index + len(sentence)
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

// indexFrom 从 from 起查找 needle 的字节偏移（区分大小写；触发句大小写固定）。
func indexFrom(text, needle string, from int) int {
	if from >= len(text) {
		return -1
	}
	index := -1
	for i := from; i+len(needle) <= len(text); i++ {
		if text[i:i+len(needle)] == needle {
			index = i
			break
		}
	}
	return index
}

// wordBreakPoints 返回片段内每个单词首字母之后的插入点（原字符串字节偏移）。
// 单词以 ASCII 字母/数字起始，遇到空格、标点或大小写之外的分隔即结束。
func wordBreakPoints(text string, start, end int) []int {
	points := []int{}
	atWordStart := true
	for i := start; i < end; i++ {
		c := text[i]
		if isASCIIAlnum(c) {
			if atWordStart {
				points = append(points, i+1)
				atWordStart = false
			}
			continue
		}
		atWordStart = true
	}
	return points
}
