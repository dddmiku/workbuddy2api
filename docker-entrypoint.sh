#!/bin/sh
# ═══ 更新日志 ═══
# 2026-09-17：新增 PID 1 监督脚本，支撑容器内热更新：网关以子进程运行，热更新时
#             子进程把监听套接字交给新实例后以 75 退出，本脚本不重启、保持容器存活，
#             由新实例继续服务；收到 SIGTERM 时转发信号并随子进程退出。
# 2026-09-17：交接完成后的等待循环响应 SIGTERM，避免 docker stop 只能靠超时杀容器。
set -u

# 默认二进制（镜像内）；存在热更新落地的 current 指针时优先用它，重启后仍是新版本。
IMAGE_BIN="/app/wb2api"
UPDATE_DIR="${WB2API_UPDATE_DIR:-/app/data/updates}"
CURRENT_POINTER="${UPDATE_DIR}/current"
EXIT_HANDOVER=75

pick_binary() {
  if [ -f "$CURRENT_POINTER" ]; then
    candidate="$(cat "$CURRENT_POINTER" 2>/dev/null || true)"
    if [ -n "$candidate" ] && [ -x "$candidate" ]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  fi
  printf '%s\n' "$IMAGE_BIN"
}

child_pid=0
terminating=0

forward_signal() {
  terminating=1
  if [ "$child_pid" -ne 0 ]; then
    kill -TERM "$child_pid" 2>/dev/null || true
  fi
}

trap forward_signal TERM INT

while :; do
  binary="$(pick_binary)"
  "$binary" "$@" &
  child_pid=$!
  wait "$child_pid"
  code=$?
  child_pid=0

  if [ "$terminating" -eq 1 ]; then
    exit "$code"
  fi

  if [ "$code" -eq "$EXIT_HANDOVER" ]; then
    # 热更新：监听套接字已在新实例手里，这里不再拉起第二个实例，
    # 只保持 PID 1 存活，容器不因主进程退出而被销毁。
    echo "[entrypoint] hot update handover complete; new instance is serving, staying alive"
    # 收到 SIGTERM（docker stop / compose down）时跳出，正常退出容器。
    while [ "$terminating" -eq 0 ]; do
      sleep 3600 &
      sleeper=$!
      wait "$sleeper" || true
      kill "$sleeper" 2>/dev/null || true
    done
    echo "[entrypoint] stopping after handover"
    exit 0
  fi

  echo "[entrypoint] gateway exited with code $code"
  exit "$code"
done
