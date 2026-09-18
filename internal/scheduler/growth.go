// ═══ 更新日志 ═══
// 2026-09-18：成长中心脚本沿用排程上下文，关停后不再继续执行或启动子命令。
// 2026-09-15: 新增。成长中心两类排程：连登档位兑换（redeem）与抽奖清空（lottery）。
//   来源：对照 workbuddy.py 的 do_redeem_by_streak / do_lottery / use_makeup_card，
//   网关原有排程不含这三项。

// growth.go 成长中心的三个脚本类排程：连登兑换 / 抽奖 / 补签。
//
// 为什么是脚本而不是原生 Go：这两个动作打的是 chat 域（copilot.tencent.com）的 growth
// 接口，scripts/task_common.py 已沉淀同源的请求助手（headers / base / client_token 约定），
// 复用现成管道比在 Go 里再铺一遍 upstream 调用面更稳，也与 school/cat 的脚本类任务一致。
package scheduler

import "context"

// RunRedeemNow 立即执行连登档位兑换：growth_center.py ALL --redeem-only --yes。
// 档位不足（403）与已兑换（409）都属正常跳过——脚本自行判定并在 summary 里记账，
// 不当失败、不抛错。失败只记 WARN，不影响调度主循环。
func (s *Scheduler) RunRedeemNow() {
	s.runRedeem(context.Background())
}

func (s *Scheduler) runRedeem(ctx context.Context) {
	runScriptContext(ctx, "redeem", repoRoot(), [][]string{
		{pythonCmd(), "scripts/growth_center.py", "ALL", "--redeem-only", "--yes"},
	})
}

// RunLotteryNow 立即执行成长中心抽奖：growth_center.py ALL --lottery-only --yes。
// 抽到次数用完即止；任一次 draw 失败立即停止本轮（继续抽只会把频控越撞越死），
// 剩余次数留到下一次排程。
func (s *Scheduler) RunLotteryNow() {
	s.runLottery(context.Background())
}

func (s *Scheduler) runLottery(ctx context.Context) {
	runScriptContext(ctx, "lottery", repoRoot(), [][]string{
		{pythonCmd(), "scripts/growth_center.py", "ALL", "--lottery-only", "--yes"},
	})
}

// RunMakeupNow 立即执行补签：growth_center.py ALL --makeup-only --yes。
// 只补"最近一次漏签"，且只在补签卡有余量时才动手——没卡、没漏签都正常跳过。
// 兑换档位会发卡，所以补签的时点排在兑换之后（同为 9 点的场景）。
func (s *Scheduler) RunMakeupNow() {
	s.runMakeup(context.Background())
}

func (s *Scheduler) runMakeup(ctx context.Context) {
	runScriptContext(ctx, "makeup", repoRoot(), [][]string{
		{pythonCmd(), "scripts/growth_center.py", "ALL", "--makeup-only", "--yes"},
	})
}
