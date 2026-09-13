#!/usr/bin/env bash
# ai-chat 服务定义与公共工具 —— 被 start.sh / stop.sh source
# 每个服务一行：name|port|cwd|command
# cwd 是启动工作目录（配置文件里 logPath/aof 为相对路径，依赖 CWD）
set -u

BASE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"   # 现在就是仓库根
LOG_DIR="$BASE/runtime/logs"
PID_DIR="$BASE/runtime/pids"

# 顺序即启动顺序（依赖方在后），停止时按逆序处理
SERVICES=(
  "kvstore|5160|$BASE|./kvstore/kvstore/kvstore configs/kvstore-ai.conf"
  "tokenizer|3002|$BASE/tokenizer|nuxt --port 3002 --module tokenizer.py --workers 2"
  "semantic|3003|$BASE/semantic|nuxt --port 3003 --module semantic.py --workers 2"
  "sensitive|50053|$BASE/keywords-filter|$BASE/bin/keywords-filter --config=dev.config.yaml --dict=dict.txt"
  "keyword|50054|$BASE/keywords-filter|$BASE/bin/keywords-filter --config=dev.kw.config.yaml --dict=keyword-dict.txt"
  "mock|8083|$BASE/mock-openai-api|$BASE/bin/mock-openai-api --config=dev.config.yaml"
  "proxy|8084|$BASE/openai-api-proxy|$BASE/bin/openai-api-proxy --config=dev.config.yaml"
  "service|50055|$BASE/ai-chat-service|$BASE/bin/ai-chat-service --config=dev.config.yaml"
  "backend|7080|$BASE/ai-chat-backend|$BASE/bin/ai-chat-backend --config=dev.config.yaml"
)

# frpc 公网隧道：不在 SERVICES 里（它不监听本地端口，无端口可探活），
# 由 start.sh 的 ensure_frpc / stop.sh 单独处理。
FRPC_NAME="frpc"
FRPC_BIN="$BASE/bin/frpc"
FRPC_ENV="$BASE/deploy/app/.env"
FRPC_TEMPLATE="$BASE/deploy/app/tunnel/frpc.yaml.envsubst"
FRPC_CONFIG="$BASE/deploy/app/tunnel/frpc.yaml"
FRPC_PUBLIC_PORT=39000          # 云端 frps 控制口，用于探测隧道是否已建立

SERVICE_NAMES="$(for e in "${SERVICES[@]}"; do echo "${e%%|*}"; done | tr '\n' ' ')"

logfile() { echo "$LOG_DIR/$1.log"; }
pidfile() { echo "$PID_DIR/$1.pid"; }

# 把 env 文件的 KEY=VALUE 安全导出到当前环境（逐行解析，不做 eval/source；值含 & # 空格也安全）。
# 解析规则与 deploy/scripts/lib.sh 的 require_env 保持一致 —— 改动请同步两处。
# 同名变量以文件为准（覆盖已有环境变量），与「.env 文件优先于环境变量」的既有约定一致。
load_env_file() {
  local envfile="$1" _line _key _val
  [ -f "${envfile}" ] || return 0
  while IFS= read -r _line || [ -n "${_line}" ]; do
    _line="${_line%$'\r'}"
    if [ -z "${_line}" ] || [ "${_line#\#}" != "${_line}" ] || [ "${_line}" = "${_line%%=*}" ]; then
      continue
    fi
    if [ "${_line#export }" != "${_line}" ]; then _line="${_line#export }"; fi
    _key="${_line%%=*}"; _val="${_line#*=}"
    if [ "${_val}" != "${_val#\"}" ] && [ "${#_val}" -ge 2 ] && [ "${_val%\"}" != "${_val}" ]; then
      _val="${_val#\"}"; _val="${_val%\"}"
    fi
    if [ "${_val}" != "${_val#\'}" ] && [ "${#_val}" -ge 2 ] && [ "${_val%\'}" != "${_val}" ]; then
      _val="${_val#\'}"; _val="${_val%\'}"
    fi
    declare -gx "${_key}=${_val}"
  done < "${envfile}"
}

# 端口是否在监听（ss -tln 的 Local Address 列，IPv4 形如 0.0.0.0:5160，IPv6 形如 *:50053）
port_listening() {
  local port="$1"
  ss -tln 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}$"
}

pid_alive() {
  local pid="$1"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

# 隧道是否真的建成 —— 以 frpc 日志为准（进程存在 ≠ 已登录成功）。
#
# 曾经用"到 frps 控制口有 ESTABLISHED 连接"来判断，是错的：frpc 配了
# loginFailExit: false，token 不符时会每 10s 重连一次，每次连接都短暂处于
# ESTABLISHED，于是"正在重试"被误判成"已登录"，start.sh 报出假的 ✔。
#
# 判据：最近一次成功（login to server success / start proxy success）之后，
# 没有再出现失败（token 不符 / 连接错误）。只看尾部的日志，避免很久以前的成功
# 记录掩盖当前故障。
frpc_registered() {
  local log pos_ok pos_err
  # 进程不在 → 一律算未建立（否则上一次会话留下的旧"成功"记录会骗过判断）
  pid_alive "$(cat "$(pidfile "$FRPC_NAME")" 2>/dev/null)" || return 1
  log="$(logfile "$FRPC_NAME")"
  [ -f "$log" ] || return 1
  pos_ok="$(tail -n 200 "$log" | grep -nE "login to server success|start proxy success" | tail -1 | cut -d: -f1)"
  pos_err="$(tail -n 200 "$log" | grep -nE "token in login doesn't match|connect to server error|login to server failed" | tail -1 | cut -d: -f1)"
  [ -n "$pos_ok" ] || return 1
  if [ -n "$pos_err" ] && [ "$pos_err" -gt "$pos_ok" ]; then
    return 1
  fi
  return 0
}

# 日志里是否有"token 不匹配"这一确定性失败（供调用方给出针对性提示）。
frpc_token_mismatch() {
  local log; log="$(logfile "$FRPC_NAME")"
  [ -f "$log" ] && grep -q "token in login doesn't match" "$log"
}
