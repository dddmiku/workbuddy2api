// ═══ 更新日志 ═══
// 2026-09-16：移除业务正文清洗，旧 sanitize 参数仅兼容配置；保留既有协议适配。
// 2026-09-16：请求及 console 系统消息适配保留 JSON 数字字面量，避免 schema 和业务值损失精度。
// 2026-09-17：合并 fork 测试入口约定，保留参数兼容、协议整理与数字/正文保真。
// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"strings"

	"workbuddy2api/internal/jsonutil"
)

// PrepareBodyOpt 适配上游协议并保留消息业务内容。
// legacySanitize 参数已废弃，保留调用兼容性；true/false 均不清洗内容，新调用应传 false。
// DeptestOnly: 保留跨包回归测试入口；生产经 prepareBody 调用 PrepareBodyOptWithEffortsAndDefault。
func PrepareBodyOpt(src []byte, legacySanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, legacySanitize, nil, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
//
// DeptestOnly: 仅测试引用（upstream stability/thinking/cache_key/sse 族 +
// server 稳定性回归）；生产经 prepareBody 走 PrepareBodyOptWithEffortsAndDefault。
// 跨包测试引用，迁 export_test.go 不可行。保留作无默认档的降级管线锚点。
//
// 向后兼容封装：不传 defaultEfforts（无模型声明默认档），thinking.go 回退硬编码 high。
// DeptestOnly: 保留跨包测试使用的无默认档管线，生产使用带默认档的完整入口。
func PrepareBodyOptWithEfforts(src []byte, legacySanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, legacySanitize, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault 在 PrepareBodyOptWithEfforts 基础上按模型
// reasoning.defaultEffort 补默认档（缺显式 effort 时优先用模型声明档，空串/未知回退硬编码）。
func PrepareBodyOptWithEffortsAndDefault(src []byte, legacySanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := jsonutil.Decode(src, &obj); err != nil || obj == nil {
		return src
	}
	obj["stream"] = true
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeRoles(obj)
	// 孤儿 tool_call↔tool 配对清理（见 tool_pairing.go）：所有模型一律执行。
	// 这是协议适配的安全网——不完整配对的
	// tool_calls/tool 结果会让上游对之后每条消息都返 400，必须先行剔除。
	if msgs, ok := obj["messages"].([]any); ok {
		// 先修顺序：把插在 assistant.tool_calls 与其结果之间的消息后移，
		// 否则并行调用被打断会被上游判 11148（image_resize_notice 场景）。
		if packed, ch := repackToolResultBlocks(msgs); ch {
			obj["messages"] = packed
			msgs = packed
		}
		// 再删残留：顺序修好后仍无法配对的条目（真正缺结果的调用 / 孤儿结果）。
		if cleaned, ch := cleanupOrphanToolCalls(msgs); ch {
			obj["messages"] = cleaned
		}
	}
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	model, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, model))
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	warnDeprecatedSanitization(legacySanitize)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("WARN: [upstream] reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("WARN: [upstream] reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化仅适配上游 role 白名单，保留消息正文；旧 sanitize 配置的任意取值均照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("[upstream] role normalized developer->system idx=%d", i)
		}
	}
}

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45，防 console 域上游 code 11128）：
// 首条消息非 system 时在 messages 最前补一条 fallback system（"You are a helpful assistant."）。
// 仅对 global 请求调用（CN 现状不动；即使首条就是 system 也不重复注入）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}
