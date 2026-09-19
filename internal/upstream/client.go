// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
// ═══ 更新日志 ═══
// 2026-09-18：错误响应正文也遵守聊天空闲超时，避免已收到错误响应头后永久阻塞读体及账号租约。
// 2026-09-16：请求全程使用同一凭据快照，同账号刷新合并并由 Auth 原子提交，消除刷新与聊天/模型/计费的竞争。
// 2026-09-16：把明确的未批准渠道错误与内容策略拦截分开，避免伪造违禁词原因。
// 2026-09-17：合并较新模型目录、限流及传输修复，并保留请求快照与刷新合并以防回归。
// 2026-09-19：内部路径回落和同号重试在丢弃响应前反馈观测，避免已上报用量或未知状态被吞掉。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone            ErrKind = iota // 成功
	ErrHardCredit                     // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                       // 429 软限流 → 短冷却
	ErrSessionDead                    // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                       // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                         // 5xx 上游故障
	ErrContentBlocked                 // 内容策略拦截：不罚账号，返回错误并保留原请求内容
	ErrBadParams                      // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 请求终态，不轮转
	ErrAccountFault                   // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrModelBlocked                   // 11102「该后端无此模型」→ (账号,模型) 负缓存避让，切模型/切账号
	ErrClient                         // 其他 4xx / 业务错误
	ErrContextTooLong                 // 上下文超限（11115 prompt is too long）→ 请求终态，不轮转、透传原文
	ErrChannelRejected                // 明确拒绝未批准的调用渠道：请求终态，不推断为内容违规
	ErrUpstreamWAF                    // 上游 WAF 拦截页（403 HTML，按正文特征判定）：请求终态，不轮转、不回显 HTML
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrAccountFault:
		return "account_fault"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrClient:
		return "client"
	case ErrContextTooLong:
		return "context_too_long"
	case ErrChannelRejected:
		return "channel_rejected"
	case ErrUpstreamWAF:
		return "upstream_waf"
	default:
		return "none"
	}
}

// matchMode 描述错误分类 marker 的匹配通道。6 组词表 + 文案分类共用同一匹配器，
// 消除「小写 Contains + 原文 Contains 双通道」在 Classification 各分支的手写循环重复。
type matchMode uint8

const (
	// matchFold 大小写不敏感：lower(body) 含 lower(pat) 或 body 原字串含 pat。
	// 双通道与历史手写循环逐字等价（小写比较 + 中文原文比较）。
	matchFold matchMode = iota
	// matchExact 大小写敏感的字面包含。
	matchExact
	// matchLower 在 lower(body) 上做包含匹配（pat 须已小写）。
	matchLower
)

// errorRule 一条错误分类规则：命中 patterns 中任一 marker 即归类为 kind。
type errorRule struct {
	kind     ErrKind
	mode     matchMode
	patterns []string
}

// matchPattern 报告 body 是否命中单个 marker pattern。
func matchPattern(p string, mode matchMode, body, lower string) bool {
	switch mode {
	case matchFold:
		return strings.Contains(lower, strings.ToLower(p)) || strings.Contains(body, p)
	case matchExact:
		return strings.Contains(body, p)
	case matchLower:
		return strings.Contains(lower, p)
	default:
		return false
	}
}

// hit 报告 rule 是否命中 body（任一 marker 命中即真）。
func (r errorRule) hit(body, lower string) bool {
	for _, p := range r.patterns {
		if matchPattern(p, r.mode, body, lower) {
			return true
		}
	}
	return false
}

// firstHit 返回第一条命中的 marker 原文（供「哪个词命中」的场景）；无命中返回 ""。
func (r errorRule) firstHit(body, lower string) string {
	for _, p := range r.patterns {
		if matchPattern(p, r.mode, body, lower) {
			return p
		}
	}
	return ""
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardRule 余额不足关键词（大小写不敏感 + 中文原文双通道）。
var hardRule = errorRule{kind: ErrHardCredit, mode: matchFold, patterns: []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}}

// softRateRule 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateRule = errorRule{kind: ErrSoftRate, mode: matchFold, patterns: []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}}

var sessionDeadRule = errorRule{kind: ErrSessionDead, mode: matchExact, patterns: []string{"Offline user session not found", "12153"}}

// contentBlockedRule 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 泛化的安全策略文案保留原分类；若主错误明确指出调用渠道未获批准，
// Classify 会先返回 ErrChannelRejected，不能据此给调用者安上内容违规原因。
var contentBlockedRule = errorRule{kind: ErrContentBlocked, mode: matchLower, patterns: []string{
	"blocked by security policy",
}}

// wafBlockRule 上游 WAF 拦截页特征：返回的是 HTML 页面而不是 JSON 信封。
//
// 2026-09-17 实测（国际版 www.workbuddy.ai，腾讯云 WAF）：请求正文含 `<!DOCTYPE html>`、
// `<script>…</script>` 或 SQL 注入样式文本时返回 403 + 该页；纯 HTML 表格与长代码通过，
// 说明判定看正文内容、不看账号。命中即请求终态（见 handler 对应分支）：
// 换号同域同样被拦，且 HTML 页不能当作「请求参数被拒」回显给调用方。
var wafBlockRule = errorRule{kind: ErrUpstreamWAF, mode: matchLower, patterns: []string{
	"waf block page",
	"waf-intl.qq.com",
}}

// contentBlockedClientMsg 内容拦截返回给调用方的固定文案。
// [关键词] 填分类词（色情 / nsfw / 暴力 等），绝不填业务 code、账号、冷却、upstream 前缀。
const contentBlockedClientMsg = "触发网站风控违禁词，无法调用模型：内容命中网关内容防火墙规则[%s]，已被拦截。请修改内容后重试。"

const contentBlockedFallbackKeyword = "违禁词"

// contentBlockedKeywords 审核分类词，按优先级扫描上游文案（大小写不敏感）。
// 只收录可直接展示给调用方的分类标签，不收录错误码（如 11128）。
var contentBlockedKeywords = []string{
	"色情", "porn", "nsfw", "adult",
	"暴力", "violence",
	"政治", "politics",
	"赌博", "gambling",
	"毒品", "drug",
	"违禁词",
}

