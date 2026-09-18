#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-18：运行真实用量页函数核对日期筛选口径，防止按比例虚构模型明细或把历史总量显示成当日数据。
# 2026-09-18：显式按 UTF-8 读取 Node 输出，保证 Windows 默认中文代码页下也能执行回归。

import json
from pathlib import Path
import shutil
import subprocess
import unittest


@unittest.skipUnless(shutil.which("node"), "Node.js is required for browser-function checks")
class UsageIntegrityTests(unittest.TestCase):
    def run_usage(self, expression):
        source = Path(__file__).with_name("usage.js")
        program = """
const vm = require('vm'); const fs = require('fs');
const nodes = {usageFilter:{innerHTML:''}};
const context = {document:{addEventListener(){}}, Date, console,
  $:s=>nodes[s.slice(1)] || null, esc:x=>String(x), exactTokens:x=>String(x)};
vm.createContext(context); vm.runInContext(fs.readFileSync(process.argv[1], 'utf8'), context);
console.log(JSON.stringify(vm.runInContext(process.argv[2], context)));
"""
        result = subprocess.run([shutil.which("node"), "-e", program, str(source), expression],
                                capture_output=True, text=True, encoding="utf-8", timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_date_filter_does_not_invent_model_breakdowns(self):
        result = self.run_usage("""US.from='2026-09-17';US.to='2026-09-17';
usageModelsFor({totals:{total_tokens:200},days:[{day:'2026-09-17',totals:{total_tokens:100}}],
models:[{model:'a',totals:{total_tokens:100}},{model:'b',totals:{total_tokens:100}}]})""")
        self.assertEqual(result, [], "daily model data cannot be recovered by proportional scaling")

    def test_no_daily_history_means_no_data_for_selected_day(self):
        result = self.run_usage("US.from='2026-09-17';US.to='2026-09-17';usageTotalsForScope({totals:{requests:200,total_tokens:9999},days:[]})")
        self.assertEqual(result.get("requests"), 0)
        self.assertEqual(result.get("total_tokens"), 0)

    def test_all_time_button_uses_all_time_total(self):
        result = self.run_usage("renderUsageFilter([{day:'2026-09-17',totals:{requests:5}}],200);$('#usageFilter').innerHTML")
        self.assertIn("全部<i>200 次</i>", result)

    def test_valid_daily_and_unfiltered_totals_remain_exact(self):
        result = self.run_usage("""var item={totals:{requests:200},days:[{day:'2026-09-17',totals:{requests:3}},{day:'2026-09-18',totals:{requests:5}}]};
US.from='2026-09-17';US.to='2026-09-17';var a=usageTotalsFor(item).requests;US.from='';US.to='';[a,usageTotalsFor(item).requests]""")
        self.assertEqual(result, [3, 200])


if __name__ == "__main__":
    unittest.main()
