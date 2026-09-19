// ═══ 更新日志 ═══
// 2026-09-19：结束标记早于剩余工具参数时等待传输收尾，且不在实际成功前发送成功终态。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func lateArgumentsFrame(legacy bool, args any, finish string) string {
	fn := map[string]any{"arguments": args}
	delta := map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": fn}}}
	if legacy {
		delta = map[string]any{"function_call": fn}
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	return integrityEvent(string(raw))
}

// This checks what a streaming client can safely execute when it sees a finish,
// independently of the gateway accumulator's own validation implementation.
func assertToolFinishFollowsCompleteArguments(t *testing.T, raw string, legacy bool, want string) {
	t.Helper()
	var arguments strings.Builder
	finishes := 0
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		choices, _ := event["choices"].([]any)
		for _, value := range choices {
			choice := value.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if legacy {
				fn, _ := delta["function_call"].(map[string]any)
				text, _ := fn["arguments"].(string)
				arguments.WriteString(text)
			} else {
				calls, _ := delta["tool_calls"].([]any)
				for _, call := range calls {
					fn, _ := call.(map[string]any)["function"].(map[string]any)
					text, _ := fn["arguments"].(string)
					arguments.WriteString(text)
				}
			}
			if finish, _ := choice["finish_reason"].(string); finish != "" {
				finishes++
				if arguments.String() != want || !json.Valid([]byte(arguments.String())) {
					t.Fatalf("client received a successful finish with unfinished arguments: %q", arguments.String())
				}
			}
		}
	}
	if finishes != 1 || arguments.String() != want {
		t.Fatalf("finishes=%d arguments=%q want=%q", finishes, arguments.String(), want)
	}
}

func TestToolArgumentsCanFinishAfterEarlyFinishMarker(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, snapshot := range []bool{false, true} {
			for _, end := range []string{"done", "eof"} {
				t.Run(fmt.Sprintf("legacy=%t/snapshot=%t/%s", legacy, snapshot, end), func(t *testing.T) {
					prefix := integrityTool(`{"invoice_id":`)
					finish := "tool_calls"
					if legacy {
						prefix = integrityEvent(`{"choices":[{"index":0,"delta":{"function_call":{"name":"lookup","arguments":"{\"invoice_id\":"}}}]}`)
						finish = "function_call"
					}
					const want = `{"invoice_id":11128}`
					tail := lateArgumentsFrame(legacy, "11128}", "")
					if snapshot {
						tail = integritySnapshot(map[string]any{"name": "lookup", "arguments": want}, legacy)
					}
					raw := prefix + integrityFinish(finish) + tail
					if end == "done" {
						raw += integrityEvent("[DONE]")
					}
					if _, err := Aggregate(strings.NewReader(raw)); err != nil {
						t.Errorf("complete tool stream was rejected before its tail: %v", err)
					}
					rec := httptest.NewRecorder()
					if err := Stream(rec, strings.NewReader(raw)); err != nil {
						t.Fatalf("complete tool stream failed: %v", err)
					}
					assertToolFinishFollowsCompleteArguments(t, rec.Body.String(), legacy, want)
				})
			}
		}
	}
}

func TestLateInvalidArgumentsNeverPublishSuccess(t *testing.T) {
	for _, tail := range []string{"", lateArgumentsFrame(false, "bad}", "")} {
		raw := integrityTool(`{"invoice_id":`) + integrityFinish("tool_calls") + tail + integrityEvent("[DONE]")
		rec := httptest.NewRecorder()
		if err := Stream(rec, strings.NewReader(raw)); err == nil {
			t.Fatal("genuinely invalid arguments were accepted")
		}
		if strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
			t.Fatal("an invalid tool call was marked successful")
		}
		if _, err := Aggregate(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid aggregate was accepted")
		}
	}
}

func TestUpstreamFailureAfterFinishDoesNotPublishSuccess(t *testing.T) {
	raw := integrityTool(`{"invoice_id":11128}`) + integrityFinish("tool_calls") + integrityEvent(`{"error":{"message":"upstream failed after the finish marker"}}`)
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err == nil {
		t.Fatal("upstream failure was accepted")
	}
	if strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatal("an upstream failure exposed a successful tool finish")
	}
}
