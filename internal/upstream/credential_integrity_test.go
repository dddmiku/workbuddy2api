// ═══ 更新日志 ═══
// 2026-09-16：移植真实审计的刷新/请求头 race，并验证完整请求凭据一致、并发刷新合并与显式 realm 保留。
// 2026-09-16：补全合成模型目录的 cli agent 归属，确保并发测试经过真实 FetchModels 成功路径。
// 2026-09-17：目录模拟同时覆盖 v3 与企业端点，保留凭据代际断言并验证合并后的双路成功探测。
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

type credentialIntegrityTransport func(*http.Request) (*http.Response, error)

func (f credentialIntegrityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func credentialIntegrityResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCredentialIntegrityConcurrentRefreshAndReaders(t *testing.T) {
	a := &auth.Auth{UID: "integrity", AccessToken: "access-0", RefreshToken: "refresh-0", Domain: "domain-0.example", ExpiresAt: 9999999999}
	var generation atomic.Int64
	c := &Client{ChatBaseCN: "https://chat.example", BillingBaseCN: "https://billing.example", HTTP: &http.Client{Transport: credentialIntegrityTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			n := generation.Add(1)
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"accessToken": fmt.Sprintf("access-%d", n), "refreshToken": fmt.Sprintf("refresh-%d", n), "domain": fmt.Sprintf("domain-%d.example", n), "expiresIn": 3600}})
			return credentialIntegrityResponse(string(raw)), nil
		}
		if domain := r.Header.Get("X-Domain"); domain != "" {
			want := "domain-" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer access-") + ".example"
			if domain != want {
				t.Errorf("mixed credential generations: domain=%q want=%q", domain, want)
			}
		}
		if strings.HasSuffix(r.URL.Path, "chat/completions") {
			return credentialIntegrityResponse("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
		}
		if strings.HasSuffix(r.URL.Path, "models") || r.URL.Path == "/v3/config" {
			return credentialIntegrityResponse(`{"code":0,"data":{"models":[{"id":"integrity-model","maxOutputTokens":8192}],"agents":[{"name":"cli","models":["integrity-model"]}]}}`), nil
		}
		return credentialIntegrityResponse(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 300; i++ {
			if err := c.RefreshToken(a); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			for _, fill := range []func(*http.Request){
				func(r *http.Request) { c.ChatHeaders(r, a, "", ChatMeta{}) },
				func(r *http.Request) { c.BillingHeaders(r, a) },
				func(r *http.Request) { c.RefreshHeaders(r, a) },
			} {
				req, _ := http.NewRequest("POST", "https://headers.example", nil)
				fill(req)
				if domain := req.Header.Get("X-Domain"); domain != "" {
					want := "domain-" + strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer access-") + ".example"
					if domain != want {
						t.Errorf("request headers mixed credential generations: %q != %q", domain, want)
					}
				}
			}
			_ = a.NeedsRefresh(time.Minute)
			_ = a.Realm()
			_ = a.RealmStored()
			if i%25 == 0 {
				rc, _, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
				if err != nil {
					t.Error(err)
					return
				}
				rc.Close()
				if _, err := c.FetchModels(a); err != nil {
					t.Error(err)
					return
				}
				if _, err := c.UserResource(a); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	close(start)
	wg.Wait()
}

func TestCredentialIntegrityRefreshSingleFlight(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			a := &auth.Auth{UID: "singleflight", AccessToken: "old-access", RefreshToken: "old-refresh", Domain: "copilot.tencent.com", ExpiresAt: 9999999999}
			started, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int64
			failure := errors.New("synthetic refresh outage")
			c := &Client{ChatBaseCN: "https://refresh.example", HTTP: &http.Client{Transport: credentialIntegrityTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				once.Do(func() { close(started) })
				<-release
				if fail {
					return nil, failure
				}
				return credentialIntegrityResponse(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`), nil
			})}}
			const readers = 24
			results := make(chan error, readers)
			go func() { results <- c.RefreshToken(a) }()
			<-started
			var entered sync.WaitGroup
			entered.Add(readers - 1)
			for i := 1; i < readers; i++ {
				go func() { entered.Done(); results <- c.RefreshToken(a) }()
			}
			entered.Wait()
			// 所有调用保持重叠，给待运行 goroutine 进入 flight 的机会；上游由显式闸门控制。
			time.Sleep(30 * time.Millisecond)
			readable := make(chan struct{})
			go func() { _ = a.Snapshot(); _ = a.NeedsRefresh(time.Minute); _ = a.Realm(); close(readable) }()
			select {
			case <-readable:
			case <-time.After(time.Second):
				t.Fatal("refresh network I/O held the credential lock")
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("overlapping refreshes made %d upstream calls, want 1", n)
			}
			unblock()
			for i := 0; i < readers; i++ {
				select {
				case err := <-results:
					if fail && !errors.Is(err, failure) {
						t.Errorf("waiter lost refresh error: %v", err)
					}
					if !fail && err != nil {
						t.Errorf("waiter failed: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("refresh waiter was never released")
				}
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("coalesced refresh made %d total upstream calls", n)
			}
			if got := a.Snapshot(); fail && (got.AccessToken != "old-access" || got.RefreshToken != "old-refresh") {
				t.Errorf("failed refresh changed credentials")
			}
			if got := a.Snapshot(); !fail && (got.AccessToken != "new-access" || got.RefreshToken != "new-refresh") {
				t.Errorf("successful refresh did not atomically publish both tokens")
			}
		})
	}
}

func TestCredentialIntegrityRefreshRetainsExplicitRealm(t *testing.T) {
	a, err := auth.Parse([]byte(`{"accessToken":"a","refreshToken":"r","domain":"copilot.tencent.com","realm":"global","uid":"explicit-global"}`))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{GlobalEnabled: true, ChatBaseCN: "https://cn.example", ChatBaseGlobal: "https://global.example", HTTP: &http.Client{Transport: credentialIntegrityTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "global.example" || r.Header.Get("Origin") != "https://www.workbuddy.ai" || r.Header.Get("Accept-Language") != "en-US" {
			t.Errorf("explicit realm was lost from refresh snapshot: url=%s origin=%s language=%s", r.URL, r.Header.Get("Origin"), r.Header.Get("Accept-Language"))
		}
		return credentialIntegrityResponse(`{"code":0,"data":{"accessToken":"b","refreshToken":"s","expiresIn":3600}}`), nil
	})}}
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	if a.RealmStored() != "global" {
		t.Fatal("refresh changed explicit stored realm")
	}
}
