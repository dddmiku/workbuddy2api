#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-19：合入两张长表格的固定表头和窄屏横向滚动支持。
# 2026-09-16：加入密钥管理页的样式与行为源码，继续生成可直接部署的单文件控制台。
# 2026-09-17：加入用量统计页的样式与行为源码。
# 2026-09-17：加入「版本与热更新」卡片的样式与行为源码（update.css / update.js）。
"""把 src/ 里的样式与结构、app.js 拼成单文件 index.html。

index.html 由本脚本生成，改版式请改 src/*.css 与 src/body.html，
改行为请改 app.js，然后跑一次：

    python build.py
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "src")
OUT = os.path.join(HERE, "index.html")

HEAD = """<!DOCTYPE html>
<html lang="zh-CN" data-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="light dark">
<title>workbuddy2api · 控制台</title>
<style>
"""

TAIL_CSS = """</style>
</head>
<body>
"""


def read(name):
    with open(os.path.join(SRC, name), "r", encoding="utf-8") as fh:
        return fh.read()


def main():
    css = "".join(read(n) for n in ("css_a.css", "css_b.css", "css_c.css", "keys.css",
                                    "usage.css", "logs.css", "update.css", "table_headers.css"))
    body = read("body.html")
    with open(os.path.join(HERE, "app.js"), "r", encoding="utf-8") as fh:
        js = fh.read()
    with open(os.path.join(HERE, "keys.js"), "r", encoding="utf-8") as fh:
        js += "\n" + fh.read()
    with open(os.path.join(HERE, "usage.js"), "r", encoding="utf-8") as fh:
        js += "\n" + fh.read()
    with open(os.path.join(HERE, "update.js"), "r", encoding="utf-8") as fh:
        js += "\n" + fh.read()
    with open(os.path.join(HERE, "table_headers.js"), "r", encoding="utf-8") as fh:
        js += "\n" + fh.read()
    if "/*__APP_JS__*/" not in body:
        sys.stderr.write("body.html 缺少 /*__APP_JS__*/ 占位符\n")
        return 1
    html = HEAD + css + TAIL_CSS + body.replace("/*__APP_JS__*/", js)
    tmp = OUT + ".tmp"
    with open(tmp, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(html)
    os.replace(tmp, OUT)
    print("index.html 已生成：%d 字节" % len(html.encode("utf-8")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
