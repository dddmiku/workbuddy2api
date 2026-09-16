package server

import (
	"encoding/json"
	"os"
	"testing"

	"workbuddy2api/internal/upstream"
)

// TestDiagBigPayloadPipeline 定位出站裁剪为何未触发：
// 真实大载荷经 responses→chat 转换后，体积与图片数是否还足以触发裁剪。
func TestDiagBigPayloadPipeline(t *testing.T) {
	raw, err := os.ReadFile("/tmp/big_payload.json")
	if err != nil {
		t.Skipf("no payload: %v", err)
	}
	chat, _, err := responsesToChat(raw)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	t.Logf("responses body = %d bytes", len(raw))
	t.Logf("chat body      = %d bytes", len(chat))

	var obj map[string]any
	if err := json.Unmarshal(chat, &obj); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	t.Logf("messages       = %d", len(msgs))

	nImg, nList, nStr, totalImgChars := 0, 0, 0, 0
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			nStr++
		case []any:
			nList++
			for _, p := range c {
				pm, _ := p.(map[string]any)
				if pm == nil {
					continue
				}
				if ty, _ := pm["type"].(string); ty == "image_url" || ty == "input_image" {
					nImg++
					if url, ok := pm["image_url"].(map[string]any); ok {
						if s, ok := url["url"].(string); ok {
							totalImgChars += len(s)
						}
					}
				}
			}
		}
	}
	t.Logf("content 形态: list=%d string=%d, images=%d (chars=%d)",
		nList, nStr, nImg, totalImgChars)

	for _, mb := range []int{7, 30} {
		out := upstream.ShrinkOutboundImages(chat, mb<<20)
		t.Logf("Shrink(budget=%2dMB): %d -> %d bytes", mb, len(chat), len(out))
	}
}
