// ═══ 更新日志 ═══
// 2026-09-17：合并模型级避让与完整响应校验，仅在确认成功后解除模型负缓存。
// 2026-09-17：密钥可绑定模型白名单，超出范围的请求在选号前拒绝。
// 2026-09-16：保留调用者指令，停止全局自动降级；校验输入并按真实流结果记录成功。
// 2026-09-17：热更新触发不再要求先手动检查远端版本（没查过时由 Apply 自己查）。
// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/hotupdate"
	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
	"workbuddy2api/internal/version"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string         // 空 = 不鉴权
	APIKeys   *apikeys.Store // 配置后以持久化密钥库为准，空库不放行。
	MaxRotate int            // 单请求最多换号次数，默认 3
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// PromptMode "passthrough"（默认，透传客户端原始 system）/ "custom"（网关替换）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// PromptActNote 运行约定文本，追加在带工具请求的 system 末尾（见 prompt.ActNote）。
	// 空 = 不追加（调用方用 ActNoteFor 解析配置后再传入）。
	PromptActNote string

	// Tasks 排程任务控制器（可选；nil = /tasks 报 available=false，面板渲染说明态）。
	Tasks TaskController

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// Usage 按调用密钥累计的 token 账本（可选；nil = /usage 报未启用）。
	// 只有成功请求参与累计，数据来自上游 usage，缺失即不记 token（缺失≠0）。
	Usage *usage.Store

	// Update 热更新管理器（可选；nil = /update/* 报未启用）。
	Update *hotupdate.Manager
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "passthrough" // 缺省 passthrough：透传客户端原始 system
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// Responses API 兼容层（NarraFork / Codex 等客户端走这条）：内部委托 chatCompletions。
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	// 排程任务自省与手动触发（账户管理面板的「定时任务」页）。
	h.mux.HandleFunc("GET /tasks", h.withAuth(h.tasks))
	h.mux.HandleFunc("POST /tasks/{key}/run", h.withAuth(h.taskRun))
	h.mux.HandleFunc("GET /tasks/{key}/log", h.withAuth(h.taskLog))
	// 用量统计只走本机 Unix socket（管理台「用量统计」页）：普通调用密钥拿不到全量用量，
	// 单密钥自己的用量在日志与面板里按 key 归属，不需要公开端点。
	h.mux.HandleFunc("GET /usage", h.requireInternal(h.usageStats))
	// 热更新同样只走本机管理通道：能触发版本切换的入口不能暴露给调用密钥。
	h.mux.HandleFunc("GET /update", h.requireInternal(h.updateStatus))
	h.mux.HandleFunc("POST /update/check", h.requireInternal(h.updateCheck))
	h.mux.HandleFunc("POST /update/apply", h.requireInternal(h.updateApply))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(internalAdminContextKey{}) == true {
			next(w, r)
			return
		}
		if h.cfg.APIKeys != nil {
			authz := r.Header.Get("Authorization")
			info, ok := h.cfg.APIKeys.Resolve(strings.TrimPrefix(authz, "Bearer "))
			if !strings.HasPrefix(authz, "Bearer ") || !ok {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, info)))
			return
		}
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			// 常量时间比较（发现 7）：!= 短路时序随前缀长度变化，公网暴露下
			// 理论上可逐字节探测 key 前缀；ConstantTimeCompare 消除该信号。
			provided := strings.TrimPrefix(authz, "Bearer ")
			if !strings.HasPrefix(authz, "Bearer ") ||
				subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.APIKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

type internalAdminContextKey struct{}

// apiKeyContextKey 携带本次请求使用的密钥信息（模型白名单等）。
type apiKeyContextKey struct{}

// requestKeyInfo 返回鉴权命中的密钥信息；单密钥模式或内部管理请求返回零值。
func requestKeyInfo(r *http.Request) (apikeys.Info, bool) {
	info, ok := r.Context().Value(apiKeyContextKey{}).(apikeys.Info)
	return info, ok
}

