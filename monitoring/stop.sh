#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for _cmd in ss fuser; do
  command -v "$_cmd" >/dev/null 2>&1 || { echo "✘ 缺依赖命令: $_cmd（请安装 iproute2/psmisc）"; exit 1; }
done
PID_DIR="$ROOT/pids"

port_listening() { ss -tln 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${1}$"; }

stop_one() {
  local name="$1" port="$2" pf pid
  pf="$PID_DIR/$name.pid"
  if [ -f "$pf" ] && pid_alive "$(cat "$pf")"; then
    pid="$(cat "$pf")"; kill -TERM "$pid"; echo "  [$name] TERM 已发送"
  elif port_listening "$port"; then
    fuser -k -TERM "${port}/tcp" 2>/dev/null || true; echo "  [$name] fuser 回退停止"
  else
    echo "  [$name] 未运行"
  fi
  rm -f "$pf"
}

pid_alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }

echo "== 停止监控 =="
stop_one prometheus 9090
stop_one grafana 3000
for port in 9401 9402 9403; do stop_one "pocketkv-exporter-${port}" "$port"; done
stop_one mysqld_exporter 9104
stop_one node_exporter 9100

for pair in "prometheus 9090" "grafana 3000" "pocketkv-exporter-9401 9401" \
            "pocketkv-exporter-9402 9402" "pocketkv-exporter-9403 9403" \
            "mysqld_exporter 9104" "node_exporter 9100"; do
  set -- $pair
  name="$1"; port="$2"
  for i in $(seq 1 20); do port_listening "$port" || break; sleep 0.5; done
  if port_listening "$port"; then
    echo "  [$name] $port 未释放, fuser 强停"
    fuser -k -KILL "${port}/tcp" 2>/dev/null || true
    sleep 1
  fi
done

remaining=0
for port in 9090 3000 9401 9402 9403 9104 9100; do
  port_listening "$port" && { echo "  [✘] $port 仍在监听"; remaining=1; }
done
if [ "$remaining" -eq 0 ]; then echo "✔ 监控已停止, 端口已释放"; else exit 1; fi
