#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-15: 初版。workbuddy2api 账号管理面板后端：
#   扫码加号(OAuth url/poll)、启用/禁用(改名 .disabled)、删除(移入回收站)、
#   账号池状态与积分聚合、容器重启。网关自身无管理接口，故独立成服务。
# 2026-09-16: 内建登录层，撤掉 nginx basic auth：
#   自带 /login 登录页与会话 cookie（滑动 12 小时）、登录失败按 IP 限流、
#   账密在线修改（用户名 + 密码），旧 htpasswd 的 $apr1$ 口令继续可校验。
# 2026-09-16：新增登录保护的多密钥管理路由，使用本机管理通道、严格请求校验并隐藏配置中的完整密钥。
# 2026-09-17：路径、端口与容器名支持环境变量覆盖，便于与网关同一 Compose 项目部署。
# 2026-09-17：密钥管理支持模型绑定字段，并新增供前端选择模型的 /api/models。

"""workbuddy2api 账号管理面板 —— 后端

只监听 127.0.0.1，经 nginx 暴露到 /admin/。登录与本面板账号由本进程自己管，
凭证落在同目录 credentials.json，首次启动从 /etc/nginx/.htpasswd_wb2admin 继承
用户名与 apr1 口令哈希，因此原有密码不作废。依赖：仅标准库 + docker CLI。
"""

import base64
import hashlib
import hmac
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import threading
import time
import key_management
from http.cookies import SimpleCookie
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen
from urllib.parse import urlsplit

HERE = os.path.dirname(os.path.abspath(__file__))
# 部署形态：宿主机 systemd（默认值）或与网关同项目的容器（由环境变量覆盖）。
# BASE 之外的目录都随 BASE 走，容器里把网关目录整体挂到 /gateway 即可。
BASE = os.environ.get("WB2API_GATEWAY_DIR", "/opt/workbuddy2api")
AUTH_DIR = os.environ.get("WB2API_ADMIN_DIR", HERE)
AUTHS_DIR = os.path.join(BASE, "auths")
TRASH_DIR = os.path.join(BASE, "auths-trash")
CONFIG_PATH = os.path.join(BASE, "config.json")
INDEX_PATH = os.path.join(HERE, "index.html")
LOGIN_PATH = os.path.join(HERE, "login.html")
CRED_PATH = os.path.join(AUTH_DIR, "credentials.json")
HTPASSWD_PATH = os.environ.get("WB2API_HTPASSWD_PATH", "/etc/nginx/.htpasswd_wb2admin")

SESSION_TTL = 12 * 3600          # 会话滑动过期
PBKDF2_ROUNDS = 200000
PBKDF2_PREFIX = "pbkdf2_sha256"
LOGIN_MAX_FAILS = 6              # 同一 IP 在窗口内的失败次数
LOGIN_WINDOW = 300.0
COOKIE_NAME = "wb2a_admin"

CONTAINER = os.environ.get("WB2API_CONTAINER", "workbuddy2api")
GATEWAY = os.environ.get("WB2API_GATEWAY_URL", "http://127.0.0.1:7863")
LISTEN_HOST = os.environ.get("WB2API_ADMIN_HOST", "127.0.0.1")
try:
    LISTEN_PORT = int(os.environ.get("WB2API_ADMIN_PORT", "7864"))
except ValueError:
    LISTEN_PORT = 7864

REALMS = ("cn", "global")
UID_RE = re.compile(r"^[A-Za-z0-9_-]{1,80}$")
AUTH_FILE_RE = re.compile(r"^workbuddy-(?P<uid>.+?)\.json(?P<disabled>\.disabled)?$")

CREDIT_TTL = 60.0
_credit_cache = {"ts": 0.0, "data": None}
_lock = threading.Lock()


# ═══════════════════════════════════════════════════════════════════════
# 登录层：口令哈希、凭证文件、签名会话、限流
# ═══════════════════════════════════════════════════════════════════════

ITOA64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
_cred_lock = threading.Lock()
_fails = {}          # ip -> [timestamp, ...]
_revoked = set()     # 已签出但被主动作废的 nonce


def _to64(value, count):
    out = []
    for _ in range(count):
        out.append(ITOA64[value & 0x3F])
        value >>= 6
    return "".join(out)


def apr1_crypt(password, salt):
    """Apache apr1（MD5-crypt）口令。与 htpasswd -m / openssl passwd -apr1 等价。

    面板接手前，密码由 nginx basic auth 的 .htpasswd 管；为了不把老密码作废，
    首启用同一段算法校验存量哈希。
    """
    pw = password.encode("utf-8")
    sl = salt.encode("utf-8")[:8]
    magic = b"$apr1$"
    ctx = hashlib.md5(pw + magic + sl)
    alt = hashlib.md5(pw + sl + pw).digest()
    n = len(pw)
    i = n
    while i > 0:
        ctx.update(alt[:16] if i > 16 else alt[:i])
        i -= 16
    i = n
    while i:
        ctx.update(b"\x00" if (i & 1) else pw[:1])
        i >>= 1
    digest = ctx.digest()
    for i in range(1000):
        c = hashlib.md5()
        c.update(pw if (i & 1) else digest)
        if i % 3:
            c.update(sl)
        if i % 7:
            c.update(pw)
        c.update(digest if (i & 1) else pw)
        digest = c.digest()
    out = ""
    for a, b, cc in ((0, 6, 12), (1, 7, 13), (2, 8, 14), (3, 9, 15), (4, 10, 5)):
        out += _to64(digest[a] << 16 | digest[b] << 8 | digest[cc], 4)
    out += _to64(digest[11], 2)
    return "$apr1$" + salt[:8] + "$" + out


