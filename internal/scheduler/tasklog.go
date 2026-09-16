// ═══ 更新日志 ═══
// 2026-09-15: 新增。任务日志归集：把任务执行期间的日志行按任务归到内存环里，
//   供 GET /tasks/{key}/log 与管理面板「任务日志」查看。
//   起因：开学季/夜猫子是子脚本任务，exec.Cmd 默认不接 stdout/stderr，
//   容器日志里只剩一行 "school: ok"——出了问题看不到脚本说了什么。

package scheduler

import (
	"io"
	"log"
	"strings"
	"sync"
)

// taskLogRing 每类任务保留的日志行数上限（只留最近一次执行的后段，
// 面板看的是"刚才那次跑了什么"，不是完整归档）。
const taskLogRing = 400

// taskLogSink 任务日志环。cur 为当前正在执行的任务 key：为空表示没有任务在跑，
// 此时的日志行一律丢弃——否则运行期日志（HTTP 请求表格等）会把任务日志冲掉。
type taskLogSink struct {
	mu   sync.Mutex
	cur  TaskKey
	ring map[TaskKey][]string
}

var taskSink = &taskLogSink{ring: map[TaskKey][]string{}}

// teeWriter 把日志行同时写进基底 writer 与任务日志环。
// 基底先写：即便环这一侧出问题，容器日志也不会因此丢行。
type teeWriter struct {
	base io.Writer
	ring *taskLogSink
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.base.Write(p)
	t.ring.Write(p)
	return n, err
}

var logTeeMu sync.Mutex

// ensureLogTee 保证标准 logger 的输出分流一份进任务日志环。
//
// 每次任务开始前都调用，而不是只装一次：log.SetOutput 是全局单点，测试与宿主
// 都可能替换它，那条链随时会断——只装一次的写法一旦被替换就永久失效，
// 表现是"面板上任务日志永远是空的"。这里检测到链条不在自己手上就按当前
// writer 重新包一层，既不吞掉别人的输出，也保证归集自持。
func ensureLogTee() {
	logTeeMu.Lock()
	defer logTeeMu.Unlock()
	if t, ok := log.Writer().(*teeWriter); ok && t.ring == taskSink {
		return
	}
	log.SetOutput(&teeWriter{base: log.Writer(), ring: taskSink})
}

// Write 实现 io.Writer：把一行追加到当前任务的环里。
// 没有任务在跑时静默丢弃（返回成功，不干扰 MultiWriter 的其它分支）。
func (s *taskLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == "" {
		return len(p), nil
	}
	line := strings.TrimRight(string(p), "\n")
	if strings.TrimSpace(line) == "" {
		return len(p), nil
	}
	buf := append(s.ring[s.cur], line)
	if len(buf) > taskLogRing {
		buf = buf[len(buf)-taskLogRing:]
	}
	s.ring[s.cur] = buf
	return len(p), nil
}

func (s *taskLogSink) begin(k TaskKey) {
	ensureLogTee()
	s.mu.Lock()
	s.cur = k
	s.ring[k] = nil // 新一轮执行从干净的一页开始
	s.mu.Unlock()
}

func (s *taskLogSink) end() {
	s.mu.Lock()
	s.cur = ""
	s.mu.Unlock()
}

// emit 把子脚本输出的一行按"当前任务"的上下文记进标准 logger，
// 从而同时落到容器日志与任务日志环。无任务上下文时原样记。
func (s *taskLogSink) emit(line string) {
	s.mu.Lock()
	key := s.cur
	s.mu.Unlock()
	if key == "" {
		log.Print(line)
		return
	}
	log.Printf("[%s] %s", key, line)
}

// scriptSink 接管子脚本的 stdout/stderr：按行分流进日志（见 taskLogSink.emit）。
// 不做行缓冲的跨 Write 拼接：脚本输出本就是行缓冲，行尾残留会在下一条 Write 里补齐；
// 极端情况下一条超长行可能被拆成两行，对"看脚本说了什么"这个用途可以接受。
type scriptSink struct{}

func (scriptSink) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		taskSink.emit(strings.TrimRight(line, "\r"))
	}
	return len(p), nil
}

// TaskLog 返回某任务最近一次执行的日志行（旧 → 新）。
// 未知 key 返回 ErrUnknownTask；已知但尚未跑过返回空切片与 nil。
func (s *Scheduler) TaskLog(key string) ([]string, error) {
	k, ok := kindOf(key)
	if !ok {
		return nil, ErrUnknownTask
	}
	taskSink.mu.Lock()
	defer taskSink.mu.Unlock()
	lines := taskSink.ring[k.key()]
	out := make([]string, len(lines))
	copy(out, lines)
	return out, nil
}
