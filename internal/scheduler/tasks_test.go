// ═══ 更新日志 ═══
// 2026-09-15: 新增。排程任务自省与手动触发的单测：快照顺序与开关语义、
//   互斥占用、未知 key。

package scheduler

import (
	"strings"
	"testing"
)

func TestTaskSnapshotOrderAndSwitches(t *testing.T) {
	s := New(Config{}) // 零值 Config = 六类全启用、小时回落默认
	snap := s.TaskSnapshot()
	wantKeys := []string{"checkin", "travel", "activity", "keepalive", "school", "cat",
		"redeem", "lottery", "makeup"}
	if len(snap) != len(wantKeys) {
		t.Fatalf("应返回 %d 类任务，实际 %d", len(wantKeys), len(snap))
	}
	for i, k := range wantKeys {
		if snap[i].Key != k {
			t.Fatalf("第 %d 项应为 %s，实际 %s", i, k, snap[i].Key)
		}
		if snapshotIsEmpty(snap[i]) {
			t.Fatalf("%s 的名称/说明为空", k)
		}
	}
	if len(snap[0].Hours) != 2 || snap[0].Hours[0] != 9 || snap[0].Hours[1] != 21 {
		t.Fatalf("签到小时应为 [9 21]，实际 %v", snap[0].Hours)
	}
	// 启用且未跑过：有下次时点、无上次时点。
	if snap[0].NextRun == nil {
		t.Fatal("启用任务应有下次运行时间")
	}
	if snap[0].LastRun != nil {
		t.Fatal("从未运行过的任务，上次运行应为 null")
	}

	// 显式禁用后：Enabled=false 且不再给下次时点（与 nextWake 跳过零时点同口径）。
	s2 := New(Config{CatDisabled: true})
	cat := s2.TaskSnapshot()[5]
	if cat.Key != "cat" {
		t.Fatalf("第 6 项应为 cat，实际 %s", cat.Key)
	}
	if cat.Enabled {
		t.Fatal("CatDisabled=true 时 cat 应为停用")
	}
	if cat.NextRun != nil {
		t.Fatal("停用任务不应给出下次运行时间")
	}
}

func snapshotIsEmpty(info TaskInfo) bool {
	return info.Name == "" || info.Detail == ""
}

func TestBeginTaskMutualExclusion(t *testing.T) {
	s := New(Config{})
	k, ok := kindOf("checkin")
	if !ok {
		t.Fatal("checkin 应能解析为内部任务种类")
	}
	if !s.beginTask(k) {
		t.Fatal("首次占用应成功")
	}
	if s.beginTask(k) {
		t.Fatal("重复占用应失败")
	}
	// 手动触发撞上正在执行的任务：返回 ErrTaskBusy 而不是排队。
	if err := s.TriggerTask("checkin"); err != ErrTaskBusy {
		t.Fatalf("应返回 ErrTaskBusy，实际 %v", err)
	}
	// 占用期间快照应报 running。
	if !s.TaskSnapshot()[0].Running {
		t.Fatal("占用期间快照应标记 running")
	}
	s.endTask(k)
	if s.TaskSnapshot()[0].Running {
		t.Fatal("释放后不应再标记 running")
	}
	if s.TaskSnapshot()[0].LastRun == nil {
		t.Fatal("释放后应记下完成时刻")
	}
}

func TestTriggerTaskUnknownKey(t *testing.T) {
	s := New(Config{})
	if err := s.TriggerTask("nosuch"); err != ErrUnknownTask {
		t.Fatalf("未知 key 应返回 ErrUnknownTask，实际 %v", err)
	}
	if err := s.TriggerTask(""); err != ErrUnknownTask {
		t.Fatalf("空 key 应返回 ErrUnknownTask，实际 %v", err)
	}
}

func TestTaskKeyRoundTrip(t *testing.T) {
	for _, key := range []string{"checkin", "travel", "activity", "keepalive", "school", "cat",
		"redeem", "lottery", "makeup"} {
		k, ok := kindOf(key)
		if !ok {
			t.Fatalf("%s 应可解析", key)
		}
		if got := k.key(); string(got) != key {
			t.Fatalf("taskKind 与 TaskKey 往返不一致：%s → %s", key, got)
		}
	}
}