def parse_htpasswd(path):
    """读 .htpasswd，返回 (用户名, 原始哈希) 或 (None, None)。"""
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                line = line.strip()
                if not line or line.startswith("#") or ":" not in line:
                    continue
                name, _, digest = line.partition(":")
                if name.strip() and digest.strip():
                    return name.strip(), digest.strip()
    except OSError:
        pass
    return None, None


def hash_password(password):
    salt = secrets.token_bytes(16)
    dk = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"), salt, PBKDF2_ROUNDS)
    return "%s$%d$%s$%s" % (
        PBKDF2_PREFIX, PBKDF2_ROUNDS,
        base64.b64encode(salt).decode("ascii"),
        base64.b64encode(dk).decode("ascii"))


def verify_password(password, stored):
    """校验明文口令。stored 支持 pbkdf2_sha256$... 与 $apr1$... 两种。"""
    if not stored:
        return False
    if stored.startswith("$apr1$"):
        parts = stored.split("$")
        if len(parts) < 4:
            return False
        salt = parts[2][:8]
        try:
            return hmac.compare_digest(apr1_crypt(password, salt), stored)
        except Exception:
            return False
    if stored.startswith(PBKDF2_PREFIX + "$"):
        try:
            _, rounds, salt_b64, hash_b64 = stored.split("$", 3)
            salt = base64.b64decode(salt_b64)
            want = base64.b64decode(hash_b64)
            got = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"),
                                      salt, int(rounds))
            return hmac.compare_digest(got, want)
        except Exception:
            return False
    return False


def _new_credentials():
    """没有任何存量凭证时的兜底：随机口令写进 initial-password.txt，避免锁死。"""
    pw = secrets.token_urlsafe(12)
    doc = {
        "version": 1,
        "username": "wbadmin",
        "password": hash_password(pw),
        "sessionKey": secrets.token_hex(32),
        "updatedAt": int(time.time()),
        "history": [],
    }
    _save_credentials(doc)
    hint = os.path.join(AUTH_DIR, "initial-password.txt")
    try:
        with open(hint, "w", encoding="utf-8") as fh:
            fh.write("username: wbadmin\npassword: %s\n" % pw)
        os.chmod(hint, 0o600)
    except OSError:
        pass
    sys.stderr.write("未找到凭证，已生成初始账号，见 %s\n" % hint)
    return doc


def _save_credentials(doc):
    tmp = CRED_PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=2, ensure_ascii=False)
        fh.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, CRED_PATH)


def load_credentials():
    """读凭证；文件不存在时从 nginx 的 .htpasswd 继承用户名与 apr1 哈希。"""
    with _cred_lock:
        try:
            with open(CRED_PATH, "r", encoding="utf-8") as fh:
                doc = json.load(fh)
            if doc.get("username") and doc.get("password"):
                if not doc.get("sessionKey"):
                    doc["sessionKey"] = secrets.token_hex(32)
                    _save_credentials(doc)
                return doc
        except (OSError, ValueError):
            pass

        name, digest = parse_htpasswd(HTPASSWD_PATH)
        if name and digest:
            doc = {
                "version": 1,
                "username": name,
                "password": digest,
                "sessionKey": secrets.token_hex(32),
                "updatedAt": int(time.time()),
                "history": [],
                "inherited": True,
            }
            _save_credentials(doc)
            sys.stderr.write("已从 %s 继承登录口令（用户名 %s）\n"
                             % (HTPASSWD_PATH, name))
            return doc
        return _new_credentials()


def _b64u(raw):
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def _unb64u(text):
    pad = "=" * (-len(text) % 4)
    return base64.urlsafe_b64decode(text + pad)


def issue_session(username, ttl=SESSION_TTL):
    """签名会话令牌：payload.nonce + HMAC。key 落盘，面板重启后仍有效。"""
    doc = load_credentials()
    nonce = secrets.token_hex(12)
    payload = {"u": username, "e": int(time.time()) + ttl, "n": nonce}
    body = _b64u(json.dumps(payload, separators=(",", ":")).encode("utf-8"))
    key = bytes.fromhex(doc["sessionKey"])
    sig = _b64u(hmac.new(key, body.encode("ascii"), hashlib.sha256).digest())
    return body + "." + sig, payload


def read_session(token):
    """校验令牌。返回 payload 或 None。"""
    if not token or "." not in token:
        return None
    body, _, sig = token.partition(".")
    doc = load_credentials()
    try:
        key = bytes.fromhex(doc["sessionKey"])
        want = hmac.new(key, body.encode("ascii"), hashlib.sha256).digest()
        if not hmac.compare_digest(want, _unb64u(sig)):
            return None
        payload = json.loads(_unb64u(body).decode("utf-8"))
    except Exception:
        return None
    if payload.get("n") in _revoked:
        return None
    if int(payload.get("e") or 0) < time.time():
        return None
    if payload.get("u") != doc.get("username"):
        return None
    return payload


