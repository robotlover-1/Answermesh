# ai-chat 监控（Prometheus + Grafana）

采集并可视化 `ai-chat-service :8080/metrics`，独立于 8 个服务启停。

## 目录

```
monitoring/
├── bin/                  # prometheus / promtool / mysqld_exporter（下载，gitignore）
├── grafana/              # grafana 解压目录 = homepath（gitignore）
├── exporters/pocketkv-exporter/  # PocketKV INFO/MEMSTAT → Prometheus
├── prometheus/prometheus.yml     # scrape 配置（answermesh/pocketkv/mysql/node）
├── prometheus/alerts.yml         # 复制/AOF/RDMA/业务/MySQL 告警规则
├── config/grafana/provisioning/  # Prometheus + MySQL 只读数据源、看板自动配置
├── dashboards/01..08-*.json      # 8 块主看板（ai-chat.json 为旧版，06 为替代）
├── logs/ pids/           # 运行状态（gitignore）
└── start.sh stop.sh
```

## 首次准备（解压二进制）

压缩包在 `/home/pp/Desktop/ls_study/proj/`：

```bash
cd /home/pp/Desktop/ls_study/proj/9.1-kvstore/monitoring
# prometheus（顶层目录 prometheus-2.45.0.linux-amd64/）
tar xzf /home/pp/Desktop/ls_study/proj/prometheus-2.45.0.linux-amd64.tar.gz -C /tmp
cp /tmp/prometheus-2.45.0.linux-amd64/prometheus bin/
cp /tmp/prometheus-2.45.0.linux-amd64/promtool bin/
# grafana（顶层目录 grafana-v10.4.0/，改名 grafana）
tar xzf /home/pp/Desktop/ls_study/proj/grafana-10.4.0.linux-amd64.tar.gz
mv grafana-v10.4.0 grafana
```

## 使用

```bash
cd /home/pp/Desktop/ls_study/proj/9.1-kvstore/monitoring
./start.sh    # 幂等；起 Prometheus :9090 + Grafana :3000
./stop.sh     # 停止；./stop.sh 无参数
```

- Prometheus Web UI：http://localhost:9090（Targets 页看采集状态）
- Grafana：http://localhost:3000（admin / admin）→ 数据源与看板已自动配置

## 指标暴露与访问控制

- ai-chat-service 的 `/metrics` 默认只监听 `127.0.0.1:8080`（`server.metrics_addr`）；
  跨主机/容器抓取时显式改成 `0.0.0.0:8080`，并用内网策略限制来源。
- 需要认证时设置 `AI_CHAT_METRICS_TOKEN`（YAML 的 `server.metrics_access_token`），
  服务端会对 `/metrics` 校验 `Authorization: Bearer <token>`；同时在
  `prometheus/prometheus.yml` 打开注释的 `authorization` 段并导出同名 env。
- 看板使用真实的 zRPC histogram（`ai_chat_chat_service_zrpc_{unary,stream}_duration_seconds_*`）、
  `go_goroutines`，以及入库队列指标 `records_enqueue_failures_total{reason}` /
  `records_db_insert_failures_total{reason}` / `records_queue_length`。
  旧 gRPC `requests_total` / `request_duration_ms` / `curr_num_goroutine` 已不再产生数据。
- 应用指标沿用既有 `ai_chat_chat_service_*` 命名空间（方案文档中的 `answermesh_*` 为示意名），
  避免已有告警/历史数据失效；Token 指标用 `llm_tokens_total{kind="input|output|estimated_saved"}`
  区分模型实际用量与缓存命中估算节省。

## PocketKV Exporter

```bash
cd monitoring/exporters/pocketkv-exporter
make build && make test
# 一个 Exporter 对应一个 PocketKV 实例：master 9401，slave 9402/9403
./bin/pocketkv-exporter --target=127.0.0.1:5160 --listen=127.0.0.1:9401 --role=master
./bin/pocketkv-exporter --target=127.0.0.1:5161 --listen=127.0.0.1:9402 --role=slave
```

只发 `INFO` + `MEMSTAT` 两条只读命令，覆盖 19 个 slab class、复制 offset、Commit Gate、
RDMA job、AOF 滞后与 proxy 恢复缓存。目标不可达时仍暴露 `pocketkv_exporter_scrape_success 0`。
详见 [exporters/pocketkv-exporter/README.md](exporters/pocketkv-exporter/README.md)。

