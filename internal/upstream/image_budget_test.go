package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// bigImageURL 造一个指定长度的 data URI（内容无关，只影响字节数）。
func bigImageURL(n int) string {
	return "data:image/png;base64," + strings.Repeat("A", n)
}

// chatBodyWithImages 造一个 chat 请求体：n 张图，各 imgLen 字节。
func chatBodyWithImages(n, imgLen int) []byte {
	msgs := []any{}
	for i := 0; i < n; i++ {
		parts := []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": bigImageURL(imgLen)}},
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": parts})
	}
	b, _ := json.Marshal(map[string]any{"model": "m", "messages": msgs})
	return b
}

// respBodyWithImages 造一个 responses 请求体：n 张图挂在 function_call_output 上。
func respBodyWithImages(n, imgLen int) []byte {
	items := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "start"},
		}},
	}
	for i := 0; i < n; i++ {
		callID := "call_" + string(rune('a'+i%26))
		items = append(items,
			map[string]any{"type": "function_call", "call_id": callID, "name": "view_image",
				"arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": callID, "output": []any{
				map[string]any{"type": "input_image", "image_url": bigImageURL(imgLen)},
			}},
		)
	}
	b, _ := json.Marshal(map[string]any{"model": "m", "input": items})
	return b
}

// pairStats 统计配对健康度（与生产裁剪逻辑共用同一定义）。
type pairStats struct {
	calls, outs, orphanOut, unanswered int
}

func countPairs(t *testing.T, body []byte) pairStats {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var s pairStats
	input, _ := obj["input"].([]any)
	callIDs := map[string]bool{}
	outIDs := map[string]bool{}
	for _, it := range input {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		switch ty, _ := m["type"].(string); ty {
		case "function_call":
			id, _ := m["call_id"].(string)
			callIDs[id] = true
			s.calls++
		case "function_call_output":
			id, _ := m["call_id"].(string)
			outIDs[id] = true
			s.outs++
		}
	}
	for id := range outIDs {
		if !callIDs[id] {
			s.orphanOut++
		}
	}
	for id := range callIDs {
		if !outIDs[id] {
			s.unanswered++
		}
	}
	return s
}

// countImages 数请求体里还剩多少图片 part。
func countImages(t *testing.T, body []byte) int {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return len(collectImageParts(obj))
}

// TestShrinkWithinBudgetIsNoop 已在预算内 → 逐字节原样返回（正例零影响）。
func TestShrinkWithinBudgetIsNoop(t *testing.T) {
	body := chatBodyWithImages(3, 1000)
	got := ShrinkOutboundImages(body, 10_000_000)
	if string(got) != string(body) {
		t.Fatalf("in-budget body must be returned unchanged")
	}
}

// TestShrinkDisabledIsNoop maxBytes<=0 → 功能未启用，原样返回。
func TestShrinkDisabledIsNoop(t *testing.T) {
	body := chatBodyWithImages(3, 100_000)
	for _, lim := range []int{0, -1} {
		if got := ShrinkOutboundImages(body, lim); string(got) != string(body) {
			t.Fatalf("maxBytes=%d must disable shrinking", lim)
		}
	}
}

// TestShrinkNoImagesIsNoop 体积超预算但不含图片 → 无从裁起，原样返回。
func TestShrinkNoImagesIsNoop(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": strings.Repeat("x", 5000)},
	}})
	if got := ShrinkOutboundImages(b, 1000); string(got) != string(b) {
		t.Fatalf("body without images must be returned unchanged")
	}
}

// TestShrinkDropsOldestFirst 从最旧的图片开始丢，最新的一张必须保住。
func TestShrinkDropsOldestFirst(t *testing.T) {
	// 5 张图，每张约 40KB；预算设成只装得下最后一张多一点。
	body := chatBodyWithImages(5, 40_000)
	got := ShrinkOutboundImages(body, 60_000)
	if len(got) > 60_000 {
		t.Fatalf("still over budget: %d", len(got))
	}
	var obj map[string]any
	_ = json.Unmarshal(got, &obj)
	msgs, _ := obj["messages"].([]any)
	// 最旧的消息里图片应已被替换为文本占位
	first, _ := msgs[0].(map[string]any)
	parts, _ := first["content"].([]any)
	for _, p := range parts {
		if isImagePart(p) {
			t.Fatalf("oldest image must be dropped first")
		}
	}
	// 最新一条消息里的图片必须还在
	last, _ := msgs[len(msgs)-1].(map[string]any)
	lparts, _ := last["content"].([]any)
	found := false
	for _, p := range lparts {
		if isImagePart(p) {
			found = true
		}
	}
	if !found {
		t.Fatalf("newest image must be preserved")
	}
}

// TestShrinkKeepsToolPairing 裁剪不得破坏 function_call ↔ function_call_output 配对。
func TestShrinkKeepsToolPairing(t *testing.T) {
	body := respBodyWithImages(6, 50_000)
	before := countPairs(t, body)
	got := ShrinkOutboundImages(body, 200_000)
	after := countPairs(t, got)

	if after != before {
		t.Fatalf("pairing changed: before=%+v after=%+v", before, after)
	}
	if after.orphanOut != 0 || after.unanswered != 0 {
		t.Fatalf("pairing broken: %+v", after)
	}
	if len(got) > 200_000 {
		t.Fatalf("still over budget: %d", len(got))
	}
	if n := countImages(t, got); n >= 6 {
		t.Fatalf("expected some images dropped, still have %d", n)
	}
}

// TestShrinkResponsesToolOutput 验证 responses 形态（function_call_output 内的图片）也被识别。
func TestShrinkResponsesToolOutput(t *testing.T) {
	body := respBodyWithImages(4, 60_000)
	if n := countImages(t, body); n != 4 {
		t.Fatalf("precondition: want 4 images, got %d", n)
	}
	got := ShrinkOutboundImages(body, 80_000)
	if len(got) > 80_000 {
		t.Fatalf("still over budget: %d", len(got))
	}
	if n := countImages(t, got); n == 4 {
		t.Fatalf("expected oldest images to be dropped")
	}
	// 占位必须是 input_text（responses 形态的文本类型）
	if !strings.Contains(string(got), "历史图片已省略") {
		t.Fatalf("placeholder text missing")
	}
	if strings.Contains(string(got), `"type":"text"`) {
		t.Fatalf("responses form must use input_text, not text")
	}
}

// TestShrinkChatUsesTextPart chat 形态的占位必须是 "text" 而非 "input_text"。
func TestShrinkChatUsesTextPart(t *testing.T) {
	body := chatBodyWithImages(4, 60_000)
	got := ShrinkOutboundImages(body, 80_000)
	if strings.Contains(string(got), `"type":"input_text"`) {
		t.Fatalf("chat form must use text, not input_text")
	}
	if !strings.Contains(string(got), `"type":"text"`) {
		t.Fatalf("chat placeholder part missing")
	}
}

// TestShrinkBadJSONIsNoop 坏 JSON 不二次错误化，原样返回。
func TestShrinkBadJSONIsNoop(t *testing.T) {
	body := []byte(`{"model":"m","messages":[`)
	if got := ShrinkOutboundImages(body, 10); string(got) != string(body) {
		t.Fatalf("invalid JSON must be returned unchanged")
	}
}

// TestShrinkOnlyImagesBody 全是图片、没有文本时也要能压到预算内。
func TestShrinkOnlyImagesBody(t *testing.T) {
	body := chatBodyWithImages(8, 30_000)
	got := ShrinkOutboundImages(body, 50_000)
	if len(got) > 50_000 {
		t.Fatalf("still over budget: %d", len(got))
	}
}
