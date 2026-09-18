#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-19：锁定积分查询的非阻塞语义：/api/state 不再同步等 `./credit`
#             （冷跑实测 8.3 秒），首屏必须立刻拿到状态，积分稍后补。

import time
import unittest
from unittest.mock import patch

import app


class CreditLatencyTests(unittest.TestCase):
    def setUp(self):
        # 每个用例从干净缓存开始；后台线程不残留。
        app._credit_cache["ts"] = 0.0
        app._credit_cache["data"] = None
        with app._lock:
            app._credit_refreshing = False

    def tearDown(self):
        # 等后台刷新线程收尾，避免污染下一个用例。
        for _ in range(100):
            with app._lock:
                if not app._credit_refreshing:
                    break
            time.sleep(0.02)
        with app._lock:
            app._credit_refreshing = False
        app._credit_cache["ts"] = 0.0
        app._credit_cache["data"] = None

    def test_cold_state_does_not_wait_for_credit_query(self):
        """冷缓存时 get_credits 必须立刻返回 pending，把 8 秒查询甩到后台。"""
        started = time.monotonic()
        with patch.object(app, "_query_credits", side_effect=lambda: time.sleep(3) or {"accounts": []}):
            result = app.get_credits(force=False)
        elapsed = time.monotonic() - started
        self.assertLess(elapsed, 0.5, "冷缓存时不该等积分查询（实测 8.3 秒）")
        self.assertTrue(result.get("pending"), "冷缓存应返回 pending 占位: %r" % result)

    def test_stale_cache_returns_old_value_immediately(self):
        """缓存过期时先给旧值（页面数字不跳空），后台再换新。"""
        app._credit_cache["data"] = {"accounts": [{"uid": "u1", "remain": 7}], "total": {"remain": 7}}
        app._credit_cache["ts"] = time.time() - app.CREDIT_TTL - 10
        started = time.monotonic()
        with patch.object(app, "_query_credits", side_effect=lambda: time.sleep(3) or {"accounts": []}):
            result = app.get_credits(force=False)
        elapsed = time.monotonic() - started
        self.assertLess(elapsed, 0.5, "过期缓存也应立即返回")
        self.assertEqual(result["total"]["remain"], 7, "过期缓存要先把旧值给页面")

    def test_force_refresh_still_waits_for_fresh_data(self):
        """用户显式刷新（force）必须同步拿到新值，不能返回 pending。"""
        payload = {"accounts": [{"uid": "u1", "remain": 42}], "total": {"remain": 42}}
        with patch.object(app, "_query_credits", return_value=payload):
            result = app.get_credits(force=True)
        self.assertFalse(result.get("pending"), "force 刷新不该返回 pending")
        self.assertEqual(result["total"]["remain"], 42)

    def test_fresh_cache_is_served_without_querying(self):
        """TTL 内的缓存直接命中，不再起后台查询。"""
        payload = {"accounts": [{"uid": "u1", "remain": 5}], "total": {"remain": 5}}
        app._credit_cache["data"] = payload
        app._credit_cache["ts"] = time.time()
        with patch.object(app, "_query_credits") as query:
            result = app.get_credits(force=False)
        self.assertEqual(result, payload)
        query.assert_not_called()

    def test_build_state_surfaces_pending_flag(self):
        """build_state 要把「积分还在后台查」告诉前端，前端才会稍后补取。"""
        with patch.object(app, "gateway_get", return_value={}), \
             patch.object(app, "list_auth_files", return_value=[]), \
             patch.object(app, "container_running", return_value=True), \
             patch.object(app.key_management, "socket_path", return_value=None), \
             patch.object(app, "credits_by_uid", return_value=({}, {"pending": True, "accounts": []})):
            state = app.build_state(force_credit=False)
        self.assertTrue(state.get("creditPending"), "build_state 必须透出 creditPending")

    def test_build_state_reports_no_pending_with_data(self):
        with patch.object(app, "gateway_get", return_value={}), \
             patch.object(app, "list_auth_files", return_value=[]), \
             patch.object(app, "container_running", return_value=True), \
             patch.object(app.key_management, "socket_path", return_value=None), \
             patch.object(app, "credits_by_uid",
                          return_value=({}, {"accounts": [], "total": {"remain": 1}})):
            state = app.build_state(force_credit=False)
        self.assertFalse(state.get("creditPending"), "拿到数据后不该再标 pending")


if __name__ == "__main__":
    unittest.main()