// modelAllowedByKey 判断请求模型是否在密钥白名单内。
//
// 匹配按「realm + 裸名」比较：白名单项可写裸名（两个域通用）或带前缀的全名。
// 白名单为空表示不限制，保持旧密钥行为。
func modelAllowedByKey(info apikeys.Info, requestModel string) bool {
	if len(info.Models) == 0 {
		return true
	}
	realm, bare := resolveModel(requestModel)
	for _, allowed := range info.Models {
		allowedRealm, allowedBare := resolveModel(allowed)
		if allowedBare != bare {
			continue
		}
		if allowedRealm != realm && strings.Contains(allowed, ":") {
			continue
		}
		return true
	}
	return false
}

// InternalHandler 只挂载到权限为 0600 的 Unix socket，使面板管理不依赖任一调用密钥。
func (h *Handler) InternalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), internalAdminContextKey{}, true)))
	})
}

// requireInternal 限定只允许本机管理 socket（InternalHandler 注入的上下文）访问：
// 公开端口上同一个 mux 也会匹配这条路由，没有这道闸就等于把全量用量暴露给任一调用密钥。
func (h *Handler) requireInternal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(internalAdminContextKey{}) != true {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "internal endpoint")
			return
		}
		next(w, r)
	}
}

// usageStats 返回按密钥累计的 token 用量（管理台「用量统计」页数据源）。
func (h *Handler) usageStats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "用量账本未启用：config 里设置 usage_file 后重启网关",
		})
		return
	}
	snapshot := h.cfg.Usage.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"file":       h.cfg.Usage.Path(),
		"since":      snapshot.Since,
		"updated_at": snapshot.UpdatedAt,
		"totals":     snapshot.Totals,
		"keys":       snapshot.Keys,
	})
}

// recordUsage 把一次成功请求的用量写进密钥账本。
// st.toks 为 -1（上游没给 usage）时只累计请求数，不臆造 token。
func (h *Handler) recordUsage(st *chatStat, model string) {
	if h.cfg.Usage == nil || st == nil {
		return
	}
	h.cfg.Usage.Record(st.keyID, st.keyName, st.keyMask, model, st.prompt, st.toks, st.cached,
		st.credit, st.hasCred, time.Now())
}

// updateStatus 返回热更新状态（当前版本、远端最新版本、最近错误）。
func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Update == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "enabled": false,
			"message": "热更新未启用：config 里设置 update.enabled=true 后重启网关",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": h.cfg.Update.Status()})
}

// updateCheck 查询远端最新版本（只读，不改动任何东西）。
func (h *Handler) updateCheck(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Update == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "热更新未启用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	status, err := h.cfg.Update.Check(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error(), "status": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

// updateApply 触发一次热更新。
//
// 下载与交接在后台完成（以秒计），接口立即返回；真正的停机发生在交接成功之后，
// 由 main 的优雅停机路径把在途请求跑完。前端用 /update 轮询进度。
func (h *Handler) updateApply(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Update == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "热更新未启用"})
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if r.Body != nil {
		// 载荷可以完全省略（管理台发 {}），但一旦带了内容就必须是合法的小 JSON，
		// 否则一次手滑的请求会静默变成"升到最新版"。
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, (1<<12)+1))
		if readErr != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "读取请求体失败"})
			return
		}
		if len(raw) > 1<<12 {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "请求体过大"})
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "请求体不是合法 JSON"})
				return
			}
		}
	}
	status := h.cfg.Update.Status()
	switch status.State {
	case hotupdate.StateChecking, hotupdate.StateDownloading, hotupdate.StateHandover:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "已有更新任务在进行中", "status": status})
		return
	}
	target := strings.TrimSpace(body.Tag)
	// 只有「已经查过远端、且确认没有新版本」才直接拒绝。没查过就交给 Apply 自己去查，
	// 否则管理台必须先点一次「检查更新」才能升级，用户看到的是莫名其妙的"已经是最新"。
	if !status.UpdateReady && target == "" && !status.CheckedAt.IsZero() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "message": fmt.Sprintf("已经是最新版本（当前 %s，远端 %s）", status.Current, status.LatestTag),
			"status": status,
		})
		return
	}
	go func() {
		// 独立 ctx：请求返回后这次下载仍要跑完。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := h.cfg.Update.Apply(ctx, target); err != nil {
			log.Printf("ERROR: [update] apply failed: %v", err)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "已开始热更新：新实例接管后，本实例会把手上的请求跑完再退出",
		"status": h.cfg.Update.Status(),
	})
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"version":        version.Version,
		"commit":         version.Commit,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
	// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// countsMapFrom 把 CountsDetailed 五元组编码为 /status realm_totals 的字段对象。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 1h），失败/无号返回空列表（无静态兜底）。