// ContentBlockedClientMessage 把上游内容拦截改写成网关防火墙口径，不含账号/错误码。
func ContentBlockedClientMessage(body string) string {
	return fmt.Sprintf(contentBlockedClientMsg, contentBlockedKeyword(body))
}

// contentBlockedKeyword 从审核文案抽出分类关键词；抽不到则回「违禁词」。
func contentBlockedKeyword(body string) string {
	text := body
	var env struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && strings.TrimSpace(env.Msg) != "" {
		text = env.Msg
	}
	lower := strings.ToLower(text)
	for _, kw := range contentBlockedKeywords {
		if strings.Contains(lower, kw) {
			return kw
		}
	}
	return contentBlockedFallbackKeyword
}

// contextTooLongRule 上下文超限关键词（上游 code 11115）。
//
// 定位：请求体 token 数超过模型上限，上游返回 HTTP 400 +
// "prompt is too long: N tokens > M maximum"（extError.code=context_length_exceeded）。
// 这是**调用方的会话/参数问题**，与账号健康毫无关系——换号必然失败，
// 且每次轮转都在浪费一次上游请求。
//
// 独立成 ErrContextTooLong（而非复用 ErrBadParams）的理由：ErrBadParams 仍会轮转换号，
// 而上下文超限换任何账号都是同一个结果（请求体没变），轮转纯属浪费配额。
// 本类要求：不冷却 / 不熔断 / 不计错 / **立即中止轮转**，并把上游原文透传给调用方，
// 让它看到 "prompt is too long: 1147681 tokens > 1048576 maximum" 自行压缩或换短会话。
// 若落回 ErrClient 会被「只换号不罚」白白耗尽全部账号后吐 503
// （实测 48 次上游请求仅产出 16 条 503，且用户看不到真实原因）。
// ContextTooLongDetail 从上游 11115 响应体里抽出可展示的原文（msg 字段）。
//
// 期望形态：{"code":11115,"msg":"prompt is too long: 1147681 tokens > 1048576 maximum",...}
// 抽不到 msg（形态变化 / 非 JSON）时退回整段 body，保证调用方总能看到上游给了什么。
// 绝不返回空串：空串会让调用方拿到 "prompt is too long: " 这种无信息量的句子。
func ContextTooLongDetail(body string) string {
	var env struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal([]byte(body), &env) == nil {
		if m := strings.TrimSpace(env.Msg); m != "" {
			return m
		}
	}
	b := strings.TrimSpace(body)
	if b == "" {
		return "request exceeds model context window"
	}
	return b
}

var contextTooLongRule = errorRule{kind: ErrContextTooLong, mode: matchExact, patterns: []string{
	"prompt is too long",
	`"code":11115`,
	"context_length_exceeded",
}}

// badParamsRule 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsRule = errorRule{kind: ErrBadParams, mode: matchExact, patterns: []string{
	"Unmarshal chat params failed",
	`"code":11101`,
}}

// alreadyCheckinRule "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinRule = errorRule{mode: matchFold, patterns: []string{"已签到", "already"}}

// accountFaultRule 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - "request illegal"（code 11140）→ 上游 auth/auth_forbidden，账号级授权风控
//     （实测 global 账号：同一规范化请求 A1 403/11140 vs A2 429/14017，差异全由账号
//     数据决定）。需重新 OAuth 登录才能恢复，短冷却只能阻止继续送死。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 注意 11140 **不能**按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate（上方 softRateRule
// 先命中）。故此处只收 msg 关键词 "request illegal"（auth_forbidden 的真实文案），
// 120 与 private 均落同一分类。14017 文案唯一（无软限流歧义），可安全收录。
var accountFaultRule = errorRule{kind: ErrAccountFault, mode: matchFold, patterns: []string{
	"request illegal",
	"trial not activated",
	"trial version is not yet activated",
}}

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetPattern 匹配「将在 … 重置」，捕获中间的时间串。
const softRateResetPattern = `将在 (.+?) 重置`

// 限流判定正则预编译为包级 var（发现 8）：IsModelRateLimit / ParseRateReset
// 在每次错误分类、每个限流 body 上调用，函数体内 MustCompile 是纯浪费；
// 错误风暴（429 轰炸）时尤甚。模式串均为纯常量，与 sanitize.go 的包级
// 预编译先例保持一致。regexp 并发安全（匹配只读），无需额外锁。
var (
	reModelRateLimit = regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	reSoftRateReset  = regexp.MustCompile(softRateResetPattern)
)

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	return reModelRateLimit.MatchString(body)
}

// modelBlockCode 明确指向「该后端无此模型」的业务 code（reference converter.MODEL_NOT_SERVABLE_CODES）。
const modelBlockCode = "11102"

// modelBlockMsgMarker 11102 答复的确定性文案（官方 error message 固定短语）。
// 只收这个窄短语，不收 "model ... not found" 宽正则——后者会误伤其他业务的 not found 措辞
// （reference maiphucgiang 报告提的「11102 撞在 ID 上」坑的同类问题：宁缺毋滥）。
const modelBlockMsgMarker = "service info not found"

// ModelBlockReason 11102 负缓存条目在 pool.modelCooldowns 里的 reason 前缀。
// handler 写 BlockModelBackoff；pool.BlockModelClear 按 "11102" 前缀识别条目
// （与 6004 条目的 "6004 model rate limit" reason 互不干扰，两者共存于同一 map 键）。
const ModelBlockReason = "11102 model not available"

