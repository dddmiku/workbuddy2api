// ═══ 更新日志 ═══
// 2026-09-18：用真实失败历史做离线验证：转换后的 chat 体不得再出现相邻 assistant。
package server

import (
	"encoding/json"
	"os"
	"testing"
)

func TestNoAdjacentAssistantsInRealHistory(t *testing.T) {
	path := os.Getenv("WB2A_REPLAY")
	if path == "" {
		t.Skip("WB2A_REPLAY not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := responsesToChat(raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var chat struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatal(err)
	}
	adjacent := 0
	for index := 1; index < len(chat.Messages); index++ {
		a, _ := chat.Messages[index-1]["role"].(string)
		b, _ := chat.Messages[index]["role"].(string)
		if a == "assistant" && b == "assistant" {
			adjacent++
		}
	}
	t.Logf("messages=%d adjacent_assistant=%d", len(chat.Messages), adjacent)
	if adjacent != 0 {
		t.Fatalf("真实历史转换后仍有 %d 处相邻 assistant（global 会 11155）", adjacent)
	}
}
