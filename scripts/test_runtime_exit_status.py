#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-18：以离线替身验证任务执行和状态读取失败会产生非零退出码，避免调度器把失败当成功。
import contextlib
import io
import sys
import unittest
from unittest.mock import patch

import growth_center
import task_runner


def exit_code(main):
    with contextlib.redirect_stdout(io.StringIO()):
        try:
            return main() or 0
        except SystemExit as exc:
            return exc.code or 0


class RuntimeExitStatusTests(unittest.TestCase):
    def test_task_failure_has_nonzero_exit(self):
        def fail_account(auth, options, stats):
            stats["fail"] += 1

        with patch.object(sys, "argv", ["task_runner.py", "fixture"]), patch.object(task_runner.tc, "load_auth", return_value={"uid": "fixture", "realm": "cn"}), patch.object(task_runner, "process_account", side_effect=fail_account):
            self.assertNotEqual(exit_code(task_runner.main), 0)

    def test_missing_account_has_nonzero_exit(self):
        with patch.object(sys, "argv", ["task_runner.py", "missing"]), patch.object(task_runner.tc, "load_auth", side_effect=SystemExit("fixture missing")):
            self.assertNotEqual(exit_code(task_runner.main), 0)

    def test_growth_status_failure_has_nonzero_exit(self):
        with patch.object(sys, "argv", ["growth_center.py", "fixture", "--redeem-only"]), patch.object(growth_center, "collect_accounts", return_value=[{"uid": "fixture"}]), patch.object(growth_center, "fetch_streak_data", return_value=(None, "fixture unavailable")):
            self.assertNotEqual(exit_code(growth_center.main), 0)

    def test_normal_skip_stays_successful(self):
        with patch.object(sys, "argv", ["task_runner.py", "fixture"]), patch.object(task_runner.tc, "load_auth", return_value={"uid": "fixture", "realm": "global"}):
            self.assertEqual(exit_code(task_runner.main), 0)


if __name__ == "__main__":
    unittest.main()
