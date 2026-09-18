#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-18：通过隔离 HTTP 服务复现管理写操作来源校验、JSON 输入、退出续期和凭证损坏边界。

import http.client
import http.cookiejar
import json
from pathlib import Path
import tempfile
import threading
import unittest
import urllib.request
from http.cookies import SimpleCookie
from http.server import ThreadingHTTPServer
from unittest.mock import patch

import app


class QuietServer(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        pass


class SessionHandler(app.Handler):
    def _session(self, *args, **kwargs):
        return {"u": "fixture-admin"} if self.headers.get("Cookie") == "fixture-session" else None

    def log_message(self, *args):
        pass


class RealSessionHandler(app.Handler):
    def log_message(self, *args):
        pass


class ManagementBoundaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = QuietServer(("127.0.0.1", 0), SessionHandler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join(timeout=3)

    def request(self, path, body="{}", headers=None):
        values = {"Cookie": "fixture-session", "Content-Type": "application/json", "X-Admin-Request": "1"}
        values.update(headers or {})
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=1)
        try:
            connection.request("POST", path, body=body, headers=values)
            response = connection.getresponse()
            data = response.read()
            return response.status, data
        except (OSError, http.client.HTTPException) as error:
            return "connection_failed", type(error).__name__
        finally:
            connection.close()

    def test_all_mutating_routes_require_same_origin_and_management_header(self):
        routes = {
            "/api/service/restart": "service_restart",
            "/api/account/delete": "account_delete",
            "/api/account/toggle": "account_toggle",
            "/api/task/toggle": "task_toggle",
            "/api/task/run": "task_run",
            "/api/login/start": "login_start",
            "/api/login/poll": "login_poll",
        }
        for route, method in routes.items():
            for headers in ({"Origin": "https://untrusted.invalid"}, {"X-Admin-Request": ""}):
                with self.subTest(route=route, headers=headers), patch.object(app.Handler, method, return_value={"ok": True}) as action:
                    code, _ = self.request(route, headers=headers)
                    self.assertEqual(code, 403)
                    action.assert_not_called()

    def test_invalid_json_never_triggers_restart(self):
        for body in ("not json", "null", "[]", "123", '"text"'):
            with self.subTest(body=body), patch.object(app.Handler, "service_restart", return_value={"ok": True}) as action:
                self.assertEqual(self.request("/api/service/restart", body=body)[0], 400)
                action.assert_not_called()

    def test_body_framing_and_type_are_bounded_before_action(self):
        cases = [({"Content-Length": "invalid"}, 400), ({"Content-Length": "-1"}, 400),
                 ({"Content-Length": "9999999"}, 413), ({"Content-Type": "text/plain"}, 415)]
        for headers, expected in cases:
            with self.subTest(headers=headers), patch.object(app.Handler, "service_restart", return_value={"ok": True}) as action:
                self.assertEqual(self.request("/api/service/restart", headers=headers)[0], expected)
                action.assert_not_called()

    def test_toggle_requires_explicit_boolean_and_valid_identifier(self):
        cases = [("/api/account/toggle", "account_toggle", {"uid": "a" * 32, "disabled": "false"}),
                 ("/api/account/toggle", "account_toggle", {"uid": "a" * 32}),
                 ("/api/task/toggle", "task_toggle", {"key": "checkin", "enabled": "false"}),
                 ("/api/task/toggle", "task_toggle", {"key": [], "enabled": True})]
        for route, method, payload in cases:
            with self.subTest(route=route, payload=payload), patch.object(app.Handler, method, return_value={"ok": True}) as action:
                self.assertEqual(self.request(route, body=json.dumps(payload))[0], 400)
                action.assert_not_called()

    def test_logs_keep_stderr_when_stdout_is_present(self):
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        try:
            with patch.object(app, "docker", return_value=(0, "ordinary request output", "WARN: diagnostic on stderr")):
                connection.request("GET", "/api/logs", headers={"Cookie": "fixture-session"})
                response = connection.getresponse()
                data = json.loads(response.read())
            self.assertIn("ordinary request output", data["logs"])
            self.assertIn("diagnostic on stderr", data["logs"])
        finally:
            connection.close()


class SessionBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "credentials.json"
        self.doc = {"version": 1, "username": "fixture-admin", "password": app.hash_password("fixture-password-123"),
                    "sessionKey": "12" * 32, "updatedAt": 1, "history": []}
        self.path.write_text(json.dumps(self.doc), encoding="utf-8")
        for name, value in [("AUTH_DIR", self.directory.name), ("CRED_PATH", str(self.path)),
                            ("HTPASSWD_PATH", str(Path(self.directory.name) / "missing-htpasswd")),
                            ("_revoked", set()), ("_fails", {})]:
            patcher = patch.object(app, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.server = QuietServer(("127.0.0.1", 0), RealSessionHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.close_server)

    def close_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)

    def logout(self, token):
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        try:
            connection.request("POST", "/api/auth/logout", body="{}",
                               headers={"Cookie": app.COOKIE_NAME + "=" + token,
                                        "Content-Type": "application/json", "X-Admin-Request": "1"})
            response = connection.getresponse()
            result = response.status, response.getheaders(), response.read()
            return result
        finally:
            connection.close()

    def test_root_deployment_login_cookie_authenticates_next_request(self):
        jar = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
        base = "http://%s:%s" % self.server.server_address
        login = urllib.request.Request(base + "/api/auth/login", data=json.dumps(
            {"username": "fixture-admin", "password": "fixture-password-123"}).encode(),
            headers={"Content-Type": "application/json", "X-Admin-Request": "1"})
        with opener.open(login, timeout=2) as response:
            self.assertTrue(json.load(response)["ok"])
        try:
            with opener.open(base + "/api/session", timeout=2) as response:
                code = response.status
        except urllib.error.HTTPError as error:
            code = error.code
        self.assertEqual(code, 200)

    def test_logout_does_not_send_a_fresh_sliding_session(self):
        token, _ = app.issue_session("fixture-admin", ttl=120)
        status, headers, _ = self.logout(token)
        self.assertEqual(status, 200)
        cookies = [value for name, value in headers if name.lower() == "set-cookie"]
        self.assertTrue(cookies)
        self.assertTrue(all(value.startswith(app.COOKIE_NAME + "=;") for value in cookies), "logout issued a fresh session")
        self.assertIsNone(app.read_session(token))

    def test_logout_revocation_survives_process_memory_reset(self):
        token, _ = app.issue_session("fixture-admin")
        self.assertEqual(self.logout(token)[0], 200)
        app._revoked.clear()
        self.assertIsNone(app.read_session(token))

    def test_logout_revokes_copies_from_before_sliding_renewal(self):
        original, _ = app.issue_session("fixture-admin", ttl=120)
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        try:
            connection.request("GET", "/api/session", headers={"Cookie": app.COOKIE_NAME + "=" + original})
            response = connection.getresponse()
            response.read()
            renewed = ""
            for name, value in response.getheaders():
                if name.lower() == "set-cookie":
                    cookie = SimpleCookie(value)
                    if cookie.get(app.COOKIE_NAME) and cookie[app.COOKIE_NAME].value:
                        renewed = cookie[app.COOKIE_NAME].value
        finally:
            connection.close()
        self.assertTrue(renewed)
        self.assertEqual(self.logout(renewed)[0], 200)
        app._revoked.clear()
        self.assertIsNone(app.read_session(original))
        self.assertIsNone(app.read_session(renewed))

    def test_corrupt_credentials_are_not_replaced_by_new_login(self):
        for original in (b'{"username":"fixture-admin",broken', b"null", b"[]", b"{}"):
            with self.subTest(document_type=original[:12]):
                self.path.write_bytes(original)
                with self.assertRaises((OSError, ValueError)):
                    app.load_credentials()
                self.assertEqual(self.path.read_bytes(), original)


if __name__ == "__main__":
    unittest.main()