// IsModelBlocked 报告 body 是否是「该后端无此模型」(11102) 的确定性答复。
//
// 只比对 code/msg 等独立字段，绝不做整段文本子串匹配：错误体还带 requestId 等字段，
// 拿整段文本匹配会把 "11102" 撞在 ID 上、误避让一个本来能用的模型（reference
// converter._parse_not_servable 的坑，app/model_blocks.py:408-411 讨论）。
// 判定 = code 字段精确等于 "11102"，或 msg/message 字段命中窄短语 "service info not found"
// （两者任一命中即真）。只看 400/404：429 带 11102 属限流语义（不在此判定范围）。
// 字段遍历覆盖顶层与 error 子对象两层（对齐 converter 的 nodes 收集口径）。
func IsModelBlocked(status int, body string) bool {
	if (status != http.StatusBadRequest && status != http.StatusNotFound) || body == "" {
		return false
	}
	// 轻量预检：body 既无 "11102" 又无 marker 时直接短路（大多数 4xx 零分配返回）。
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(strings.ToLower(body), modelBlockMsgMarker) {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	nodes := []map[string]any{root}
	if inner, ok := root["error"].(map[string]any); ok {
		nodes = append(nodes, inner)
	}
	code, msg := "", ""
	for _, node := range nodes {
		for _, key := range []string{"code", "errCode", "error_code"} {
			if v, ok := node[key]; ok && v != nil && code == "" {
				code = strings.TrimSpace(fmt.Sprint(v))
			}
		}
		for _, key := range []string{"msg", "message"} {
			if v, ok := node[key].(string); ok && v != "" && msg == "" {
				msg = strings.TrimSpace(v)
			}
		}
	}
	if code == modelBlockCode {
		return true
	}
	return strings.Contains(strings.ToLower(msg), modelBlockMsgMarker)
}

// ParseRateReset 从任何限流响应 body 里统一解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
//
// 是否走模型级豁免、时日对齐到 until 还是 modelCooldowns，由冷却决策侧（pool）按
// IsModelRateLimit 判定，本函数只负责「把上游明说的恢复时刻抽出来」。没有时间文案
// 的限流也照常由调用方退回有界退避（绝不臆造时间）。
func ParseRateReset(body string) (time.Time, bool) {
	m := reSoftRateReset.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// isUnapprovedChannel 只依据主错误文案识别已观测的渠道拒绝。
// code=11128、泛化 displayMsg 或回显请求中的同名文本均不能单独证明拒绝原因。
func isUnapprovedChannel(body string) bool {
	message := body
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &envelope) == nil && envelope != nil {
		message = ""
		_ = json.Unmarshal(envelope["msg"], &message)
		if message == "" {
			var upstreamError struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope["error"], &upstreamError) == nil {
				message = upstreamError.Message
			}
		}
	}
	message = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(message), "."))
	return strings.EqualFold(message, "Illegal API invocation from an unapproved channel") ||
		strings.EqualFold(message, "unapproved channel")
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  0. 11102（IsModelBlocked）——「该后端无此模型」确定性答复，语义最具体，最先判
//     （详见 IsModelBlocked 注释）。
//
// 之后在 HTTP 402 判定后识别明确渠道主错误，先于宽泛内容/额度关键词。
//  1. 402 / hardRule —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit，本次保持不变
//     （issue #28 已记录该反向误判风险，待上游原始响应确认后再定）。
//  2. sessionDeadRule —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. accountFaultRule —— 账号级授权/配额故障（11140 request illegal auth 风控、
//     14017 trial not activated register 未完成）。与 429 一起纳入轮换冷却，且必须
//     先于 softRate/status429 判定：14017 常带 429 状态码，若落到 status==429 兜底
//     会误归 soft_rate（"限流"语义不符：限流可指数退避等自愈，账号级故障等不来）。
//     11140 的 model 级限流变体（rate-limiting 文案）因 marker 不含该文案而天然
//     落到 softRateRule 层，不受影响。
//  4. softRateRule —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时在此短路，
//     结果同为 soft_rate，与下一层一致。
//  5. status==429 —— body 无文案时的兜底识别。
//  6. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
func Classify(status int, body string) ErrKind {
	// 11102「该后端无此模型」须最先判：它是「模型在后端不存在」的确定性答复，语义比
	// 计费/限流都更具体——若不先判，msg 里的 "service info not found" 虽不含余额词、
	// 但可能被更宽的 4xx 兜底归为 ErrClient（只换号不避让），该坏号会留在池内反复被选中。
	// 先于 hardRule：11102 答复的 msg 是模型不存在，不含 credit/quota/积分 等计费词，
	// 正常不会撞 hardRule，但前置判定让语义零歧义（防上游未来在 msg 里混入余额词）。
	if IsModelBlocked(status, body) {
		return ErrModelBlocked
	}
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	if isUnapprovedChannel(body) {
		return ErrChannelRejected
	}
	lower := strings.ToLower(body)
	// 上游 WAF 拦截页先于内容/参数关键词判定：它既不是内容审核文案（不得归
	// content_blocked），也不是请求参数错误（不得原样回显 HTML 给调用方）。
	if wafBlockRule.hit(body, lower) {
		return ErrUpstreamWAF
	}
	if hardRule.hit(body, lower) {
		return ErrHardCredit
	}
	if sessionDeadRule.hit(body, lower) {
		return ErrSessionDead
	}
	if accountFaultRule.hit(body, lower) {
		return ErrAccountFault
	}
	if softRateRule.hit(body, lower) {
		return ErrSoftRate
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
	// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
	// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
	// 归 ErrBadParams：不冷却/不熔断/不计错，但**仍然轮转**（不同账号可能有不同的
	// 模型权限，值得再试一次）。
	if status >= 400 {
		if contentBlockedRule.hit(body, lower) {
			return ErrContentBlocked
		}
		if contextTooLongRule.hit(body, lower) {
			return ErrContextTooLong
		}
		if badParamsRule.hit(body, lower) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardRule 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 键按 realm 分层（map[realm]map[model][]efforts）：CN 探测结果不得被 global 同模型名
	// 请求复用（同名不同档位会错误降级，C-2）。global 侧暂无 efforts 探测 → 桶缺失即透传。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string

	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string

	// globalModels 缓存 global 模型名目录纯动态探测结果（1h TTL + 5min 负缓存），
	// 见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 兼容旧配置；已废弃，不再清洗或替换业务文本。
	SanitizeFingerprints bool

	// OutboundImageBudgetBytes 出站请求体字节预算（<=0 = 不裁剪）。
	// 入站两道边界（nginx client_max_body_size / 网关 max_body_mb）只决定「接不接收」，
	// 不代表上游能收多大。这里在出站咽喉做按字节裁剪：超预算时从最旧的图片开始
	// 替换为文本占位（见 image_budget.go），保证发往上游的体积始终在验证过的范围内。
	OutboundImageBudgetBytes int

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认 WorkBuddy
	// 三段式与 billingUA 单段式）。空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing/checkin 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空，见 billingUA）。
	// issue #42 深挖：官网「使用端」列基于出站请求的 UA/X-Product 服务端归因，
	// 官方 WorkBuddy 桌面 UA 见 defaultWorkBuddyUAFor。默认值已对齐官方（A 段变更），
	// 用户仍可显式配置完全自定义的 UA。
	UserAgent string

	// DeviceToken 设备风控 Token（X-Device-Token 头）兜底来源：config upstream.device_token。
	// 仅当 auth.Auth.DeviceToken 为空时才取此值；两者皆空则不注入该头。
	// 容器内无桌面端 Turing SDK，这是把外部（宿主/桌面端）生成的 token 注入的入口。
	// 另见 DeviceTokenFile 缓存读取：宿主可把 token 落 /app/data/device_token 共用。
	DeviceToken string

	// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
	// 读取频率限 5 分钟一次缓存（见 device_token.go），>1KB 或读失败则忽略。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceTokenFile string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空（默认）= "WorkBuddy"：伪造官方桌面端指纹（X-IDE-* 四头 + X-Agent-Purpose，
	// 见 injectAttribution / attributionClientName）。显式配 "SaaS" 还原旧行为
	// （仅 X-Product="SaaS"，不设 X-IDE-*）；配其他值则四头跟随该值。
	ClientName string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + B 段的
	// X-IDE-Version）。空 = 内置默认 defaultClientVersion（对齐官方 5.5.4 分发包）。
	// config upstream.client_version 覆盖。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认 defaultCliVersion
	// （对齐官方内置 CLI 2.137.1）。config upstream.cli_version 覆盖。
	CliVersion string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 参数传入 ChatStream，
	// 由 ChatHeaders 注入（不再挂共享字段，杜绝并发串扰）。
	PassthroughIP bool

	ChatBaseCN    string
	BillingBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 即显式逃生门：即使用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: false,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体；SanitizeFingerprints 仅兼容旧配置，不再改写正文。
// 显式传 realm 使 effort 降级按域取桶：CN 探测信息不得作用到 global 请求（C-2）。
// conversationID 为网关解析出的会话标识（用于 prompt_cache_key 注入的会话段；
// body 里自带 conversation_id 时以 body 为准）。uid8 来自账号 UID，是跨账号硬隔离段。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts, defs := c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm)
	if realmKey(realm) == "global" {
		// global 域降级源 = 远端探测桶（权威）∪ 产品静态兜底表（全局 21 名内档位如
		// deepseek-v4.1-flash ['high']）。当前探测桶为空时也按静态表降级，不全程透传
		// （issue #84：往 WorkBuddy 上游发 low/max 非法，须降级到 high）。
		efforts, defs = globalEffortMap(efforts, defs)
	}
	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints, efforts, defs)
	// prompt_cache_key 注入（P0 费用优化，费用降 ~17×）：按账号隔离的稳定缓存键，
	// 让同一客户端对同一账号的连续请求命中上游前缀缓存。
	body = InjectPromptCacheKey(body, uid, conversationID)
	// 出站图片预算（最后一环）：前面所有改写都可能让体积膨胀，这里统一按字节收口。
	// 放在 prompt_cache_key 之后：裁剪只动图片 part，缓存键不受影响。
	body = ShrinkOutboundImages(body, c.OutboundImageBudgetBytes)
	return body
}

// effortsSnapshot 返回指定 realm 的 effort 能力缓存副本；该域无探测 → nil（透传不降级）。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 缓存副本；
// 该域无探测或无声明默认档 → nil（thinking.go 回退硬编码 high）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

// storeEfforts 按 realm 写入 effort 能力缓存桶（efforts + defaultEfforts），并发安全。
// 供 CN FetchModels 与 global 探测共用：拉取到的模型档位落桶后，出站请求体 normalizeReasoningEffort
// 才能按域降级。efforts 与 defs 均空时删除该 realm 桶（等价「该域无可降级档位」）。
// 调用方负责在「无新数据」时跳过写（CN 侧空桶不清既有桶，见 FetchModels 尾部）。
func (c *Client) storeEfforts(realm string, efforts map[string][]string, defs map[string]string) {
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	k := realmKey(realm)
	if len(efforts) == 0 && len(defs) == 0 {
		delete(c.efforts, k)
		delete(c.defaultEfforts, k)
		return
	}
	c.efforts[k] = efforts
	c.defaultEfforts[k] = defs
}

// GlobalEffortSnapshot 导出 global 域 effort 能力缓存（探测下发 ∪ 静态兜底合并后的桶），
// 供 /v1/models 输出 reasoning_supported_efforts / reasoning_default_effort。
// 返回副本；桶未填充（无 global 账号或从未探测）→ nil（调用方回落静态兜底表）。
func (c *Client) GlobalEffortSnapshot() (efforts map[string][]string, defaults map[string]string) {
	return c.effortsSnapshot("global"), c.defaultEffortsSnapshot("global")
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（R9：国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths 按 realm 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report 不参与，
// 其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths 同上，针对 daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
// body 读失败（连接中断/空闲掐流/截断）返回普通错误（非 *Error）——半截 body 不进
// Classify，不参与账号惩罚（传输层故障不该喂熔断误罚号）。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// refreshIOTimeout 单次 refresh 网络调用的总时长上限；网络调用不持凭据锁。
const refreshIOTimeout = 30 * time.Second

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 同账号并发刷新由 Auth.RefreshOnce 合并；URL 与全部头字段使用同一凭据快照。
func (c *Client) RefreshToken(a *auth.Auth) error {
	return a.RefreshOnce(func(snapshot *auth.Auth) (auth.TokenUpdate, error) {
		if strings.TrimSpace(snapshot.RefreshToken) == "" {
			return auth.TokenUpdate{}, fmt.Errorf("no refreshToken")
		}
		url := c.chatBase(snapshot) + "/v2/plugin/auth/token/refresh"
		ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return auth.TokenUpdate{}, err
		}
		c.RefreshHeaders(req, snapshot)
		data, err := c.doJSON(req)
		if err != nil {
			return auth.TokenUpdate{}, err
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(data, &tok); err != nil || strings.TrimSpace(tok.AccessToken) == "" {
			return auth.TokenUpdate{}, fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
		}
		update := auth.TokenUpdate{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, Domain: tok.Domain}
		if tok.ExpiresIn > 0 {
			update.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
		}
		return update, nil
	})
}

// chatPath 按 realm 返回 chat 端点路径（不含 base）：
// global → /v2/chat/completions（404/405 时由 ChatStream fallback /console/chat/completions）；
// cn → /v2/chat/completions（现状逐字，零回归）。
//
// 2026-09-17 国际版客户端抓包：官方 CLI host 实发 /v2/chat/completions（同域其它接口走
// /v2/…、/console/as/… 混排，chat 只走 /v2）。global 早前优先 /console 是上游新旧
// 路径分叉期的兼容顺序，与官方客户端不一致；WAF 按路径分规则，非官方路径更易被判。
// 现改为官方顺序：先 /v2，仅 404/405 才回落 /console。
func (c *Client) chatPath(a *auth.Auth) string {
	return chatCompletionsPath
}

// 路径常量：CN 现状路径（chatCompletionsPath）与 global 双候选路径。
const (
	chatCompletionsPath   = "/v2/chat/completions"
	globalChatConsolePath = "/console/chat/completions"
)

// chatFallbackHTTPStatus global chat fallback 只在 404/405 时发生（R9：上游新旧路径分叉）。
func chatFallbackHTTPStatus(status int) bool { return status == 404 || status == 405 }

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// DeptestOnly: 全库仅 upstream 包测试引用；生产全走 ChatStreamContext
// （handler 传 r.Context()）。迁 export_test.go 不可行——测试需要真实
// HTTP 回放走完整 chatPaths/monitorBody 链路，与生产共用同一实现。
// 等价于 ChatStreamContext(context.Background(), ...)：不带调用方取消语义。
// 新调用方应优先用 ChatStreamContext 传入请求 ctx（客户端断连即中断在途调用、释放租约）。
//
// global realm：先打 /v2/chat/completions（官方客户端实测路径），404/405 时同一 base
// 二次换 /console/chat/completions（旧路径兜底）
// （上游新旧路径分叉，PLAN R9 fallback 顺序）。cn：/v2/chat/completions 现状不变。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但从 ctx 派生请求 context：调用方（handler）传入
// r.Context() 后，客户端断连/请求取消会立即中断在途上游调用、释放连接与账号在途名额，
// 不再空转到 IdleTimeout。ctx 为 nil 时回落 Background。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	a = a.Snapshot()
	if ctx == nil {
		ctx = context.Background()
	}
	// global 首次路径 404/405 时换 fallback 路径重试；ensureConsoleSystem 在 prepareBody 后统一套用
	// 全局脚本：首条消息非 system 时前置兜底 system（防 console 域上游 code 11-128）。
	prepared := c.prepareBody(body, a.Realm(), a.UID, meta.ConversationID)

	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	// reqCtx 的 cancel 在每个出口显式调用（Do 失败 / ≥400 / 成功分支移交 monitorBody），
	// 循环本身各分支必 return——无循环尾兜底代码（此前外层 var cancel 从未赋值 + 尾部
	// 不可达 cancel() 是潜伏 nil-panic，已删；chatPaths 恒非空由构造保证）。
	pathCount := len(c.chatPaths(a))
	for attempt, path := range c.chatPaths(a) {
		url := c.chatBase(a) + path
		// wafLevel：WAF 断词升级档位。0 = 原样重发前；1 = 模式级断词；
		// 2 = 风险 token 断词；3 = 逐 token 断词。原样请求先发，只有真的撞到
		// WAF 拦截页才改写，正常请求的正文不变；每档最多再发一次，避免反复拉扯。
		wafLevel := 0
		// channelNeutralized：渠道校验触发句是否已断词重发过一次。与 wafLevel 同构，
		// 每路径最多一次，避免反复拉扯。
		channelNeutralized := false
		lastSent := prepared
	retry:
		for {
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
			if err != nil {
				return nil, 0, nil, err
			}
			c.ChatHeaders(req, a, clientIP, meta)
			// 从调用方 ctx 派生：保留取消传播（父 ctx 取消 → 本 ctx 取消），
			// 同时 monitorBody.Close 仍能独立 cancel 本分支（空闲掐流）。
			reqCtx, cancel := context.WithCancel(ctx)
			req = req.WithContext(reqCtx)
			resp, err := c.chatHTTP().Do(req)
			if err != nil {
				cancel()
				log.Printf("ERR: [upstream] chat_stream uid=%s: transport error: %v", logfmt.UID8(a.UID), err)
				return nil, 0, nil, err
			}
			if resp.StatusCode >= 400 {
				errorBody := monitorBody(resp.Body, c.IdleTimeout, cancel)
				raw, rerr := io.ReadAll(io.LimitReader(errorBody, 1<<20))
				errorBody.Close()
				cancel()
				// body 读失败（掐流/截断）→ 传输层错误：半截 raw 不交回调用方进 Classify，
				// 否则 handler 侧 applyErrorPolicy 会按误判分类罚号。
				if rerr != nil {
					log.Printf("ERR: [upstream] chat_stream uid=%s: read body: %v", logfmt.UID8(a.UID), rerr)
					return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
				}
				kind := Classify(resp.StatusCode, string(raw))
				log.Printf("WARN: [upstream] chat_stream uid=%s: upstream %d %s body=%s",
					logfmt.UID8(a.UID), resp.StatusCode, kind, truncate(string(raw), 200))
				// 国际版 WAF 按正文特征拦截（脚本、命令注入、路径穿越等），返回的是前置
				// WAF 的拦截页而非模型答复。按两级做零宽断词后同路径重发：一级按已知模式
				// 断词，二级按 token 断词覆盖命令注入/路径穿越族。零宽字符不参与词义，
				// 模型读到的仍是同一段文本，用户不必自己删改内容。
				// 每档最多重发一次；仍被拦则按请求终态返回，交 handler 回可行错误文案。
				if kind == ErrUpstreamWAF {
					// 逐级升级：某一级没有可断词内容时直接尝试下一级，而不是放弃。
					for wafLevel < 3 {
						var candidate []byte
						switch wafLevel {
						case 0:
							candidate, _ = NeutralizeWAFTriggers(prepared)
						case 1:
							candidate, _ = NeutralizeWAFTokens(prepared)
						default:
							candidate, _ = NeutralizeWAFTokensDeep(prepared)
						}
						wafLevel++
						if bytes.Equal(candidate, prepared) {
							continue
						}
						prepared = candidate
						log.Printf("WARN: [upstream] waf neutralized level=%d for retry uid=%s path=%s", wafLevel, logfmt.UID8(a.UID), path)
						break
					}
					if wafLevel > 0 && !bytes.Equal(prepared, lastSent) {
						lastSent = prepared
						observeChatRetry(ctx, raw)
						continue retry
					}
				}
				// 渠道校验（11128 unapproved channel）：上游按系统说明里的渠道归属句
				// 判定客户端来自未授权渠道。零宽断词该句后同路径重发一次即可通过
				// （与 WAF 断词同技，词义不变，模型仍读到同一句话）。原样请求先发，
				// 只有真的被拒才改写，正常客户端（说明里没有该句）的正文不变。
				if kind == ErrChannelRejected && !channelNeutralized {
					if candidate, changed := NeutralizeChannelTrigger(prepared); changed {
						prepared = candidate
						channelNeutralized = true
						lastSent = prepared
						log.Printf("WARN: [upstream] channel trigger neutralized for retry uid=%s path=%s", logfmt.UID8(a.UID), path)
						observeChatRetry(ctx, raw)
						continue retry
					}
				}
				// global 首次路径 404/405 → 换 fallback 路径重试；其余状态码直接返回。
				if attempt < pathCount-1 && chatFallbackHTTPStatus(resp.StatusCode) {
					observeChatRetry(ctx, raw)
					break retry
				}
				return nil, resp.StatusCode, raw, nil
			}
			// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
			// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
			// 取消传播由 http.Transport 在 body Close / 父 ctx 取消时处理，连接正常清理。
			return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
		}
	}
	panic("unreachable: chatPaths is never empty") // for range 空集时编译器仍要求兜底 return；chatPaths 恒非空（构造保证），永不触达
}

// chatPaths 返回按 realm 的 chat 路径候选序列：
// global → [/v2, console]（向 Fallback 迭代，官方路径优先）；
// cn → [/v2]（单元素，现状）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{chatCompletionsPath, globalChatConsolePath}
	}
	return []string{chatCompletionsPath}
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens + 上游模型对象全字段）。
// CN /console 与 global /v2 的模型对象同构（2026-09-15 global 真实账号 /v2 探测
// 实证，字段集与任务书 hy3 样本一致），故共用此结构；上游省略的字段保持零值，
// /v1/models 侧按「空值省略」透出（不编造）。
type ModelInfo struct {
	ID             string
	Name           string
	ContextWindow  int64    // = maxInputTokens
	MaxTokens      int64    // = maxOutputTokens
	Efforts        []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort  string   // reasoning.defaultEffort（空=未声明，thinking.go 回退硬编码）
	SupportsImages bool     // 顶层 supportsImages（多模态能力，透出到 /v1/models）

	// 以下为模型目录全字段补齐（任务书 models-full-fields）：
	Description       string   // descriptionZh 中文描述
	Credits           string   // credits 积分倍率原文（如 "x0.05"），仅展示不参与选号
	Tags              []string // tags 模型标签（含 badge:限时免费 等）
	Vendor            string   // vendor 厂商标识
	IsDefault         bool     // isDefault 是否默认模型
	SupportsReasoning bool     // supportsReasoning 是否支持推理
	SupportsToolCall  bool     // supportsToolCall 是否支持工具调用
	OnlyReasoning     bool     // onlyReasoning 是否纯推理模型
	MaxAllowedSize    int64    // maxAllowedSize 最大允许上下文（与 maxInputTokens 口径并列，上游各自下发）
	ReasoningEffort   string   // reasoning.effort 推理模式（与 supportedEfforts 数组不同源）
	ReasoningSummary  string   // reasoning.summary 推理摘要模式（如 "auto"）
}

