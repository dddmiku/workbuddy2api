#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-18：验证账号写入失败、启停冲突和快速重复删除时仍保留已有凭据与回收件。

import json
import itertools
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import app


class AccountStorageTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.auths = self.root / "auths"
        self.auths.mkdir()
        self.trash = self.root / "trash"
        self.uid = "a" * 32
        self.active = self.auths / ("workbuddy-" + self.uid + ".json")
        self.disabled = self.active.with_name(self.active.name + ".disabled")
        self.handler = app.Handler.__new__(app.Handler)
        for name, value in [("AUTHS_DIR", str(self.auths)), ("TRASH_DIR", str(self.trash))]:
            p = patch.object(app, name, value); p.start(); self.addCleanup(p.stop)

    def entry(self, path, disabled=False):
        return {"uid": self.uid, "path": str(path), "file": path.name, "disabled": disabled}

    def test_failed_new_login_keeps_disabled_credential(self):
        self.disabled.write_text("previous credential", encoding="utf-8")
        with patch.object(app.os, "replace", side_effect=OSError("injected persistence failure")), patch.object(app, "chown_app"):
            with self.assertRaises(OSError):
                app.write_auth_file({"uid": self.uid, "access_token": "fixture-only-token", "expires_in": 3600})
        self.assertTrue(self.disabled.exists(), "failed save deleted the previous disabled credential")
        self.assertEqual(self.disabled.read_text(encoding="utf-8"), "previous credential")

    def test_toggle_conflict_does_not_delete_other_credential(self):
        self.active.write_text("active credential", encoding="utf-8")
        self.disabled.write_text("different disabled credential", encoding="utf-8")
        with patch.object(app, "find_entry", return_value=self.entry(self.active)), \
                patch.object(app, "chown_app"), patch.object(app, "restart_container", return_value=(True, "ok", 0)) as restart:
            result = self.handler.account_toggle({"uid": self.uid, "disabled": True})
        self.assertFalse(result["ok"])
        self.assertEqual(self.active.read_text(encoding="utf-8"), "active credential")
        self.assertEqual(self.disabled.read_text(encoding="utf-8"), "different disabled credential")
        restart.assert_not_called()

    def test_same_second_deletes_keep_both_recovery_files(self):
        with patch.object(app, "find_entry", side_effect=lambda uid: self.entry(self.active)), \
                patch.object(app.time, "strftime", return_value="20260918-120000"), \
                patch.object(app, "restart_container", return_value=(True, "ok", 0)):
            for value in ("first credential", "second credential"):
                self.active.write_text(value, encoding="utf-8")
                self.assertTrue(self.handler.account_delete({"uid": self.uid})["ok"])
        self.assertEqual(sorted(p.read_text(encoding="utf-8") for p in self.trash.iterdir()),
                         ["first credential", "second credential"])

    def test_restart_does_not_report_unhealthy_service_as_success(self):
        for health in (None, {"error": {"message": "unavailable"}}):
            with self.subTest(health=health), patch.object(app, "docker", return_value=(0, "container", "")), \
                    patch.object(app, "gateway_get", return_value=health), patch.object(app.time, "sleep"), \
                    patch.object(app.time, "monotonic", side_effect=itertools.count()):
                ok, _, _ = app.restart_container()
                self.assertFalse(ok)

    def test_restart_accepts_a_real_healthy_service_response(self):
        with patch.object(app, "docker", return_value=(0, "container", "")), \
                patch.object(app, "gateway_get", return_value={"service": "workbuddy2api", "healthy": 0}), \
                patch.object(app.time, "sleep"):
            ok, _, _ = app.restart_container()
            self.assertTrue(ok)


if __name__ == "__main__":
    unittest.main()
