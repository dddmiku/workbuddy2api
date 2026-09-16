// Package pool 账号池：单一状态机（健康/冷却/熔断）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
package pool

import (
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 冷却到次日 04:00（等签到恢复）
	CoolSoft                 // 429 → 短冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID           string    `json:"uid"`
	Realm         string    `json:"realm,omitempty"`
	Nickname      string    `json:"nickname,omitempty"`
	Credits       int64     `json:"credits"`
	Cooling       bool      `json:"cooling"`
	CoolKind      string    `json:"cool_kind,omitempty"`
	CoolRemaining int64     `json:"cool_remaining_sec,omitempty"`
	Until         time.Time `json:"until,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	SoftStreak    int       `json:"soft_streak,omitempty"` // 连续软冷却次数（指数退避指数，见 entry.softStreak）
	// RateLimitedModels 当前仍在限额的模型列表（issue #36 限额台账）。
	// 仅「带解析时间 6004」触发的模型级独立冷却（modelCooldowns 未到期条目）时非空，
	// 每模型一行；运维据此看到"账号 A 的模型 X 还在限额中，预计 Z 时间恢复"。到期即消失（零回归）。
	RateLimitedModels []RateLimitedModel `json:"rate_limited_models,omitempty"`
	Disabled          bool               `json:"disabled"`
	DisabledReason    string             `json:"disabled_reason,omitempty"` // 仅 disabled 账号：禁用原因（运维可见）
	SuccessCount      int64              `json:"success_count,omitempty"`
	ErrTotal          int64              `json:"err_total,omitempty"`
	LastSuccessTime   time.Time          `json:"last_success,omitempty"`
	LastErrTime       time.Time          `json:"last_err,omitempty"`
	// 运行态（不持久化）：在途请求数 + 熔断器状态。
	InFlight     int       `json:"in_flight"`
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}

// RateLimitedModel 单个被限流模型的台账行（issue #36）。
type RateLimitedModel struct {
	Model string `json:"model"`
	// Until 冷却到期时刻 = 该模型的独立冷却截止（modelCooldowns[m].Until，截断后），
	// 多模型限流时不再等于 Status.Until（账号级）。
	Until time.Time `json:"until,omitempty"`
	// ResetAt 上游「将在 … 重置」的原始墙钟（未经 soft_rate_max 截断）；未截断时
	// Until==ResetAt（两者同值）。截断/未截断都透出，台账始终可见上游权威时点。
	ResetAt time.Time `json:"reset_at,omitempty"`
	// Reason 触发原因（透出运维可读文案，同 Status.Reason）。
	Reason string `json:"reason,omitempty"`
}

type entry struct {
	a       *auth.Auth
	credits int64
	// creditsExpiring 即将过期（签到时按 expiringSoon 窗口判定）的可用积分子集，
	// 是 credits 的一部分（credits = creditsExpiring + 长期积分）。选号权重对其
	// 额外加成：优先消耗快过期积分，避免官方活动赠送的奖励积分到期作废
	// （issue:积分过期）。持久化（stateAccount.CreditsExpiring）：重启后到下次
	// 签到之间第四因子（weightOf ×8）不应失忆——签到 09:00/21:00 定期刷新，
	// 窗口外重启会丢快过期积分偏好，可能让奖励积分到期作废。
	creditsExpiring int64
	successCount    int64 // 累计成功
	errTotal        int64 // 累计错误（终身累计，供状态展示与 EMA 反推；选号权重改用下方 EMA）
	// successEMA / errorEMA 成功率的 EMA 观测（替代终身累计比率做选号权重）：
	// 旧口径 successCount/(successCount+errTotal) 终身不衰减——历史故障永久压低
	// 权重、长寿账号区分度收敛。EMA 让近期行为主导（成功事件拉 successEMA、错误
	// 事件拉 errorEMA，比率 = successEMA/(successEMA+errorEMA)）。alpha=0.1
	// （比 NoteModelCost 的 0.3 更平滑——选号权重不应被单次成败主导）。
	// 持久化（stateAccount.SuccessEMA/ErrorEMA）；旧 state.json 缺字段时从
	// successCount/errTotal 反推初始值（向后兼容）。
	successEMA  float64
	errorEMA    float64
	lastErr     time.Time // 最近一次错误时间
	lastSuccess time.Time // 最近一次成功时间
	coolKind    CoolKind
	until       time.Time // 冷却截止（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）
	disabled    bool
	reason      string
	lastUsed    time.Time // 最近被选中时刻（防并发撞号）
	// usedSeq 单调递增的选中序号：每次被 pick 选中时取 p.pickSeq 自增值。
	// Windows 等平台 time.Now() 精度有限（~0.5ms），高并发/快速连续选号时多个
	// 账号 lastUsed 完全相等，基于 wall-clock 的 LRU/防惊群判定失效（高并发/低精度时钟下：
	// lastUsed 全等 → LRU Before 全 false → 恒选 candsAll[0] → 集中单号）。
	// usedSeq 提供严格全序，与时间精度无关。运行态，不持久化。
	usedSeq uint64
	// breakerUntil / fails / retryCount 为熔断器运行态。
	// breakerUntil + retryCount 持久化（stateAccount.BreakerUntil/RetryCount）：
	// breakerUntil 持久化以避免熔断期重启失忆（账号立即回到可选池再撞 5xx 雷区），
	// retryCount 持久化以保留"越熔越长"的退避累积（重启归零会失去累积保护）。
	// fails 不持久化——短期计数，重启从 0 累计可接受（达 breakerThreshold=3 才熔断）。
	// fails 是唯一的"连续失败"计数器：任何错误喂入，达到 breakerThreshold 触发熔断（指数退避），
	// 跨入口累计，成功/熔断/统一复活时清零（保留 retryCount 驱动退避指数）。
	breakerUntil time.Time // 熔断截止（指数退避）
	fails        int       // 连续失败计数（熔断用，唯一权威）
	retryCount   int       // 已熔断次数（指数退避的指数）
	// softStreak 连续软冷却次数（CoolSoft），独立于熔断器 fails 的**冷却域**计数器：
	// fails 会被熔断触发清零、且被 hard 冷却与 NoteError 污染，无法表达"连续软限流"。
	// 重置点只有两处（都是账号被证明恢复的时刻）：NoteSuccess、reviveCoolingLocked。
	// 持久化（stateAccount.SoftStreak）：重启后软限流仍在退避，不因重启回到基数。
	softStreak int
	// modelCooldowns 6004 模型级 limit 的**独立**冷却表：model → 该模型的冷却截止/重置。
	// 与 until（全账号级）正交：6004 只写本表、不写 until，因此多个模型同时 6004 时
	// 各自独立计时，互不覆盖（A 触发后 B 再触发，A 的冷却截止不被 B 覆盖——这是
	// 单 until 字段做不到的）。only 6004 触发时记录；空 map = 无模型级限流（不豁免）。
	// 持久化语义（stateAccount.ModelCooldowns）：重启后恢复，恢复时惰性过滤已过期
	// 条目。PR #96 把 6004 改成精确对齐上游重置墙钟后，单模型冷却可长达数小时，
	// 跨重启是常态；不持久化会导致 healthyForModel 重启失忆、重新踩 6004 雷区。
	modelCooldowns map[string]modelCooldown
	// sessionDeadFails 连续 12153（ErrSessionDead）计数。12153 在真实环境会被临时性触发
	// （网络抖动/上游闪断/refresh 竞态），一次失败就永久禁用太粗暴——连续达到阈值才判死。
	// 持久化（stateAccount.SessionDeadFails）：上游持续 session dead 时重启归零会导致
	// 重学（再吃 2 次失败才禁用，期间每次都白打一轮上游）；清零点（refresh/chat 成功、
	// 手工复活）同样落盘，重启后不残留旧计数。
	sessionDeadFails int
	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64

	// modelCost 实测扣费账本：model → 观测（运行态，不持久化）。
	// 由每次成功请求的 usage.credit 折算而来（上游没有"按模型的用量"接口，
	// get-user-resource 只给套餐级积分汇总，只能实测）。选号时据此把
	// 「该模型上免费/便宜的号」排在前面。
	modelCost map[string]modelCostEntry
}

// modelCostOf 返回该账号在指定 model 上的有效成本观测；无观测或观测过期返回 ok=false。
func (e *entry) modelCostOf(model string, now time.Time) (modelCostEntry, bool) {
	if model == "" {
		return modelCostEntry{}, false
	}
	mc, ok := e.modelCost[model]
	if !ok || mc.LastSeen.IsZero() {
		return modelCostEntry{}, false
	}
	if now.Sub(mc.LastSeen) > modelCostTTL {
		return modelCostEntry{}, false // 过期：时段性优惠（夜间免费）不得跨时段生效
	}
	return mc, true
}

// healthy 报告账号当前是否可选（未禁用、未处于任一冷却/熔断期）。
func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	return true
}

// modelExempt 报告账号是否处于「6004 模型级软冷却」形态：存在任一有效的 6004
// 模型级冷却（modelCooldowns 非空），且尚未禁用、未熔断。
// 此形态下账号仅对限流中的模型不可用，对其他模型仍可选（issue #31）。
// 本谓词仅供探活侧使用（ServableNow/ServableForRealm）：/healthz 无请求模型
// 上下文，用「存在豁免形态」表达"该账号还有别的模型可服务"；
// chat 侧按请求模型细粒度判定（healthyForModel：全账号健康且该模型不在独立
// 冷却内才放行），探活存在性语义与选号在豁免账号上口径一致。
// 调用方负责 now 与冷却有效性的判断（本方法只看形态，不看冷却是否已过期）。
func (e *entry) modelExempt() bool {
	return len(e.modelCooldowns) > 0 &&
		!e.disabled && e.breakerUntil.IsZero()
}

// modelCooled 报告账号对指定 model 是否正处 6004 模型级冷却（该模型的独立冷却未过期）。
// 空 reqModel / 未记录 → false（不因模型级维度限制账号）。
func (e *entry) modelCooled(now time.Time, reqModel string) bool {
	if reqModel == "" {
		return false
	}
	mc, ok := e.modelCooldowns[reqModel]
	if !ok {
		return false
	}
	return !mc.Until.IsZero() && now.Before(mc.Until)
}

// healthyForModel 报告账号对指定 model 是否可选（含 6004 模型级独立冷却判定）。
//
// 优先级（全账号级先判，模型级后判）：
//   - 全账号不可用（disabled / 账号级 until / breakerUntil，见 healthy）→ 永不可选；
//     账号整体不可用时查该模型的独立冷却没有意义，直接短路返回 false。
//   - 仅全账号健康时，才查该模型是否正处 6004 独立冷却
//     （modelCooldowns[reqModel] 未过期）→ 不可选；
//   - 否则可选。
//
// 模型级维度只锁定触发模型：多模型同时 6004 时各自独立，被 B 限流的账号对 A 请求
// 仍可选（A 不在 modelCooldowns 拦截且账号级 healthy 成立）。空 reqModel /
// 未记录模型 → 等价 healthy。6004 从不写账号级 until（见 CooldownSoftForModel），
// 因此不存在「账号级冷却因病 6004 而起、应豁免其他模型」的形态。
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	if !e.healthy(now) { // 全账号级（disabled/until/breakerUntil）先判
		return false
	}
	if e.modelCooled(now, reqModel) { // 全账号健康时再查该模型的 6004 独立冷却
		return false
	}
	return true
}

// pruneExpiredModelCooldowns 删除 modelCooldowns 中已过期的条目（惰性清理）。
// pick 写锁路径与 revive 调用，防止 map 无限膨胀；status 只读遍历天然跳过过期项，
// 无需清理。调用方必须已持有 p.mu 写锁。
func (e *entry) pruneExpiredModelCooldowns(now time.Time) {
	if len(e.modelCooldowns) == 0 {
		return
	}
	for m, mc := range e.modelCooldowns {
		if mc.Until.IsZero() || !now.Before(mc.Until) {
			delete(e.modelCooldowns, m)
		}
	}
}

// expiry 返回账号当前仍在生效的最近冷却/熔断截止时间（两个截止取较早者）；不在冷却期返回零值。
// 供全冷却兜底选取"最早到期"账号用。
func (e *entry) expiry(now time.Time) time.Time {
	var t time.Time
	if !e.until.IsZero() && now.Before(e.until) {
		t = e.until
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if t.IsZero() || e.breakerUntil.Before(t) {
			t = e.breakerUntil
		}
	}
	return t
}

// fallbackKind 报告兜底账号属于哪一类冷却（soft：即时软冷却；breaker：熔断期）。
// 只对参与兜底的账号调用（CoolHard 已被 pickEarliestExpiryLocked 排除）。判定口径：
// 若熔断截止是当前生效的最近截止（含"仅有熔断无软冷却"），记为 breaker；否则记为 soft。
func (e *entry) fallbackKind(now time.Time) string {
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if e.until.IsZero() || !now.Before(e.until) || e.breakerUntil.Before(e.until) {
			return "breaker"
		}
	}
	return "soft"
}

// stateAccount 单个账号的持久化状态（JSON tag 全小写下划线，向后兼容：缺字段零值）。
type stateAccount struct {
	Credits      int64     `json:"credits"`
	Disabled     bool      `json:"disabled"`
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	CoolKind     CoolKind  `json:"cool_kind"`
	SuccessCount int64     `json:"success_count,omitempty"`
	// err_total 累计错误计数。旧版 err_count（连续错误）仍可读：加载时映射到 err_total，
	// 仅作一次性迁移，不再回写 err_count。
	// 运维可见的运行态计数（err_total/soft_streak/session_dead_fails/credits_expiring/
	// error_ema）不用 omitempty：零值缺失会让人误以为"没记录"，实际是零值被省略。
	ErrTotal    int64     `json:"err_total"`
	ErrCount    int       `json:"err_count,omitempty"` // 兼容旧文件的迁移源，仅读取
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr     time.Time `json:"last_err,omitempty"`
	// SuccessEMA / ErrorEMA 成功率的 EMA 观测（选号权重第 3 因子数据源，见
	// entry.successEMA）。旧 state.json 缺字段 → 加载时从 successCount/errTotal
	// 反推初始值（比率归一），向后兼容。
	SuccessEMA float64 `json:"success_ema,omitempty"`
	ErrorEMA   float64 `json:"error_ema"`
	// SoftStreak 连续软冷却次数（软退避指数）。旧 state.json 缺此字段 → 零值，
	// 退避从基数重新开始（向后兼容）。
	SoftStreak int `json:"soft_streak"`
	// SessionDeadFails 连续 12153 计数（判定 session 死亡的进度）。持久化以保留
	// 「重启后连续计数继续累计」——上游持续 session dead 时重启归零会重学 2 次失败。
	// 零值也显式写出（运维口径，见 err_total 注释）。
	SessionDeadFails int `json:"session_dead_fails"`

	// BreakerUntil 熔断截止（指数退避）。仅未过期才持久化（落盘/恢复均惰性过滤），
	// 避免熔断期重启失忆：breakerUntil 在未来时重启后仍阻断选号。过期/零值不写。
	// 用 *time.Time（而非 time.Time）：Go 的 omitempty 对非指针 time.Time 的零值
	// 不生效（会序列化成 0001-01-01T00:00:00Z）；指针 nil 才能真正被 omitempty 省略，
	// 与落盘"过期不写"的口径一致。
	BreakerUntil *time.Time `json:"breaker_until,omitempty"`
	// RetryCount 已熔断次数（指数退避的指数）。持久化以保留"越熔越长"的退避累积——
	// 重启归零会让反复熔断只从最小退避开始。仅在 BreakerUntil 未过期时才有意义，
	// 恢复时若 BreakerUntil 已过期则 retryCount 归零（不保留无用退避指数）。
	RetryCount int `json:"retry_count,omitempty"`
	// CreditsExpiring 快过期积分子集（credits 的子集）。持久化以保留第四因子
	// （weightOf ×8）的快过期积分偏好——重启后到下次签到之间不应失忆。
	// 零值也显式写出（运维口径，见 err_total 注释）。
	CreditsExpiring int64 `json:"credits_expiring"`
	// ModelCooldowns 6004 模型级独立冷却表（model → 冷却记录）。持久化：
	// PR #96 把 6004 改成精确对齐上游重置墙钟后，单模型冷却可长达数小时，
	// 跨重启是常态；不持久化导致每次重启 healthyForModel 失忆、重新踩一遍
	// 6004 雷区（选号撞限流号耗尽 MaxRotate → 429）。恢复时惰性过滤已过期条目。
	ModelCooldowns map[string]stateModelCooldown `json:"model_cooldowns,omitempty"`
}

// stateModelCooldown 单个 (账号, 模型) 的 6004 独立冷却持久化记录，与运行态
// modelCooldown 同构（Until/ResetAt/Reason 字段名与语义对齐），落盘/恢复往返无损。
type stateModelCooldown struct {
	Until   time.Time `json:"until,omitempty"`
	ResetAt time.Time `json:"reset_at,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// modelCostTTL 成本观测的有效期。取 6 小时：既覆盖"夜间免费"这类时段性优惠的
// 单次会话，又不至于让昨天的价格决定今天的选择——过期的免费观测若永久有效，
// 白天会把已开始收费的号继续当成免费。
const modelCostTTL = 6 * time.Hour

// modelCostEntry 运行时成本账本（独立于持久化结构，避免账本污染 state.json；
// 成本随上游活动变化，仅内存态，重启后重新学习）。
type modelCostEntry struct {
	CostPer1k float64
	LastSeen  time.Time
	Samples   int
}

// modelCooldown 单个 (账号, 模型) 的模型级独立冷却记录（运行态，不持久化）。
// 承载两种「该模型在此账号上不可用」语义：
//   - 6004 模型级限流：Until 对齐上游重置墙钟；ResetAt 记录权威恢复时刻。
//   - 11102 该后端无此模型：Until 为指数退避 TTL（6h 起、封顶 24h）；Hits 记录
//     累计命中次数驱动退避（6004 无 hits 概念，Hits 恒 0）。
type modelCooldown struct {
	// Until 该模型的冷却截止（6004：now+min(resetAt-now, soft_rate_max)；11102：now+退避 TTL）。
	Until time.Time
	// ResetAt 上游「将在 … 重置」的原始墙钟（未经 soft_rate_max 截断）。
	// 与 Until 的区别同旧 softRateReset：Until 可能截断，ResetAt 是上游权威恢复时刻。
	// 11102 无重置文案，ResetAt 恒零值。
	ResetAt time.Time
	// Reason 触发原因（透出运维可读文案，同 Status.Reason）。
	Reason string
	// Hits 11102 负缓存的累计命中次数（驱动指数退避）。运行态不落盘（同 modelCost 口径：
	// 重启后从 6h 基数重新学习）；6004 条目 Hits 恒 0。持久化来回不会写入该字段。
	Hits int
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax 软冷却指数退避的默认封顶：softRateMax 未注入（<=0）时按此值算，
// 避免测试/裸用池时退避无上限。
const defaultSoftRateMax = 2 * time.Hour

// 11102「该后端无此模型」负缓存的退避参数（吸收 model_blocks.py 语义，复用 modelCooldowns
// 机制承载）。首次命中冷却 6h，半开到期放行重试；再命中按 Hits 指数退避（×2^min(hits-1,6)），
// 封顶 24h（最多一天再试一次）；该模型请求成功即清。6004 限流不参与本退避（各自独立语义）。
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockMaxTTL  = 24 * time.Hour
	modelBlockShift   = 6 // 2^6=64 倍后封顶：6h×64>24h，实际封顶锚定 24h
)

// sessionDeadThreshold 连续 ErrSessionDead（12153）达到该次数才永久禁用。
// 12153 会被临时性触发（网络抖动/上游闪断/refresh 竞态），一次失败即禁用的旧行为
// 会误杀健康账号（P0-1：13 个 disabled 号全是误判）。3 次连续才判死：容忍偶发抖动，
// 又不会让真正的死 session 留在池里反复被选中。
const sessionDeadThreshold = 3

// sessionDeadReason 12153 判定为 session 死亡时的持久化 reason。
const sessionDeadReason = "12153 session dead"

// SessionDeadThreshold 暴露连续 12153 的禁用阈值（供 scheduler 日志/运维文档引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

// softStreakShiftMax 软冷却退避的最大左移位数（防 1<<streak 溢出成负数/零）。
// 无论 streak 累积多少，封顶逻辑总会先生效，此值只是溢出兜底。
const softStreakShiftMax = 16

// defaultIdle* 闲置补偿默认参数（claude-api selectWeightedRandom 参考口径）。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// successAlpha 成功率 EMA 的平滑系数。取 0.1：比 NoteModelCost 的 0.3 更平滑——
// 选号权重不应被单次成败主导（约 10 次观测收敛），又能让「上游修复后的连续成功」
// 在十几次请求内把权重拉回来（旧终身累计口径下历史错误是分母的永久部分，永不可恢复）。
const successAlpha = 0.1
