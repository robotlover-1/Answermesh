# AnswerMesh：MySQL 可靠性 / 配置安全 / zRPC 指标整改

> 对应评审：`01_两个项目最新源码复查与修正建议_2026-10-29` 的 RV-04 / RV-05。
> 本轮改动已通过 `go build ./...` 与 `go test ./...`；未在完整 MySQL/Redis/LLM 栈上做压测。

## 1. MySQL 初始化与连接池

`ai-chat-service/pkg/db/mysql.InitMysql`：

- 空 DSN 返回明确错误，不再 panic；
- `sql.Open` 后做带 5s 超时的 `PingContext`，失败返回错误并关闭连接池；
- 设置 `MaxOpenConns/MaxIdleConns/ConnMaxLifetime/ConnMaxIdleTime`；
- 新增 `Close()`；
- `chat-server/main.go` 用 `log.FatalF` 退出并 `defer recordsData.Close(...)`。

## 2. chat_records 写入

`ai-chat-service/chat-server/data/chat_records.go`：

- 接口全部带 `context.Context`：`Add(ctx, record)` / `GetById(ctx, id)` / `Close(ctx)`；
- 使用 `ExecContext`/`QueryRowContext`，检查 `LastInsertId` 错误；
- 有界异步写入：固定 4 个 worker + 2048 容量队列，`Add` 只入队；
  - 队列满时按调用方 ctx（服务端 200ms）背压/失败，不再每请求启动无界 goroutine；
  - 入库失败统一记录错误（不打印用户问题/答案/关键词等敏感内容）；
  - `Close` 停止接收并等待队列排空（受 ctx 限制）。

`chat-server/server/server.go` 的普通/流式两条路径统一改为 `enqueueRecord()`，
缓存写入仍异步但带 2s 超时；`QuestionsTotalCounter` 在入队前统一计数。

## 3. 配置/凭据

- 移除仓库中的 MySQL 明文密码、服务 access token、API key 等；YAML 改为
  `${AI_CHAT_*}` / `${OPENAI_PROXY_ACCESS_TOKEN}` 占位符。
- `ai-chat-service`、`ai-chat-backend`、`keywords-filter`、`openai-api-proxy`
  的配置加载器支持 `${VAR}` 展开与显式 env 覆盖。
- `docker/compose.yaml`、`ai-chat-stack/compose.yaml` 为相关服务增加
  `environment:` 变量透传；`deploy/app/.env.example`、`ai-chat-service/dev.env.example`
  提供变量清单（不含真实值）。
- 需要运维侧处理：**轮换此前已提交到 git 的 token/数据库密码**，并在部署环境
  通过 Secret/环境变量注入。

## 4. 可观测性

`chat-server/metrics-bus` 新增：

- `ai_chat_chat_service_zrpc_unary_duration_seconds{method,status}`
- `ai_chat_chat_service_zrpc_stream_duration_seconds{method,status}`
- `ai_chat_chat_service_records_write_errors_total`

`RegisterChatZRPC` 现在包装 unary/stream handler 记录耗时；标签只含 method/status，
不写入问题文本、答案、token 或 DSN。旧 gRPC interceptor 指标保留但不再代表 zRPC 新流量。

## 5. 验证与后续

```bash
cd ai-chat-service && go build ./... && go test ./...
cd ../ai-chat-backend && go build ./...
cd ../keywords-filter && go build ./...
cd ../openai-api-proxy && go build ./...
```

后续（本轮未做）：

- 在真实 MySQL/Redis/LLM 栈上跑 zRPC 请求，确认 `zrpc_*_duration_seconds` 的
  count/P99 随真实流量增长；
- 为 `chat_records` 增加迁移/索引与历史查询（PR-M1），以及 Outbox/事务（PR-M2）；
- 对已暴露凭据完成外部轮换并更新部署 Secret。
