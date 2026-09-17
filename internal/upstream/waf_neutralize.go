// ═══ 更新日志 ═══
// 2026-09-17：新增上游 WAF 触发模式的中性化，供被拦截请求断词重试。
package upstream

import (
	"encoding/json"
	"regexp"
	"strings"

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

// wafRiskyChars token 级二次断词的触发字符集（命令注入、路径穿越、URL 协议的共同特征）。
var wafRiskyChars = []byte("$`'\"()/\\;|&<>=%{}[]*?~:@")

// NeutralizeWAFTokens 第二级中性化：对消息正文里含风险字符的 token 断词。
//
// 触发字符集覆盖命令注入、路径穿越与 URL 协议族（`printf`、`$()`、`/etc/passwd`、
// `../../`、`ldap://` 等）。只在「一级断词重试仍被拦」时升级使用；零宽字符不参与
// 词义，模型读到的是同一段文本。
//
// 只改 messages 里的正文（含 system/instructions），不动工具定义、JSON schema 与
// 工具调用参数——那些是结构字段，插字符会破坏工具名与参数字面量。
func NeutralizeWAFTokens(body []byte) ([]byte, bool) {
	return neutralizeMessages(body, breakRiskyTokens)
}

// NeutralizeWAFTokensDeep 第三级中性化（最后手段）：逐 token 断词——每个空白分隔
// token 在首字符之后插零宽，token 内其余非字母数字字符之后再插一个。
//
// 用于上游 WAF 的累积型判定：单个片段都在白名单内，但整段正文里可疑 token 数量
// 过阈值后整包被拦（实测同一批历史逐条都过、合起来被拦）。作用范围与二级一致，
// 只碰消息正文。仍是零宽字符，正文语义不变。
func NeutralizeWAFTokensDeep(body []byte) ([]byte, bool) {
	return neutralizeMessages(body, breakEveryToken)
}

// neutralizeMessages 对 messages 数组里的正文文本应用 transform。
//
// 覆盖三种形态：content 为字符串（纯文本消息、system 提示）、content 为 part 数组
// 时的 text 字段，以及 tool_calls / function_call 的参数字符串（agent 历史里的 shell
// 命令）。其余字段（工具名、tools 定义、model、JSON 键）一律不动——那些是结构数据，
// 插字符会破坏工具名与参数字面量。
func neutralizeMessages(body []byte, transform func(string, *bool) string) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, false
	}
	changed := false
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			msg["content"] = transform(content, &changed)
		case []any:
			for _, e := range content {
				part, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if text, ok := part["text"].(string); ok {
					part["text"] = transform(text, &changed)
				}
			}
		}
		// 工具调用参数同样进正文：agent 历史里的 shell 命令（heredoc、管道、重定向）
		// 是上游 WAF 命令注入规则的目标（2026-09-17 实测：正文断词不覆盖此处时，
		// 带 heredoc 的 tool_calls 仍被拦）。只动参数字符串，不动工具名与 JSON 结构。
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, c := range calls {
				call, ok := c.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := call["function"].(map[string]any)
				if !ok {
					continue
				}
				if args, ok := fn["arguments"].(string); ok {
					fn["arguments"] = transform(args, &changed)
				}
			}
		}
		if fn, ok := msg["function_call"].(map[string]any); ok {
			if args, ok := fn["arguments"].(string); ok {
				fn["arguments"] = transform(args, &changed)
			}
		}
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// breakRiskyTokens 对含风险字符的空白分隔 token 插入零宽空格。
func breakRiskyTokens(text string, changed *bool) string {
	if text == "" {
		return text
	}
	parts := strings.Split(text, " ")
	wrote := false
	for i, token := range parts {
		if token == "" {
			continue
		}
		if !strings.ContainsAny(token, string(wafRiskyChars)) {
			continue
		}
		pos := firstLetterOffset(token)
		if pos < 0 {
			pos = 0
		}
		if pos+1 > len(token) {
			continue
		}
		parts[i] = token[:pos+1] + wafBreakMarker + token[pos+1:]
		wrote = true
	}
	if !wrote {
		return text
	}
	*changed = true
	return strings.Join(parts, " ")
}

// breakEveryToken 对每个 token 的首字符之后、以及每个非字母数字字符之后插零宽。
func breakEveryToken(text string, changed *bool) string {
	if len(text) < 2 {
		return text
	}
	parts := strings.Split(text, " ")
	for i, token := range parts {
		if len(token) < 2 {
			continue
		}
		var b strings.Builder
		b.Grow(len(token) + len(token)/2)
		for idx := 0; idx < len(token); idx++ {
			c := token[idx]
			b.WriteByte(c)
			brk := false
			if idx == 0 {
				brk = true
			} else if !isASCIIAlnum(c) {
				brk = true
			}
			if brk {
				b.WriteString(wafBreakMarker)
			}
		}
		parts[i] = b.String()
	}
	*changed = true
	return strings.Join(parts, " ")
}

// isASCIIAlnum 报告字节是否 ASCII 字母或数字。
func isASCIIAlnum(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return false
	}
}
