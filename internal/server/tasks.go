// ═══ 更新日志 ═══
// 2026-09-15: 新增。排程任务的 HTTP 面：GET /tasks 返回任务快照，
//   POST /tasks/{key}/run 手动触发。供账户管理面板的「定时任务」页使用。

package server

import (
	"errors"
	"net/http"

	"workbuddy2api/internal/scheduler"
)

// TaskController 排程任务的自省与手动触发（由 scheduler.Scheduler 实现）。
// 用窄接口而不是直接持有 *scheduler.Scheduler：server 只用到这两个能力，
// 测试可以塞假实现，不必把整个排程器搭起来。
type TaskController interface {
	TaskSnapshot() []scheduler.TaskInfo
	TriggerTask(key string) error
	// TaskLog 返回某类任务最近一次执行的日志行（旧 → 新）。
	// 未知 key 返回 scheduler.ErrUnknownTask；跑过但无输出返回空切片。
	TaskLog(key string) ([]string, error)
}

// tasks 返回全部排程任务的快照。
// 未接入排程器时返回 available=false 而不是 404：面板据此渲染"本进程无排程"的说明态，
// 比一个错误码对使用者更有用。
func (h *Handler) tasks(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "tasks": []any{}})
		return
	}
	snap := h.cfg.Tasks.TaskSnapshot()
	if snap == nil {
		snap = []scheduler.TaskInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "tasks": snap})
}

// taskLog 返回某类任务最近一次执行的日志行。
// 与 /tasks 分开是刻意的：日志动辄几百行，快照要轮询刷新，不能背着这份体积。
func (h *Handler) taskLog(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "tasks_unavailable",
			"本进程未接入排程器，无法读取任务日志")
		return
	}
	key := r.PathValue("key")
	lines, err := h.cfg.Tasks.TaskLog(key)
	if err != nil {
		status := http.StatusBadRequest
		msg := err.Error()
		if errors.Is(err, scheduler.ErrUnknownTask) {
			msg = "未知任务：" + key
		}
		writeOpenAIError(w, status, "task_log_failed", msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key": key, "lines": lines, "count": len(lines),
	})
}

// taskRun 手动触发单类任务。立即返回 202，任务在网关侧后台执行；结果落容器日志。
// 不做同步等待：签到/旅行这类要遍历全部账号打上游，耗时以十秒计，
// 让管理面板的 HTTP 请求挂在那里等既超时又占连接。
func (h *Handler) taskRun(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "tasks_unavailable",
			"本进程未接入排程器，无法手动触发任务")
		return
	}
	key := r.PathValue("key")
	if err := h.cfg.Tasks.TriggerTask(key); err != nil {
		status := http.StatusBadRequest
		msg := err.Error()
		switch {
		case errors.Is(err, scheduler.ErrTaskBusy):
			status = http.StatusConflict
			msg = "该任务正在执行中，等它跑完再试"
		case errors.Is(err, scheduler.ErrUnknownTask):
			msg = "未知任务：" + key
		}
		writeOpenAIError(w, status, "task_trigger_failed", msg)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "key": key, "message": "已触发，结果见容器日志",
	})
}
