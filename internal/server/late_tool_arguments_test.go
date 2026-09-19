// ═══ 更新日志 ═══
// 2026-09-19：Chat 和 Responses 四条真实 Handler 路径均允许迟到参数补齐，最终只发送一次完成态。
package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestHandlerAcceptsArgumentsAfterEarlyFinish(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("chat/stream=%t", stream), func(t *testing.T) {
			raw := outputIntegritySSE(
				protocolDiagnosisChatFrame(t, protocolDiagnosisMessage([]string{"safe"}, `{"value":`), "tool_calls"),
				protocolDiagnosisChatFrame(t, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}}}}, ""),
			)
			rec := protocolDiagnosisChat(t, protocolDiagnosisChatRequest("required", true, stream), raw)
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("late complete arguments rejected: status=%d body=%s", rec.Code, rec.Body)
			}
			if stream {
				errors, finishes := protocolDiagnosisChatTerminal(t, rec.Body.String())
				if errors != 0 || finishes != 1 {
					t.Fatalf("errors=%d finishes=%d", errors, finishes)
				}
			}
		})
	}
	for _, mode := range []string{"stream", "aggregate"} {
		t.Run("responses/"+mode, func(t *testing.T) {
			req := continuationIntegrityRequest(t, mode == "stream")
			prefix := map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "late_call", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"invoice_id":`}}}}
			tail := map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "11128}"}}}}
			reply := continuationIntegritySSE(t, req,
				continuationIntegrityFrame(t, prefix, "tool_calls"),
				continuationIntegrityFrame(t, tail, ""))
			continuationIntegrityMustComplete(t, reply, mode == "stream")
			calls := continuationIntegrityCalls(reply.response)
			if len(calls) != 1 || calls[0]["arguments"] != `{"invoice_id":11128}` {
				t.Fatalf("late arguments changed: %v", calls)
			}
		})
	}
}