def _client_ip(handler):
    """真实客户端 IP。

    线上是 Cloudflare -> nginx -> 面板：nginx 用 real_ip 模块把 $remote_addr
    还原成访客 IP 并写进 X-Real-IP，这个头由 nginx 覆盖、客户端伪造不了，所以
    优先取它。CF-Connecting-IP 只在 real_ip 模块没生效时兜底——它属于客户端
    可自行携带的头，绕过 Cloudflare 直连源站时能伪造，不能当第一顺位。
    登录限流按这里分桶：取错会把不相干的人关进同一个小黑屋，或者让人随便换头
    就绕开限流。
    """
    for header in ("X-Real-IP", "CF-Connecting-IP", "X-Forwarded-For"):
        raw = handler.headers.get(header)
        if raw:
            return raw.split(",")[0].strip()
    return handler.client_address[0] if handler.client_address else "?"


def login_blocked(ip):
    now = time.time()
    with _cred_lock:
        hits = [t for t in _fails.get(ip, []) if now - t < LOGIN_WINDOW]
        _fails[ip] = hits
        return len(hits) >= LOGIN_MAX_FAILS, len(hits)


def login_failed(ip):
    with _cred_lock:
        _fails.setdefault(ip, []).append(time.time())


def login_ok(ip):
    with _cred_lock:
        _fails.pop(ip, None)


def api_key():
    try:
        with open(CONFIG_PATH, "r", encoding="utf-8") as f:
            return json.load(f).get("api_key", "") or ""
    except Exception:
        return ""


def docker(args, timeout=60, check=False):
    try:
        p = subprocess.run(["docker"] + list(args), capture_output=True,
                           text=True, timeout=timeout)
        out, err = p.stdout.strip(), p.stderr.strip()
        if check and p.returncode != 0:
            raise RuntimeError(err or out or ("docker 退出码 %d" % p.returncode))
        return p.returncode, out, err
    except subprocess.TimeoutExpired:
        return 124, "", "docker 命令超时(%ds)" % timeout
    except FileNotFoundError:
        return 127, "", "找不到 docker 命令"


def container_running():
    rc, out, _ = docker(["inspect", "-f", "{{.State.Running}}", CONTAINER], timeout=20)
    return rc == 0 and out.strip() == "true"


def gateway_get(path, timeout=15):
    try:
        management_socket = key_management.socket_path(CONFIG_PATH, BASE)
        if management_socket:
            code, payload = key_management.request(management_socket, "GET", path, timeout=timeout)
            return payload if code < 400 else None
    except (OSError, ValueError):
        return None
    key = api_key()
    req = Request(GATEWAY + path, headers={"Authorization": "Bearer " + key})
    try:
        with urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode("utf-8"))
    except HTTPError as e:
        try:
            return json.loads(e.read().decode("utf-8"))
        except Exception:
            return None
    except (URLError, OSError, ValueError):
        return None


TASK_ENABLE_KEY = {
    "checkin": "checkin_enabled",
    "travel": "travel_enabled",
    "activity": "activity_enabled",
    "keepalive": "keepalive_enabled",
    "school": "school_enabled",
    "cat": "cat_enabled",
    "redeem": "redeem_enabled",
    "lottery": "lottery_enabled",
    "makeup": "makeup_enabled",
}


def gateway_post(path, timeout=30):
    """带 api_key 向网关发 POST。返回 (ok, payload)。"""
    try:
        management_socket = key_management.socket_path(CONFIG_PATH, BASE)
        if management_socket:
            code, payload = key_management.request(management_socket, "POST", path, timeout=timeout)
            return code < 400, payload
    except (OSError, ValueError):
        return False, {"ok": False, "message": "无法读取网关配置"}
    key = api_key()
    req = Request(GATEWAY + path, data=b"", method="POST",
                  headers={"Authorization": "Bearer " + key})
    try:
        with urlopen(req, timeout=timeout) as r:
            return True, json.loads(r.read().decode("utf-8"))
    except HTTPError as e:
        try:
            return False, json.loads(e.read().decode("utf-8"))
        except Exception:
            return False, {"error": {"message": "网关返回 %d" % e.code}}
    except (URLError, OSError, ValueError) as ex:
        return False, {"error": {"message": "连不上网关：%s" % ex}}


