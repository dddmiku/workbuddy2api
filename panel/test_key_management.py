#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-16：验证密钥管理的管理员登录、请求来源、大小和字段限制，使用本地服务与合成会话。
# 2026-09-17：覆盖模型绑定字段的透传与非法输入的本地拒绝。

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


if __name__ == "__main__":
    unittest.main()
