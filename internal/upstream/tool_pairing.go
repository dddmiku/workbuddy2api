// ═══ 更新日志 ═══
// 2026-09-18：按实际工具回合配对结果，避免跨轮借用旧结果；重排覆盖首结果前的插入说明。
// tool_pairing.go 出站请求体的孤儿 tool_call↔tool 配对清理（吸收参考仓库
// sse.ts:91-123 resolveToolPairing 语义，适配网关的 OpenAI wire 消息形态）。
//
// 背景：OpenAI 兼容协议要求带 tool_calls 的 assistant 消息，其每一个 tool_call id
// 都必须有对应的一条 role:tool 结果消息；反之 role:tool 消息也必须有对应的前置
// tool_call。缺任一侧，上游都会以 HTTP 400 拒绝整个请求。
//
// 工具执行失败时（参数非法、超时、工具不存在……）客户端会把 assistant 的 tool_calls
// 持久化进会话历史，却写不回结果消息。这条坏历史随后被每次请求原样重放——上游对之后
// 每一条用户消息都返回 400，整条会话报废。网关是最后一道防线：发出请求前剔除无法配对
// 的条目让会话自愈，宁可丢一轮工具上下文，也好过整条会话死亡。
package upstream

// cleanupOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果（所有模型，独立于
// deepseek-only 的 sanitize 开关）。语义对齐参考仓库 resolveToolPairing：
//
//   - 每批 assistant.tool_calls 只匹配紧随其后的工具结果块；
//   - 同批每个 id 只对应一条调用与结果；缺结果的调用和无前置调用的结果对称删除；
//   - 不从历史其他回合借用结果；正常完整配对保持原始内容；
//   - 无任何工具流量 -> 原 slice 原样返回，changed=false（零分配零改动）。
//
// 这是「让请求通过」的安全网：只要存在合法配对就整段保留这些字段，绝不吞掉正确配对。
// 返回清理后的 slice（无改动时等于原 slice，勿依赖其是否新分配）及是否发生删除。
// repackToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的非 tool 消息
// 挪到整组之后，保证同一批 tool_call 的结果在 wire 上连续。
//
// 背景：Codex 的 image_resize_notice 特性会把 <image_resize_notice> 作为一条 user/system
// 消息插在 tool 输出后面（见 codex 二进制 features 表 images.resize_notice）。并行调用时
// 它插在两份 tool 结果中间：
//
//	assistant tool_calls=[c00 c01]
//	tool c00
//	developer <image_resize_notice>   <- 插在中间
//	tool c01
//
// OpenAI 兼容协议要求 tool 结果紧跟 assistant，中间插任何消息都算配对断裂，上游判
// 11148 tool_call_sequence_broken 并顶死整条会话（实测真实会话 33 处并行调用里唯一
// 被打断的那处正是会话卡死点）。这里只调顺序、不改内容：
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	-> assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 同组结果仍按原出现顺序排列，因此不引入新的顺序敏感问题。
// 无插入消息时零改动零分配。
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		// 收集紧随其后（允许被其他消息打断）的同批 tool 结果，按原相对顺序
		out = append(out, messages[i])
		i++
		var results []any
		var between []any
		sawNonTool := false
		remaining := len(want)
		seen := map[string]bool{}
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				if !seen[id] {
					seen[id] = true
					remaining--
				}
				if sawNonTool {
					changed = true
				}
				i++
				if remaining == 0 {
					break
				}
				continue
			}
			// 下一组 assistant.tool_calls 是新的组头，绝不能当插入物吞掉：收进
			// between 它就被原样吐出，且永远不再被外层循环当作组头处理，它自己
			// 那批结果也就永远得不到重排。真实会话 msg[181]（view_image ×2）正是
			// 这样漏掉的——被上一组的收集循环吞进 between，于是 [183] 仍夹在
			// [182]/[184] 两条 tool 结果中间，上游照旧判 11148。必须 break，把
			// 组头交还外层循环。
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			// 同批结果尚未收齐时，中间消息视为插入物，暂存待后移
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	hasTraffic := false
	for _, item := range messages {
		if message, ok := item.(map[string]any); ok {
			calls, _ := message["tool_calls"].([]any)
			if message["role"] == "tool" || (message["role"] == "assistant" && len(calls) > 0) {
				hasTraffic = true
				break
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	changed := false
	out := make([]any, 0, len(messages))
	for index := 0; index < len(messages); {
		message, ok := messages[index].(map[string]any)
		if !ok {
			out = append(out, messages[index])
			index++
			continue
		}
		if message["role"] == "tool" {
			changed = true
			index++
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		if message["role"] != "assistant" || len(calls) == 0 {
			out = append(out, messages[index])
			index++
			continue
		}
		declared := map[string]bool{}
		for _, raw := range calls {
			if call, ok := raw.(map[string]any); ok {
				if id, _ := call["id"].(string); id != "" {
					declared[id] = true
				}
			}
		}
		matched := map[string]bool{}
		var results []any
		end := index + 1
		for end < len(messages) {
			result, ok := messages[end].(map[string]any)
			if !ok || result["role"] != "tool" {
				break
			}
			id, _ := result["tool_call_id"].(string)
			if declared[id] && !matched[id] {
				matched[id] = true
				results = append(results, messages[end])
			} else {
				changed = true
			}
			end++
		}
		keptCalls := make([]any, 0, len(calls))
		for _, raw := range calls {
			if call, ok := raw.(map[string]any); ok {
				if id, _ := call["id"].(string); matched[id] {
					keptCalls = append(keptCalls, raw)
					delete(matched, id)
				}
			}
		}
		if len(keptCalls) != len(calls) {
			changed = true
			copy := make(map[string]any, len(message))
			for key, value := range message {
				copy[key] = value
			}
			if len(keptCalls) == 0 {
				delete(copy, "tool_calls")
			} else {
				copy["tool_calls"] = keptCalls
			}
			message = copy
		}
		out = append(out, message)
		out = append(out, results...)
		index = end
	}
	if !changed {
		return messages, false
	}
	return out, true
}
