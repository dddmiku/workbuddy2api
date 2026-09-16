// ═══ 更新日志 ═══
// 2026-09-15: 新增。排程任务自省 + 手动触发：供 /tasks 端点与账户管理面板读取各类任务的
//   小时 / 开关 / 下次运行 / 上次完成 / 是否在跑，并可按 key 手动触发任意一类。
//   互斥复用 dispatch 的 beginTask/endTask，手动触发与定时点不会重复打上游。

package scheduler

import (
	"context"
	"errors"
	"log"
	"time"
)

// TaskKey 九类排程任务的稳定对外标识。这是契约：/tasks 端点与管理面板按它寻址，
// 不要随内部 taskKind 的枚举顺序变动（内部顺序属于实现细节，可直接重排）。
type TaskKey string

const (
	TaskKeyCheckin   TaskKey = "checkin"   // 签到 + 余额查询解冻
	TaskKeyTravel    TaskKey = "travel"    // 猫猫旅行：领养 / 派出 / 领奖
	TaskKeyActivity  TaskKey = "activity"  // 活跃上报：连登天数 + 领猫前置
	TaskKeyKeepalive TaskKey = "keepalive" // token 保活
	TaskKeySchool    TaskKey = "school"    // 开学季任务
	TaskKeyCat       TaskKey = "cat"       // 夜猫子任务
	TaskKeyRedeem    TaskKey = "redeem"    // 连登档位兑换
	TaskKeyLottery   TaskKey = "lottery"   // 成长中心抽奖
	TaskKeyMakeup    TaskKey = "makeup"    // 补签（补最近一次漏签）
)

// ErrUnknownTask 未知任务 key；ErrTaskBusy 该任务当前正在执行。
var (
	ErrUnknownTask = errors.New("unknown task key")
	ErrTaskBusy    = errors.New("task is already running")
)

// TaskInfo 单类任务的对外快照。时间字段用指针：JSON 里 null 表达"从未运行过 / 已禁用无排程"，
// 与"1970 年"这类哨兵值区分开，前端不必猜。
type TaskInfo struct {
	Key     string     `json:"key"`
	Name    string     `json:"name"`
	Detail  string     `json:"detail"`
	Hours   []int      `json:"hours"`
	Enabled bool       `json:"enabled"`
	NextRun *time.Time `json:"next_run"`
	LastRun *time.Time `json:"last_run"`
	Running bool       `json:"running"`
}

// key 内部 taskKind → 对外 TaskKey。返回空串表示未登记的种类（不该出现）。
func (k taskKind) key() TaskKey {
	switch k {
	case taskCheckin:
		return TaskKeyCheckin
	case taskTravel:
		return TaskKeyTravel
	case taskActivity:
		return TaskKeyActivity
	case taskKeepalive:
		return TaskKeyKeepalive
	case taskSchool:
		return TaskKeySchool
	case taskCat:
		return TaskKeyCat
	case taskRedeem:
		return TaskKeyRedeem
	case taskLottery:
		return TaskKeyLottery
	case taskMakeup:
		return TaskKeyMakeup
	}
	return ""
}

// kindOf 对外 TaskKey → 内部 taskKind。
func kindOf(key string) (taskKind, bool) {
	switch TaskKey(key) {
	case TaskKeyCheckin:
		return taskCheckin, true
	case TaskKeyTravel:
		return taskTravel, true
	case TaskKeyActivity:
		return taskActivity, true
	case TaskKeyKeepalive:
		return taskKeepalive, true
	case TaskKeySchool:
		return taskSchool, true
	case TaskKeyCat:
		return taskCat, true
	case TaskKeyRedeem:
		return taskRedeem, true
	case TaskKeyLottery:
		return taskLottery, true
	case TaskKeyMakeup:
		return taskMakeup, true
	}
	return 0, false
}

// describe 任务的展示名与说明。与 README「定时积分任务」一节逐条对应，
// 面板直接透出，避免同一套文案在两处各写一遍后漂移。
func describe(k TaskKey) (string, string) {
	switch k {
	case TaskKeyCheckin:
		return "签到", "每日签到 + 余额查询；余额恢复后自动解冻冷却中的账号"
	case TaskKeyActivity:
		return "活跃上报", "对话事件连发上报，点亮连登天数、解锁领养前置"
	case TaskKeyTravel:
		return "猫猫旅行", "独立排程：领养 / 派出 / 领奖闭环推进"
	case TaskKeyKeepalive:
		return "token 保活", "全账号刷新 token；session 失效连续 3 次才禁用"
	case TaskKeySchool:
		return "开学季任务", "任务点亮 + claim + 自动抽空抽奖余额；活动下线时自动跳过"
	case TaskKeyCat:
		return "夜猫子任务", "夜猫窗口（23:00–08:00 CST）内补一次 black_cat 任务"
	case TaskKeyRedeem:
		return "连登兑换", "按连登档位自动兑换（7 / 14 / 28 天）；已兑换与天数不足都安全跳过"
	case TaskKeyLottery:
		return "成长抽奖", "成长中心抽奖，清空当日可用次数；一次失败即停，不硬撞频控"
	case TaskKeyMakeup:
		return "补签", "用补签卡补最近一次漏签；今天不算漏签，上线日之前的空格不碰"
	}
	return string(k), ""
}

