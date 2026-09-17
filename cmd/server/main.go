// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/hotupdate"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	var keyStore *apikeys.Store
	if cfg.APIKeysFile != "" {
		keyStore, err = apikeys.Open(cfg.APIKeysFile, cfg.APIKey)
		if err != nil {
			log.Fatalf("load API keys: %v", err)
		}
		if cfg.APIKeysSocket == "" {
			cfg.APIKeysSocket = filepath.Join(filepath.Dir(cfg.APIKeysFile), "api_keys.sock")
		}
	}
	// 用量账本：按调用密钥累计 token。默认与密钥库同目录（usage.json），
	// 显式配 usage_file 可在单密钥模式下也记账。
	var usageStore *usage.Store
	if cfg.UsageFile == "" && cfg.APIKeysFile != "" {
		cfg.UsageFile = filepath.Join(filepath.Dir(cfg.APIKeysFile), "usage.json")
	}
	if cfg.UsageFile != "" {
		usageStore, err = usage.Open(cfg.UsageFile, 0)
		if err != nil {
			log.Fatalf("load usage ledger: %v", err)
		}
		defer usageStore.Close()
		log.Printf("usage ledger enabled: %s", cfg.UsageFile)
	}
	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站图片预算：入站放宽后守住发往上游的体积上限（MB → 字节）。
	up.OutboundImageBudgetBytes = cfg.Server.OutboundImageBudgetMB << 20
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		SchoolHours:         cfg.Schedule.SchoolHours,
		CatHours:            cfg.Schedule.CatHours,
		RedeemHours:         cfg.Schedule.RedeemHours,
		LotteryHours:        cfg.Schedule.LotteryHours,
		MakeupHours:         cfg.Schedule.MakeupHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		ExpiringSoonWindow:  cfg.ExpiringSoonDur, // 快过期积分优先消耗（issue:积分过期）
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:      !cfg.Schedule.SchoolEnabled,
		CatDisabled:         !cfg.Schedule.CatEnabled,
		RedeemDisabled:      !cfg.Schedule.RedeemEnabled,
		LotteryDisabled:     !cfg.Schedule.LotteryEnabled,
		MakeupDisabled:      !cfg.Schedule.MakeupEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v 点（school_open_day_2026.py ALL --run --yes）", cfg.Schedule.SchoolHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}
	if !cfg.Schedule.RedeemEnabled {
		log.Printf("连登兑换已禁用（schedule.redeem_enabled=false）")
	} else {
		log.Printf("连登兑换已启用：%v 点（growth_center.py ALL --redeem-only --yes）", cfg.Schedule.RedeemHours)
	}
	if !cfg.Schedule.LotteryEnabled {
		log.Printf("成长抽奖已禁用（schedule.lottery_enabled=false）")
	} else {
		log.Printf("成长抽奖已启用：%v 点（growth_center.py ALL --lottery-only --yes）", cfg.Schedule.LotteryHours)
	}
	if !cfg.Schedule.MakeupEnabled {
		log.Printf("补签已禁用（schedule.makeup_enabled=false）")
	} else {
		log.Printf("补签已启用：%v 点（growth_center.py ALL --makeup-only --yes）", cfg.Schedule.MakeupHours)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 监听套接字：热更新后的新实例从环境变量继承 FD，其余情况正常监听。
	// 用 listener 而不是 ListenAndServe，才能把套接字交给新实例。
	mainLn, inherited, err := hotupdate.Listen(cfg.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Listen, err)
	}
	defer mainLn.Close()
	if inherited {
		log.Printf("[update] inherited listening socket from the previous instance (fd=%s)",
			os.Getenv(hotupdate.EnvListenFD))
	}

	// 热更新管理器：查版本、下载校验、交接。OnSwitched 触发本进程优雅停机——
	// 新实例已经在同一套接字上 accept，本进程只负责把在途请求跑完。
	handoverDone := make(chan string, 1)
	updateManager := hotupdate.NewManager(hotupdate.Options{
		Enabled:  cfg.Update.Enabled,
		Repo:     cfg.Update.Repo,
		Token:    cfg.Update.Token,
		Dir:      updateDir(cfg),
		Args:     os.Args[1:],
		Listener: mainLn,
		OnSwitched: func() {
			select {
			case handoverDone <- "hot update":
			default:
			}
		},
	})

	h := server.NewHandler(server.Config{
		Pool:          p,
		Upstream:      up,
		APIKey:        cfg.APIKey,
		APIKeys:       keyStore,
		Session:       sessRouter,
		StickyCount:   sessCount,
		RedisMode:     redisMode,
		SoftCooldown:  cfg.SoftRateDur,
		PromptMode:    cfg.Prompt.Mode,
		PromptText:    cfg.PromptText,
		PromptActNote: server.ActNoteFor(cfg.Prompt.ActNote),
		Update:        updateManager,
		MaxBodyBytes:  int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
		Tasks:         sch,                               // /tasks 端点：排程自省 + 手动触发
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		Usage:         usageStore,
	})

	var adminServer *http.Server
	if keyStore != nil {
		adminLn, err := apikeys.ListenUnix(cfg.APIKeysSocket)
		if err != nil {
			log.Fatalf("listen API key admin socket: %v", err)
		}
		defer adminLn.Close()
		mux := http.NewServeMux()
		mux.Handle("/keys", keyStore.AdminHandler())
		mux.Handle("/keys/", keyStore.AdminHandler())
		mux.Handle("/", h.InternalHandler())
		adminServer = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
		defer adminServer.Close()
		go func() {
			if err := adminServer.Serve(adminLn); err != nil && err != http.ErrServerClosed {
				log.Printf("[api-keys] admin server: %v", err)
			}
		}()
		log.Printf("API key management enabled (%d keys)", len(keyStore.List()))
	}
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// 取值大于 MaxBodyMB 在常规带宽下的上传耗时；聊天请求体上限默认 8MB。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		reason := "signal"
		select {
		case <-ctx.Done():
		case why := <-handoverDone:
			reason = why
		}
		if reason != "signal" {
			// 热更新：新实例已接管监听，这里只等在途请求收尾；等待上限放宽到能覆盖
			// 长 SSE（上游 idle_timeout 默认 300s + 收尾）。不设上限会在更新时掐断
			// 正在进行的对话。
			log.Printf("[update] draining in-flight requests before exit (%s)", reason)
		}
		p.Flush() // 信号触发：先落盘再做优雅停机
		// Flush 已把最后一笔状态快照提交给 Redis（fire-and-forget）；store.Close
		// 等 Upstash 在途/排队写排空再关连接——最后一笔镜像必须写完才退出（发现 4）。
		// Noop 的 Close 是空操作；单写上限 5s × 上限 8，Close 内部另有超时兜底。
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		wait := 5 * time.Second
		if reason != "signal" {
			wait = hotupdate.ShutdownTimeout()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		if adminServer != nil {
			_ = adminServer.Shutdown(shutdownCtx)
		}
		_ = srv.Shutdown(shutdownCtx)
		if reason != "signal" {
			// 约定退出码：容器 PID 1 看到它就不再拉起新实例（套接字已在别人手里），
			// 但保持容器存活。
			log.Printf("[update] handover complete, exiting %d", hotupdate.ExitHandover)
			os.Exit(hotupdate.ExitHandover)
		}
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	// 先起 accept 循环再通知就绪：父进程收到通知后会立刻停止接受新连接，
	// 如果这时本进程还没开始 accept，连接会压在队列里直到本进程接管（不影响正确性，
	// 但排队越短越好）。
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(mainLn) }()
	// 交接就绪通知：父进程据此确认新实例已开始服务，然后才停旧实例。
	if err := hotupdate.NotifyReady(); err != nil {
		log.Printf("WARN: [update] notify ready: %v", err)
	}
	if err := <-serveErr; err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