def set_task_enabled(key, enabled):
    """改 config.json 的 schedule.<key>_enabled。返回 (ok, message)。

    容器把 config.json 以只读挂进去，所以改完必须重启才生效——由调用方负责重启。
    保留原文件的缩进与键序（json.load 保持插入顺序），只在目标键上动刀。
    """
    field = TASK_ENABLE_KEY.get(key)
    if not field:
        return False, "未知任务：%s" % key
    try:
        with open(CONFIG_PATH, "r", encoding="utf-8") as f:
            cfg = json.load(f)
    except Exception as ex:
        return False, "读取 config.json 失败：%s" % ex
    sch = cfg.get("schedule")
    if not isinstance(sch, dict):
        sch = {}
        cfg["schedule"] = sch
    if sch.get(field) is enabled:
        return True, "状态未变化"
    sch[field] = bool(enabled)
    try:
        tmp = CONFIG_PATH + ".tmp"
        # config.json 含调用密钥：重写后保持原有权限与属主，避免变成 world-readable。
        try:
            original = os.stat(CONFIG_PATH)
        except OSError:
            original = None
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(cfg, f, indent=2, ensure_ascii=False)
            f.write(chr(10))
        if original is not None:
            try:
                os.chmod(tmp, original.st_mode & 0o777)
                os.chown(tmp, original.st_uid, original.st_gid)
            except OSError:
                pass
        os.replace(tmp, CONFIG_PATH)
    except Exception as ex:
        return False, "写入 config.json 失败：%s" % ex
    return True, "已保存（重启后生效）"


def chown_app(path):
    try:
        shutil.chown(path, user=10001, group=10001)
    except Exception:
        pass
    try:
        os.chmod(path, 0o600)
    except Exception:
        pass


def list_auth_files():
    items = []
    if not os.path.isdir(AUTHS_DIR):
        return items
    for name in sorted(os.listdir(AUTHS_DIR)):
        m = AUTH_FILE_RE.match(name)
        if not m:
            continue
        items.append({
            "uid": m.group("uid"),
            "file": name,
            "disabled": bool(m.group("disabled")),
            "path": os.path.join(AUTHS_DIR, name),
        })
    return items


def read_auth(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return None


def auth_summary(entry):
    raw = read_auth(entry["path"]) or {}
    acct = raw.get("account") or {}
    auth = raw.get("auth") or {}
    exp = auth.get("expiresAt") or 0
    try:
        exp = int(exp)
    except (TypeError, ValueError):
        exp = 0
    return {
        "uid": entry["uid"],
        "nickname": acct.get("nickname") or acct.get("uid") or entry["uid"][:8],
        "enterpriseId": acct.get("enterpriseId") or "",
        "realm": auth.get("realm") or "",
        "domain": auth.get("domain") or "",
        "expiresAt": exp,
        "disabled": entry["disabled"],
        "parsable": bool(raw),
    }


def write_auth_file(poll_result):
    uid = poll_result.get("uid") or ""
    if not UID_RE.match(uid):
        raise RuntimeError("上游返回的 uid 不合法: %r" % uid)
    try:
        exp_in = int(poll_result.get("expires_in") or 0)
    except (TypeError, ValueError):
        exp_in = 0

    doc = {
        "account": {
            "uid": uid,
            "enterpriseId": poll_result.get("enterprise_id") or "",
            "nickname": poll_result.get("nickname") or "",
        },
        "auth": {
            "accessToken": poll_result.get("access_token") or "",
            "refreshToken": poll_result.get("refresh_token") or "",
            "expiresAt": int(time.time()) + exp_in,
            "domain": poll_result.get("domain") or "",
            "realm": poll_result.get("realm") or "cn",
        },
    }
    if not doc["auth"]["accessToken"]:
        raise RuntimeError("上游没有返回 access_token")

    os.makedirs(AUTHS_DIR, exist_ok=True)
    final_path = os.path.join(AUTHS_DIR, "workbuddy-%s.json" % uid)
    stale = final_path + ".disabled"
    if os.path.exists(stale):
        os.remove(stale)

    tmp = final_path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1)
    os.replace(tmp, final_path)
    chown_app(final_path)

    return {
        "uid": uid,
        "nickname": doc["account"]["nickname"],
        "realm": doc["auth"]["realm"],
        "expiresAt": doc["auth"]["expiresAt"],
        "expiresInDays": round(exp_in / 86400.0, 1),
    }


def find_entry(uid):
    if not UID_RE.match(uid or ""):
        return None
    for e in list_auth_files():
        if e["uid"] == uid:
            return e
    return None


def restart_container():
    t0 = time.time()
    rc, out, err = docker(["restart", CONTAINER], timeout=120)
    if rc != 0:
        return False, "重启失败: %s" % (err or out), 0.0
    for _ in range(40):
        time.sleep(1)
        h = gateway_get("/healthz", timeout=8)
        if h is not None:
            return True, "已重启并加载账号", round(time.time() - t0, 1)
    return True, "已重启(健康检查未及时响应，可稍后刷新)", round(time.time() - t0, 1)


def get_credits(force=False):
    with _lock:
        now = time.time()
        if not force and _credit_cache["data"] and now - _credit_cache["ts"] < CREDIT_TTL:
            return _credit_cache["data"]
    if not container_running():
        return {"error": "容器未运行"}
    rc, out, err = docker(["exec", CONTAINER, "./credit"], timeout=90)
    if rc != 0:
        return {"error": err or out or "credit 执行失败"}
    try:
        data = json.loads(out)
    except ValueError:
        return {"error": "credit 输出无法解析", "raw": out[:400]}
    with _lock:
        _credit_cache["ts"] = time.time()
        _credit_cache["data"] = data
    return data


