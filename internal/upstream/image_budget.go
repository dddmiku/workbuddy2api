// image_budget.go 出站图片预算：把发往上游的请求体按字节压进预算内。
//
// ── 为什么需要这一层（全部为实测结论） ──
//
//  1. Codex 每轮都会把历史里每一次 view_image 的截图原样重传。实测一个 471 条的
//     会话请求体 8.62MB，其中 27 张图片占 7.53MB（87.3%）。图片不随轮次衰减。
//
//  2. 图片字节与 token 严重脱钩：同一张图固定按 ~962 token 计费，但字节数在
//     102 B/token 到 1042 B/token 之间浮动（最大 25 倍）。因此 Codex 自己的
//     compaction（按 token 触发，阈值 700k）永远来不及在字节撑爆前回收——
//     实测会话死在 261,790 / 700,000 tokens 就被拒了。
//
//  3. 入站有两道独立边界，任一先命中都会拒掉请求：
//     nginx client_max_body_size（回 HTML 413）与网关 server.max_body_mb（回 JSON 413）。
//     两者都只是「接不接收」，不解决「上游能收多大」。
//
// ── 本文件的职责 ──
//
// 在出站咽喉（Client.prepareBody）做一次按字节的裁剪：从「最旧的图片」开始替换为
// 文本占位，直到序列化体积落进预算。入站边界放宽到能收下大请求，出站预算守住
// 上游真正能接受的大小——这样上游永远收到的是验证过可用的体积。
//
// ── 为什么只替换 content part、绝不增删 message ──
//
// function_call ↔ tool 的配对关系由 message 承载。删 message 会制造孤儿配对，
// 上游对不完整配对会返 400（网关已有 cleanupOrphanToolCalls 兜这种脏数据，
// 但那是补救，不该由裁剪主动制造）。替换 part 则完全不动配对结构。
package upstream

import (
	"encoding/json"
	"log"
)

// imageOmittedPlaceholder 图片被省略时写回的占位文本。
//
// 措辞有意明确两点：图片是因为「体积」被省略的（不是内容空），以及重新获取的途径。
// 含糊的占位（如 "[image]"）会让模型以为原图本来就是空的，从而对截图内容做出错误推断。
const imageOmittedPlaceholder = "[历史图片已省略：本次请求体积超过中转上限，较早的图片未随请求发送。" +
	"如仍需该图片内容，请再次调用 view_image 读取对应文件。]"

// imagePart 记录一个图片 part 的位置与其 URL 长度。
type imagePart struct {
	parts   []any // 承载该 part 的切片（就地替换）
	index   int   // 在 parts 中的下标
	chatFmt bool  // true=chat 的 image_url 形态；false=responses 的 input_image 形态
}

// collectImageParts 按文档顺序（旧 → 新）收集请求体里所有图片 part。
//
// 兼容两种协议形态，因为网关同时服务两类客户端：
//   - chat      ：messages[].content[] 里的 {"type":"image_url",...}
//   - responses ：input[].content[]（message 贴图）
//     input[].output[]（function_call_output —— view_image 的结果走这条）
//
// 数组顺序即对话时间顺序，所以「从索引 0 开始丢」等价于「丢最旧的」。
func collectImageParts(obj map[string]any) []imagePart {
	var found []imagePart

	scan := func(v any, chatFmt bool) {
		parts, ok := v.([]any)
		if !ok {
			return
		}
		for i, p := range parts {
			if isImagePart(p) {
				found = append(found, imagePart{parts: parts, index: i, chatFmt: chatFmt})
			}
		}
	}

	if msgs, ok := obj["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				scan(mm["content"], true)
			}
		}
	}
	if input, ok := obj["input"].([]any); ok {
		for _, item := range input {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := im["type"].(string); t == "function_call_output" {
				scan(im["output"], false)
			} else {
				scan(im["content"], false)
			}
		}
	}
	return found
}

// isImagePart 判定 part 是否为图片 part。
//
// 只看 type——不回看 URL 是否为空：空 URL 的图片 part 同样占位、同样该被裁剪替换，
// 且它仍然是「图片」语义，替换成占位文本是正确降级。
func isImagePart(part any) bool {
	p, ok := part.(map[string]any)
	if !ok {
		return false
	}
	t, _ := p["type"].(string)
	return t == "image_url" || t == "input_image"
}

// placeholderPart 按协议形态生成占位 part。
//
// chat 用 "text"，responses 用 "input_text"——两套协议的文本 part 类型名不同，
// 用错会被对端判为非法 part。
func placeholderPart(chatFmt bool) map[string]any {
	if chatFmt {
		return map[string]any{"type": "text", "text": imageOmittedPlaceholder}
	}
	return map[string]any{"type": "input_text", "text": imageOmittedPlaceholder}
}

// marshalSize 返回 v 序列化后的字节数；失败返回 0（估算退化为「不减」，
// 由后面的真实长度兜底循环兜住，不会因此漏丢）。
func marshalSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// ShrinkOutboundImages 把请求体压到 maxBytes 以内：从最旧的图片开始替换为占位文本。
//
// 以下情形原样返回，不做任何改动（全部是「不该动」而非「动不了」）：
//   - maxBytes <= 0：功能未启用
//   - len(body) <= maxBytes：已在预算内，零改动（正例零影响）
//   - body 不是 JSON 对象 / 不含图片 part：无从裁起，交给上游按原样判定
//
// 只替换 content part、不增删 message，因此 function_call ↔ tool 配对零改动。
func ShrinkOutboundImages(body []byte, maxBytes int) []byte {
	if maxBytes <= 0 || len(body) <= maxBytes {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	parts := collectImageParts(obj)
	if len(parts) == 0 {
		return body
	}

	// 第一轮：按「替换前 / 替换后序列化差值」估算要丢几张。
	// 用估算而非每轮真实序列化，是为了避免在 8MB 级 JSON 上反复 Marshal。
	cur := len(body)
	dropped := 0
	for _, p := range parts {
		if cur <= maxBytes {
			break
		}
		delta := marshalSize(p.parts[p.index]) - marshalSize(placeholderPart(p.chatFmt))
		p.parts[p.index] = placeholderPart(p.chatFmt)
		if delta > 0 {
			cur -= delta
		}
		dropped++
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}

	// 第二轮：估算偏差（JSON 转义、数字格式等）用真实长度兜底继续丢，
	// 最多把剩余图片全丢光；仍超预算则交由上游判定，不在此处伪造成功。
	for len(out) > maxBytes && dropped < len(parts) {
		p := parts[dropped]
		p.parts[p.index] = placeholderPart(p.chatFmt)
		dropped++
		if out, err = json.Marshal(obj); err != nil {
			return body
		}
	}

	log.Printf("[upstream] outbound image budget: %d -> %d bytes (dropped %d/%d oldest images, budget=%d)",
		len(body), len(out), dropped, len(parts), maxBytes)
	return out
}
