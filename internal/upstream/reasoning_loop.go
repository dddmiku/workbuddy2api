// ═══ 更新日志 ═══
// 2026-09-19：为已观察到循环的DeepSeek模型提供有界、可关闭的重复短行推理保护；不改正文、工具或用量。
package upstream

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ReasoningLoopErrorCode = "upstream_reasoning_loop"

const (
	reasoningLoopMinChars         = 8000
	reasoningLoopWindow           = 256
	reasoningLoopMaxUnique        = 12
	reasoningLoopCoveragePercent  = 95
	reasoningLoopMaxRepeatedRunes = 32
	reasoningLoopLineBufferRunes  = 4096
)

// StreamOptions supplements the existing Stream/Aggregate APIs. Omitting it
// retains their previous behavior; the HTTP handler explicitly supplies its configuration.
type StreamOptions struct {
	Model              string
	ReasoningLoopGuard bool
}

func reasoningLoopModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if realm, bare, found := strings.Cut(model, ":"); found {
		switch realm {
		case "cn", "global", "sg":
			model = bare
		default:
			return false
		}
	}
	return model == "deepseek-v4.1-flash"
}

func IsReasoningLoopError(err error) bool {
	var streamErr *StreamError
	return errors.As(err, &streamErr) && streamErr.Code == ReasoningLoopErrorCode
}

type reasoningLineKey struct {
	digest [sha256.Size]byte
	unique uint64
}

type reasoningLoopGuard struct {
	characters int
	line       []rune
	overlong   bool
	nonempty   bool
	pendingCR  bool
	serial     uint64
	window     [reasoningLoopWindow]reasoningLineKey
	position   int
	size       int
	counts     map[reasoningLineKey]int
}

func (g *reasoningLoopGuard) reset() {
	g.characters, g.position, g.size = 0, 0, 0
	g.line = g.line[:0]
	g.overlong, g.nonempty, g.pendingCR = false, false, false
	g.serial = 0
	clear(g.counts)
}

func reasoningSpace(r rune) bool {
	// Python str.strip also recognizes these ASCII separators.
	return unicode.IsSpace(r) || (r >= '\x1c' && r <= '\x1f')
}

func reasoningLineBreak(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

func (g *reasoningLoopGuard) add(text string) error {
	for _, r := range text {
		if g.pendingCR {
			g.pendingCR = false
			if r == '\n' {
				g.countCharacter()
				if err := g.completeLine(); err != nil {
					return err
				}
				continue
			}
			if err := g.completeLine(); err != nil {
				return err
			}
		}
		g.countCharacter()
		if r == '\r' {
			g.pendingCR = true
			continue
		}
		if reasoningLineBreak(r) {
			if err := g.completeLine(); err != nil {
				return err
			}
			continue
		}
		g.nonempty = g.nonempty || !reasoningSpace(r)
		if !g.overlong {
			if g.line == nil {
				g.line = make([]rune, 0, reasoningLoopLineBufferRunes)
			}
			if len(g.line) == reasoningLoopLineBufferRunes {
				g.overlong = true
				g.line = g.line[:0]
			} else {
				g.line = append(g.line, r)
			}
		}
	}
	return nil
}

func (g *reasoningLoopGuard) countCharacter() {
	if g.characters < int(^uint(0)>>1) {
		g.characters++
	}
}

func (g *reasoningLoopGuard) finish() error {
	g.pendingCR = false
	return g.completeLine()
}

func (g *reasoningLoopGuard) completeLine() error {
	if !g.nonempty {
		g.line = g.line[:0]
		g.overlong = false
		return nil
	}
	key := reasoningLineKey{}
	text := ""
	if !g.overlong {
		text = strings.TrimFunc(string(g.line), reasoningSpace)
	}
	if g.overlong || utf8.RuneCountInString(text) > reasoningLoopMaxRepeatedRunes {
		// Long lines still occupy the window, but never increase repetition.
		// Neither their text nor their unbounded length is retained in the detector.
		g.serial++
		key.unique = g.serial
	} else {
		key.digest = sha256.Sum256([]byte(text))
	}
	g.line = g.line[:0]
	g.overlong, g.nonempty = false, false
	if g.counts == nil {
		g.counts = make(map[reasoningLineKey]int, reasoningLoopWindow)
	}
	if g.size == reasoningLoopWindow {
		old := g.window[g.position]
		g.counts[old]--
		if g.counts[old] == 0 {
			delete(g.counts, old)
		}
	} else {
		g.size++
	}
	g.window[g.position] = key
	g.position = (g.position + 1) % reasoningLoopWindow
	g.counts[key]++
	if g.characters < reasoningLoopMinChars || g.size != reasoningLoopWindow || len(g.counts) > reasoningLoopMaxUnique {
		return nil
	}
	repeated := 0
	for _, count := range g.counts {
		if count >= 2 {
			repeated += count
		}
	}
	if repeated*100 < reasoningLoopCoveragePercent*reasoningLoopWindow {
		return nil
	}
	message := fmt.Sprintf("检测到重复推理循环，已停止该次请求；可整理上下文后重试（本段推理%d字符，窗口%d行，唯一行%d，重复覆盖%d/%d）。",
		g.characters, g.size, len(g.counts), repeated, reasoningLoopWindow)
	return &StreamError{Code: ReasoningLoopErrorCode, Message: message, Upstream: map[string]any{
		"code": ReasoningLoopErrorCode, "type": "invalid_request_error", "message": message,
	}}
}

func newStreamState(options []StreamOptions) *streamState {
	state := &streamState{}
	if len(options) > 0 {
		state.loopGuardEnabled = options[0].ReasoningLoopGuard && reasoningLoopModel(options[0].Model)
	}
	return state
}

func streamChoiceIndex(choice map[string]any) int {
	index, _ := choice["index"].(float64)
	return int(index)
}

func (s *streamState) observeReasoningLoops(obj map[string]any) error {
	if !s.loopGuardEnabled {
		return nil
	}
	choices, _ := obj["choices"].([]any)
	progressed := make(map[int]bool)
	for _, raw := range choices {
		choice := raw.(map[string]any)
		index := streamChoiceIndex(choice)
		state := s.choices[index]
		if state.loopGuard == nil {
			state.loopGuard = &reasoningLoopGuard{}
		}
		if state.loopProgress != state.progress {
			state.loopGuard.reset()
			state.loopProgress = state.progress
			progressed[index] = true
		}
	}
	for _, raw := range choices {
		choice := raw.(map[string]any)
		index := streamChoiceIndex(choice)
		state := s.choices[index]
		if !progressed[index] {
			delta, _ := choice["delta"].(map[string]any)
			text, _ := delta["reasoning_content"].(string)
			if err := state.loopGuard.add(text); err != nil {
				return err
			}
		}
		if reason, _ := choice["finish_reason"].(string); reason != "" {
			if err := state.loopGuard.finish(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamState) finishReasoningLoops() error {
	if s.loopGuardEnabled {
		for _, choice := range s.choices {
			if choice.loopGuard != nil {
				if err := choice.loopGuard.finish(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