def credits_by_uid(force=False):
    data = get_credits(force=force)
    if not isinstance(data, dict) or "accounts" not in data:
        return {}, data
    return {a.get("uid"): a for a in (data.get("accounts") or [])}, data


def build_state(force_credit=False):
    pool = gateway_get("/status") or {}
    pool_by_uid = {a.get("uid"): a for a in (pool.get("accounts") or [])}
    cred_by_uid, credit_raw = credits_by_uid(force=force_credit)

    accounts = []
    for e in list_auth_files():
        s = auth_summary(e)
        p = pool_by_uid.get(s["uid"]) or {}
        c = cred_by_uid.get(s["uid"]) or {}
        s["pool"] = {
            "inPool": bool(p),
            "healthy": bool(p) and (not p.get("cooling")) and (not p.get("disabled")),
            "cooling": bool(p.get("cooling")),
            "until": p.get("until"),
            "disabled": bool(p.get("disabled")),
            "disabledReason": p.get("disabled_reason") or "",
            "inFlight": p.get("in_flight") or 0,
            "breakerFails": p.get("breaker_fails") or 0,
            "lastSuccess": p.get("last_success"),
            "lastErr": p.get("last_err"),
        }
        s["credits"] = {
            "remain": c.get("remain"),
            "size": c.get("size"),
            "used": c.get("used"),
            "packages": c.get("packages"),
            "ok": c.get("ok"),
        }
        accounts.append(s)

    changed = [a["uid"] for a in accounts if a["disabled"] and a["pool"]["inPool"]]

    return {
        "service": "wb2api-admin",
        "containerRunning": container_running(),
        "accounts": accounts,
        "pool": {
            "total": pool.get("total"),
            "healthy": pool.get("healthy"),
            "cooling": pool.get("cooling"),
            "disabled": pool.get("disabled"),
            "inFlightFull": pool.get("in_flight_full"),
            "realmTotals": pool.get("realm_totals"),
            "stickySessions": pool.get("sticky_sessions"),
        },
        "creditTotals": (credit_raw or {}).get("total") if isinstance(credit_raw, dict) else None,
        "creditError": (credit_raw or {}).get("error") if isinstance(credit_raw, dict) else None,
        "pendingRestart": bool(changed),
        "apiKey": "",
        "apiKeysManaged": bool(key_management.socket_path(CONFIG_PATH, BASE)),
    }


