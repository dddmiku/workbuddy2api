#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-16：通过权限受限的本机 Unix socket 调用网关管理接口，避免使用可分发的调用密钥作为管理凭证。
"""Private transport shared by the authenticated panel and gateway."""

import http.client
import json
import os
import socket


def socket_path(config_path, base):
    with open(config_path, "r", encoding="utf-8") as file:
        config = json.load(file)
    registry = config.get("api_keys_file")
    if not registry:
        return None
    path = config.get("api_keys_socket") or os.path.join(os.path.dirname(registry), "api_keys.sock")
    if path.startswith("/app/"):
        path = os.path.join(base, path[5:])
    return path if os.path.isabs(path) else os.path.normpath(os.path.join(base, path))


def request(path, method, endpoint, body=None, timeout=15):
    if not path:
        return 503, {"ok": False, "message": "密钥管理尚未启用"}
    connection = UnixConnection(path, timeout)
    try:
        data = None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8")
        connection.request(method, endpoint, body=data, headers={"Content-Type": "application/json"})
        response = connection.getresponse()
        raw = response.read((1 << 20) + 1)
        if len(raw) > 1 << 20:
            raise ValueError("management response exceeds limit")
        result = json.loads(raw.decode("utf-8"))
        if not isinstance(result, dict):
            raise ValueError("invalid management response")
        return response.status, result
    except (OSError, http.client.HTTPException, ValueError):
        return 503, {"ok": False, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
    finally:
        connection.close()


class UnixConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)
