// ═══ 更新日志 ═══
// 2026-09-19：锁定推理与正文/工具交错时的单一条目生命周期，防止重用ID、重放旧摘要或丢失输出。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

func TestResponsesReasoningItemRemainsStableAcrossInterleavedOutput(t *testing.T) {
	reasonA := `{"choices":[{"index":0,"delta":{"reasoning_content":"甲"}}]}`
	reasonB := `{"choices":[{"index":0,"delta":{"reasoning_content":"乙"}}]}`
	reasonC := `{"choices":[{"index":0,"delta":{"reasoning_content":"丙"}}]}`
	text := `{"choices":[{"index":0,"delta":{"content":"正文"}}]}`
	callA := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"id\":11128}"}}]}}]}`
	callB := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{\"id\":11129}"}}]}}]}`
	for _, tc := range []struct {
		name, finish, status, reasoning, text string
		frames                                []string
		tools                                 int
		failure                               bool
	}{
		{name: "continuous", finish: "stop", status: "completed", reasoning: "甲乙", text: "正文", frames: []string{reasonA, reasonB, text}},
		{name: "tool_between_reasoning", finish: "tool_calls", status: "completed", reasoning: "甲乙", frames: []string{reasonA, callA, reasonB}, tools: 1},
		{name: "text_between_reasoning", finish: "stop", status: "completed", reasoning: "甲乙", text: "正文", frames: []string{reasonA, text, reasonB}},
		{name: "two_tools_between_reasoning", finish: "tool_calls", status: "completed", reasoning: "甲乙丙", frames: []string{reasonA, callA, reasonB, callB, reasonC}, tools: 2},
		{name: "length_after_interleaving", finish: "length", status: "incomplete", reasoning: "甲乙", frames: []string{reasonA, callA, reasonB}, tools: 1},
		{name: "error_after_interleaving", status: "failed", reasoning: "甲乙", frames: []string{reasonA, callA, reasonB}, tools: 1, failure: true},
		{name: "text_before_first_reasoning", finish: "stop", status: "completed", reasoning: "甲乙", text: "正文", frames: []string{text, reasonA, reasonB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, req, err := responsesToChat([]byte(`{"model":"cn:deepseek-v4.1-flash","stream":true,"input":"offline fixture","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer"}}}}]}`))
			if err != nil {
				t.Fatal(err)
			}
			frames := append([]string{}, tc.frames...)
			if tc.failure {
				frames = append(frames, `{"error":{"code":"fixture_failure","message":"offline failure"}}`)
			} else {
				frames = append(frames, `{"choices":[{"index":0,"delta":{},"finish_reason":"`+tc.finish+`"}]}`)
			}
			rec := httptest.NewRecorder()
			rw := newResponsesWriter(rec, req)
			err = upstream.Stream(rw, strings.NewReader(outputIntegritySSE(frames...)))
			if (err != nil) != tc.failure {
				t.Fatalf("stream error=%v want failure=%t", err, tc.failure)
			}
			rw.finish()
			names, events := eventsOf(t, rec.Body.String())
			final := outputIntegrityFinal(t, names, events, "response."+tc.status)
			added, itemDone, partAdded, partDone, textDone := 0, 0, 0, 0, 0
			reasonID := ""
			var outputIndex any
			var reasoning strings.Builder
			toolDone := 0
			for index, name := range names {
				event := events[index]
				if name == evItemAdded || name == evItemDone {
					item := event["item"].(map[string]any)
					if item["type"] == "function_call" && name == evItemDone {
						toolDone++
					}
					if item["type"] != "reasoning" {
						continue
					}
					if name == evItemAdded {
						added++
						if added != 1 {
							t.Fatalf("reasoning item reopened after being added: %s", rec.Body)
						}
						reasonID, _ = item["id"].(string)
						outputIndex = event["output_index"]
						if len(item["summary"].([]any)) != 0 {
							t.Fatal("new reasoning item replayed an old summary")
						}
					} else {
						itemDone++
						wantStatus := "completed"
						if tc.status != "completed" {
							wantStatus = "incomplete"
						}
						if item["status"] != wantStatus {
							t.Fatalf("reasoning closed before terminal status was known: %v", item["status"])
						}
					}
					if item["id"] != reasonID || event["output_index"] != outputIndex {
						t.Fatal("reasoning item changed ID or output index")
					}
				}
				switch name {
				case evRsPartAdded:
					partAdded++
				case evRsDelta:
					if itemDone != 0 || textDone != 0 {
						t.Fatal("reasoning delta emitted after done")
					}
					reasoning.WriteString(event["delta"].(string))
				case evRsDone:
					textDone++
					if event["text"] != tc.reasoning {
						t.Fatalf("done text omitted later reasoning: %v", event["text"])
					}
				case evRsPartDone:
					partDone++
				default:
					continue
				}
				if event["item_id"] != reasonID || event["output_index"] != outputIndex || event["summary_index"] != float64(0) {
					t.Fatal("reasoning event identity changed")
				}
			}
			if added != 1 || itemDone != 1 || partAdded != 1 || partDone != 1 || textDone != 1 || reasoning.String() != tc.reasoning {
				t.Fatalf("inconsistent reasoning lifecycle: added=%d done=%d part=%d/%d textDone=%d text=%q", added, itemDone, partAdded, partDone, textDone, reasoning.String())
			}
			wantToolDone := tc.tools
			if tc.status != "completed" {
				wantToolDone = 0
			}
			if toolDone != wantToolDone {
				t.Fatalf("tool done count=%d want=%d", toolDone, wantToolDone)
			}
			reasonItems, tools := 0, 0
			var textContent strings.Builder
			for index, value := range final["output"].([]any) {
				item := value.(map[string]any)
				switch item["type"] {
				case "reasoning":
					reasonItems++
					if item["id"] != reasonID || float64(index) != outputIndex || item["summary"].([]any)[0].(map[string]any)["text"] != tc.reasoning {
						t.Fatal("final reasoning item differs from stream identity or text")
					}
				case "function_call":
					tools++
					want := map[string]string{"call_a": `{"id":11128}`, "call_b": `{"id":11129}`}[item["call_id"].(string)]
					if item["name"] != "lookup" || item["arguments"] != want {
						t.Fatalf("tool identity or arguments changed: %#v", item)
					}
				case "message":
					for _, part := range item["content"].([]any) {
						textContent.WriteString(part.(map[string]any)["text"].(string))
					}
				}
			}
			if reasonItems != 1 || tools != tc.tools || textContent.String() != tc.text {
				t.Fatal("final response lost or duplicated reasoning, text, or tools")
			}
		})
	}
}
