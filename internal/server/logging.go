// ═══ 更新日志 ═══
// 2026-09-16：统计读取器保留底层错误，避免带末尾数据的断流被误报为正常 EOF。
// 2026-09-17：请求行加 key= 列（调用方密钥身份），并带上 prompt/completion 明细供用量账本记账。
// 2026-09-18：请求行加 in=（输入 tokens）与 hit=（其中缓存命中）两列，账本同步记录缓存维度：
//
//	思考模式下每轮都要重发整段上下文，只看 tok= 会让人觉得"用量明明很大却记了这么点"。
//
// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	// 调用方密钥身份（鉴权命中时填，单密钥模式留空 → 显示 "-"）。
	keyID    string
	keyName  string
	keyMask  string
	prompt   int
	cached   int // 输入里命中提示缓存的 token 数（<0 表示未知）
	hasUsage bool
	credit   float64
	hasCred  bool

	logged bool
}

// keyLabel 请求行里的密钥标识：优先名字，其次掩码密钥，都没有则 "-"。
func (s *chatStat) keyLabel() string {
	if s == nil {
		return "-"
	}
	if s.keyName != "" {
		return s.keyName
	}
	if s.keyMask != "" {
		return s.keyMask
	}
	return "-"
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1,
		prompt: -1, cached: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status,
		s.prompt, s.cached, s.toks, s.keyLabel())
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br        *bufio.Reader
	start     time.Time
	ttfb      time.Duration
	seen      bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage  bool // 末帧是否带 usage
	hasCredit bool // 是否出现过带 credit 的 usage（缺失≠0，见 Credit() 注释）
	tokens    int
	credit    float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt    int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）
	cached    int     // usage.prompt_cache_hit_tokens / prompt_tokens_details.cached_tokens
	pend      []byte  // 已读未返回的行缓存
	readErr   error
	dataParts []string
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// PromptTokens 返回末帧 usage.prompt_tokens（缺失为 0，与 completion 一起供用量账本累计）。
func (s *chatStatsReader) PromptTokens() int { return s.prompt }

// CachedTokens 返回末帧 usage 里「输入缓存命中」的 token 数。
// 上游用 prompt_cache_hit_tokens 报这个值，OpenAI 形状的响应放在 prompt_tokens_details.cached_tokens；
// 两者都没有时返回 -1（未知 ≠ 0，账本据此区分「没命中」与「没观测」）。
func (s *chatStatsReader) CachedTokens() int {
	if s.cached < 0 {
		return -1
	}
	return s.cached
}

// Credit 返回末帧 usage.credit（本次真实扣费）。ok=true 要求 usage 存在**且** credit
// 字段显式出现——字段缺失时 ok=false（缺失≠0：不能把"缺观测"当"0 成本"写入账本，
// 否则收费的号可能被误判 tier0 免费层）。显式 credit:0 仍是合法免费观测（ok=true）。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasUsage && s.hasCredit }

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int { return s.prompt + s.tokens }

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		s.dataParts = nil
		return
	}
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	s.dataParts = append(s.dataParts, payload)
	payload = strings.Join(s.dataParts, "\n")
	var chunk struct {
		Usage *struct {
			CompletionTokens int      `json:"completion_tokens"`
			PromptTokens     int      `json:"prompt_tokens"`
			Credit           *float64 `json:"credit"` // 指针区分「缺失」与「显式 0」
			CacheHitTokens   *int     `json:"prompt_cache_hit_tokens"`
			PromptDetails    *struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	s.prompt = chunk.Usage.PromptTokens
	if chunk.Usage.CacheHitTokens != nil {
		s.cached = *chunk.Usage.CacheHitTokens
	} else if chunk.Usage.PromptDetails != nil && chunk.Usage.PromptDetails.CachedTokens != nil {
		s.cached = *chunk.Usage.PromptDetails.CachedTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	if s.readErr != nil {
		err := s.readErr
		s.readErr = nil
		return 0, err
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.readErr = err
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// promptTokens 从聚合响应提取 usage.prompt_tokens；缺失返回 -1（缺失≠0）。
func promptTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["prompt_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// cachedTokens 从聚合响应提取输入缓存命中数：优先上游的 prompt_cache_hit_tokens，
// 其次 OpenAI 形状的 prompt_tokens_details.cached_tokens；都缺失返回 -1（缺失≠0）。
func cachedTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	if v, ok := u["prompt_cache_hit_tokens"].(float64); ok {
		return int(v)
	}
	if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := details["cached_tokens"].(float64); ok {
			return int(v)
		}
	}
	return -1
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := u["credit"].(float64)
	pt, hasPrompt := u["prompt_tokens"].(float64)
	ct, hasCompletion := u["completion_tokens"].(float64)
	if !hasCredit || (!hasPrompt && !hasCompletion) {
		return 0, 0, false
	}
	return c, int(pt) + int(ct), true
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
//
// 三个 token 列都是「上游 usage 原值」：in= 输入、hit= 输入里命中缓存的、
// tok= 输出（含思考 token）。负值表示上游没给 usage，显示 "-"（缺失≠0）。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status, prompt, cached, toks int, key string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	promptField := "-"
	if prompt >= 0 {
		promptField = fmt.Sprintf("%d", prompt)
	}
	cachedField := "-"
	if cached >= 0 {
		cachedField = fmt.Sprintf("%d", cached)
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | key=%s | uid=%s | TTFB=%s | in=%s | hit=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		key,
		uidPrefix(uid),
		ttfbMS,
		promptField,
		cachedField,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