// dynModelEntry 上游模型目录（CN /console 与 global /v2 同构）的单条模型解析形态，
// FetchModels 与 global_models.go 的探测共用。iconUrl/descriptionEn/生成参数等
// 按「不透出」原则不解析（任务书 §不透出字段）。
type dynModelEntry struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	Vendor          string   `json:"vendor"`
	IsDefault       bool     `json:"isDefault"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	MaxAllowedSize  int64    `json:"maxAllowedSize"`
	Disabled        bool     `json:"disabled"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
	SupportsTool    bool     `json:"supportsToolCall"`
	OnlyReasoning   bool     `json:"onlyReasoning"`
	Reasoning       struct {
		Effort           string   `json:"effort"`
		Summary          string   `json:"summary"`
		DefaultEffort    string   `json:"defaultEffort"`
		SupportedEfforts []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// modelInfo 按解析条目构造 ModelInfo（dynEntry→ModelInfo 映射的单一事实来源，
// CN FetchModels 与 global 探测共用，杜绝两域映射漂移）。
func (m dynModelEntry) modelInfo() ModelInfo {
	return ModelInfo{
		ID:                m.ID,
		Name:              m.Name,
		ContextWindow:     m.MaxInputTokens,
		MaxTokens:         m.MaxOutputTokens,
		Efforts:           m.Reasoning.SupportedEfforts,
		DefaultEffort:     m.Reasoning.DefaultEffort,
		SupportsImages:    m.SupportsImages,
		Description:       m.Description,
		Credits:           m.Credits,
		Tags:              m.Tags,
		Vendor:            m.Vendor,
		IsDefault:         m.IsDefault,
		SupportsReasoning: m.SupportsReason,
		SupportsToolCall:  m.SupportsTool,
		OnlyReasoning:     m.OnlyReasoning,
		MaxAllowedSize:    m.MaxAllowedSize,
		ReasoningEffort:   m.Reasoning.Effort,
		ReasoningSummary:  m.Reasoning.Summary,
	}
}

// 模型目录端点路径常量（按 realm 切）：
// CN 现状 /console/enterprises/personal/models 逐字保留（零回归）；
// global 走 /v2/enterprises/personal/models（PR #20 实测 /console 500、/v2 200 含
// credits 倍率的完整模型表）。modelsPath 按 globalOn 分发。
// v3ConfigPath 是 CN/global 双域通用的 /v3/config 模型目录端点（v3 系客户端权威
// 目录；UA 门禁：仅三段式 CLI UA 可过，CommonHeaders 即满足，见
// .claude/reports/global-models-missing.md）：v3 为主、企业端点补缺（任务书
// v3-config-merge）。
const (
	cnModelsPath     = "/console/enterprises/personal/models"
	globalModelsPath = "/v2/enterprises/personal/models"
	v3ConfigPath     = "/v3/config"
)

// modelsPath 按 realm 返回动态模型目录端点路径（不含 base）。
// CN → /console/enterprises/personal/models（现状，零回归）；
// global → /v2/enterprises/personal/models（国际版实测可用路径，见 global_models.go
// probe 家族）：governed by globalOn（config global.enabled + 账号 realm 双闸）。
func (c *Client) modelsPath(a *auth.Auth) string {
	if c.globalOn(a) {
		return globalModelsPath
	}
	return cnModelsPath
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。三类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。
//   - tags 含 text-to-image：图片生成模型，非本网关用途。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		if t == "text-to-image" {
			return true
		}
	}
	return false
}

