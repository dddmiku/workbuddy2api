#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-15: 新增。成长中心两项日常动作：连登档位兑换（redeem）+ 抽奖清空（lottery）。
#   网关原有六类排程不含这两项，参考 workbuddy.py 的 do_redeem_by_streak / do_lottery
#   实现，落到与 school/cat 同一条路上（排程 -> runScript -> python3 scripts/*.py）。

"""growth_center —— 成长中心：连登档位兑换 + 抽奖清空。

为什么单独成脚本而不是写进 Go：这两个动作打的是 chat 域（copilot.tencent.com）的
growth 接口，与 task_common.py 已有的请求助手（headers / base / client_token 约定）
同源；复用现成管道比在 Go 里再实现一遍 upstream 调用面更稳，也与 school/cat 的
脚本类任务保持一致。

端点（chat 域）：
  GET  /v2/activity/growth/streak            连登天数（决定可兑换档位）
  POST /v2/activity/growth/redeem            {"tier":"7d|14d|28d","client_token":...}
  GET  /v2/activity/growth/lottery/chances   可用抽奖次数
  GET  /v2/activity/growth/lottery/summary   抽奖模块开关
  POST /v2/activity/growth/lottery/draw      {"client_token":...}

幂等性：兑换重复请求返回 409（已兑换）、天数不足返回 403，抽奖次数用完读出 0——
三种情况重复执行都安全，所以可以放在每天固定时点跑，不需要额外去重状态。

用法：
  python3 growth_center.py ALL --list              # 只读盘点（默认）
  python3 growth_center.py ALL --yes               # 兑换 + 抽奖
  python3 growth_center.py ALL --yes --redeem-only
  python3 growth_center.py ALL --yes --lottery-only
"""
import sys, os, time, uuid, argparse, glob

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc  # noqa: E402  仅复用 load_auth / AUTHS / 请求助手

# 兑换档位：档位名 -> 需要的连续签到天数（与 workbuddy.py 的 tiers 一致：7/14/28 天）
TIERS = [("7d", 7), ("14d", 14), ("28d", 28)]

PATH_REDEEM = "/v2/activity/growth/redeem"
PATH_HEATMAP = "/v2/activity/growth/heatmap"
PATH_MAKEUP_USE = "/v2/activity/growth/makeup-cards/use"
PATH_LOTTERY_CHANCES = "/v2/activity/growth/lottery/chances"
PATH_LOTTERY_SUMMARY = "/v2/activity/growth/lottery/summary"
PATH_LOTTERY_DRAW = "/v2/activity/growth/lottery/draw"


def gen_token(prefix):
    """客户端幂等令牌：服务端按它对同一动作去重，格式与既有脚本一致。"""
    return prefix + "-" + str(uuid.uuid4())


def _body_code(st, d):
    """取业务码：响应体里有 code 就用它，否则退化成 HTTP 状态码。"""
    if isinstance(d, dict) and isinstance(d.get("code"), int):
        return d.get("code")
    return st


def _msg_of(d):
    return ((d.get("message") or d.get("msg") or "") if isinstance(d, dict) else "")[:60]


def fetch_streak_data(auth):
    """拉连登与兑换状态。返回 (data, err)。

    一次 GET 同时拿到三样东西，比只取天数有用得多：
      streak.days                 连登天数
      makeup_cards.balance        补签卡余量（兑换档位会发卡）
      redemption_status.tier_*_status  服务端判定的档位状态（比本地按天数猜权威）
    还带 launch_date：上线日之前的空格子不是"漏签"，补签不能越界。
    """
    st, d = tc.do_get(auth, tc.chat_base(auth), tc.PATH_STREAK)
    if st != 200:
        return None, "http=" + str(st)
    data = (d.get("data") or {}) if isinstance(d, dict) else {}
    if not isinstance(data, dict) or not data:
        return None, "streak 响应为空或非对象"
    return data, ""


def streak_days(data):
    s = data.get("streak") if isinstance(data.get("streak"), dict) else data
    try:
        return int(s.get("days", 0))
    except Exception:
        return 0