// scheduleOf 单类任务的排程小时与开关状态（来自 Config，即 config.json 的 schedule 段）。
func (s *Scheduler) scheduleOf(k TaskKey) ([]int, bool) {
	switch k {
	case TaskKeyCheckin:
		return s.cfg.CheckinHours, !s.cfg.CheckinDisabled
	case TaskKeyTravel:
		return s.cfg.TravelHours, !s.cfg.TravelDisabled
	case TaskKeyActivity:
		return s.cfg.ActivityHours, !s.cfg.ActivityDisabled
	case TaskKeyKeepalive:
		return s.cfg.KeepaliveHours, !s.cfg.KeepaliveDisabled
	case TaskKeySchool:
		return s.cfg.SchoolHours, !s.cfg.SchoolDisabled
	case TaskKeyCat:
		return s.cfg.CatHours, !s.cfg.CatDisabled
	case TaskKeyRedeem:
		return s.cfg.RedeemHours, !s.cfg.RedeemDisabled
	case TaskKeyLottery:
		return s.cfg.LotteryHours, !s.cfg.LotteryDisabled
	case TaskKeyMakeup:
		return s.cfg.MakeupHours, !s.cfg.MakeupDisabled
	}
	return nil, false
}

// TaskSnapshot 返回全部任务的当前快照，顺序固定为
// 签到 / 猫猫旅行 / 活跃上报 / token 保活 / 开学季 / 夜猫子 / 连登兑换 / 成长抽奖 / 补签。
// 新增的两类追加在尾部：既有顺序不动，面板与既有断言都不受影响。
func (s *Scheduler) TaskSnapshot() []TaskInfo {
	now := time.Now()
	keys := []TaskKey{
		TaskKeyCheckin, TaskKeyTravel, TaskKeyActivity,
		TaskKeyKeepalive, TaskKeySchool, TaskKeyCat,
		TaskKeyRedeem, TaskKeyLottery, TaskKeyMakeup,
	}
	out := make([]TaskInfo, 0, len(keys))
	for _, key := range keys {
		name, detail := describe(key)
		hours, enabled := s.scheduleOf(key)
		info := TaskInfo{
			Key: string(key), Name: name, Detail: detail,
			Hours: hours, Enabled: enabled,
		}
		// 禁用任务没有下个时点（nextWake 同样跳过），置 nil 而不是留一个假的未来时刻。
		if enabled {
			if at := nextFire(now, hours); !at.IsZero() {
				info.NextRun = &at
			}
		}
		s.taskMu.Lock()
		if t, ok := s.taskLast[key]; ok {
			last := t
			info.LastRun = &last
		}
		info.Running = s.taskBusy[key]
		s.taskMu.Unlock()
		out = append(out, info)
	}
	return out
}

// beginTask 占用单类任务；已被占用返回 false。定时入口与手动触发共用，保证不并发。
func (s *Scheduler) beginTask(k taskKind) bool {
	key := k.key()
	if key == "" {
		return false
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.taskBusy[key] {
		return false
	}
	s.taskBusy[key] = true
	return true
}

// endTask 释放任务并记下完成时刻（供面板显示"上次运行"）。
func (s *Scheduler) endTask(k taskKind) {
	key := k.key()
	if key == "" {
		return
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	s.taskBusy[key] = false
	s.taskLast[key] = time.Now()
}

// TriggerTask 手动触发单类任务。任务在后台 goroutine 里跑，调用立即返回。
// 正在执行时返回 ErrTaskBusy——不排队：连点几次就往上堆一串重复的上游调用，
// 对签到这个"同一时刻只许一次"的接口尤其危险。
func (s *Scheduler) TriggerTask(key string) error {
	k, ok := kindOf(key)
	if !ok {
		return ErrUnknownTask
	}
	if !s.beginTask(k) {
		return ErrTaskBusy
	}
	go func() {
		defer s.endTask(k)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERR: scheduler: 手动触发 %s panic: %v", key, r)
			}
		}()
		log.Printf("scheduler: 手动触发 %s", key)
		s.runTask(context.Background(), k)
	}()
	return nil
}