// FetchModels 调上游动态模型接口（CN 侧；global 账号按 modelsPath 走 /v2，v3 补充
// 见 global_models.go FetchGlobalModels 家族，本方法职责不变）。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
//
// v3-config-merge：动态目录 = /v3/config（主）+ 企业端点（/console 或 global /v2，
// 补缺）的并集，两路**并发**探测（任务书 v3-config-merge 需求 3/4）。两域口径各自
// 保留：CN 按 agents[cli].models 过滤（零回归）；合并去重 key = 模型 id，v3 条目优先
// （credits 等字段以 v3 为准），企业端点只补 v3 缺失的模型。/v3 失败（400/网络错/解析
// 失败）不拖累企业端点结果——降级为仅企业端点，warn 日志；反之亦然（两路独立容错）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	// 两路目录探测共享同一代凭据，后续刷新不能改变本轮路由或缓存归属。
	a = a.Snapshot()
	type probeResult struct {
		infos []ModelInfo
		err   error
	}
	enterpriseCh := make(chan probeResult, 1)
	v3Ch := make(chan probeResult, 1)
	go func() {
		infos, err := c.fetchEnterpriseModels(a)
		enterpriseCh <- probeResult{infos, err}
	}()
	go func() {
		infos, err := c.fetchV3Models(a)
		v3Ch <- probeResult{infos, err}
	}()
	enterprise := <-enterpriseCh
	v3 := <-v3Ch
	if enterprise.err != nil && v3.err != nil {
		return nil, enterprise.err // 两路全失败：返回企业端点错误（既有调用方语义零漂移）
	}
	if v3.err != nil {
		// /v3 失败降级：不拖累企业端点结果（任务书实现要点：降级仅企业端点 + warn）。
		log.Printf("WARN: [upstream] fetch models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] fetch models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
	}
	out := mergeModelInfos(v3.infos, enterprise.infos)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入 efforts 桶）。
	// 空桶时跳过写：避免「某探测无档位数据」清掉既有桶（例：cn 桶已有档位，再次探测返回全无等级 → 不应清空）。
	cache := make(map[string][]string, len(out))
	defCache := make(map[string]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	if len(cache) == 0 && len(defCache) == 0 {
		return out, nil
	}
	// 按探测账号的 realm 写入对应桶：CN 探测只进 cn 桶，global 同模型名不被污染（C-2）。
	c.storeEfforts(a.Realm(), cache, defCache)
	return out, nil
}

// mergeModelInfos 合并两路模型目录：primary 为主（同 id 以 primary 条目为准——
// credits 等字段以主端点为权威），secondary 只补 primary 缺失的 id。
// 去重 key = 模型 id；输出顺序 = primary 原序在前、secondary 补充项（secondary 原序）
// 在后——稳定输出，不依赖 map 迭代序（任务书实现要点：排序保持稳定）。
func mergeModelInfos(primary, secondary []ModelInfo) []ModelInfo {
	if len(secondary) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary)+len(secondary))
	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	for _, mi := range primary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	for _, mi := range secondary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	return out
}

