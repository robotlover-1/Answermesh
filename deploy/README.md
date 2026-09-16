# AnswerMesh 公网部署（deploy/）

拓扑与主方案：docs/deploy/2026-09-05-echo-chat-tunnel-vm-public-deployment-design.md
直连方案（隧道移除后）：docs/deploy/2026-09-06-echo-chat-direct-public-deployment-and-tunnel-removal.md

## 双节点角色与资源
- 本地应用节点：完整 AnswerMesh（docker/compose，127.0.0.1:7080 仅回环）+ frpc（deploy/app）。建议 ≥4核8GB / 80GB；本地断电/休眠/断网即断公网。
- 公网边缘节点：仅 frps + Nginx + Certbot（deploy/edge），2核2G / 40GB 即可；不运行 AnswerMesh/MySQL/语义模型。
- 请求链路：`https://域名 → 云端Nginx:443 → 云端127.0.0.1:39001(frps HTTP vhost) → FRP隧道 → 本地frpc → 127.0.0.1:7080`。
- 域名：A 记录 `chat.example.com → 云端公网IPv4`（与本地宽带无关；大陆服务器网站需 ICP 备案）。
- 变量对照：见 deploy/{app,edge}/.env.example（common: PUBLIC_DOMAIN/FRP_AUTH_TOKEN 两端一致）。
- FRP token：在两个节点中**选一个**跑 `openssl rand -hex 32`，再把同一个串**手动填进两边的 `.env`**。
  它只是打印一串随机字符，不会自动写进任何文件；只填一端 → frps 回 `token in login doesn't match`，隧道建不起来。
  已有部署排障时**先查云端正在用的那个值**（`docker exec <frps容器> grep "token:" /app/config.yaml`），而不是重新生成。

## 一键
- 应用: cp deploy/app/.env.example deploy/app/.env && sudo bash deploy/scripts/deploy-app.sh
- 入口: cp deploy/edge/.env.example deploy/edge/.env && sudo bash deploy/scripts/deploy-edge.sh
- 分层验收: bash deploy/scripts/smoke-test.sh app|edge|e2e
- 首启顺序：可先跑 deploy-edge.sh（只验云端 frps/Nginx/TLS，见下），本地 frpc 上线后再跑 smoke-test.sh。
- deploy-edge.sh 结尾：`/edge-healthz` 必 200；`/api/health` 按 200=通 / 502·503·504=本地未就绪 / 其它=报错 分类，**不把本地未上线误报为云端失败**。

## MySQL（本地应用节点，宿主机/外部）
CREATE DATABASE ai_chat CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'answermesh'@'%' IDENTIFIED BY '<STRONG_PASSWORD>';
GRANT ALL PRIVILEGES ON ai_chat.* TO 'answermesh'@'%';
FLUSH PRIVILEGES;
# MYSQL_DSN=answermesh:<STRONG_PASSWORD>@tcp(host.docker.internal:3306)/ai_chat?charset=utf8mb4&parseTime=true

## 镜像注意
- frps/frpc 默认 snowdreamtech:0.62.1，**两端版本必须一致**；Dashboard 默认关闭。
- 上线前核对镜像（P1-6）：架构、ENTRYPOINT、配置路径、`verify -c` 可用、目标机 `docker image inspect` 后固定 digest。

## 证书续期（edge）
- /etc/letsencrypt/renewal-hooks/deploy/reload-answermesh-nginx.sh:
    #!/usr/bin/env bash
    docker compose -f /opt/answermesh-edge/compose.yaml exec -T nginx nginx -s reload
- sudo certbot renew --dry-run 验证。

## 可观测性（Dashboard 关闭后的补位）
- 容器存活：docker compose ps / systemd；39000 监听：ss -lnt。
- 公网探测：`/edge-healthz`(TLS/Nginx) 与 `/api/health`(业务) 定时 curl。
- 告警建议：5xx 比例、带宽、CPU、内存、磁盘；frpc 离线检测（frps 日志 no proxy / smoke 失败即告警）。
- 2核2G 守护：可加 1–2GB swap（master §7.2）；容器内存上限需先在真实流量采样再定（量级：nginx ~256MB、frps 256–512MB），勿用未验证硬限。

## 安全组
- 39000 访问策略：本地出口公网IP稳定→安全组仅限该 IP；动态(家庭/移动宽带)→临时开放时**必须** frps TLS 强制 + 高强度 token + 登录失败监控 + 定期轮换 token。不要长期不限来源。

## 交付边界
- 本仓库改动为静态验证（go build/test、nginx -t、yaml、bash -n）。
- 公网 TLS/登录/流式/断线/回滚验收在目标 VM 执行（smoke-test.sh + 主方案 §9/§13）。
- 单机演示（frpc serverAddr=127.0.0.1）：仅作功能测试，**非推荐生产方案**（生产用双节点）。

## 上线前门禁（合并 main / 正式上线前须完成，结果记入验收文档）
- 真实 2核2G 云主机跑通 deploy-edge.sh（含首次证书申请）；本地 deploy-app.sh 后 L1–L4 分层通过。
- T 验收：见 docs/deploy/2026-09-06-echo-chat-direct-public-deployment-and-tunnel-removal.md §13（T01–T18）。
- 演练：frpc 断连/恢复、云主机重启自动恢复、certbot renew --dry-run + reload hook。
- 端口扫描确认：80/443/39000 符合策略；39001/7500 公网不可达。

## Secret 纪律
- .env 与 render 产物已 gitignore；提交前 bash deploy/scripts/scan-secrets.sh。
- docker/config/*.yaml 需先 render-config app（见 docker/README.md）。