def redeem_tiers(auth, uid8, data, dry, gap):
    """按连登档位兑换。返回 (ok, already, skip, fail)。

    两道闸：服务端 redemption_status.tier_*_status 说已兑换的直接跳过（权威判定），
    再用本地连登天数做前置过滤，避免明知不够还白打一次接口。
    真正的裁决仍在服务端：403 天数不足、409 已兑换都按正常跳过处理。
    """
    ok = already = skip = fail = 0
    days = streak_days(data)
    red = data.get("redemption_status")
    if not isinstance(red, dict):
        red = {}
    for tier, need in TIERS:
        status = str(red.get("tier_" + tier + "_status", "") or "")
        if status in ("redeemed", "claimed", "used", "received"):
            print("  [" + uid8 + "] 连登 " + tier + " 档：服务端标记已兑换，跳过")
            already += 1
            continue
        if days < need:
            print("  [" + uid8 + "] 连登 " + tier + " 档：还差 " + str(need - days) + " 天，跳过")
            skip += 1
            continue
        if dry:
            print("  [" + uid8 + "] 连登 " + tier + " 档：将 POST " + PATH_REDEEM + " tier=" + tier)
            skip += 1
            continue
        st, d = tc.do_post(auth, tc.chat_base(auth), PATH_REDEEM,
                           {"tier": tier, "client_token": gen_token("redeem-" + tier)})
        code = _body_code(st, d)
        msg = _msg_of(d)
        if code == 0:
            data = (d.get("data") or {}) if isinstance(d, dict) else {}
            print("  [" + uid8 + "] 连登 " + tier + " 档兑换成功：+" + str(data.get("credit_granted", 0))
                  + " 积分 +" + str(data.get("energy_granted", 0))
                  + " 能量 +" + str(data.get("chances_granted", 0)) + " 抽奖次数")
            ok += 1
        elif code == 409 or "duplicate" in msg:
            print("  [" + uid8 + "] 连登 " + tier + " 档已兑换过，跳过")
            already += 1
        elif code == 403 or "insufficient" in msg:
            print("  [" + uid8 + "] 连登 " + tier + " 档服务端判定天数不足，跳过")
            skip += 1
        else:
            print("  [" + uid8 + "] 连登 " + tier + " 档兑换失败：http=" + str(st)
                  + " code=" + str(code) + " msg=" + msg)
            fail += 1
        time.sleep(gap)
    return ok, already, skip, fail


def find_missed_date(auth, launch):
    """从昨天往回找最近的漏签日。返回 (date, why)。

    只认 heatmap 里 score==0 且早于今天、且不早于活动上线日的格子：
      - 今天不算漏签（那是签到的活，不是补签的活）
      - 上线日之前一片空，不是"漏了"，拿卡去补会被服务端拒
    """
    st, d = tc.do_get(auth, tc.chat_base(auth), PATH_HEATMAP)
    if st != 200:
        return "", "heatmap http=" + str(st)
    cells = ((d.get("data") or {}).get("cells") or []) if isinstance(d, dict) else []
    if not cells:
        return "", "heatmap 为空"
    signed = {}
    for c in cells:
        if isinstance(c, dict) and c.get("date"):
            try:
                signed[str(c["date"])] = int(c.get("score", 0) or 0)
            except Exception:
                signed[str(c["date"])] = 0
    today = time.strftime("%Y-%m-%d")  # 容器 TZ=Asia/Shanghai，与接口 timezone 同源
    for dt in sorted(signed.keys(), reverse=True):
        if dt >= today:
            continue
        if launch and dt < launch:
            break
        if signed[dt] > 0:
            continue
        return dt, ""
    return "", "往回找到上线日都没有漏签"


