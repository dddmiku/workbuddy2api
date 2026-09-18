#!/bin/sh
# ═══ 更新日志 ═══
# 2026-09-17：新增 PID 1 监督脚本，支撑容器内热更新：网关以子进程运行，热更新时
#             子进程把监听套接字交给新实例后以 75 退出，本脚本不重启、保持容器存活，
#             由新实例继续服务；收到 SIGTERM 时转发信号并随子进程退出。
# 2026-09-17：交接完成后的等待循环响应 SIGTERM，避免 docker stop 只能靠超时杀容器。
# 2026-09-18：监督热更新后的后继进程，TERM 转发整个服务子进程树并等待退出；后继异常退出时让重启策略生效。
# 2026-09-18：解析同一配置文件和状态目录，向 Go 传递统一的更新目录，保证自定义位置重启后仍生效。
# 2026-09-18：用 shell 内建读取子进程状态，降低监督循环的额外进程开销。
set -u

# 默认二进制（镜像内）；存在热更新落地的 current 指针时优先用它，重启后仍是新版本。
IMAGE_BIN="/app/wb2api"
UPDATE_DIR="$(python3 - "$@" <<'PY'
import json
import os
import sys


def object_fields(value, name):
    if value is None:
        return {}
    if not isinstance(value, dict):
        raise ValueError(name + " must be an object")
    return {key.lower(): item for key, item in value.items()}


def text_field(value, name, default=""):
    if value is None:
        return default
    if not isinstance(value, str):
        raise ValueError(name + " must be a string")
    return value


try:
    directory = os.environ.get("WB2API_UPDATE_DIR", "").strip()
    if not directory:
        config_path = "config.json"
        args = sys.argv[1:]
        index = 0
        while index < len(args):
            arg = args[index]
            if arg == "--" or not arg.startswith("-"):
                break
            if arg in ("-config", "--config"):
                index += 1
                if index >= len(args):
                    raise ValueError("-config requires a path")
                config_path = args[index]
            elif arg.startswith(("-config=", "--config=")):
                config_path = arg.split("=", 1)[1]
            index += 1
        config = {}
        if config_path:
            try:
                with open(config_path, encoding="utf-8") as handle:
                    config = object_fields(json.load(handle), "config")
            except FileNotFoundError:
                pass
        update = object_fields(config.get("update"), "update")
        directory = text_field(update.get("dir"), "update.dir").strip()
        if not directory:
            state_file = os.environ.get("WB2A_STATE_FILE", "")
            if not state_file:
                state_file = text_field(config.get("state_file"), "state_file", "./data/state.json")
            state_dir = os.path.normpath(os.path.dirname(state_file))
            directory = os.path.normpath(os.path.join(state_dir or "data", "updates"))
            if not state_dir or state_dir == ".":
                directory = os.path.join("data", "updates")
    print(directory)
except (OSError, ValueError) as error:
    print("[entrypoint] cannot resolve update directory: " + str(error), file=sys.stderr)
    sys.exit(1)
PY
)" || exit 1
export WB2API_UPDATE_DIR="$UPDATE_DIR"
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

# Linux 容器 PID 1 会收养热更新后留下的实例。只遍历本监督进程的后代，
# 不依赖已退出的首个 child_pid，也不向容器外或其他进程组广播信号。
signal_tree() {
  signal_children=""
  { read -r signal_children < "/proc/$1/task/$1/children"; } 2>/dev/null || true
  for signal_pid in $signal_children; do
    signal_tree "$signal_pid"
  done
  if [ "$1" -ne "$$" ]; then
    kill -TERM "$1" 2>/dev/null || true
  fi
}

has_live_children() {
  checked_children=""
  { read -r checked_children < "/proc/$$/task/$$/children"; } 2>/dev/null || true
  for checked_pid in $checked_children; do
    [ -r "/proc/$checked_pid/status" ] || continue
    while read -r status_key status_value status_rest; do
      if [ "$status_key" = "State:" ]; then
        case "$status_value" in
          Z|X) break ;;
          *) return 0 ;;
        esac
      fi
    done < "/proc/$checked_pid/status"
  done
  return 1
}

wait_for_children() {
  while has_live_children; do
    sleep 0.1
  done
}

forward_signal() {
  terminating=1
  signal_tree "$$"
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
    # wait 可能被信号提前打断；处理器和最后一笔状态落盘尚未结束时不能退出 PID 1。
    wait_for_children
    exit 0
  fi

  if [ "$code" -eq "$EXIT_HANDOVER" ]; then
    # 热更新：监听套接字已在新实例手里，这里不再拉起第二个实例，
    # 持续监督被收养的后继，避免它退出后容器却永远显示运行。
    echo "[entrypoint] hot update handover complete; supervising the updated instance"
    while has_live_children; do
      if [ "$terminating" -eq 1 ]; then
        wait_for_children
        echo "[entrypoint] stopping after handover"
        exit 0
      fi
      sleep 1 &
      sleeper=$!
      wait "$sleeper" || true
      kill "$sleeper" 2>/dev/null || true
    done
    if [ "$terminating" -eq 1 ]; then
      echo "[entrypoint] stopping after handover"
      exit 0
    fi
    echo "[entrypoint] updated instance exited unexpectedly"
    exit 1
  fi

  echo "[entrypoint] gateway exited with code $code"
  exit "$code"
done
