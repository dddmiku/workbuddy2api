// ═══ 更新日志 ═══
// 2026-09-18：固定首个写已占槽的顺序，验证 Close 不丢弃关停前已提交的最后一笔镜像。
package redisstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRuntimeCloseDrainsAlreadyQueuedWrite(t *testing.T) {
	u := newTestUpstash()
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	u.goWrite(func() { close(firstStarted); <-release })
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first write did not occupy the only slot")
	}
	secondFinished := make(chan struct{})
	u.goWrite(func() { close(secondFinished) })
	closed := make(chan struct{})
	go func() { _ = u.Close(); close(closed) }()
	// 让 Close 真正开始停机后才释放在途写，排除第二笔抢先完成的假通过。
	select {
	case <-u.done:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Close did not begin")
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish draining writes")
	}
	select {
	case <-secondFinished:
	default:
		t.Fatal("Close discarded a write submitted before shutdown")
	}
}

func TestRuntimeCloseReportsDrainTimeout(t *testing.T) {
	previous := closeWaitTimeout
	closeWaitTimeout = 20 * time.Millisecond
	defer func() { closeWaitTimeout = previous }()
	u := newTestUpstash()
	started, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	u.goWrite(func() { close(started); <-release; close(completed) })
	<-started
	err := u.Close()
	close(release)
	<-completed
	if err == nil {
		t.Fatal("Close reported success although the final write did not finish before its deadline")
	}
}

type runtimeRedisHook struct{ process redis.ProcessHook }

func (h runtimeRedisHook) DialHook(next redis.DialHook) redis.DialHook     { return next }
func (h runtimeRedisHook) ProcessHook(redis.ProcessHook) redis.ProcessHook { return h.process }
func (h runtimeRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestRuntimeRedisPreservesWriteOrderForSameKey(t *testing.T) {
	for _, operation := range []string{"bind", "unbind", "snapshot"} {
		t.Run(operation, func(t *testing.T) {
			firstStarted, release, secondFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var mu sync.Mutex
			stored := ""
			client := redis.NewClient(&redis.Options{Addr: "unused.invalid:6379"})
			client.AddHook(runtimeRedisHook{process: func(_ context.Context, cmd redis.Cmder) error {
				value := ""
				if cmd.Name() == "set" {
					switch raw := cmd.Args()[2].(type) {
					case string:
						value = raw
					case []byte:
						value = string(raw)
					}
				}
				if value == "old" {
					close(firstStarted)
					<-release
				}
				mu.Lock()
				stored = value
				mu.Unlock()
				if value != "old" {
					close(secondFinished)
				}
				return nil
			}})
			u := &Upstash{client: client, sem: make(chan struct{}, 2), done: make(chan struct{})}
			if operation == "snapshot" {
				u.SaveState([]byte("old"))
			} else {
				u.SetBind("conversation-runtime", "old", time.Minute)
			}
			select {
			case <-firstStarted:
			case <-time.After(time.Second):
				t.Fatal("first write did not reach the in-memory Redis hook")
			}
			want := "new"
			switch operation {
			case "snapshot":
				u.SaveState([]byte(want))
			case "unbind":
				want = ""
				u.DelBind("conversation-runtime")
			default:
				u.SetBind("conversation-runtime", want, time.Minute)
			}
			select {
			case <-secondFinished:
			case <-time.After(30 * time.Millisecond):
			}
			close(release)
			if err := u.Close(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			got := stored
			mu.Unlock()
			if got != want {
				t.Fatalf("an older asynchronous write overwrote the newer state: got=%q want=%q", got, want)
			}
		})
	}
}