// fetchEnterpriseModels 单路探测企业模型端点（CN → /console/enterprises/personal/models；
// global → /v2/enterprises/personal/models，按 modelsPath 分发）。解析口径：对象形态
// agents[cli].models 过滤 + nonChatModel 剔除 + disabled 剔除（既有 FetchModels 逐字保留，
// v3-config-merge 重构抽出的单路函数）。
func (c *Client) fetchEnterpriseModels(a *auth.Auth) ([]ModelInfo, error) {
	a = a.Snapshot()
	url := c.chatBase(a) + c.modelsPath(a)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误（handler 侧该路径不 NoteError，见发现 6 的正确行为）。
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	// dynMap 收集模型字段；nonChatModel 过滤在写入 dynMap 前执行，
	// 确保非对话条目（nes-/completion-/codewise- 前缀、maxOutputTokens≤256、
	// tags 含 text-to-image）根本不进返回列表（来源：harness buddy.ts:547-555）。
	dynMap := make(map[string]dynModelEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}
		dynMap[m.ID] = m
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, m.modelInfo())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// fetchV3Models 单路探测 /v3/config（CN/global 双域通用，按 chatBase 切 base）。
// 解析口径与 parseGlobalModelNames 对象形态一致（data.models[].id 优先、disabled 剔除、
// 全字段落 ModelInfo）；v3 独有的 contextWindow/agent modelTags 额外字段自然忽略。
// CN 侧不按 agents[cli] 过滤（与 global 探测口径一致：v3 面取全量 models）——
// 实测 CN v3 51 模型含大量非 cli 面模型，cli 过滤后与 console 口径才有可比性；
// 但 mergeModelInfos 以 enterprise（已 cli 过滤）为 secondary 补缺，v3 全量条目中
// 只有企业端点缺失的 id 会进并集，实际生效口径仍是「cli 面并集」。
func (c *Client) fetchV3Models(a *auth.Auth) ([]ModelInfo, error) {
	a = a.Snapshot()
	url := c.chatBase(a) + v3ConfigPath
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// CommonHeaders 三段式 CLI UA 实测可通过 /v3/config 的 UA 门禁
	// （Bearer + web UA → 400 code 12403，见 global-models-missing.md）。
	c.CommonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v3 config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	names, infos, _, _, err := parseGlobalModelNames(raw)
	if err != nil {
		return nil, fmt.Errorf("v3 config parse: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("v3 config empty list")
	}
	// v3 面全量 models 不经 agents[cli] 过滤，nes-/completion-/codewise- 嵨补全/图片
	// 生成等非对话条目（企业端点的 nonChatModel 口径）同样要挡在目录外——
	// 这里按同一 nonChatModel 规则过滤（selected ID 会选模型报 code=11102）。
	out := make([]ModelInfo, 0, len(infos))
	for _, mi := range infos {
		if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
			continue
		}
		out = append(out, mi)
	}
	return out, nil
}

