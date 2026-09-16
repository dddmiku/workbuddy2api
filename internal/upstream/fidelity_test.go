// ═══ 更新日志 ═══
// 2026-09-16：以真实 Codex 工具结果和数字参数锁定业务内容保真，防止清洗改变代码与编号。
// 2026-09-17：公开派生样例改用相对工作目录，删除本地审计路径及原始请求校验值声明。
package upstream

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// 以下为隔离测试中编号改写边界整理出的公开派生工具样例。
// 工作目录使用相对示例路径，真实请求另留在本地审计归档中。
// 这里只验证本地转换对代码与编号的保真，不作为原始网络捕获或真实客户端运行证据。
const capturedCodexInvoiceOutput = `Chunk ID: d29f86
Wall time: 0.0000 seconds
Process exited with code 0
Original token count: 45
Output:
[
  {
    "invoice_id": 11128,
    "amount_cents": 1250
  },
  {
    "invoice_id": 22229,
    "amount_cents": -200
  },
  {
    "invoice_id": 33330,
    "amount_cents": 450
  }
]
`

const capturedCodexTestOutput = `Chunk ID: 387d62
Wall time: 0.0000 seconds
Process exited with code 0
Original token count: 247
Output:
#!/usr/bin/env python3
# ═══ 更新日志 ═══
# 2026-09-16：验证空集合、单条、负金额、总数及编号保真。
import json
from pathlib import Path
import unittest
from ledger import summarize

class LedgerTests(unittest.TestCase):
    def test_empty(self):
        self.assertEqual(summarize([]), {'count': 0, 'total_cents': 0, 'invoice_ids': []})
    def test_one(self):
        self.assertEqual(summarize([{'invoice_id': 77, 'amount_cents': 450}])['total_cents'], 450)
    def test_total(self):
        rows = json.loads(Path('invoices.json').read_text())
        self.assertEqual(summarize(rows)['total_cents'], 1500)
    def test_ids(self):
        rows = json.loads(Path('invoices.json').read_text())
        self.assertEqual(summarize(rows)['invoice_ids'], [11128, 22229, 33330])
    def test_negative(self):
        self.assertEqual(summarize([{'invoice_id': 7, 'amount_cents': -30}])['total_cents'], -30)

if __name__ == '__main__':
    unittest.main()
`

func TestCapturedCodexToolResultsFidelity(t *testing.T) {
	cases := []struct {
		callID    string
		arguments string
		output    string
	}{
		{
			"call_02_ET_72xQhTnzKEcteWQSDf2G5189",
			`{"cmd": "cat test_ledger.py", "workdir": "."}`,
			capturedCodexTestOutput,
		},
		{
			"call_03_ET_wKEzwuiG23u24vyJpRPK9314",
			`{"cmd": "cat invoices.json", "workdir": "."}`,
			capturedCodexInvoiceOutput,
		},
	}
	for _, legacySanitize := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_sanitize=%t", legacySanitize), func(t *testing.T) {
			var calls, results []any
			for _, c := range cases {
				calls = append(calls, map[string]any{
					"id": c.callID, "type": "function",
					"function": map[string]any{"name": "exec_command", "arguments": c.arguments},
				})
				results = append(results, map[string]any{
					"role": "tool", "tool_call_id": c.callID, "content": c.output,
				})
			}
			messages := append([]any{
				map[string]any{"role": "assistant", "content": nil, "tool_calls": calls},
			}, results...)
			got := prepareFidelityMessages(t, "deepseek-v4.1-flash", legacySanitize, messages)
			if !reflect.DeepEqual(got, messages) {
				t.Fatalf("captured Codex tool exchange changed\ngot:  %#v\nwant: %#v", got, messages)
			}
		})
	}
}

func TestBusinessNumericToolArgumentsFidelity(t *testing.T) {
	for _, arguments := range []string{
		`{"invoice_id":11128}`,
		`{"signed":-11128,"decimal":11128.75,"codes":[11128,211128]}`,
		`{"id":9007199254740993,"negative_zero":-0,"code":"11128"}`,
	} {
		for _, legacySanitize := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy_sanitize=%t", arguments, legacySanitize), func(t *testing.T) {
				messages := []any{
					map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
						map[string]any{"id": "invoice", "type": "function", "function": map[string]any{
							"name": "lookup_invoice", "arguments": arguments,
						}},
					}},
					map[string]any{"role": "tool", "tool_call_id": "invoice", "content": "found"},
				}
				got := prepareFidelityMessages(t, "deepseek-v4.1-flash", legacySanitize, messages)
				if len(got) != len(messages) {
					t.Fatalf("tool exchange lost: %#v", got)
				}
				call := got[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
				actual := call["function"].(map[string]any)["arguments"].(string)
				if !json.Valid([]byte(actual)) {
					t.Errorf("valid numeric arguments became invalid JSON: %q", actual)
				}
				if actual != arguments {
					t.Errorf("numeric arguments changed: got %q, want %q", actual, arguments)
				}
			})
		}
	}
}

func prepareFidelityMessages(t *testing.T, model string, legacySanitize bool, messages []any) []any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": model, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	out := PrepareBodyOptWithEffortsAndDefault(body, legacySanitize, nil, nil)
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("prepared body is not valid JSON: %v", err)
	}
	if result["stream"] != true {
		t.Fatal("upstream stream compatibility was lost")
	}
	got, ok := result["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing from prepared body: %s", out)
	}
	return got
}
