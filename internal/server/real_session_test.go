// ═══ 更新日志 ═══
// 2026-09-18：允许通过 WB2A_REPLAY 指定隔离的真实历史夹具，避免依赖固定 /src 路径或修改生产目录。
package server

// 真实会话载荷的端到端回归：卡死会话（452 条目 / 25 图 / 6.95MB）走生产转换路径后，
// 出站消息必须零 tool 配对违规。
//
// 载荷由测试环境提供（/src/real2.json，Codex 实际发出的那 206 条 input）；缺失时跳过，
// 因此不阻塞常规 CI。修复背景见 upstream/tool_pairing.go 的 repackToolResultBlocks：
// Codex 的 <image_resize_notice> 会插在并行 tool 结果中间，上游判 11148 顶死会话。
import (
	"encoding/json"
	"os"
	"testing"

	"workbuddy2api/internal/upstream"
)

func TestRealSessionToolPairing(t *testing.T) {
	path := os.Getenv("WB2A_REPLAY")
	if path == "" {
		path = "/src/real2.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("无真实载荷（%v），跳过", err)
	}
	chatBody, _, err := responsesToChat(raw)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	out := upstream.PrepareBodyOptWithEffortsAndDefault(chatBody, true, nil, nil)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("转换后无消息")
	}

	viol := 0
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		tcs, ok := mm["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, tc := range tcs {
			if tcm, ok := tc.(map[string]any); ok {
				if id, _ := tcm["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		got := map[string]bool{}
		for j := i + 1; j < len(msgs); j++ {
			nxt, _ := msgs[j].(map[string]any)
			if r, _ := nxt["role"].(string); r != "tool" {
				break
			}
			if id, _ := nxt["tool_call_id"].(string); id != "" {
				got[id] = true
			}
		}
		if len(got) != len(want) {
			viol++
			t.Errorf("msg[%d] tool 结果不连续：want=%d got=%d", i, len(want), len(got))
		}
	}
	if viol > 0 {
		t.Fatalf("真实载荷共 %d 处配对违规（消息 %d 条）", viol, len(msgs))
	}
	t.Logf("真实载荷零违规：%d 条出站消息，%d 字节", len(msgs), len(out))
}
