// credit.go — WorkBuddy 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//
// 输出结构:
//
//	{"service":"workbuddy","ts":N,
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"uid","nickname","remain","used","size","packages","ok","error?"}]}
//
// realm 感知：复用 upstream.Client（auth.Parse + upstream.New），global 账号查积分
// 走 workbuddy.ai /billing/meter/*（404 回落 /v2），CN 账号维持 codebuddy.cn
// /v2/billing/meter/get-user-resource（现状逐字）。聚合口径即 upstream.ResourceSummary。
// 2026-09-16：余额工具改从凭据快照判断令牌，保持与并发安全的上游客户端契约一致。
// 2026-09-19：改为有界并发查询（默认 6 路）。原先 19 个账号串行 + 每号 200ms 间隔，
// 一次冷跑要 8.3 秒；面板 /api/state 同步等这个结果，于是"刷新网页很久才出数据"。
// 并发度用旗标/环境变量可调，默认值刻意保守：既压掉绝大部分等待，又不给上游压力。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	up := upstream.New()
	up.GlobalEnabled = true // 允许按 realm 路由：global 账查积分走 workbuddy.ai
	accounts := collectWithConcurrency(authDir, up, concurrencyFromEnv())
	printAccounts(accounts, pretty)
}

// defaultConcurrency 默认并发度：19 个账号 6 路并发，冷跑从 8.3s 降到约 1.5s，
// 同时对上游保持温和（每路之间仍有节流）。
const defaultConcurrency = 6

// concurrencyFromEnv 读 WB2A_CREDIT_CONCURRENCY；非法或 <=0 时用默认值。
func concurrencyFromEnv() int {
	raw := os.Getenv("WB2A_CREDIT_CONCURRENCY")
	if raw == "" {
		return defaultConcurrency
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return defaultConcurrency
	}
	if value > 32 {
		return 32
	}
	return value
}

// collect 遍历 auths 目录并查询每个账号的积分摘要。供测试注入 fake upstream 断言
// realm 路由（main 从 os.Args/env 取况，collect 单一来源可测）。
// 文件清单走 auth.LoadAuthFiles（宽侧 workbuddy*.json）：与网关 LoadDir 同口径，
// 不带连字符的文件不再被跳过（P2-10，审查发现 10）。
func collect(authDir string, up *upstream.Client) []accountResult {
	return collectWithConcurrency(authDir, up, defaultConcurrency)
}

// collectWithConcurrency 并发查询各账号积分，保持与 collect 相同的输出顺序。
//
// 顺序必须稳定：面板按 auths 目录顺序展示账号，并发完成后要按原下标回填，
// 否则每次刷新账号行都会跳位置。上游 Client 自身并发安全（内部 RWMutex + 共享
// Transport 连接池），每个账号各自带自己的 token，互不干扰。
func collectWithConcurrency(authDir string, up *upstream.Client, workers int) []accountResult {
	files, _ := auth.LoadAuthFiles(authDir)
	if workers < 1 {
		workers = 1
	}
	// slots 按文件下标回填，filled 记录哪些下标真的产出了结果：读失败/解析失败的
	// 文件与旧实现一样被静默跳过，不会在输出里留下空行。
	slots := make([]accountResult, len(files))
	filled := make([]bool, len(files))
	if len(files) == 0 {
		return []accountResult{}
	}
	if workers > len(files) {
		workers = len(files)
	}

	// 读文件 + 解析是纯本地操作，先在主协程做掉：解析失败的条目直接跳过，
	// 只有真正要发网络请求的账号才进并发池。
	type task struct {
		index int
		auth  *auth.Auth
	}
	tasks := make([]task, 0, len(files))
	for index, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		res := accountResult{UID: a.UID, Nickname: a.Nickname}
		if a.Snapshot().AccessToken == "" {
			res.Error = "no accessToken"
			slots[index] = res
			filled[index] = true
			continue
		}
		tasks = append(tasks, task{index: index, auth: a})
	}

	jobs := make(chan task)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				res := accountResult{UID: item.auth.UID, Nickname: item.auth.Nickname}
				remain, used, size, packs, err := up.ResourceSummary(item.auth)
				if err != nil {
					res.Error = err.Error()
				} else {
					res.Remain = &remain
					res.Used = &used
					res.Size = &size
					res.Packages = packs
					res.OK = true
				}
				slots[item.index] = res
				filled[item.index] = true
			}
		}()
	}
	for _, item := range tasks {
		jobs <- item
	}
	close(jobs)
	wg.Wait()

	accounts := make([]accountResult, 0, len(files))
	for index := range slots {
		if filled[index] {
			accounts = append(accounts, slots[index])
		}
	}
	return accounts
}

// printAccounts 汇总并输出结果：-pretty 走人类可读日报，否则 JSON（与老版输出一致）。
func printAccounts(accounts []accountResult, pretty bool) {
	var totalRemain, totalUsed, totalSize int64
	okCount := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Used != nil {
				totalUsed += *a.Used
			}
			if a.Size != nil {
				totalSize += *a.Size
			}
		}
	}
	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize, okCount)
		return
	}
	out := map[string]any{
		"service": "workbuddy",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"used":     totalUsed,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty 人类可读日报：四行汇总，无账号明细。
func printPretty(accounts []accountResult, totalRemain, totalUsed, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("📊 WorkBuddy 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