// billingMeterJSON 按 realm 候选路径发 billing/meter 域请求，ErrNotFound 时换下一候选路径
// （global：/billing/meter/* → /v2/billing/meter/*；cn：单路径 /v2/billing/meter/* 现状）。
func (c *Client) billingMeterJSON(a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	a = a.Snapshot()
	var lastErr error
	for i, p := range paths {
		data, err := c.billingJSON(a, method, p, body)
		if err != nil {
			lastErr = err
			var ue *Error
			if i < len(paths)-1 && errors.As(err, &ue) && ue.Kind == ErrNotFound {
				continue // /billing/meter/* 404 → 换 /v2/billing/meter/*
			}
			return nil, err
		}
		return data, nil
	}
	return nil, lastErr
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	remain, _, err = c.UserResourceDetailed(a, 0)
	return remain, err
}

// CreditBuckets 按到期紧迫度拆分的积分余额（供 pool 优先消耗快过期积分）。
// 背景（issue:积分过期）：套餐/奖励积分按 PackageEndTime 分批过期，总量口径的
// remain 会让"明天就作废"的积分与"30 天后才过期"的积分被无差别选号，
// 导致快过期积分没优先用掉、白白作废。拆桶后选号可优先消耗 Expiring。
type CreditBuckets struct {
	// Expiring 在 soon 窗口内（<= now+soon）即将过期的可用积分。
	Expiring int64
	// Stable 其余有效积分（到期时间更远或无到期时间）。
	Stable int64
}