`./start.sh` 会在二进制存在且本地 5160/5161/5162 有监听时，自动启动 9401/9402/9403 三个
Exporter（角色依次 master/slave/slave）。端口可用
`POCKETKV_EXPORTER_PORTS="..." POCKETKV_EXPORTER_LISTEN="..."` 覆盖；
`bin/mysqld_exporter` / `bin/node_exporter` 存在时也会一并启动。

## Prometheus / 告警 / 看板

- `prometheus/prometheus.yml` 已配置 `answermesh`、`pocketkv`（3 实例）、`mysql`、`node` 四个 job；
  容器部署必须把 `127.0.0.1` 换成 compose service DNS。
- 修改后执行 `bin/promtool check config prometheus/prometheus.yml`，并在 Targets 页确认全部 UP。
- `prometheus/alerts.yml` 覆盖：Exporter 不可达、AOF fatal、Commit Gap/Desync、RDMA CQ 错误、
  副本 ACK 过期、AOF 滞后、zRPC 错误率、入库失败/积压、MySQL 连接/锁等待。
- 看板：01 存储概览、02 内存池、03 多 Slave 复制、04 RDMA、05 AOF、
  06 AnswerMesh 业务、07 MySQL 运维、08 MySQL 业务。

## MySQL 监控与只读数据源

**mysqld_exporter（数据库健康）**：使用独立监控用户，不要复用业务账号：

```sql
CREATE USER 'mysql_exporter'@'127.0.0.1'
  IDENTIFIED BY '<STRONG_SECRET>' WITH MAX_USER_CONNECTIONS 3;
GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'mysql_exporter'@'127.0.0.1';
```

`mysqld_exporter >= 0.15` 不再读取 `DATA_SOURCE_NAME`，改用 `/etc/mysqld_exporter.cnf`
（`chmod 600`，禁止进 Git）：

```ini
[client]
user=mysql_exporter
password=<STRONG_SECRET>
```

```bash
mysqld_exporter --config.my-cnf=/etc/mysqld_exporter.cnf --web.listen-address=127.0.0.1:9104
```

**Grafana MySQL 数据源（业务查询）**：只读账号 + 最小权限：

```sql
CREATE USER 'grafana_ro'@'<GRAFANA_HOST>' IDENTIFIED BY '<STRONG_SECRET>';
GRANT SELECT ON ai_chat.* TO 'grafana_ro'@'<GRAFANA_HOST>';
```

Grafana 启动前导出 `MYSQL_RO_HOST`、`MYSQL_RO_DB`、`MYSQL_RO_USER`、`MYSQL_RO_PASSWORD`，
`config/grafana/provisioning/datasources/mysql.yml` 会自动注册 uid=`mysql` 的数据源。
业务查询应限制时间范围/频率（1–5 分钟），优先查 `usage_daily` 等预聚合表，避免 5s 扫原始聊天表。

## 数据库迁移（PR-M1 起）

```bash
cd ai-chat-service
make -C .. build           # 生成 bin/ai-chat-migrate
../bin/ai-chat-migrate -list
../bin/ai-chat-migrate -dry-run
AI_CHAT_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/ai_chat?parseTime=true&charset=utf8mb4' \
  ../bin/ai-chat-migrate   # 执行 up
../bin/ai-chat-migrate -down 1
```

迁移带 `GET_LOCK` 串行化、`schema_migrations` 版本表与 up SQL SHA-256 校验；
只能由一个部署 Job 执行，应用副本启动时不自动建表。

## 注意事项

- 监控独立于 ai-chat 主链路；缺二进制时 `start.sh` 报错退出，不影响 8 个服务
- Prometheus 数据落在 `prometheus/data/`，Grafana 数据落在 `grafana/data/`（gitignore，删掉即重置）
- 指标计数是进程内的，服务重启会清零（由 Prometheus 侧时序持久化，重启 Prometheus 后历史保留）
- 停止时按端口 `fuser` 兜底，会杀掉占用 9090/3000 的**任何**进程（当前仅监控进程占用，无碍；若以后端口被复用需留意）