def use_makeup(auth, uid8, data, dry, gap):
    """补最近一次漏签。返回 (ok, skip, fail)。"""
    cards = data.get("makeup_cards") if isinstance(data.get("makeup_cards"), dict) else {}
    try:
        balance = int(cards.get("balance", 0) or 0)
    except Exception:
        balance = 0
    if balance <= 0:
        print("  [" + uid8 + "] 补签卡余量 0，跳过")
        return 0, 1, 0
    target, why = find_missed_date(auth, str(data.get("launch_date") or ""))
    if not target:
        print("  [" + uid8 + "] " + why + "，跳过")
        return 0, 1, 0
    if dry:
        print("  [" + uid8 + "] 将补签 " + target + "（余量 " + str(balance) + "）")
        return 0, 1, 0
    st, d = tc.do_post(auth, tc.chat_base(auth), PATH_MAKEUP_USE, {"target_date": target})
    if _body_code(st, d) == 0:
        print("  [" + uid8 + "] 补签成功：" + target)
        time.sleep(gap)
        return 1, 0, 0
    print("  [" + uid8 + "] 补签 " + target + " 失败：http=" + str(st) + " " + _msg_of(d))
    return 0, 0, 1


def lottery(auth, uid8, dry, gap):
    """抽到没有为止。返回 (won, fail)。"""
    st, d = tc.do_get(auth, tc.chat_base(auth), PATH_LOTTERY_CHANCES)
    if st != 200:
        print("  [" + uid8 + "] 读抽奖次数失败：http=" + str(st))
        return 0, 1
    data = (d.get("data") or {}) if isinstance(d, dict) else {}
    chances = data.get("balance", data.get("chances", data.get("remaining", 0))) or 0
    try:
        chances = int(chances)
    except Exception:
        chances = 0
    if chances <= 0:
        print("  [" + uid8 + "] 无可用抽奖次数")
        return 0, 0
    print("  [" + uid8 + "] 可用抽奖次数：" + str(chances))

    # 模块开关：活动整体关闭时直接跳过，不白打 draw。
    st2, d2 = tc.do_get(auth, tc.chat_base(auth), PATH_LOTTERY_SUMMARY)
    if st2 == 200 and isinstance(d2, dict):
        mod = (d2.get("data") or {}).get("module")
        if isinstance(mod, dict) and not mod.get("enabled", True):
            print("  [" + uid8 + "] 抽奖未开启，跳过")
            return 0, 0

    won = fail = 0
    for i in range(chances):
        if i:
            time.sleep(gap + 0.5)  # 抽奖比普通写动作更容易触发频控，间隔再放宽一点
        if dry:
            print("  [" + uid8 + "] 第 " + str(i + 1) + "/" + str(chances)
                  + " 次：将 POST " + PATH_LOTTERY_DRAW)
            continue
        st3, d3 = tc.do_post(auth, tc.chat_base(auth), PATH_LOTTERY_DRAW,
                             {"client_token": gen_token("draw")})
        if _body_code(st3, d3) == 0:
            dd = (d3.get("data") or {}) if isinstance(d3, dict) else {}
            prize = dd.get("prize_name") or dd.get("name") or dd.get("reward") or "未知奖品"
            print("  [" + uid8 + "] 第 " + str(i + 1) + "/" + str(chances) + " 次：" + str(prize))
            won += 1
        else:
            # 失败即停：继续抽只会把频控越撞越死，剩下的次数明天再说。
            print("  [" + uid8 + "] 第 " + str(i + 1) + "/" + str(chances) + " 次失败：http="
                  + str(st3) + " " + _msg_of(d3) + "（停止本次抽奖）")
            fail += 1
            break
    return won, fail


def collect_accounts(accounts):
    if accounts and not (len(accounts) == 1 and accounts[0].upper() == "ALL"):
        prefixes = accounts
    else:
        prefixes = [os.path.basename(p)[10:18]
                    for p in sorted(glob.glob(tc.AUTHS + "/workbuddy-*.json"))]
    seen, uniq = set(), []
    for p in prefixes:
        if p not in seen:
            seen.add(p)
            uniq.append(p)
    out = []
    for p in uniq:
        try:
            out.append(tc.load_auth(p))
        except SystemExit as e:
            print("ERR: " + str(e))
    return out


