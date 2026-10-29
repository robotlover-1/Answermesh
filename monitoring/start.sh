#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for _cmd in ss fuser; do
  command -v "$_cmd" >/dev/null 2>&1 || { echo "✘ 缺依赖命令: $_cmd（请安装 iproute2/psmisc）"; exit 1; }
done
LOG_DIR="$ROOT/logs"
PID_DIR="$ROOT/pids"
mkdir -p "$LOG_DIR" "$PID_DIR" "$ROOT/prometheus/data" "$ROOT/grafana/data"

if [ ! -x "$ROOT/bin/prometheus" ]; then
  echo "✘ 缺 prometheus 二进制: $ROOT/bin/prometheus（请按 README.md 解压）"; exit 1
fi
if [ ! -x "$ROOT/grafana/bin/grafana" ]; then
  echo "✘ 缺 grafana: $ROOT/grafana/bin/grafana（请按 README.md 解压）"; exit 1
fi

port_listening() { ss -tln 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${1}$"; }

start_prom() {
  if port_listening 9090; then echo "  [prometheus] ✔ already running (9090)"; return 0; fi
  # exec 让后台 job 的 PID 就是 prometheus 本体（否则 pid 文件记录的是中间 subshell）。
  ( cd "$ROOT" && exec nohup "$ROOT/bin/prometheus" \
      --config.file=prometheus/prometheus.yml \
      --storage.tsdb.path=prometheus/data \
      --web.listen-address=:9090 \
      >"$LOG_DIR/prometheus.log" 2>&1 ) & echo $! > "$PID_DIR/prometheus.pid"
  local i
  for i in $(seq 1 40); do port_listening 9090 && { echo "  [prometheus] ✔ 9090 listening"; return 0; }; sleep 0.5; done
  echo "  [prometheus] ✘ 启动失败, 日志尾部:"; tail -n 8 "$LOG_DIR/prometheus.log" | sed 's/^/    /'; return 1
}

start_graf() {
  if port_listening 3000; then echo "  [grafana] ✔ already running (3000)"; return 0; fi
  ( cd "$ROOT" && GF_SERVER_HTTP_PORT=3000 \
      GF_PATHS_DATA="$ROOT/grafana/data" \
      GF_PATHS_PROVISIONING="$ROOT/config/grafana/provisioning" \
      GF_SECURITY_DISABLE_INITIAL_ADMIN_PASSWORD_CHANGE=true \
      exec nohup "$ROOT/grafana/bin/grafana" server --homepath="$ROOT/grafana" \
      >"$LOG_DIR/grafana.log" 2>&1 ) & echo $! > "$PID_DIR/grafana.pid"
  local i
  for i in $(seq 1 60); do port_listening 3000 && { echo "  [grafana] ✔ 3000 listening"; return 0; }; sleep 0.5; done
  echo "  [grafana] ✘ 启动失败, 日志尾部:"; tail -n 8 "$LOG_DIR/grafana.log" | sed 's/^/    /'; return 1
}

# 启动一个常驻进程并等待端口就绪；失败只告警，不影响 Prometheus/Grafana。
start_daemon() {
  local name="$1" port="$2" log="$3"; shift 3
  if port_listening "$port"; then echo "  [$name] ✔ already running ($port)"; return 0; fi
  ( cd "$ROOT" && exec nohup "$@" >"$LOG_DIR/$log" 2>&1 ) & echo $! > "$PID_DIR/$name.pid"
  local i
  for i in $(seq 1 20); do port_listening "$port" && { echo "  [$name] ✔ $port listening"; return 0; }; sleep 0.25; done
  echo "  [$name] ✘ 启动失败, 日志尾部:"; tail -n 5 "$LOG_DIR/$log" 2>/dev/null | sed 's/^/    /'; rm -f "$PID_DIR/$name.pid"; return 1
}

# PocketKV Exporter：本地若已运行 5160/5161/5162，则分别映射到 9401/9402/9403。
# 端口/角色可用 POCKETKV_EXPORTER_PORTS / POCKETKV_EXPORTER_LISTEN 覆盖（空格分隔）。
start_pocketkv_exporters() {
  local bin="$ROOT/exporters/pocketkv-exporter/bin/pocketkv-exporter"
  [ -x "$bin" ] || { echo "  [pocketkv-exporter] 未构建，跳过（make -C exporters/pocketkv-exporter build）"; return 0; }
  local ports=(${POCKETKV_EXPORTER_PORTS:-5160 5161 5162})
  local listen=(${POCKETKV_EXPORTER_LISTEN:-9401 9402 9403})
  local roles=(master slave slave)
  local i
  for i in "${!ports[@]}"; do
    [ "${i}" -lt "${#listen[@]}" ] || break
    port_listening "${ports[$i]}" || continue
    start_daemon "pocketkv-exporter-${listen[$i]}" "${listen[$i]}" "pocketkv-exporter-${listen[$i]}.log" \
      "$bin" --target="127.0.0.1:${ports[$i]}" --listen="127.0.0.1:${listen[$i]}" --role="${roles[$i]:-auto}"
  done
}

# 可选外部 exporter：二进制存在才启动。
start_optional_exporters() {
  if [ -x "$ROOT/bin/mysqld_exporter" ]; then
    local cnf_args=()
    [ -f "$ROOT/config/mysqld_exporter.cnf" ] && cnf_args=(--config.my-cnf="$ROOT/config/mysqld_exporter.cnf")
    start_daemon mysqld_exporter 9104 "mysqld_exporter.log" \
      "$ROOT/bin/mysqld_exporter" "${cnf_args[@]}" --web.listen-address=127.0.0.1:9104
  fi
  if [ -x "$ROOT/bin/node_exporter" ]; then
    start_daemon node_exporter 9100 "node_exporter.log" \
      "$ROOT/bin/node_exporter" --web.listen-address=127.0.0.1:9100
  fi
}

start_pocketkv_exporters
start_optional_exporters
start_prom
start_graf
echo
echo "✔ Prometheus: http://localhost:9090  （/targets 看采集状态）"
echo "✔ Grafana:    http://localhost:3000  （admin/admin）"
