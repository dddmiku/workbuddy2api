// ═══ 更新日志 ═══
// 2026-09-18：复现错误响应头已到达但正文静默时绕过空闲超时，避免永久占用账号租约。
package upstream

import (
	"context"
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

type runtimeHTTPTransport func(*http.Request) (*http.Response, error)

func (f runtimeHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRuntimeErrorResponseBodyRespectsIdleTimeout(t *testing.T) {
	c := &Client{
		ChatBaseCN: "https://unused.invalid", IdleTimeout: 20 * time.Millisecond,
		ChatHTTP: &http.Client{Transport: runtimeHTTPTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: &ctxBoundReader{ctx: r.Context()}, Request: r}, nil
		})},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	started := time.Now()
	body, _, _, err := c.ChatStreamContext(ctx, &auth.Auth{UID: "account-runtime"}, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{})
	if body != nil {
		body.Close()
	}
	if err == nil {
		t.Fatal("stalled upstream error body was accepted")
	}
	if time.Since(started) >= 300*time.Millisecond {
		t.Fatal("reading the error body ignored the configured stream idle timeout")
	}
}
