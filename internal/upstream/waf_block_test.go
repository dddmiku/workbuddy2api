// ═══ 更新日志 ═══
// 2026-09-17：锁定上游 WAF 拦截页的分类，防止 HTML 拦截页被当成请求参数错误回显。
package upstream

import "testing"

func TestClassifyUpstreamWAFBlockPage(t *testing.T) {
	// 观测原文（国际版 www.workbuddy.ai 前置的腾讯云 WAF，HTTP 403）。
	const page = `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8" />` +
		`<meta name="viewport" content="width=device-width, initial-scale=1.0" />` +
		`<title>WAF Block Page</title>` +
		`<link rel="stylesheet" href="https://domain-config-1256704386.cos.accelerate.myqcloud.com/block-pages/403/main_en.css" />` +
		`</head><body><p class="title">Your request has been interrupted</p></body></html>`
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"observed waf block page", 403, page, "upstream_waf"},
		{"waf feedback asset only", 403,
			`<html><head><title>Blocked</title></head><body><script src="https://api.waf-intl.qq.com/waf-attack-feedback/"></script></body></html>`,
			"upstream_waf"},
		{"plain html is not a waf page", 403,
			`<html><body><table><tr><td>ok</td></tr></table></body></html>`, "client"},
		{"params error keeps its own kind", 400,
			`{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, "bad_params"},
		{"channel rejection keeps its own kind", 400,
			`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, "channel_rejected"},
		{"content policy keeps its own kind", 400,
			`{"code":11128,"msg":"blocked by security policy: NSFW content"}`, "content_blocked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body).String(); got != c.want {
				t.Fatalf("Classify=%s want=%s", got, c.want)
			}
		})
	}
}
