# AnswerMesh（ai 助手）

零声教学 AI 助手微服务版。独立仓库，kvstore 以 submodule 引入（`kvstore/`），用法与 kvstore 自身引用 NtyCo 一致。

## 仓库结构

```text
AnswerMesh/
├── ai-chat-backend/     ← Go HTTP 网关（页面 + /api/chat-process）
├── ai-chat-service/     ← Go gRPC 核心服务（对话编排 + 语义缓存）
├── keywords-filter/     ← 敏感词/关键词过滤
├── openai-api-proxy/    ← DeepSeek 反向代理（key 走环境变量）
├── mock-openai-api/     ← 离线 mock（无 key 调试用）
├── tokenizer/           ← token 计数（tiktoken，仅计 token）
├── semantic/            ← 语义检索独立服务(:3003)：e5 嵌入 + parse + decision + 安全指纹
├── ai-chat-web/         ← Vue3 前端
├── kvstore/             ← submodule → robotlover-1/pocket-kv（自研 Redis）
└── start.sh / stop.sh   ← 一键启停
```

> 语义检索依赖大模型文件 `semantic/models/e5s-v1/model.onnx`（multilingual-e5-small，ONNX/INT8）。该目录 **gitignored**，不在仓库内；缺失时 semantic 能启动但语义缓存不可用（`start.sh` 会打印提示、Go 优雅 miss，聊天不受影响）。

## 快速开始

```bash
git clone --recurse-submodules https://github.com/robotlover-1/Answermesh.git
cd AnswerMesh

# ① 配置：只填这一次 —— 填你自己的 key / 域名 / token
#    deploy/app/.env 被 gitignore，不会入库，所以每个人 clone 后填自己的那份；
#    填完这一个文件，以后每次都只是 ./start.sh，不用再带任何参数。
cp deploy/app/.env.example deploy/app/.env
vi deploy/app/.env        # 至少填 DEEPSEEK_API_KEY；要公网访问再填 PUBLIC_DOMAIN / FRP_SERVER_ADDR / FRP_AUTH_TOKEN
chmod 600 deploy/app/.env

# ② 语义模型（可选，语义缓存需要；一次即可）
bash semantic/tools/fetch_model.sh          # 从 GitHub Release 下载+sha256 校验（~79MB）
#   或 ECHO_FETCH_MODEL=1 ./start.sh 让 start.sh 缺模型时自动下载。

# ③ 启动（之后永远是这样，无需参数）
./start.sh

# kvstore(C) 与前端 dist 若缺失，start.sh 会自动 make / pnpm 重建；也可手动：
#   ( cd kvstore/kvstore && make )          # 子模块 VSEARCH 前缀参数版

# 另一种环境方案（各节点免装依赖/模型）：全栈 Docker —— 见 docker/README.md
#   cd docker && DEEPSEEK_API_KEY=sk-xxx docker compose up -d --build
```

> 不想建文件也行：`DEEPSEEK_API_KEY=sk-xxx ./start.sh` 直接把值带在命令行上。但**文件存在时以文件为准**，
> 所以要么只用文件、要么只用命令行，别两处都写。公网访问的完整前提见下节。

前置依赖：Go、make/gcc（kvstore）、pnpm/node（前端，仅首次）、Python 3.8+（host 需 `pip install -r semantic/requirements.txt tokenizer/requirements.txt`；nuxt/jieba 等按既有说明）。

## 公网访问

`./start.sh` 默认只监听本机（`http://localhost:7080`）。要从公网访问，本项目走的是 **FRP 内网穿透**：云端一台小机器只做入口转发，整套服务仍然只跑在你本地——**所以前提是先有一台公网云主机**，只有环境变量是不够的。

三个前置条件缺一不可：

| # | 需要什么 | 怎么来 |
|---|---|---|
| 1 | 一台公网云主机，跑着 frps + Nginx | 按 [deploy/README.md](deploy/README.md) 在云主机执行 `deploy/edge`（2C2G 起步） |
| 2 | 一个域名，DNS A 记录指向云主机 IP | 如 `answermesh.xyz`；大陆机房还需 ICP 备案 |
| 3 | `bin/frpc` 客户端（与云端 frps **同版本**，本项目用 0.62.1） | 一条命令：`bash deploy/scripts/fetch_frpc.sh`（`bin/` 已 gitignore，脚本会下载 + 官方 sha256 校验 + 安装） |

### 那三个值分别怎么来

| 变量 | 是什么 | 怎么获取 |
|---|---|---|
| `PUBLIC_DOMAIN` | 你要对外用的域名 | 你自己的域名。去域名服务商控制台加一条 A 记录指向云主机 IP：`example.com.  A  1.2.3.4`（大陆机房还需 ICP 备案） |
| `FRP_SERVER_ADDR` | 云主机的公网 IPv4 | **在云主机上**执行：`curl -s https://ipinfo.io/ip` 或 `curl -s https://ifconfig.me` |
| `FRP_AUTH_TOKEN` | 两端共享的密钥，**一个需要你手动填到两端的配置值** | **已有部署：直接查云端现成在用的那个**（见 ①），不要重新生成。全新部署才需要 `openssl rand -hex 32` 生成一次，再手动填到两端 |

