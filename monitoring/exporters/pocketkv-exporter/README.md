# pocketkv-exporter

把 PocketKV 的 `INFO` / `MEMSTAT` 转换为 Prometheus 指标。独立进程、只读命令，
不在 PocketKV C 运行时里增加 HTTP 复杂度。

## 构建与运行

```bash
make build          # bin/pocketkv-exporter，版本取 git 短 SHA
make test           # RESP 分片解析、指标映射、目标断线容错

# 一个 Exporter 对应一个 PocketKV 实例
./bin/pocketkv-exporter \
  --target=127.0.0.1:5160 \
  --listen=127.0.0.1:9401 \
  --role=master \
  --scrape-timeout=1500ms
```

参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--target` | `127.0.0.1:5160` | PocketKV RESP 地址 |
| `--listen` | `127.0.0.1:9401` | Exporter 监听地址，默认仅本机 |
| `--scrape-timeout` | `1500ms` | 单条 INFO/MEMSTAT 超时，范围 (0,10s] |
| `--role` | `auto` | `master`/`slave`/`auto`（auto 从 INFO 读取） |
| `--version` | — | 打印版本后退出 |

## 抓取行为

- 每次 scrape 只发 `INFO` + `MEMSTAT` 两条只读命令，连接跨 scrape 复用，出错后重连。
- RESP 解析按 Bulk 声明长度读取，保留 DATA 内的 `\r\n`；支持 Simple String / Error /
  Integer / Bulk / Array，单行与 Bulk 均有长度上限。
- 未知字段忽略；已映射字段或 class 行无法解析时累加
  `pocketkv_exporter_parse_errors_total`，不会 panic。
- 目标不可达时仍返回 `pocketkv_exporter_scrape_success 0` 和
  `pocketkv_build_info`，Prometheus 可据此告警。

## 指标（v1）

| 指标 | 类型 | 来源 |
|---|---|---|
| `pocketkv_allocator_alloc_calls_total` / `free_calls_total` | Counter | MEMSTAT |
| `pocketkv_allocator_allocated_bytes` / `requested_bytes` | Gauge | MEMSTAT |
| `pocketkv_allocator_fragmentation_ratio` / `slab_utilization_ratio` | Gauge | MEMSTAT |
| `pocketkv_allocator_class_{pages,chunks,free_chunks,page_bytes}{size_class}` | Gauge | MEMSTAT（19 类） |
| `pocketkv_replication_{master,applied,durable}_offset_bytes` | Gauge | INFO |
| `pocketkv_commit_{gap,timeout,desync}_total` | Counter | INFO |
| `pocketkv_rdma_fullsync_jobs{state}` | Gauge | INFO |
| `pocketkv_rdma_fullsync_{completed,failed}_total` | Counter | INFO |
| `pocketkv_rdma_{cq_errors,cm_rejects,resource_rejects}_total` | Counter | INFO |
| `pocketkv_rdma_connected_jobs` | Gauge | INFO |
| `pocketkv_aof_{submitted,written}_bytes` | Gauge | INFO |
| `pocketkv_aof_outstanding_batches` | Gauge | INFO |
| `pocketkv_aof_batch_count_total` | Counter | INFO |
| `pocketkv_aof_batch_duration_seconds_total` | Counter | INFO（配合 count 求平均） |
| `pocketkv_aof_error_count_total` | Counter | INFO |
| `pocketkv_aof_max_batch_age_seconds` | Gauge | INFO |
| `pocketkv_replica_{applied,durable}_offset_bytes{slot}` | Gauge | INFO（eBPF slot） |
| `pocketkv_replica_ack_age_seconds{slot}` | Gauge | INFO（eBPF slot） |
| `pocketkv_replica_transport_state{slot,state}` | Gauge | INFO（固定 state 枚举） |
| `pocketkv_proxy_recovery_cache_bytes` | Gauge | INFO |
| `pocketkv_repl_{fullsync,partialsync_ok,partialsync_err}_total` | Counter | INFO |
| `pocketkv_connected_slaves` / `pocketkv_replicas` | Gauge | INFO |
| `pocketkv_build_info{version,role}` | Gauge | 启动参数 |
| `pocketkv_exporter_scrape_success` | Gauge | Exporter |
| `pocketkv_exporter_scrape_duration_seconds` | Gauge | Exporter |
| `pocketkv_exporter_parse_errors_total` | Counter | Exporter |

`size_class` 使用实际 class size（如 `{size_class="136"}`），基数固定为 19，不使用
key/transfer_id/user_id 等动态值作标签。`aof_submitted` 与 `aof_written` 单位同为字节，
差额即持久化滞后，可直接在 PromQL 中相减。

## Prometheus

```yaml
  - job_name: pocketkv
    scrape_timeout: 2s
    static_configs:
      - targets: ["127.0.0.1:9401"]
        labels: { instance: "kv-master", role: "master" }
      - targets: ["127.0.0.1:9402"]
        labels: { instance: "kv-slave-a", role: "slave" }
```

Exporter 只监听内网/loopback；不要把 9401 暴露到公网。

## 限制

v2 已增加 per-slot（eBPF slot 0..7）offset/ACK 年龄/传输状态，以及 AOF 批次数、累计
write+fsync 耗时、在途批次与错误计数（原子累加，不在命令热路径打日志）。RDMA
posted/completed 字节与 reactor 命令耗时直方图仍需下一轮埋点；AOF 直方图暂以
count+sum 暴露，Prometheus 侧用 rate 比值得平均耗时。
