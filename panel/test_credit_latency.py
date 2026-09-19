#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-19：锁定积分查询的非阻塞语义，首屏先显示状态，积分稍后补齐。
# 2026-09-19：复现并锁定查询失败提示、失败重试间隔和并发刷新共享结果；用事件和 join 收尾，避免测试遗留后台线程。

import json
import threading
import time
import unittest
from unittest.mock import patch

import app


def credit_payload(remain):
    return {"accounts": [{"uid": "fixture", "remain": remain}], "total": {"remain": remain}}


class CreditLatencyTests(unittest.TestCase):
    def setUp(self):
        self.now = 1000.0
        self.threads = []
        self.releases = []
        self.patches = []
        real_thread = threading.Thread

        def track_thread(*args, **kwargs):
            thread = real_thread(*args, **kwargs)
            self.threads.append(thread)
            return thread

        self.patch(app.threading, "Thread", side_effect=track_thread)
        self.patch(app, "_credit_cache", {"ts": 0.0, "data": None, "error": None, "retry_at": 0.0})
        self.patch(app, "_credit_refreshing", None)
        self.patch(app.time, "time", side_effect=lambda: self.now)
        self.patch(app.time, "monotonic", side_effect=lambda: self.now)
        self.patch(app, "container_running", return_value=True)
        self.docker = self.patch(app, "docker", return_value=(0, json.dumps(credit_payload(42)), ""))

    def tearDown(self):
        # 保持依赖替身有效，先解除所有等待并 join，再恢复 app 的全局状态。
        try:
            for release in self.releases:
                release.set()
            self.join_queries()
        finally:
            for patcher in reversed(self.patches):
                patcher.stop()

    def patch(self, target, name, *args, **kwargs):
        patcher = patch.object(target, name, *args, **kwargs)
        self.patches.append(patcher)
        return patcher.start()

    def join_queries(self):
        for thread in self.threads:
            if thread.ident is not None:
                thread.join(timeout=5)
                self.assertFalse(thread.is_alive(), "测试未收尾的线程：" + thread.name)

    def blocked_query(self, remain=42):
        entered = threading.Event()
        release = threading.Event()
        self.releases.append(release)

        def query(*_args, **_kwargs):
            entered.set()
            if not release.wait(timeout=5):
                raise RuntimeError("积分测试未释放查询")
            return 0, json.dumps(credit_payload(remain)), ""

        self.docker.side_effect = query
        return entered, release

    def start_force(self, name="credit-force-test"):
        result = {}
        done = threading.Event()

        def caller():
            try:
                result["data"] = app.get_credits(force=True)
            except Exception as error:
                result["error"] = error
            finally:
                done.set()

        thread = threading.Thread(target=caller, name=name)
        thread.start()
        return result, done

    def state(self):
        with patch.object(app, "gateway_get", return_value={}), \
             patch.object(app, "list_auth_files", return_value=[]), \
             patch.object(app.key_management, "socket_path", return_value=None):
            return app.build_state(force_credit=False)

    def test_cold_state_does_not_wait_for_credit_query(self):
        entered, release = self.blocked_query()
        started = time.perf_counter()
        result = app.get_credits()
        self.assertLess(time.perf_counter() - started, 0.5)
        self.assertTrue(result.get("pending"))
        self.assertTrue(entered.wait(timeout=5))
        release.set()
        self.join_queries()
        self.assertEqual(app.get_credits()["total"]["remain"], 42)
        self.assertEqual(self.docker.call_count, 1)

    def test_stale_cache_is_marked_pending_until_replacement_arrives(self):
        app._credit_cache.update(ts=self.now - app.CREDIT_TTL - 1, data=credit_payload(7))
        entered, release = self.blocked_query()
        result = app.get_credits()
        self.assertEqual(result["total"]["remain"], 7)
        self.assertTrue(result.get("pending"), "返回旧积分时也要通知页面稍后补取新值")
        self.assertTrue(entered.wait(timeout=5))
        release.set()
        self.join_queries()
        self.assertEqual(app.get_credits()["total"]["remain"], 42)

    def test_force_refresh_waits_for_fresh_data(self):
        entered, release = self.blocked_query()
        result, done = self.start_force()
        self.assertTrue(entered.wait(timeout=5))
        self.assertFalse(done.is_set())
        release.set()
        self.join_queries()
        self.assertNotIn("error", result)
        self.assertFalse(result["data"].get("pending"))
        self.assertEqual(result["data"]["total"]["remain"], 42)

    def test_fresh_cache_is_served_without_querying(self):
        app._credit_cache.update(ts=self.now, data=credit_payload(5))
        result = app.get_credits()
        self.assertEqual(result["total"]["remain"], 5)
        self.assertFalse(result.get("pending"))
        self.docker.assert_not_called()

    def test_failed_background_query_stops_pending_and_surfaces_error(self):
        self.docker.return_value = (1, "", "fixture credit failure")
        app.get_credits()
        self.join_queries()
        state = self.state()
        self.assertFalse(state["creditPending"])
        self.assertEqual(state["creditError"], "fixture credit failure")
        self.assertEqual(self.docker.call_count, 1)

    def test_failed_background_query_preserves_last_successful_balance(self):
        app._credit_cache.update(ts=self.now - app.CREDIT_TTL - 1, data=credit_payload(7))
        self.docker.return_value = (1, "", "fixture credit failure")
        app.get_credits()
        self.join_queries()
        result = app.get_credits()
        self.assertEqual(result["total"]["remain"], 7)
        self.assertEqual(result.get("error"), "fixture credit failure")
        self.assertFalse(result.get("pending"))
        self.assertEqual(self.docker.call_count, 1)

    def test_failed_query_retries_after_interval_and_clears_error_on_success(self):
        self.docker.return_value = (1, "", "fixture credit failure")
        app.get_credits()
        self.join_queries()
        self.now += 29
        waiting = app.get_credits()
        self.assertFalse(waiting.get("pending"))
        self.assertEqual(self.docker.call_count, 1)
        self.docker.return_value = (0, json.dumps(credit_payload(42)), "")
        self.now += 2
        self.assertTrue(app.get_credits().get("pending"))
        self.join_queries()
        ready = app.get_credits()
        self.assertFalse(ready.get("pending"))
        self.assertFalse(ready.get("error"))
        self.assertEqual(ready["total"]["remain"], 42)
        self.assertEqual(self.docker.call_count, 2)

    def test_manual_refresh_can_retry_during_failure_backoff(self):
        self.docker.return_value = (1, "", "fixture credit failure")
        first = app.get_credits(force=True)
        self.assertEqual(first.get("error"), "fixture credit failure")
        self.docker.return_value = (0, json.dumps(credit_payload(42)), "")
        second = app.get_credits(force=True)
        self.assertEqual(second["total"]["remain"], 42)
        self.assertFalse(second.get("error"))
        self.assertEqual(self.docker.call_count, 2)

    def test_manual_refresh_joins_background_query_without_overwriting_new_data(self):
        entered, release = threading.Event(), threading.Event()
        force_reached_query = threading.Event()
        self.releases.append(release)
        calls = []
        calls_lock = threading.Lock()

        def query(*_args, **_kwargs):
            with calls_lock:
                calls.append(threading.current_thread().name)
                first = len(calls) == 1
            if first:
                entered.set()
                if not release.wait(timeout=5):
                    raise RuntimeError("积分测试未释放查询")
                remain = 7
            else:
                force_reached_query.set()
                remain = 42
            return 0, json.dumps(credit_payload(remain)), ""

        self.docker.side_effect = query
        original_start = app._start_credit_refresh_locked

        def observe_join():
            refresh = original_start()
            if threading.current_thread().name == "credit-force-test":
                force_reached_query.set()
            return refresh

        self.patch(app, "_start_credit_refresh_locked", side_effect=observe_join)
        app.get_credits()
        self.assertTrue(entered.wait(timeout=5))
        forced, _done = self.start_force()
        self.assertTrue(force_reached_query.wait(timeout=5))
        release.set()
        self.join_queries()
        self.assertNotIn("error", forced)
        self.assertEqual(len(calls), 1, "手动刷新必须加入正在执行的查询")
        self.assertEqual(forced["data"]["total"], app.get_credits()["total"])
        self.assertFalse(forced["data"].get("pending"))

    def test_background_exception_is_visible_and_does_not_retry_immediately(self):
        self.docker.side_effect = RuntimeError("fixture unexpected failure")
        app.get_credits()
        self.join_queries()
        result = app.get_credits()
        self.assertFalse(result.get("pending"))
        self.assertIn("fixture unexpected failure", result.get("error") or "")
        self.assertEqual(self.docker.call_count, 1)

    def test_invalid_credit_payload_does_not_replace_successful_cache(self):
        for raw in ("invalid-json", "null", "[]", '{"accounts":[null]}'):
            with self.subTest(raw=raw):
                app._credit_cache.update(ts=self.now, data=credit_payload(7))
                self.docker.return_value = (0, raw, "")
                result = app.get_credits(force=True)
                self.assertTrue(result.get("error"))
                self.assertEqual(result["total"]["remain"], 7)

    def test_build_state_surfaces_pending_flag(self):
        entered, release = self.blocked_query()
        state = self.state()
        self.assertTrue(state["creditPending"])
        self.assertTrue(entered.wait(timeout=5))
        release.set()
        self.join_queries()
        self.assertFalse(self.state()["creditPending"])


if __name__ == "__main__":
    unittest.main()
