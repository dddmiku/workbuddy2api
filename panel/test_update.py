#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-17：覆盖热更新通道：状态只读接口的登录保护、升级动作的同源闸门、
#             字段白名单（只允许 tag）与网关不可达时的降级提示。

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


STATUS = {"enabled": True, "current": "1.3.0", "commit": "abcdef1234567890",
          "state": "idle", "latest_tag": "v1.3.1", "update_ready": True,
          "asset_name": "wb2api-linux-amd64", "asset_size": 12345678,
          "checked_at": "2026-09-17T02:00:00Z", "inherited_fd": True,
          "repo": "dddmiku/workbuddy2api", "dir": "/opt/workbuddy2api/data/updates",
          "built_at": "2026-09-17T01:00:00Z", "last_error": ""}


class UpdateChannelTests(unittest.TestCase):
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

    def request(self, path, body=None, headers=None, method="POST"):
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

    def test_status_requires_admin_session(self):
        with patch.object(app.key_management, "request") as upstream:
            status, _ = self.request("/api/update", body=None, method="GET",
                                     headers={"Cookie": "", "Authorization": "Bearer ordinary-api-key"})
            self.assertEqual(status, 401)
            upstream.assert_not_called()

    def test_status_proxies_management_socket(self):
        payload = {"ok": True, "status": STATUS}
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request", return_value=(200, payload)) as upstream:
            code, result = self.request("/api/update", body=None, method="GET")
            self.assertEqual(code, 200)
            self.assertEqual(result["status"]["current"], "1.3.0")
            upstream.assert_called_once_with("/tmp/test.sock", "GET", "/update", None)

    def test_status_reports_unreachable_gateway(self):
        with patch.object(app.key_management, "socket_path", side_effect=OSError("missing")):
            code, result = self.request("/api/update", body=None, method="GET")
            self.assertEqual(code, 200)
            self.assertFalse(result["ok"])
            self.assertIn("无法读取", result["message"])

    def test_apply_requires_session_and_same_origin(self):
        with patch.object(app.key_management, "request") as upstream:
            self.assertEqual(self.request("/api/update/apply", body="{}",
                                          headers={"Cookie": ""})[0], 401)
            for headers in ({"Origin": "https://unrelated.invalid"}, {"X-Admin-Request": ""}):
                self.assertEqual(self.request("/api/update/apply", body="{}", headers=headers)[0], 403)
            upstream.assert_not_called()

    def test_apply_forwards_to_management_socket(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request",
                             return_value=(200, {"ok": True, "status": STATUS})) as upstream:
            code, result = self.request("/api/update/apply", body="{}")
            self.assertEqual(code, 200)
            self.assertTrue(result["ok"])
            upstream.assert_called_once_with("/tmp/test.sock", "POST", "/update/apply", {})

    def test_apply_accepts_only_tag_field(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request",
                             return_value=(200, {"ok": True})) as upstream:
            code, _ = self.request("/api/update/apply", body=json.dumps({"tag": "v1.3.1"}))
            self.assertEqual(code, 200)
            upstream.assert_called_once_with("/tmp/test.sock", "POST", "/update/apply", {"tag": "v1.3.1"})
        bad = ['{"tag":"../etc/passwd"}', '{"tag":"' + "v" * 65 + '"}', '{"tag":1}', '{"force":true}']
        for body in bad:
            with self.subTest(body=body), patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request("/api/update/apply", body=body)[0], 400)
                upstream.assert_not_called()

    def test_check_forwards_to_management_socket(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request",
                             return_value=(200, {"ok": True, "status": STATUS})) as upstream:
            code, _ = self.request("/api/update/check", body="{}")
            self.assertEqual(code, 200)
            upstream.assert_called_once_with("/tmp/test.sock", "POST", "/update/check", {})

    def test_same_origin_is_required_for_key_writes_too(self):
        """同源助手同时服务密钥与更新两条通道，行为必须一致。"""
        with patch.object(app.key_management, "request") as upstream:
            self.assertEqual(self.request("/api/keys", body='{"name":"c"}',
                                          headers={"Origin": "http://127.0.0.1:1"})[0], 403)
            upstream.assert_not_called()


if __name__ == "__main__":
    unittest.main()
