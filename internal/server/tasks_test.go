// ═══ 更新日志 ═══
// 2026-09-15: 新增。/tasks 与 /tasks/{key}/run 的路由与错误映射单测。

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/scheduler"
)

type fakeTasks struct {
	snap   []scheduler.TaskInfo
	err    error
	logs   []string
	logErr error
}

func (f fakeTasks) TaskSnapshot() []scheduler.TaskInfo { return f.snap }
func (f fakeTasks) TriggerTask(key string) error       { return f.err }
func (f fakeTasks) TaskLog(key string) ([]string, error) {
	return f.logs, f.logErr
}

func tasksReq(t *testing.T, h *Handler, method, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	var body map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec.Code, body
}

func TestTasksEndpointWithoutController(t *testing.T) {
	h := NewHandler(Config{})
	code, body := tasksReq(t, h, "GET", "/tasks")
	if code != http.StatusOK {
		t.Fatalf("未接入排程器也应 200，实际 %d", code)
	}
	if body["available"] != false {
		t.Fatalf("available 应为 false，实际 %v", body["available"])
	}
	if _, ok := body["tasks"].([]any); !ok {
		t.Fatalf("tasks 应为数组，实际 %#v", body["tasks"])
	}
}

func TestTasksEndpointWithController(t *testing.T) {
	h := NewHandler(Config{Tasks: fakeTasks{snap: []scheduler.TaskInfo{
		{Key: "checkin", Name: "签到", Detail: "每日签到", Hours: []int{9, 21}, Enabled: true},
	}}})
	code, body := tasksReq(t, h, "GET", "/tasks")
	if code != http.StatusOK || body["available"] != true {
		t.Fatalf("应 200 且 available=true，实际 %d / %v", code, body["available"])
	}
	list, _ := body["tasks"].([]any)
	if len(list) != 1 {
		t.Fatalf("应返回 1 个任务，实际 %d", len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["key"] != "checkin" || first["enabled"] != true {
		t.Fatalf("任务字段不对：%#v", first)
	}
}

func TestTaskRunStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"成功", nil, http.StatusAccepted},
		{"正在执行", scheduler.ErrTaskBusy, http.StatusConflict},
		{"未知任务", scheduler.ErrUnknownTask, http.StatusBadRequest},
	}
	for _, c := range cases {
		h := NewHandler(Config{Tasks: fakeTasks{err: c.err}})
		code, _ := tasksReq(t, h, "POST", "/tasks/checkin/run")
		if code != c.want {
			t.Fatalf("%s 应返回 %d，实际 %d", c.name, c.want, code)
		}
	}
}

func TestTaskRunWithoutController(t *testing.T) {
	h := NewHandler(Config{})
	code, _ := tasksReq(t, h, "POST", "/tasks/checkin/run")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("未接入排程器时手动触发应 503，实际 %d", code)
	}
}

func TestTaskRouteRequiresPost(t *testing.T) {
	// 只注册了 POST /tasks/{key}/run：用 GET 打同路径必须不匹配（而不是静默成功）。
	h := NewHandler(Config{Tasks: fakeTasks{}})
	code, _ := tasksReq(t, h, "GET", "/tasks/checkin/run")
	if code == http.StatusAccepted {
		t.Fatal("GET 不应触发任务")
	}
}

func TestTaskLogEndpoint(t *testing.T) {
	h := NewHandler(Config{Tasks: fakeTasks{logs: []string{"line one", "line two"}}})
	code, body := tasksReq(t, h, "GET", "/tasks/checkin/log")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if body["key"] != "checkin" {
		t.Fatalf("key 未回显：%v", body["key"])
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 2 || lines[0] != "line one" {
		t.Fatalf("日志行不对：%#v", body["lines"])
	}
	// 跑过但无输出：空数组而不是错误。
	h2 := NewHandler(Config{Tasks: fakeTasks{}})
	code2, body2 := tasksReq(t, h2, "GET", "/tasks/checkin/log")
	if code2 != http.StatusOK {
		t.Fatalf("无日志也应 200，实际 %d", code2)
	}
	if n, _ := body2["count"].(float64); n != 0 {
		t.Fatalf("count 应为 0，实际 %v", body2["count"])
	}
}

func TestTaskLogUnknownKey(t *testing.T) {
	h := NewHandler(Config{Tasks: fakeTasks{logErr: scheduler.ErrUnknownTask}})
	code, _ := tasksReq(t, h, "GET", "/tasks/nosuch/log")
	if code != http.StatusBadRequest {
		t.Fatalf("未知任务应 400，实际 %d", code)
	}
}

func TestTaskLogWithoutController(t *testing.T) {
	h := NewHandler(Config{})
	code, _ := tasksReq(t, h, "GET", "/tasks/checkin/log")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("未接入排程器应 503，实际 %d", code)
	}
}