class Handler(BaseHTTPRequestHandler):
    server_version = "wb2api-admin/1.1"
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s - %s" % (self.address_string(), fmt % args) + chr(10))

    def _send(self, code, body, ctype, extra=None):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Referrer-Policy", "same-origin")
        for name, value in list(extra or []) + list(getattr(self, "_pending", []) or []):
            self.send_header(name, value)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _json(self, code, payload, extra=None):
        self._send(code, json.dumps(payload, ensure_ascii=False).encode("utf-8"),
                   "application/json; charset=utf-8", extra)

    def _html(self, code, text):
        self._send(code, text.encode("utf-8"), "text/html; charset=utf-8")

    def _redirect(self, target):
        self._send(302, b"", "text/plain; charset=utf-8",
                   [("Location", target)])

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if not n:
            return {}
        try:
            return json.loads(self.rfile.read(n).decode("utf-8"))
        except Exception:
            return {}

    # ── 会话 ───────────────────────────────────────────────────────────
    def _session(self):
        """从 Cookie 里取会话；顺带把临近过期的会话续期（滑动过期）。"""
        raw = self.headers.get("Cookie") or ""
        if not raw:
            return None
        cookie = SimpleCookie()
        try:
            cookie.load(raw)
        except Exception:
            return None
        morsel = cookie.get(COOKIE_NAME)
        if not morsel:
            return None
        payload = read_session(morsel.value)
        if not payload:
            return None
        remaining = int(payload.get("e") or 0) - time.time()
        if remaining < SESSION_TTL / 3.0:
            token, _ = issue_session(payload["u"])
            self._pending = self._cookie_headers(token, SESSION_TTL)
        return payload

    def _need_session(self):
        payload = self._session()
        if not payload:
            return None
        return payload

    def _cookie_headers(self, token, max_age):
        value = ("%s=%s; Path=/admin/; HttpOnly; SameSite=Lax; Max-Age=%d"
                 % (COOKIE_NAME, token, max_age))
        return [("Set-Cookie", value)]

    def _clear_cookie(self):
        return [("Set-Cookie",
                 "%s=; Path=/admin/; HttpOnly; SameSite=Lax; Max-Age=0"
                 % COOKIE_NAME)]

    def do_GET(self):
        path = self.path.split("?")[0]
        query = self.path.split("?", 1)[1] if "?" in self.path else ""
        session = self._session()

        if path == "/login":
            if session:
                return self._redirect("./")
            try:
                with open(LOGIN_PATH, "r", encoding="utf-8") as fh:
                    return self._html(200, fh.read())
            except OSError as ex:
                return self._html(500, "登录页缺失: %s" % ex)

        if not session:
            if path.startswith("/api/"):
                return self._json(401, {"ok": False, "error": "unauthorized",
                                        "message": "登录已失效，请重新登录"})
            return self._redirect("login")

        if path in ("/", "/index.html"):
            try:
                with open(INDEX_PATH, "r", encoding="utf-8") as f:
                    return self._html(200, f.read())
            except Exception as ex:
                return self._html(500, "面板前端缺失: %s" % ex)

        if path.startswith("/vendor/"):
            fp = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                             "vendor", os.path.basename(path))
            if not os.path.isfile(fp):
                return self._json(404, {"error": "not found"})
            with open(fp, "rb") as fh:
                return self._send(200, fh.read(), "application/javascript; charset=utf-8")

        if path == "/api/state":
            force = "refresh_credit=1" in query
            try:
                return self._json(200, build_state(force_credit=force))
            except Exception as ex:
                return self._json(500, {"error": str(ex)})

        if path == "/api/session":
            doc = load_credentials()
            return self._json(200, {
                "ok": True,
                "username": doc.get("username", ""),
                "expiresAt": session.get("e"),
                "ttl": SESSION_TTL,
                "inherited": bool(doc.get("inherited")),
            })

        if path == "/api/keys":
            return self.keys_request("GET", "/keys")

        if path == "/api/models":
            # 面板经本机管理通道读取完整模型列表，不受单个调用密钥的绑定限制。
            payload = gateway_get("/v1/models")
            ids = []
            if isinstance(payload, dict):
                for item in payload.get("data") or []:
                    if isinstance(item, dict) and isinstance(item.get("id"), str) and item["id"]:
                        ids.append(item["id"])
            return self._json(200, {"ok": True, "models": sorted(set(ids))})

        if path == "/api/tasks":
            payload = gateway_get("/tasks")
            if payload is not None:
                return self._json(200, payload)
            return self._json(200, {"available": False, "tasks": [],
                                    "error": "连不上网关或网关未接入排程"})

        if path == "/api/task/log":
            mk = re.search(r"key=([a-z_]+)", query)
            key = mk.group(1) if mk else ""
            if key not in TASK_ENABLE_KEY:
                return self._json(200, {"ok": False, "lines": [], "message": "未知任务：%s" % key})
            payload = gateway_get("/tasks/%s/log" % key)
            if payload is None:
                return self._json(200, {"ok": False, "lines": [], "message": "连不上网关或网关未接入排程"})
            lines = payload.get("lines") or []
            return self._json(200, {"ok": True, "lines": lines, "count": len(lines),
                                    "message": (payload.get("error") or {}).get("message", "")})

        if path == "/api/logs":
            lines = 120
            m = re.search(r"lines=(\d{1,4})", query)
            if m:
                lines = min(int(m.group(1)), 1000)
            rc, out, err = docker(["logs", "--tail", str(lines), CONTAINER], timeout=40)
            return self._json(200, {"ok": rc == 0, "logs": out or err, "rc": rc})

        return self._json(404, {"error": "not found"})

    def do_POST(self):
        path = self.path.split("?")[0]
        if path in ("/api/keys", "/api/keys/update", "/api/keys/delete"):
            return self.keys_post(path)
        body = self._body()
        try:
            if path == "/api/auth/login":
                return self.auth_login(body)
            if path == "/api/auth/logout":
                return self.auth_logout()

            session = self._session()
            if not session:
                return self._json(401, {"ok": False, "error": "unauthorized",
                                        "message": "登录已失效，请重新登录"})

            if path == "/api/auth/password":
                return self.auth_password(body, session)
            if path == "/api/login/start":
                return self._json(200, self.login_start(body))
            if path == "/api/login/poll":
                return self._json(200, self.login_poll(body))
            if path == "/api/account/toggle":
                return self._json(200, self.account_toggle(body))
            if path == "/api/account/delete":
                return self._json(200, self.account_delete(body))
            if path == "/api/task/run":
                return self._json(200, self.task_run(body))
            if path == "/api/task/toggle":
                return self._json(200, self.task_toggle(body))
            if path == "/api/service/restart":
                return self._json(200, self.service_restart())
            if path == "/api/credit":
                return self._json(200, {"credit": get_credits(force=True)})
        except Exception as ex:
            return self._json(500, {"ok": False, "message": str(ex)})
        return self._json(404, {"error": "not found"})

    def keys_request(self, method, endpoint, body=None):
        try:
            path = key_management.socket_path(CONFIG_PATH, BASE)
        except (OSError, ValueError):
            return self._json(503, {"ok": False, "message": "无法读取密钥管理配置"})
        code, result = key_management.request(path, method, endpoint, body)
        return self._json(code, result)

    def keys_post(self, path):
        if not self._session():
            return self._json(401, {"ok": False, "message": "请先登录管理面板"})
        origin = self.headers.get("Origin")
        try:
            parsed = urlsplit(origin) if origin else None
            origin_ok = not parsed or (parsed.scheme in ("http", "https") and parsed.netloc.lower() == self.headers.get("Host", "").lower() and not parsed.username)
        except ValueError:
            origin_ok = False
        if not origin_ok or self.headers.get("X-Admin-Request") != "1":
            return self._json(403, {"ok": False, "message": "请求来源无效，请从管理页面重新操作"})
        if self.headers.get("Content-Type", "").split(";", 1)[0].strip().lower() != "application/json":
            return self._json(415, {"ok": False, "message": "请使用 JSON 格式提交"})
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            length = 0
        if not 0 < length <= 8192:
            return self._json(413, {"ok": False, "message": "请求体需在 1—8192 字节内"})
        try:
            self.connection.settimeout(10)
            body = json.loads(self.rfile.read(length).decode("utf-8"))
        except (OSError, ValueError):
            return self._json(400, {"ok": False, "message": "密钥信息格式不正确"})
        if not isinstance(body, dict):
            return self._json(400, {"ok": False, "message": "密钥信息必须是 JSON 对象"})
        if path == "/api/keys":
            if set(body) - {"name", "note", "models"}:
                return self._json(400, {"ok": False, "message": "包含不支持的字段"})
            return self.keys_request("POST", "/keys", body)
        key_id = body.get("id")
        if not isinstance(key_id, str) or not re.fullmatch(r"legacy|key_[0-9a-f]{24}", key_id):
            return self._json(400, {"ok": False, "message": "密钥标识不正确"})
        if path == "/api/keys/delete":
            if set(body) != {"id"}:
                return self._json(400, {"ok": False, "message": "包含不支持的字段"})
            return self.keys_request("DELETE", "/keys/" + key_id)
        changes = {k: v for k, v in body.items() if k != "id"}
        if not changes or set(changes) - {"name", "note", "enabled", "models"}:
            return self._json(400, {"ok": False, "message": "没有有效的修改字段"})
        if "models" in changes:
            models = changes["models"]
            if not isinstance(models, list) or len(models) > 64 or any(
                    not isinstance(item, str) or not item or len(item) > 64 or
                    item.strip() != item or any(ch.isspace() or ord(ch) < 32 for ch in item)
                    for item in models) or len(set(models)) != len(models):
                return self._json(400, {"ok": False, "message": "模型绑定需为最多 64 个不重复的模型名"})
        return self.keys_request("PATCH", "/keys/" + key_id, changes)

    # ── 登录 / 改密 ────────────────────────────────────────────────────
    def auth_login(self, body):
        ip = _client_ip(self)
        blocked, hits = login_blocked(ip)
        if blocked:
            return self._json(429, {
                "ok": False,
                "message": "失败次数过多，请 %d 分钟后再试" % int(LOGIN_WINDOW // 60),
            })

        doc = load_credentials()
        name = str(body.get("username") or "").strip()
        pw = str(body.get("password") or "")
        if not name or not pw:
            login_failed(ip)
            return self._json(400, {"ok": False, "message": "请输入用户名和密码"})

        name_ok = hmac.compare_digest(name.encode("utf-8"),
                                      str(doc.get("username", "")).encode("utf-8"))
        if not (name_ok and verify_password(pw, doc.get("password"))):
            login_failed(ip)
            left = max(0, LOGIN_MAX_FAILS - (hits + 1))
            return self._json(401, {
                "ok": False,
                "message": "用户名或密码错误" + ("，还可尝试 %d 次" % left if left else ""),
            })

        login_ok(ip)
        token, payload = issue_session(doc["username"])
        return self._json(200, {"ok": True, "username": doc["username"],
                                "expiresAt": payload["e"], "ttl": SESSION_TTL},
                          extra=self._cookie_headers(token, SESSION_TTL))

    def auth_logout(self):
        payload = self._session()
        if payload and payload.get("n"):
            with _cred_lock:
                _revoked.add(payload["n"])
                if len(_revoked) > 4096:
                    _revoked.clear()
        return self._json(200, {"ok": True, "message": "已退出登录"},
                          extra=self._clear_cookie())

    def auth_password(self, body, session):
        doc = load_credentials()
        current = str(body.get("current") or "")
        new_user = str(body.get("username") or "").strip()
        new_pw = str(body.get("password") or "")
        confirm = str(body.get("confirm") or "")

        if not verify_password(current, doc.get("password")):
            return self._json(400, {"ok": False, "message": "当前密码不正确"})
        if len(new_pw) < 8:
            return self._json(400, {"ok": False, "message": "新密码至少 8 位"})
        if new_pw != confirm:
            return self._json(400, {"ok": False, "message": "两次输入的新密码不一致"})
        if new_pw == current:
            return self._json(400, {"ok": False, "message": "新密码不能与当前密码相同"})
        if not re.match(r"^[A-Za-z0-9_.-]{3,32}$", new_user or ""):
            return self._json(400, {
                "ok": False,
                "message": "用户名需为 3-32 位字母、数字、下划线、点或连字符",
            })

        with _cred_lock:
            history = doc.get("history") or []
            history.append({"password": doc.get("password"), "at": int(time.time())})
            doc = {
                "version": 1,
                "username": new_user,
                "password": hash_password(new_pw),
                "sessionKey": secrets.token_hex(32),
                "updatedAt": int(time.time()),
                "history": history[-5:],
                "inherited": False,
            }
            try:
                _save_credentials(doc)
            except OSError as ex:
                return self._json(500, {"ok": False, "message": "写入失败：%s" % ex})

        # 换密即换 sessionKey：所有旧会话（包括当前这条）一并失效，强制重登
        token, payload = issue_session(doc["username"])
        return self._json(200, {
            "ok": True,
            "message": "账号信息已更新，其他设备上的登录已失效",
            "username": doc["username"],
            "expiresAt": payload["e"],
        }, extra=self._cookie_headers(token, SESSION_TTL))

    def login_start(self, body):
        realm = (body.get("realm") or "cn").strip().lower()
        if realm not in REALMS:
            return {"ok": False, "message": "realm 只能是 cn 或 global"}
        if not container_running():
            return {"ok": False, "message": "容器未运行，先启动服务"}
        rc, out, err = docker(["exec", CONTAINER, "./login", "--realm=%s" % realm, "url"],
                              timeout=60)
        if rc != 0:
            return {"ok": False, "message": err or out or "获取授权链接失败"}
        url = out.strip().splitlines()[-1].strip() if out.strip() else ""
        if not url.startswith("http"):
            return {"ok": False, "message": "没拿到授权链接: %s" % (out or err)[:200]}
        return {"ok": True, "url": url, "realm": realm}

    def login_poll(self, body):
        realm = (body.get("realm") or "cn").strip().lower()
        if realm not in REALMS:
            return {"ok": False, "message": "realm 只能是 cn 或 global"}
        rc, out, err = docker(["exec", CONTAINER, "./login", "--realm=%s" % realm, "poll"],
                              timeout=90)
        if rc != 0:
            return {"ok": False, "pending": True, "message": err or out or "poll 失败"}
        try:
            result = json.loads(out)
        except ValueError:
            return {"ok": False, "message": "poll 输出无法解析: %s" % out[:200]}

        with _lock:
            summary = write_auth_file(result)
            ok, msg, secs = restart_container()
        if not ok:
            return {"ok": False, "message": "凭证已保存，但 %s" % msg, "account": summary}
        return {"ok": True, "account": summary,
                "message": "账号已添加(%s)，重启耗时 %ss" % (summary["nickname"], secs)}

    def account_toggle(self, body):
        uid = body.get("uid") or ""
        want_disabled = bool(body.get("disabled"))
        with _lock:
            entry = find_entry(uid)
            if not entry:
                return {"ok": False, "message": "找不到账号 %s" % uid}
            if entry["disabled"] == want_disabled:
                return {"ok": True, "message": "状态未变化", "restart": None}
            src = entry["path"]
            dst = src + ".disabled" if want_disabled else src[:-len(".disabled")]
            if os.path.exists(dst):
                os.remove(dst)
            os.rename(src, dst)
            chown_app(dst)
            ok, msg, secs = restart_container()
        action = "禁用" if want_disabled else "启用"
        return {"ok": ok, "restart": secs if ok else None,
                "message": "已%s %s，%s" % (action, entry["uid"][:8], msg)}

    def account_delete(self, body):
        uid = body.get("uid") or ""
        with _lock:
            entry = find_entry(uid)
            if not entry:
                return {"ok": False, "message": "找不到账号 %s" % uid}
            os.makedirs(TRASH_DIR, exist_ok=True)
            stamp = time.strftime("%Y%m%d-%H%M%S")
            dst = os.path.join(TRASH_DIR, "%s.%s" % (entry["file"], stamp))
            shutil.move(entry["path"], dst)
            ok, msg, secs = restart_container()
        return {"ok": ok, "restart": secs if ok else None,
                "trashed": os.path.basename(dst),
                "message": "已删除 %s(回收件 %s)，%s" % (
                    entry["uid"][:8], os.path.basename(dst), msg)}

    def task_run(self, body):
        key = (body.get("key") or "").strip()
        if key not in TASK_ENABLE_KEY:
            return {"ok": False, "message": "未知任务：%s" % key}
        ok, payload = gateway_post("/tasks/%s/run" % key)
        if not ok:
            msg = (payload.get("error") or {}).get("message") or "触发失败"
            return {"ok": False, "message": msg}
        return {"ok": True, "message": "已触发，结果可在下方日志里看到"}

    def task_toggle(self, body):
        key = (body.get("key") or "").strip()
        enabled = bool(body.get("enabled"))
        with _lock:
            ok, msg = set_task_enabled(key, enabled)
            if not ok:
                return {"ok": False, "message": msg}
            if msg == "状态未变化":
                return {"ok": True, "message": msg}
            rok, rmsg, secs = restart_container()
        return {"ok": rok, "restart": secs if rok else None,
                "message": "已%s「%s」，%s" % ("启用" if enabled else "停用", key, rmsg if rok else rmsg)}

    def service_restart(self):
        with _lock:
            ok, msg, secs = restart_container()
        return {"ok": ok, "restart": secs if ok else None, "message": msg}


def main():
    os.makedirs(AUTHS_DIR, exist_ok=True)
    srv = ThreadingHTTPServer((LISTEN_HOST, LISTEN_PORT), Handler)
    sys.stderr.write("wb2api-admin listening on %s:%d" % (LISTEN_HOST, LISTEN_PORT) + chr(10))
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
