// ═══ 更新日志 ═══
// 2026-09-18：复现跨轮调用编号复用、重复结果和首结果前插入说明导致的错误配对。
package upstream

import (
	"encoding/json"
	"testing"
)

func protocolDiagnosisCall(id, text string) map[string]any {
	return map[string]any{"role": "assistant", "content": text, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}
}

func protocolDiagnosisResult(id, text string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": text}
}

func protocolDiagnosisPrepared(t *testing.T, messages []any) []any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"model": "glm-5.2", "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(PrepareBodyOpt(raw, false), &out); err != nil {
		t.Fatal(err)
	}
	return out["messages"].([]any)
}

func protocolDiagnosisAssertSequence(t *testing.T, messages []any) {
	t.Helper()
	pending := map[string]bool{}
	for _, item := range messages {
		message := item.(map[string]any)
		if message["role"] == "tool" {
			id, _ := message["tool_call_id"].(string)
			if !pending[id] {
				t.Fatalf("orphan/duplicate/out-of-order result remains: %v", message)
			}
			delete(pending, id)
			continue
		}
		if len(pending) > 0 {
			t.Fatalf("unfinished tool group was interrupted: pending=%v next=%v", pending, message)
		}
		calls, _ := message["tool_calls"].([]any)
		for _, raw := range calls {
			id, _ := raw.(map[string]any)["id"].(string)
			if id == "" || pending[id] {
				t.Fatal("empty/duplicate call ID remains")
			}
			pending[id] = true
		}
	}
	if len(pending) > 0 {
		t.Fatalf("orphan call remains at end: %v", pending)
	}
}

func TestProtocolDiagnosisPairingDoesNotBorrowEarlierResult(t *testing.T) {
	messages := []any{protocolDiagnosisCall("same", "first"), protocolDiagnosisResult("same", "first answer"), map[string]any{"role": "user", "content": "next"}, protocolDiagnosisCall("same", "second"), map[string]any{"role": "user", "content": "recover"}}
	out := protocolDiagnosisPrepared(t, messages)
	protocolDiagnosisAssertSequence(t, out)
	if len(out) != len(messages) || out[1].(map[string]any)["content"] != "first answer" || out[3].(map[string]any)["content"] != "second" {
		t.Fatal("valid earlier result or assistant content changed")
	}
}

func TestProtocolDiagnosisPairingDropsDuplicateAndUnidentifiedResults(t *testing.T) {
	for _, messages := range [][]any{
		{protocolDiagnosisCall("one", "first"), protocolDiagnosisResult("one", "answer"), protocolDiagnosisResult("one", "duplicate")},
		{map[string]any{"role": "tool", "content": "no identifier"}},
		{protocolDiagnosisResult("before", "too early"), protocolDiagnosisCall("before", "call")},
	} {
		protocolDiagnosisAssertSequence(t, protocolDiagnosisPrepared(t, messages))
	}
}

func TestProtocolDiagnosisPairingReordersNoticeBeforeFirstResult(t *testing.T) {
	messages := []any{protocolDiagnosisCall("first", "read"), map[string]any{"role": "developer", "content": "image resize notice"}, protocolDiagnosisResult("first", "image"), map[string]any{"role": "user", "content": "continue"}}
	out := protocolDiagnosisPrepared(t, messages)
	protocolDiagnosisAssertSequence(t, out)
	if len(out) != len(messages) || out[2].(map[string]any)["content"] != "image resize notice" {
		t.Fatal("notice was discarded instead of moved behind its tool result")
	}
}

func TestProtocolDiagnosisPairingPreservesIndependentReuse(t *testing.T) {
	messages := []any{protocolDiagnosisCall("same", "first"), protocolDiagnosisResult("same", "answer one"), map[string]any{"role": "user", "content": "next"}, protocolDiagnosisCall("same", "second"), protocolDiagnosisResult("same", "answer two")}
	out := protocolDiagnosisPrepared(t, messages)
	protocolDiagnosisAssertSequence(t, out)
	if len(out) != len(messages) || out[1].(map[string]any)["content"] != "answer one" || out[4].(map[string]any)["content"] != "answer two" {
		t.Fatal("valid independent pairs with reused identifiers were changed")
	}
}
