#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-16：验证密钥管理的管理员登录、请求来源、大小和字段限制，使用本地服务与合成会话。
# 2026-09-17：覆盖模型绑定字段的透传与非法输入的本地拒绝。
# 2026-09-17：覆盖用量统计页的数据通道：登录保护 + 经本机管理 socket 读 /usage。
# 2026-09-17：覆盖请求日志解析：带 key= 列的新行、旧格式行与警告行分流。

import http.client
import json
import threading
import unittest
from unittest.mock import patch
from http.server import ThreadingHTTPServer
import app


class Handler(app.Handler):
    def _session(self):
        return {"u": "test-admin"} if self.headers.get("Cookie") == "test-session" else None

    def log_message(self, *args):
        pass


class KeyManagementTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def request(self, path="/api/keys", body='{"name":"client"}', headers=None, method="POST"):
        opts = {"Cookie": "test-session", "Content-Type": "application/json", "X-Admin-Request": "1"}
        if headers:
            opts.update(headers)
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        try:
            connection.request(method, path, body=body, headers=opts)
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def test_requires_admin_session_not_api_key(self):
        with patch.object(app.key_management, "request") as upstream:
            for method in ("GET", "POST"):
                status, _ = self.request(headers={"Cookie": "", "Authorization": "Bearer ordinary-api-key"}, method=method)
                self.assertEqual(status, 401)
            upstream.assert_not_called()

    def test_rejects_untrusted_origin_and_missing_custom_header(self):
        for headers in ({"Origin": "https://unrelated.invalid"}, {"X-Admin-Request": ""}):
            with patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request(headers=headers)[0], 403)
                upstream.assert_not_called()

    def test_validates_body_size_type_and_ids(self):
        cases = [("/api/keys", " " * 8193, 413), ("/api/keys", "[]", 400), ("/api/keys", "null", 400), ("/api/keys", "broken", 400), ("/api/keys/delete", '{"id":"../../config"}', 400), ("/api/keys/update", '{"id":"legacy","sha256":"x"}', 400)]
        for path, body, expected in cases:
            with self.subTest(path=path, body=body[:40]), patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request(path, body)[0], expected)
                upstream.assert_not_called()
        self.assertEqual(self.request(headers={"Content-Type": "text/plain"})[0], 415)

    def test_forwards_only_authenticated_management_requests(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), patch.object(app.key_management, "request", return_value=(201, {"ok": True, "key": "synthetic-once"})) as upstream:
            code, result = self.request()
            self.assertEqual(code, 201)
            self.assertEqual(result["key"], "synthetic-once")
            upstream.assert_called_once_with("/tmp/test.sock", "POST", "/keys", {"name": "client"})

    def test_forwards_model_binding_and_rejects_bad_lists(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), patch.object(app.key_management, "request", return_value=(201, {"ok": True, "key": "synthetic"})) as upstream:
            code, _ = self.request(body=json.dumps({"name": "c", "models": ["cn:deepseek-v4.1-flash"]}))
            self.assertEqual(code, 201)
            upstream.assert_called_once_with("/tmp/test.sock", "POST", "/keys",
                                             {"name": "c", "models": ["cn:deepseek-v4.1-flash"]})
        bad_update = [
            '{"id":"legacy","models":"cn:deepseek-v4.1-flash"}',
            '{"id":"legacy","models":["a b"]}',
            '{"id":"legacy","models":["dup","dup"]}',
            '{"id":"legacy","models":["' + "x" * 65 + '"]}',
        ]
        for body in bad_update:
            with self.subTest(body=body[:40]), patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request("/api/keys/update", body)[0], 400)
                upstream.assert_not_called()

    def test_usage_requires_admin_session(self):
        with patch.object(app.key_management, "request") as upstream:
            status, _ = self.request("/api/usage", body=None,
                                     headers={"Cookie": "", "Authorization": "Bearer ordinary-api-key"},
                                     method="GET")
            self.assertEqual(status, 401)
            upstream.assert_not_called()

    def test_usage_proxies_management_socket(self):
        payload = {"ok": True, "totals": {"requests": 3, "total_tokens": 120},
                   "keys": [{"key_id": "k1", "name": "团队 A", "masked_key": "wb2a_ab…cd",
                             "totals": {"requests": 3, "total_tokens": 120},
                             "models": [{"model": "cn:deepseek-v4.1-flash",
                                         "totals": {"requests": 3, "total_tokens": 120}}]}],
                   "since": "2026-09-17T00:00:00Z", "updated_at": "2026-09-17T01:00:00Z",
                   "file": "/opt/workbuddy2api/data/usage.json"}
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request", return_value=(200, payload)) as upstream:
            code, result = self.request("/api/usage", body=None, method="GET")
            self.assertEqual(code, 200)
            self.assertTrue(result["ok"])
            self.assertEqual(result["keys"][0]["name"], "团队 A")
            upstream.assert_called_once_with("/tmp/test.sock", "GET", "/usage", None)

    def test_usage_reports_disabled_ledger(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request", return_value=(200, {"ok": False, "message": "用量账本未启用"})):
            code, result = self.request("/api/usage", body=None, method="GET")
            self.assertEqual(code, 200)
            self.assertFalse(result["ok"])
            self.assertIn("未启用", result["message"])

    def test_parse_request_log_keeps_key_column_and_warnings(self):
        text = "\n".join([
            "2026/09/17 22:04:21 INFO boot",
            "| #098 | 22:04:21 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | TTFB=3414ms | tok=110 | 34.3tok/s | total=3.4s |",
            "| #099 | 22:04:35 | cn:deepseek | stream | 200 | key=- | uid=fbede7cd | TTFB=2749ms | tok=161 | 49.5tok/s | total=3.3s |",
            "2026/09/17 22:05:00 WARN: [upstream] chat_stream uid=1e04e34d: upstream 400 channel_rejected",
            "| #100 | 22:05:01 | cn:deepseek | sync | 502 | key=脚本机 | uid=4e183777 | TTFB=- | tok=- | -tok/s | total=0.2s |",
        ])
        rows, other = app.parse_request_log(text)
        self.assertEqual(len(rows), 3)
        self.assertEqual(rows[0]["seq"], "098")
        self.assertEqual(rows[0]["key"], "团队 A")
        self.assertEqual(rows[0]["model"], "global:deep")
        self.assertEqual(rows[0]["status"], "200")
        self.assertEqual(rows[0]["uid"], "1e04e34d")
        self.assertEqual(rows[0]["tok"], "110")
        self.assertEqual(rows[0]["total"], "3.4s")
        self.assertEqual(rows[1]["key"], "-")
        self.assertEqual(rows[2]["status"], "502")
        self.assertEqual(len(other), 2)
        self.assertIn("channel_rejected", other[-1])

    def test_parse_request_log_accepts_legacy_rows_without_key(self):
        rows, other = app.parse_request_log(
            "| #001 | 10:00:00 | deepseek | stream | 200 | uid=00e26541 | TTFB=100ms | tok=5 | 50.0tok/s | total=0.1s |")
        self.assertEqual(len(rows), 1)
        # 旧格式没有 key= 列：归一化成 "-"，前端按同一列渲染。
        self.assertEqual(rows[0]["key"], "-")
        self.assertEqual(other, [])


if __name__ == "__main__":
    unittest.main()
