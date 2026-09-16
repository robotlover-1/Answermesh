# zrpc —— AnswerMesh 跨语言 RPC（C 实现 + cgo 接入 Go）

对原 zrpc（C 教学骨架，见 `reference/zrpc-original`）协议加固后形成的 **zrpc v2**，
用于替换 AnswerMesh 中的三条 gRPC 链路。最终以静态库 `libzrpc.a` + cgo bridge
（`zrpc-go/`）接入 Go 业务。

> 来源与许可证见 [LICENSE-NOTICE.md](LICENSE-NOTICE.md)。
> 迁移已完成：协议/帧/IO、C unary client + NtyCo server、`zrpc-go/` cgo bridge、
> keywords-filter 与 chat-service 全部切到 zrpc（parity 黄金一致证据见 `tests/`）。
> 迁移期的 spec / 计划 / 任务记录已清理，不再随仓库保留。

## 目录布局

```text
third_party/zrpc/
├── LICENSE-NOTICE.md       # 来源、授权与许可证记录
├── README.md
├── reference/              # 原始 zrpc 快照，仅参考，不编译
│   └── zrpc-original/
├── ntyco/                  # NtyCo core 源码（授权引入，server 调度用，随 lib 编译）
├── include/
│   ├── zrpc.h              # 公共 ABI 伞头
│   ├── zrpc_protocol.h     # 协议：帧头/消息类型/常量/状态码/帧编解码/IO
│   ├── zrpc_json.h         # JSON 信封（method/auth/deadline + verbatim payload）
│   ├── zrpc_client.h       # C client ABI（unary + ping）
│   └── zrpc_server.h       # C server ABI（register/serve/send_*）
├── src/
│   ├── zrpc_io.c           # read/write_full（poll 路径 + NtyCo 协程路径）
│   ├── zrpc_frame.c        # v2 帧编解码 + CRC32
│   ├── zrpc_json.c         # 信封 helpers（raw 节点保真业务 JSON）
│   ├── zrpc_error.c        # status → 字符串
│   ├── zrpc_client.c       # unary client（普通线程阻塞 IO，自动回退 libc）
│   ├── zrpc_server.c       # unary server（NtyCo 协程 accept/读；每连接写锁）
│   └── cJSON.c / cJSON.h   # MIT，vendored
├── tests/                  # C 单元测试
│   ├── test_frame.c        # 纯帧层（sanitizer-safe）
│   ├── test_io.c           # socketpair 故障注入（sanitizer-safe）
│   ├── test_json.c         # 信封（sanitizer-safe）
│   └── test_unary.c        # client↔NtyCo server 端到端 + load(10万)
├── Makefile                # build/libzrpc.a；make test；make clean
└── build/                  # 构建产物（gitignore）
```

## v2 帧头（20B，网络字节序）

```text
magic 2B | version 1B | type 1B | request_id 8B | length 4B | crc32(payload) 4B
```

常量：magic `0x5A52`（"ZR"）、version `2`、最大帧 4 MiB。
消息类型：request/response/stream_data/stream_end/error/cancel/ping/pong（1–8）。
REQUEST 载荷为 JSON 信封 `{"method","auth":"Bearer ..","deadline_unix_ms","payload":<业务原始JSON>}`，
业务 JSON 以 cJSON raw 节点逐字嵌入，不做重序列化。

## 构建与测试

```bash
make -C third_party/zrpc            # 产出 build/libzrpc.a（含 NtyCo）
make -C third_party/zrpc test       # 普通构建：4 个测试全跑
# 功能端到端 + 10 万次 unary 泄漏压测
./tests/bin/test_unary load 100000 1        # 单连接顺序（fd/RSS canary）
./tests/bin/test_unary load 100000 8        # 8 线程并发
# sanitizer：纯 C 测试（frame/io/json）。test_unary 驱动 NtyCo ucontext 协程，
# ASan 不支持 makecontext/swapcontext 会产生误报，故 sanitizer 构建自动跳过它。
# NtyCo 路径的内存健康由两层证据覆盖：(1) load 压测 fd/RSS canary 稳定；
# (2) 给共享栈顶页加 PROT_NONE 守卫页后 10 万次压测(每次 yield 均复制到栈顶)
# 全程无段错误 —— 证实 ASan 报的 1 字节越界确为 ucontext/ASan 交互伪影，非真实越界。
CFLAGS="-O1 -g -fsanitize=address,undefined -fno-omit-frame-pointer" \
  make -C third_party/zrpc clean test
```

## 已知限制（随 Task 推进解决）

- NtyCo server 协程内暂无 idle/io 超时（Task 5/8）。
- 优雅停机：跨线程关闭 fd 无法即时唤醒 NtyCo 调度线程（Task 8）。
- 同一连接默认顺序处理多个 unary（一次一帧）；无多路复用（Task 3 起按计划分层）。
