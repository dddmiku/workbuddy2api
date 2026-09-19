// ═══ 更新日志 ═══
// 2026-09-19：回归短行循环阈值、Unicode/CRLF分片、EOF/取消、模型范围及有界检测内存。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReasoningLoopGuardExactCRLFThresholdAcrossChunks(t *testing.T) {
	text := strings.Repeat(strings.Repeat("甲", 30)+"\r\n", 64) + strings.Repeat(strings.Repeat("乙", 29)+"\r\n", 192)
	runes := []rune(text)
	if len(runes) != 8000 {
		t.Fatal("fixture no longer reaches the exact character threshold")
	}
	for _, width := range []int{1, 2, 7, 31, 4096} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			guard := &reasoningLoopGuard{}
			var observed error
			for offset := 0; offset < len(runes); offset += width {
				end := offset + width
				if end > len(runes) {
					end = len(runes)
				}
				observed = guard.add(string(runes[offset:end]))
				if observed != nil {
					if end != len(runes) {
						t.Fatal("guard triggered before the exact threshold")
					}
					break
				}
			}
			if !IsReasoningLoopError(observed) || guard.characters != 8000 || guard.size != 256 {
				t.Fatalf("threshold mismatch: chars=%d size=%d err=%v", guard.characters, guard.size, observed)
			}
			if strings.Contains(observed.Error(), strings.Repeat("甲", 10)) {
				t.Fatal("guard error exposed reasoning text")
			}
		})
	}
}

func TestReasoningLoopGuardEOFCompletesLastLogicalLine(t *testing.T) {
	line := strings.Repeat("x", 32)
	guard := &reasoningLoopGuard{}
	if err := guard.add(strings.Repeat(line+"\n", 255) + line); err != nil {
		t.Fatalf("unterminated last line was counted too early: %v", err)
	}
	if guard.size != 255 || !IsReasoningLoopError(guard.finish()) {
		t.Fatalf("EOF did not complete the 256th line: size=%d", guard.size)
	}
}

func TestReasoningLoopGuardLongAndDiverseControlsRemainUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"long_single_line", strings.Repeat("这是正常的长行", 40000)},
		{"matrix_rows", strings.Repeat(strings.Repeat("0 ", 40)+"\n", 300)},
		{"sql_templates", strings.Repeat("INSERT INTO measurements(sensor_id, observed_value) VALUES (1, 0);\n", 300)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := &reasoningLoopGuard{}
			if err := guard.add(tc.text); err != nil {
				t.Fatalf("long-line control rejected: %v", err)
			}
			if err := guard.finish(); err != nil {
				t.Fatal(err)
			}
			if guard.characters != utf8.RuneCountInString(tc.text) || cap(guard.line) > 4096 || guard.size > 256 || len(guard.counts) > 256 {
				t.Fatalf("detector lost characters or exceeded its memory bound: chars=%d cap=%d window=%d unique=%d", guard.characters, cap(guard.line), guard.size, len(guard.counts))
			}
		})
	}
	guard := &reasoningLoopGuard{}
	for index := 0; index < 2000; index++ {
		if err := guard.add(fmt.Sprintf("独立步骤 %d\n", index)); err != nil {
			t.Fatal(err)
		}
		if len(guard.counts) > 256 {
			t.Fatal("line identities grew beyond the window")
		}
	}
}

func reasoningGuardTestFrame(text string) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": text}}}})
	return "data: " + string(raw) + "\n\n"
}

func TestReasoningLoopGuardModelScopeAndDisabledCompatibility(t *testing.T) {
	text := strings.Repeat(strings.Repeat("x", 32)+"\n", 300)
	raw := reasoningGuardTestFrame(text) + "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	for _, model := range []string{"deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", "global:deepseek-v4.1-flash", "sg:deepseek-v4.1-flash", " CN:DEEPSEEK-V4.1-FLASH "} {
		t.Run(model, func(t *testing.T) {
			result, err := Aggregate(strings.NewReader(raw), StreamOptions{Model: model, ReasoningLoopGuard: true})
			if result != nil || !IsReasoningLoopError(err) {
				t.Fatalf("explicit same-model alias was not guarded: %v", err)
			}
		})
	}
	for _, options := range []StreamOptions{
		{Model: "deepseek-v4.1-flash", ReasoningLoopGuard: false},
		{Model: "deepseek-v3", ReasoningLoopGuard: true},
		{Model: "other:deepseek-v4.1-flash", ReasoningLoopGuard: true},
		{Model: "deepseek-v4.1-flash-other", ReasoningLoopGuard: true},
	} {
		result, err := Aggregate(strings.NewReader(raw), options)
		if err != nil || result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["reasoning_content"] != text {
			t.Fatalf("disabled/non-target guard changed output for %q: %v", options.Model, err)
		}
	}
	if _, err := Aggregate(strings.NewReader(raw)); err != nil {
		t.Fatalf("callers omitting StreamOptions lost compatibility: %v", err)
	}
}

type reasoningGuardCancelledReader struct{ source *strings.Reader }

func (r reasoningGuardCancelledReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if err == io.EOF {
		return n, context.Canceled
	}
	return n, err
}

func TestReasoningLoopGuardDoesNotTurnCancellationIntoEOFFinalization(t *testing.T) {
	line := strings.Repeat("x", 32)
	partial := reasoningGuardTestFrame(strings.Repeat(line+"\n", 255) + line)
	options := StreamOptions{Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true}
	_, err := Aggregate(reasoningGuardCancelledReader{strings.NewReader(partial)}, options)
	if !errors.Is(err, context.Canceled) || IsReasoningLoopError(err) {
		t.Fatalf("cancellation was replaced by a loop result: %v", err)
	}
	rec := httptest.NewRecorder()
	err = Stream(rec, reasoningGuardCancelledReader{strings.NewReader(partial)}, options)
	if !errors.Is(err, context.Canceled) || IsReasoningLoopError(err) || strings.Contains(rec.Body.String(), ReasoningLoopErrorCode) {
		t.Fatalf("stream cancellation was replaced by loop detection: %v", err)
	}
}