// 绑定模型的密钥只会看到自己可用的模型，客户端据此选择也不会撞 403。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	list := h.modelList()
	if info, ok := requestKeyInfo(r); ok && len(info.Models) > 0 {
		filtered := make([]map[string]any, 0, len(list))
		for _, entry := range list {
			id, _ := entry["id"].(string)
			if id != "" && modelAllowedByKey(info, id) {
				filtered = append(filtered, entry)
			}
		}
		list = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   list,
	})
}

// globalModels 国际版（global realm）模型名名单（PLAN §7.2 附录 21 名）——已删。
// 纯动态化后 handler 不再持有任何静态名单：无 global 账号 / 探测失败 → 空列表。

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	// 去掉 "credits" 后缀
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
// CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel 对称）。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底；
// global.enabled=false（显式逃生门）时只列 CN（global 名单不出现）。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	for _, mi := range h.fetchDynamicModels() {
		entry := map[string]any{
			"id":                "cn:" + mi.ID,
			"object":            "model",
			"created":           1753600000,
			"owned_by":          "workbuddy",
			"context_length":    mi.ContextWindow,
			"max_output_tokens": mi.MaxTokens,
		}
		if mi.ContextWindow == 0 {
			entry["context_length"] = 131072 // 兜底
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// P0：effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （issue #84 客户端可发现档位，不再盲传）。无档位→省略字段（非空数组）。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 探测结果（fetchGlobalModels 纯动态，失败/无号 → 空）；无 global 账号时
	// 空名单且零上游调用。
	if h.cfg.GlobalEnabled {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			entry := map[string]any{
				"id":             "global:" + id,
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "workbuddy",
				"context_length": 131072,
			}
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				// 富条目命中：context_length/max_output_tokens 用探测真实值替换
				// 131072 兜底（与 CN 动态分支同口径；上游零值保留兜底）。
				if mi.ContextWindow > 0 {
					entry["context_length"] = mi.ContextWindow
				}
				if mi.MaxTokens > 0 {
					entry["max_output_tokens"] = mi.MaxTokens
				}
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 与 fetchDynamicModels（CN 侧）同语义不同归位：缓存/失败回落封在 upstream.FetchGlobalModels
// （内部 1h + 5min 负缓存）。本方法只负责"何时探测"：
//   - 池中无 global 账号 → 空名单 + nil 账号（不发起上游调用）；
//   - 有 global 账号 → 单账号 Pick（global 域谓词），交 upstream 探测。
//
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// fetchDynamicModels 从池中任一健康 CN 账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接返回 nil（纯动态，无静态表兜底），
// 避免反复打上游。
// 只从 CN realm 账号拉取（PickExcludingForRealm(nil,"","cn")）：全局账号的模型列表
// 未必与 CN 一致，动态模型表只服务 CN 前缀（global 走独立探测）。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "cn")
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError（P1-6/发现 6）：
		// NoteError 喂的是 chat 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道
		// 健康的账号；models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	// 调试开关：设置 WB2A_DUMP_REQ 即把上游侧收到的原始请求体落盘，供离线二分定位指纹命中行。
	// 仅在排查上游指纹拦截时开启；不设置时零开销、不落盘。
	// 只落"大请求"（超过上限一半）：小探针（{"input":"hi"} 之类）会覆盖掉真正要看的对话请求。
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(body)*2 >= int(limit) {
		if err := os.WriteFile("/app/data/last_request.json", body, 0o600); err != nil {
			log.Printf("ERR: [server] dump req: %v", err)
		}
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	var requestObject map[string]json.RawMessage
	if err := json.Unmarshal(body, &requestObject); err != nil || requestObject == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
		return
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid request fields: "+err.Error())
		return
	}
	if err := validateChatRequest(body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/账本/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel := resolveModel(peek.Model)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()
	// 调用方密钥身份：请求行 key= 列与用量账本都按它归属（单密钥模式没有 Info）。
	if info, ok := requestKeyInfo(r); ok {
		st.keyID, st.keyName, st.keyMask = info.ID, info.Name, info.MaskedKey
	}

	// 密钥模型绑定：只放行白名单内的模型，拒绝发生在选号之前——不占用账号、不轮转、不冷却。
	if info, ok := requestKeyInfo(r); ok && !modelAllowedByKey(info, peek.Model) {
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
			fmt.Sprintf("this API key is restricted to its bound models and cannot use %q", peek.Model))
		st.status = http.StatusForbidden
		return
	}

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// 按模型解析：同一个会话可能换模型，绑定号若在当前模型上被 6004 限额（对其他模型
	// 仍可用），必须重分配——否则会被钉在这个号上反复失败。
	// 提取与下方会话头族的聚合键共用同一结果，故**不受粘性开关影响**：粘性未启用
	// （Session==nil）时聚合键仍应是会话级，而不是退化成轮级。
	sessKey := session.ExtractKey(body)
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// 传给 ResolveForModel 的是**完整**模型名（peek.Model，含 realm 前缀）。
		// 粘性命中校验走 injected AvailableForModel 闭包 → 闭包内部 resolveModel 剥前缀
		// 得 realm+bare，再按 realm 过滤可用集合。若传已剥前缀的 bareModel，闭包对裸名
		// 恒剥出 realm=cn，跨 realm 粘性会话会被错误钉回 CN 集合；完整前缀才能让
		// 闭包正确过滤到 global 集合（见 cmd/server/wiring.go realmAwareAvailableForModel）。
		// 模型名也参与成本账本与选号过滤，不能用 "-" 占位污染模型键。
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级兜底聚合键：无会话键的客户端（OpenAI 兼容协议——dsh / Codex / Cherry
	// Studio 等请求体里既无 conversationId 也无 metadata）sessKey 恒空，会话头族的
	// 聚合主键此前只能逐请求新生成，agent 多轮在上游用量明细里仍是一条请求一条记录。
	// 这里按 body 里最后一条 user 消息派生轮级键（同轮内所有上游调用同键）。
	// 必须在下方 prompt.Rewrite / rewriteModel 之前取——改写会动 messages 内容。
	turnKey := ""
	if sessKey == "" {
		turnKey = session.TurnKey(body)
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 只有显式 custom 配置才替换 system/developer；passthrough 始终保留原文。
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造，见
	//     ResolveConversationID；官方后台不校验一致，空会话则不建立聚合键）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先（客户端已
	//     有自己的对话轮 ID 则以客户端为准），否则按粘性 key 进程内稳定生成（同会话
	//     恒同值）；粘性 key 也为空时走轮级兜底（session.TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级 NewMessageID——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if sessKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键客户端：轮级兜底——同轮内 tool call 多轮 / 换号重试 / 降级重发
		// 共享同键，用户发下一条消息自动换键。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	for i := 0; i < h.cfg.MaxRotate; i++ {
		if r.Context().Err() != nil {
			st.status = 499
			return
		}
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑。
				unbindSticky()
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次粘性命中往返（语义与 fail()/粘性命中-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 12153 一次失败不杀号（临时触发会误杀）：与 scheduler keepalive/checkin
					// 同口径走连续计数，达到 sessionDeadThreshold 才禁用。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			acct.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh uid=%s: save auth failed: %v", logfmt.UID8(acct.UID), err)
			}
		}

		// 客户端 IP 透传（仅 PassthroughIP 开启）：按请求取首段作为参数传入 ChatStream，
		// 不再读写共享字段——并发请求各自携带独立 IP，互不串扰（issue：ClientIP 竞态）。
		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		// 传 r.Context()：客户端断连/请求取消立即中断在途上游调用并释放租约，
		// 不再让"幽灵请求"占满账号在途名额直到 IdleTimeout。
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			if kind == upstream.ErrChannelRejected {
				fail(acct.UID)
				writeOpenAIError(w, http.StatusBadRequest, "upstream_channel_rejected",
					"upstream rejected the client channel: Illegal API invocation from an unapproved channel")
				st.status = http.StatusBadRequest
				return
			}
			// 上游 WAF 按请求正文特征拦截（XSS/SQL 规则），返回的是 HTML 拦截页而非 JSON。
			// 判定看正文、不看账号：同域内换任何号都会撞同一条规则，因此请求终态——
			// 不轮转、不罚账号，也不把 HTML 页当作「请求参数被拒」回显给调用方。
			if kind == upstream.ErrUpstreamWAF {
				fail(acct.UID)
				writeOpenAIError(w, http.StatusBadRequest, "upstream_waf_blocked",
					"upstream WAF rejected this request even after the gateway broke the matching "+
						"patterns; start a new conversation, or remove the HTML/script/SQL-looking part")
				st.status = http.StatusBadRequest
				return
			}
			// 上游内容拒绝属于当前请求；直接返回，不修改其他会话或替换正文重试。
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样，
				// 任务书授权上游错误码/账号语义对客户端可见），不再改写成网关固定文案。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIError(w, http.StatusBadRequest, "content_blocked", msg)
				st.status = http.StatusBadRequest
				return
			}
			if kind == upstream.ErrContextTooLong {
				// 上下文超限：请求体本身超模型上限，换任何账号都是同一结果。
				// 立即回客户端并透传上游原文（含真实 token 数与上限），不轮转、不罚账号。
				// 让调用方看到 "prompt is too long: N tokens > M maximum" 自行压缩或开新会话。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
				fail(acct.UID)
				// 不再额外加 "prompt is too long: " 前缀——上游 msg 本身就以它开头，
				// 硬加会得到 "prompt is too long: prompt is too long: N tokens > M maximum"。
				writeOpenAIError(w, http.StatusBadRequest, "context_length_exceeded",
					upstream.ContextTooLongDetail(string(respBody)))
				st.status = http.StatusBadRequest
				return
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			if kind == upstream.ErrBadParams || kind == upstream.ErrClient {
				fail(acct.UID)
				detail := string(respBody)
				acct.Lock()
				secretValues := []string{acct.AccessToken, acct.RefreshToken, acct.DeviceToken}
				acct.Unlock()
				secretValues = append(secretValues, h.cfg.APIKey, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				for _, secret := range secretValues {
					if len(secret) > 4 {
						detail = strings.ReplaceAll(detail, secret, "[redacted]")
					}
				}
				if len(detail) > 2000 {
					detail = logfmt.Truncate(detail, 2000)
				}
				// 11155（reasoning_content_missing）归因：上游只看「assistant 消息有没有
				// reasoning_content 字段」，出错时把出站消息形状压成一行，复现即可定位
				// 是客户端没带推理项、转换层丢了文本，还是字段缺失。不含任何正文。
				if strings.Contains(detail, "reasoning_content_missing") || strings.Contains(detail, `"code":11155`) {
					stats, hasStats := reasoningStatsFrom(r.Context())
					log.Printf("WARN: [server] upstream 11155 shape uid=%s key=%s %s",
						logfmt.UID8(acct.UID), st.keyName, chatShapeSummary(body, stats, hasStats))
				}
				writeOpenAIError(w, http.StatusBadRequest, "upstream_invalid_request", "upstream rejected request params: "+detail)
				st.status = http.StatusBadRequest
				return
			}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
			fail(acct.UID)
			continue
		}
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			streamErr := upstream.Stream(w, stats)
			if checker, ok := w.(interface{ CompletionError() error }); ok && streamErr == nil {
				streamErr = checker.CompletionError()
			}
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.prompt = stats.PromptTokens()
			st.cached = stats.CachedTokens()
			if streamErr != nil {
				rc.Close()
				if r.Context().Err() != nil {
					st.status = 499
				} else {
					st.status = http.StatusBadGateway
					log.Printf("WARN: [server] stream incomplete uid=%s model=%s error=%v", logfmt.UID8(acct.UID), bareModel, streamErr)
				}
				fail(acct.UID)
				return
			}
			h.cfg.Pool.NoteSuccess(acct.UID)
			h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if credit, ok := stats.Credit(); ok {
				h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
				st.credit, st.hasCred = credit, true
			} else if _, hasUsage := stats.Tokens(); hasUsage {
				// R9(c) 防护观测：usage 存在但 credit 缺失（如 global SSE 末帧未带 credit）。
				// 不算合法成本观测（缺失≠0），仅记一条 WARN 协助排障，绝不写入账本。
				log.Printf("WARN: [server] stream usage without credit uid=%s model=%s (no cost observation)", logfmt.UID8(acct.UID), bareModel)
			}
			h.recordUsage(st, peek.Model)
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		if checker, ok := w.(interface{ ValidateCompletion(map[string]any) error }); ok {
			if err := checker.ValidateCompletion(resp); err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "response_contract_violation", err.Error())
				st.status = http.StatusBadGateway
				return
			}
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		st.prompt = promptTokens(resp)
		st.cached = cachedTokens(resp)
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if credit, total, ok := usageCreditTotal(resp); ok {
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
			st.credit, st.hasCred = credit, true
		}
		h.recordUsage(st, peek.Model)
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	//
	// 背景：此前把上游原始错误文本统一改写，防账号 UID / 上游内部错误码（11128 / 12153 /
	// 6004 后台措辞）泄露——但副作用是客户端看不到真实错误，根本没法排查上游问题。
	// 任务书规定上游错误码/账号语义**允许**泄露给客户端（有意为之），排查必须看到原文。
	//
	//   - 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	//     requestId 原样保留，如 {"code":6004,"msg":"…","requestId":"…"}）。HTTP 状态码
	//     按 OpenAI 兼容口径映射类别：ErrSoftRate → 429（限流语义、客户端应等待重试），
	//     其余保持 503（网关侧无健康账号可用）。业务 code 取本地分类可读名
	//     （rate_limit_exceeded / no_healthy_account）。
	//   - 本地调度类错误（无可用账号 acct==nil、传输层抖动、非上游返回的 lastErr）
	//     → 保留自有文案 no_healthy_account（本地错误没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
		}
	}
	writeOpenAIError(w, status, code, msg)
	st.status = status
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：内容拦截会立即 400 返回，其余种类 continue 换号。
//
// 七条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避（soft_rate 基数起、
//     softStreak 翻倍、封顶 soft_rate_max，冷却中兜底探测不翻倍）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号，直接回 400 content_blocked。
//   - ErrBadParams → 不罚账号，直接回 400 并保留脱敏后的诊断。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让（复用 modelCooldowns
//     机制，Until=指数退避 TTL，选号侧 healthyForModel 避开，切模型即可用）。
//   - ErrClient 在调用前直接回 400；其他未知错误不喂熔断。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 统一对齐上游重置时间（重构核心）：只要 body 带「将在 … 重置」，无论业务
		// code 是 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不
		// softStreak 指数堆加。
		//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型
		//     豁免（既有 issue #31 语义）。
		//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免
		//     （普通账号级限流不该因切模型绕过）。
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if upstream.IsModelRateLimit(body) {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, resetAt, "429 rate limit")
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max）；已在冷却中的兜底探测不翻倍（见 CooldownSoftRate）。
		h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Time{}, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：软冷却到期也不会自动
		//     恢复（需重新 OAuth 登录），到期后重新选号只会再撞 403 浪费一次轮换——
		//     硬禁用（Disable），不再参与选号。/status 以 disabled + disabled_reason 呈现。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 两条路径对坏号都立刻换号（同一请求轮转出池），只是后续可恢复性不同。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContextTooLong:
		// 上下文超限：请求体问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// 轮转循环已在该 kind 上直接 return，不消耗其他账号。
	case upstream.ErrContentBlocked:
		// 内容策略拦截：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 首遇由 chatCompletions 内降级重试处理；最终仍拦则回 400
		// content_blocked（防火墙文案），不再轮转、不暴露账号/冷却/错误码。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），写 modelCooldowns[model]，Until 为指数退避 TTL（6h 起、封顶
		// 24h）。选号侧 healthyForModel 对该账号自动避开该模型；切模型/切账号即可用。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