// Total 返回两桶合计可用积分（= UserResource 的 remain 口径）。
func (b CreditBuckets) Total() int64 { return b.Expiring + b.Stable }

// packageEndLayout 上游 PackageEndTime 的时间格式（与请求体过滤串同口径）。
const packageEndLayout = "2006-01-02 15:04:05"

// UserResourceDetailed 同 UserResource，但按到期时间把余额拆成 CreditBuckets。
// soon>0 时把到期时间 <= now+soon 的套餐余额计入 Expiring；soon<=0 时全部归 Stable。
// PackageEndTime 解析失败/缺失的套餐保守归入 Stable（不误标为快过期而插队）。
// 单套餐取数口径（Cycle* 优先）与 UserResource 完全一致，保证向后兼容。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain int64, buckets CreditBuckets, err error) {
	a = a.Snapshot()
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return 0, CreditBuckets{}, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					PackageEndTime      string `json:"PackageEndTime"` // "2006-01-02 15:04:05"，缺省/空 = 无到期
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, CreditBuckets{}, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
		// 分桶：仅 soon>0 且能解析出有效到期时间、且确实在窗口内 → Expiring。
		if soon > 0 && r > 0 && acct.PackageEndTime != "" {
			// 上游时间为 UTC+8 墙钟（与 softRateResetLoc 同口径，官网展示时区）。
			if end, perr := time.ParseInLocation(packageEndLayout, acct.PackageEndTime, softRateResetLoc); perr == nil {
				if !end.After(now.Add(soon)) {
					buckets.Expiring += r
					continue
				}
			}
		}
		buckets.Stable += r
	}
	return remain, buckets, nil
}

// ResourceSummary 查询账号积分套餐的完整聚合口径（remain=剩余可花积分、used=已用、
// size=总量、packs=套餐数），供运维工具（cmd/credit）按 realm 展示真实余额。
// 与 UserResource 的差异：UserResource 只取 remain；本方法额外聚合 used/size/packs，
// 且 TotalDosage 作 size 下限（与 cmd/credit 历史口径一致，见其 packageRemainUsed）。
//
// realm 感知继承 billingMeterPaths：global 账号打 workbuddy.ai /billing/meter/*（404
// fallback /v2），CN 账号维持 /v2/billing/meter/get-user-resource（现状逐字，零回归）。
func (c *Client) ResourceSummary(a *auth.Auth) (remain, used, size int64, packs int, err error) {
	a = a.Snapshot()
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalDosage int64 `json:"TotalDosage"`
				Accounts    []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		r, u, s := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		remain += r
		used += u
		size += s
	}
	packs = len(resp.Response.Data.Accounts)
	// TotalDosage 作 size 下限（历史口径：已消耗的不该比总剂量小）。
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := resp.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, nil
}

// respAccount 供 packageRemainUsed 解析的套餐字段（与 cmd/credit resourcePackage 同构）。
type respAccount struct {
	CapacityRemain      int64
	CapacityUsed        int64
	CapacitySize        int64
	CycleCapacityRemain int64
	CycleCapacityUsed   int64
	CycleCapacitySize   int64
}

// packageRemainUsed 聚合单套餐的 remain/used/size（历史口径见 cmd/credit/billing.go，
// 迁移至此作为单一事实来源）。Cycle 期套餐优先：用 CycleCapacity 三字段，
// used 取 CycleUsed 与 size-remain 的较大者；否则回退 Capacity 三字段。
func packageRemainUsed(a respAccount) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	a = a.Snapshot()
	_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
	return err
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	return alreadyCheckinRule.hit(ue.Msg, strings.ToLower(ue.Msg))
}

func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}