> ⚠️ **`openssl rand -hex 32` 什么都不"设置"。** 它只是往屏幕打印一串随机字符——不改文件、
> 不动配置、两端谁都不会自动知道。它的输出必须由你**手动**送到两个地方：
> ① 写进云端 `deploy/edge/.env`，再重渲染 `frps.yaml` 并重启 frps（不重启不生效）；
> ② 传给本地 `./start.sh`。
> 漏掉任何一端 → frps 回 `token in login doesn't match token from configuration`，
> 隧道建不起来，公网访问表现为 frps 自带的 404 页（`The server is powered by frp.`）。
> **排障时正确的第一步是「查云端现有值」，不是「生成新值」。**

配套的命令，抄了就能用（顺序按"先查、再决定要不要生成"）：

```bash
# ① 【先查】已有云端部署的话，直接用它现在生效的 token —— 不要重新生成
#    最可靠：读运行中的 frps 容器里渲染好的配置（不依赖你知道部署目录叫什么）
docker exec $(docker ps --format '{{.Names}}' | grep -i frps | head -1) \
  grep "token:" /app/config.yaml
#    或看部署目录里的 .env（目录名可能仍是更名前的 echo-chat-edge）
grep FRP_AUTH_TOKEN /opt/*edge*/deploy/edge/.env

# ② 【仅当要换新 token 时】生成一次 —— 注意它只是打印，不会写进任何配置
openssl rand -hex 32

# ③ 把 ② 的串【手动】填到云端，并让 frps 重新加载
#    在【云主机】、部署目录里执行：
#      vi deploy/edge/.env                    # FRP_AUTH_TOKEN=<②的串>
#      bash deploy/scripts/render-config.sh edge
#      docker compose -p <项目名> restart frps      # 项目名见 docker compose ls
#    复核（必须能看到新串）：
#      docker exec <frps容器> grep "token:" /app/config.yaml

# ④ 拿到云主机公网 IP —— 在【云主机】上执行
curl -s https://ipinfo.io/ip; echo        # 或 curl -s https://ifconfig.me; echo

# ⑤ 配好域名 A 记录后，本地验证是否指向云主机（回显的应是云 IP）
dig +short example.com

# ⑥ 装 frpc 客户端（0.62.1，与云端 frps 同版本；自动 sha256 校验）
bash deploy/scripts/fetch_frpc.sh

# ⑦ 本机启动时把同一个串传给 start.sh（见下「启动」）
```

`FRP_BIND_PORT` 通常不用管（默认 `39000`，云端 `deploy/edge/.env` 里可改）。

### 启动

三样齐备后，**二选一**把配置给 `start.sh`：

```bash
# A) 环境变量：写成一行，变量之间用空格隔开（最不容易出错）
PUBLIC_DOMAIN=example.com FRP_SERVER_ADDR=1.2.3.4 FRP_AUTH_TOKEN=<与云端一致的 token> ./start.sh

# B) 配置文件（会自动渲染 deploy/app/tunnel/frpc.yaml）——正式部署建议用这个
cp deploy/app/.env.example deploy/app/.env   # 填 PUBLIC_DOMAIN / FRP_SERVER_ADDR / FRP_AUTH_TOKEN
./start.sh
```

> **写多行时注意**：行尾续行符 `\` **前面必须留一个空格**。
> `FRP_AUTH_TOKEN=abc\` + 换行 + `./start.sh` 会被 bash 拼成 `FRP_AUTH_TOKEN=abc./start.sh`——
> 整条命令退化成"纯变量赋值"，`start.sh` 根本不会执行，而且**一行输出都没有**（很容易误判成"跑通了但没效果"）。
> 正确写法：
> ```bash
> PUBLIC_DOMAIN=example.com \
> FRP_SERVER_ADDR=1.2.3.4 \
> FRP_AUTH_TOKEN=abc \
> ./start.sh
> ```
> 判断有没有真跑起来：有输出 `== 环境/补编译 ==` 和 `[frpc] 渲染配置 ...` 才算。

A 与 B 同时存在时以 **B（`.env` 文件）优先**。也可以让 `start.sh` 顺带把 frpc 也下了：

```bash
ECHO_FETCH_FRPC=1 PUBLIC_DOMAIN=example.com FRP_SERVER_ADDR=1.2.3.4 FRP_AUTH_TOKEN=<token> ./start.sh
```

成功后 `start.sh` 结尾会打印 `公网入口 → https://<域名>（frpc 隧道已建立）`；没配置时打印 `公网入口未启用（本机访问不受影响）`——**这是正常的，不影响本地使用**。

> 仅验证云端 frps/Nginx/TLS 是否正常：`bash deploy/scripts/deploy-edge.sh`；分层验收：`bash deploy/scripts/smoke-test.sh edge`。

## 功能说明

- **免密登录**：首次访问自动以 `device_id` 注册并分配额度，无短信验证码。
- **明文 KV 存储**：问题-回答以 `<原始问题>` → `<原始回答>` 存进 kvstore（key/value 均为原文，不做 hash，`redis-cli -p 5160 GET <问题>` 可直接读）；语义检索向量索引在版本化命名空间 `semd:e5s:v1:<原始问题>`（e5 384 维），另有安全指纹 `semfp:v1:<fp>` 精确命中。
- **公有大模型**：DeepSeek（OpenAI 兼容），模型 `deepseek-v4-flash`。
- **来源标注**：每条回答标注「公有大模型」/「缓存命中」。
- **tokens 统计**：页面按会话累计「总消耗」与「节省」tokens（缓存命中不计费、记为节省）。

## 相关仓库

- 存储：[pocket-kv](https://github.com/robotlover-1/pocket-kv)（kvstore，submodule 引入）
- 助手：本仓库 [AnswerMesh](https://github.com/robotlover-1/Answermesh)