def main():
    ap = argparse.ArgumentParser(description="成长中心：连登档位兑换 + 抽奖清空")
    ap.add_argument("accounts", nargs="*", help="uid 前缀（可多个）或 ALL")
    ap.add_argument("--list", action="store_true", help="只读盘点（默认行为）")
    ap.add_argument("--redeem-only", action="store_true", help="只做连登兑换")
    ap.add_argument("--lottery-only", action="store_true", help="只做抽奖")
    ap.add_argument("--makeup-only", action="store_true", help="只做补签（补最近一次漏签）")
    ap.add_argument("--yes", action="store_true", help="放行写操作（默认 dry-run）")
    ap.add_argument("--gap", type=float, default=1.5, help="写动作间隔秒数（默认 1.5，最小 1.0）")
    a = ap.parse_args()

    if a.gap < 1.0:
        a.gap = 1.0

    if sum(1 for x in (a.redeem_only, a.lottery_only, a.makeup_only) if x) > 1:
        print("ERR: --redeem-only / --lottery-only / --makeup-only 互斥，只能给一个")
        sys.exit(2)
    do_redeem = not (a.lottery_only or a.makeup_only)
    do_lottery = not (a.redeem_only or a.makeup_only)
    do_makeup = not (a.redeem_only or a.lottery_only)

    mode = "LIST"
    if not a.list:
        if a.redeem_only:
            mode = "REDEEM"
        elif a.lottery_only:
            mode = "LOTTERY"
        elif a.makeup_only:
            mode = "MAKEUP"
        else:
            mode = "BOTH"
    dry = not a.yes
    print("mode=" + mode + (" dry-run（写操作需 --yes 放行）" if dry else " REAL")
          + " gap=" + str(a.gap))

    auths = collect_accounts(a.accounts)
    if not auths:
        print("ERR: 无可用账号（检查 auths/ 目录）")
        sys.exit(1)

    stats = {"accounts": 0, "redeem_ok": 0, "redeem_already": 0, "redeem_skip": 0,
             "redeem_fail": 0, "makeup_ok": 0, "makeup_skip": 0, "makeup_fail": 0,
             "won": 0, "draw_fail": 0, "streak_fail": 0}
    stats["accounts"] = len(auths)

    for auth in auths:
        uid8 = (auth.get("uid") or "")[:8]
        print("[" + uid8 + "] " + str(auth.get("nick") or ""))

        # 兑换与补签都读同一份 streak 快照：一次 GET 拿到连登、补签卡余量、档位状态。
        data = None
        if do_redeem or do_makeup:
            data, why = fetch_streak_data(auth)
            if data is None:
                print("  [" + uid8 + "] 读连登/兑换状态失败（" + why + "）")
                stats["streak_fail"] += 1

        if do_redeem and data is not None:
            print("  [" + uid8 + "] 连登 " + str(streak_days(data)) + " 天")
            ok, already, skip, fail = redeem_tiers(auth, uid8, data, dry, a.gap)
            stats["redeem_ok"] += ok
            stats["redeem_already"] += already
            stats["redeem_skip"] += skip
            stats["redeem_fail"] += fail

        # 补签排在兑换之后：兑换档位会发补签卡，同一轮里就能用上。
        if do_makeup and data is not None:
            ok, skip, fail = use_makeup(auth, uid8, data, dry, a.gap)
            stats["makeup_ok"] += ok
            stats["makeup_skip"] += skip
            stats["makeup_fail"] += fail

        if do_lottery:
            won, fail = lottery(auth, uid8, dry, a.gap)
            stats["won"] += won
            stats["draw_fail"] += fail

    print("summary: accounts=" + str(stats["accounts"])
          + " redeem_ok=" + str(stats["redeem_ok"])
          + " already=" + str(stats["redeem_already"])
          + " redeem_skip=" + str(stats["redeem_skip"])
          + " redeem_fail=" + str(stats["redeem_fail"])
          + " makeup_ok=" + str(stats["makeup_ok"])
          + " makeup_skip=" + str(stats["makeup_skip"])
          + " makeup_fail=" + str(stats["makeup_fail"])
          + " won=" + str(stats["won"])
          + " draw_fail=" + str(stats["draw_fail"])
          + " streak_fail=" + str(stats["streak_fail"]))
    # 有真实失败才非零退出：兑换/补签/抽奖都设计成幂等，重复执行不算失败。
    if stats["redeem_fail"] or stats["draw_fail"] or stats["makeup_fail"]:
        sys.exit(1)


if __name__ == "__main__":
    main()
