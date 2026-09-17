#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-17：锁定表格数值列对齐：表头用 th.r、数据用 td.num，两边都要右对齐，
#             否则会出现「表头在右、数字在左」的错位（用户实测反馈）。
# 2026-09-17：锁定启动顺序：拼接脚本里只能有一处 init()，且必须等全部段执行完。
# 2026-09-17：锁定首屏健壮性：直接打开 /#system 时数据还没到，渲染不能抛异常。

import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
INDEX = os.path.join(HERE, "index.html")


def read_source(name):
    with open(os.path.join(HERE, "src", name), "r", encoding="utf-8") as fh:
        return fh.read()


class TableAlignmentTests(unittest.TestCase):
    def test_log_table_right_aligns_header_and_cells(self):
        css = read_source("logs.css")
        self.assertIn(".log-table td.num", css)
        self.assertIn(".log-table th.r", css)
        rule = [line for line in css.splitlines() if ".log-table td.num" in line][0]
        self.assertIn("text-align:right", rule)

    def test_usage_table_right_aligns_header_and_cells(self):
        css = read_source("usage.css")
        self.assertIn(".usage-table td.num", css)
        self.assertIn(".usage-table th.r", css)
        rule = [line for line in css.splitlines() if ".usage-table td.num" in line][0]
        self.assertIn("text-align:right", rule)

    def test_numeric_headers_use_right_class(self):
        body = read_source("body.html")
        # 日志页的 token 列：输入 / 缓存命中 / 输出（tok 改名为输出，语义不变）。
        for header in ("TTFB", "输入", "缓存命中", "输出", "tok/s", "total"):
            with self.subTest(header=header):
                self.assertIn('<th class="r">%s</th>' % header, body)
        for header in ("请求", "输入 tokens", "缓存命中", "输出 tokens", "合计 tokens"):
            with self.subTest(header=header):
                self.assertIn('<th class="r">%s</th>' % header, body)

    def test_numeric_cells_use_num_class(self):
        with open(os.path.join(HERE, "app.js"), "r", encoding="utf-8") as fh:
            logs_js = fh.read()
        with open(os.path.join(HERE, "usage.js"), "r", encoding="utf-8") as fh:
            usage_js = fh.read()
        self.assertIn('<td class="mono num">', logs_js)
        self.assertIn('<td class="num">', usage_js)

    def test_built_index_contains_alignment_rules(self):
        # 生成的 index.html 才是真正被浏览器加载的文件：规则必须真的拼进去了。
        with open(INDEX, "r", encoding="utf-8") as fh:
            html = fh.read()
        self.assertIn(".log-table td.num,.log-table th.r{text-align:right}", html)
        self.assertIn(".usage-table td.num,.usage-table th.r{text-align:right}", html)
        self.assertIn('<th class="r">TTFB</th>', html)
        self.assertIn('<td class="mono num">', html)


class BootOrderTests(unittest.TestCase):
    """拼接脚本的启动顺序：keys.js 曾自己调 init()，在 usage.js 之前执行，
    导致从 #usage 进入时 US 未定义、页面永远停在加载态。"""

    def test_only_app_js_starts_the_app(self):
        for name in ("keys.js", "usage.js"):
            with self.subTest(name=name):
                with open(os.path.join(HERE, name), "r", encoding="utf-8") as fh:
                    lines = [line.strip() for line in fh]
                calls = [line for line in lines if line == "init();"]
                self.assertEqual(calls, [], "%s 不应自己调用 init()" % name)

    def test_app_js_defers_start_until_script_finishes(self):
        with open(os.path.join(HERE, "app.js"), "r", encoding="utf-8") as fh:
            js = fh.read()
        self.assertIn("document.addEventListener('DOMContentLoaded', init)", js)
        # 末尾必须是延迟启动，不能是裸 init()
        self.assertFalse(js.rstrip().endswith("init();"),
                         "app.js 末尾不能直接 init()：那时后续拼接段还没执行")

    def test_built_index_has_single_deferred_start(self):
        with open(INDEX, "r", encoding="utf-8") as fh:
            html = fh.read()
        body = html.split("<script>", 1)[-1]
        self.assertEqual(body.count("\ninit();"), 0,
                         "生成的页面里不应再有裸 init() 调用")
        self.assertIn("DOMContentLoaded", body)


class FirstPaintTests(unittest.TestCase):
    """首屏渲染不能依赖"数据已经拉回来"：面板允许带 #view 直接打开或刷新。"""

    def setUp(self):
        with open(os.path.join(HERE, "app.js"), "r", encoding="utf-8") as fh:
            self.js = fh.read()

    def test_render_system_tolerates_missing_state(self):
        start = self.js.index("function renderSystem(){")
        body = self.js[start:self.js.index("\n}", start)]
        self.assertIn("if (!d) return", body,
                      "renderSystem 必须先判空：否则直接打开 #system 会抛异常并中断 go()")

    def test_system_view_loads_update_card_before_rendering(self):
        start = self.js.index("function go(v){")
        body = self.js[start:self.js.index("\n}", start)]
        self.assertLess(body.index("loadUpdate()"), body.index("renderSystem()"),
                        "更新卡片不依赖 /api/state，必须在 renderSystem 之前触发")

    def test_built_index_keeps_system_guards(self):
        with open(INDEX, "r", encoding="utf-8") as fh:
            html = fh.read()
        self.assertIn("function renderSystem(){", html)
        self.assertIn("if (!d) return", html)
        self.assertIn("loadUpdate()", html)
        self.assertIn("#updRows", html)


if __name__ == "__main__":
    unittest.main()