func TestTaskLogRing(t *testing.T) {
	s := New(Config{})
	// 日志环是包级全局：本包其它测试（如 school_test 的假 cat 脚本）也会往里写。
	// 断言前先清空，否则用例之间产生顺序依赖——单跑过、全量跑挂。
	taskSink.mu.Lock()
	taskSink.ring = map[TaskKey][]string{}
	taskSink.mu.Unlock()

	// 未知 key：报错而不是空日志。
	if _, err := s.TaskLog("nosuch"); err != ErrUnknownTask {
		t.Fatalf("未知 key 应返回 ErrUnknownTask，实际 %v", err)
	}
	// 已登记但没跑过：空切片、无错误。
	lines, err := s.TaskLog("checkin")
	if err != nil {
		t.Fatalf("已知 key 不应报错：%v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("未跑过应为空，实际 %d 行", len(lines))
	}

	// 模拟一次执行：begin 后写日志，应归到该任务名下。
	taskSink.begin(TaskKeyCheckin)
	taskSink.emit("第一行")
	taskSink.emit("第二行")
	taskSink.end()

	lines, _ = s.TaskLog("checkin")
	if len(lines) != 2 {
		t.Fatalf("应归集 2 行，实际 %d：%v", len(lines), lines)
	}
	// 行首带 log 包的时间戳（LstdFlags），这里只断言任务前缀与正文。
	if !strings.Contains(lines[0], "[checkin] 第一行") || !strings.Contains(lines[1], "[checkin] 第二行") {
		t.Fatalf("行内容不对：%q / %q", lines[0], lines[1])
	}
	// 不串到别的任务。
	other, _ := s.TaskLog("cat")
	if len(other) != 0 {
		t.Fatalf("不该串到 cat：%v", other)
	}

	// 环有上限：超出后只留最近的。
	taskSink.begin(TaskKeyCat)
	for i := 0; i < taskLogRing+50; i++ {
		taskSink.emit("x")
	}
	taskSink.end()
	lines, _ = s.TaskLog("cat")
	if len(lines) != taskLogRing {
		t.Fatalf("环上限应为 %d，实际 %d", taskLogRing, len(lines))
	}

	// 无任务上下文时写入被丢弃，不污染任何任务。
	taskSink.emit("飘在空中的一行")
	lines, _ = s.TaskLog("checkin")
	if len(lines) != 2 {
		t.Fatalf("无上下文写入不该进任何任务：%v", lines)
	}
	// 重新 begin 会清掉上一轮。
	taskSink.begin(TaskKeyCheckin)
	taskSink.emit("新一轮")
	taskSink.end()
	lines, _ = s.TaskLog("checkin")
	if len(lines) != 1 || !strings.Contains(lines[0], "[checkin] 新一轮") {
		t.Fatalf("新一轮应清空旧记录：%v", lines)
	}
}

func TestNewTasksRedeemLottery(t *testing.T) {
	s := New(Config{})
	byKey := map[string]TaskInfo{}
	for _, ti := range s.TaskSnapshot() {
		byKey[ti.Key] = ti
	}
	for _, k := range []string{"redeem", "lottery", "makeup"} {
		ti, ok := byKey[k]
		if !ok {
			t.Fatalf("快照缺少任务 %s", k)
		}
		if !ti.Enabled {
			t.Fatalf("%s 零值配置下应默认启用", k)
		}
		if ti.NextRun == nil {
			t.Fatalf("%s 启用时应有下次运行时间", k)
		}
		if ti.Name == "" || ti.Detail == "" {
			t.Fatalf("%s 名称/说明为空", k)
		}
	}
	if byKey["redeem"].Hours[0] != 9 || byKey["lottery"].Hours[0] != 21 || byKey["makeup"].Hours[0] != 9 {
		t.Fatalf("默认小时不对：redeem=%v lottery=%v makeup=%v",
			byKey["redeem"].Hours, byKey["lottery"].Hours, byKey["makeup"].Hours)
	}

	// 显式禁用后：停用 + 不再给下次时点（与其余六类同口径）。
	s2 := New(Config{RedeemDisabled: true, LotteryDisabled: true, MakeupDisabled: true})
	for _, ti := range s2.TaskSnapshot() {
		if ti.Key != "redeem" && ti.Key != "lottery" && ti.Key != "makeup" {
			continue
		}
		if ti.Enabled {
			t.Fatalf("%s 显式禁用后仍报启用", ti.Key)
		}
		if ti.NextRun != nil {
			t.Fatalf("%s 停用后不应给出下次运行时间", ti.Key)
		}
	}

	// 未知 key 仍报错（新增两类不能把 kindOf 的兜底弄坏）。
	if _, err := s2.TaskLog("nosuchtask"); err != ErrUnknownTask {
		t.Fatalf("未知 key 应报 ErrUnknownTask，实际 %v", err)
	}
}
