# third_party/zrpc 许可证与来源说明（License & Provenance Notice）

日期：2026-09-04
适用：本目录下新增的 zrpc v2 代码，以及 `reference/` 中的参考源码快照。

## 1. 结论

- zrpc v2（本目录 `include/`、`src/`、`tests/` 中新建的代码）为**本项目从零编写**的实现，
  仅参考了原始 zrpc 的"CRC + 长度帧 + JSON + 注册式方法表"思想，不复制原始实现代码。
- `reference/zrpc-original/` 保存的是原始 zrpc 的源码快照，**仅作来源与对照参考，不参与 libzrpc.a 的编译**。
- 原始 zrpc 与 NtyCo 的源码**已获项目方授权在本仓库内使用**（见第 4 节）。

## 2. reference/zrpc-original —— 原始 zrpc 快照

| 项 | 值 |
|---|---|
| 来源 | https://gitlab.0voice.com/King/zrpc.git（0voice / 王博京 教学项目） |
| 获取日期 | 2026-09-04 |
| 许可证 | 原始项目未随源码提供 LICENSE 文件 |
| 授权 | 项目方确认可用于本项目（第 4 节） |
| 用途 | 仅作参考/来源对照；不编译、不随交付镜像分发其二进制 |
| 包含 | README.MD、Makefile、src/{zrpc.c, zrpc.h, zrpc_client.c, zrpc_server.c, zrpc_method.c, cJSON.c, cJSON.h, zrpc_register.json}、tools/ |

原始实现特征（供对照，非本项目约束）：
单次 `send/recv` 假设整帧收齐、`CRC32(4B)+length(2B)` 帧头、固定 caller ID、注册式方法表、
基于 NtyCo 协程调度。zrpc v2 已针对这些特征逐一加固（详见本目录 README 与源码）。

## 3. cJSON

- 来源：https://github.com/DaveGamble/cJSON
- 许可证：**MIT**，Copyright (c) 2009-2017 Dave Gamble and cJSON contributors
- 用途：JSON 编解码；随 zrpc v2 一起编译，将置于 `src/cJSON.c`、`src/cJSON.h`，保留 MIT 版权声明。

## 4. NtyCo

- 来源：https://github.com/wangbojing/NtyCo
- 本仓库参考点：`kvstore` 子模块（pocket-kv，submodule commit `94abe3a…`）内以 NtyCo 承担协程调度，
  已成功构建 `libntyco.a` 并在 kvstore 服务中使用。
- 许可证：wangbojing/NtyCo 未随源码提供 LICENSE 文件。
- 授权：项目方确认可使用（第 5 节）。
- 本项目现状：Task 2 决策采用 NtyCo 承担 zrpc server 的 accept/读调度（计划 §5.5 方案 B）。
  已将其 core 源码引入本目录 `ntyco/`（随 `libzrpc.a` 一起编译，来源如上）。
  非协程线程（Go/cgo 侧、C client）调用 `recv/send/accept/close` 会自动回退真实 libc，仅协程内表现为协程语义。
  NtyCo 基于 ucontext 切换栈，ASan 对 makecontext/swapcontext 产生 stack-overflow 伪报。
  该伪报已用保护页验证：给共享栈顶页设 PROT_NONE 后跑 10 万次 unary（每次 yield 都 `_save_stack`
  复制到栈顶），全程无段错误，证实并非真实越界；内存健康改由 load 的 fd/RSS canary + 该守卫页实验覆盖。
- 已知边界：跨线程关闭 fd 无法即时唤醒 NtyCo 调度线程（优雅停机推迟到 Task 8 处理）。
- **与上游的差异（2026-09-13）**：`ntyco/` 只做了两类改动，均以 `[本项目补写]` 注释标记，便于日后同步上游时比对。
  1. **补齐 3 处缺失的 `return`**（真实缺陷，非风格问题）：`nty_schedule_create()`、`nty_epoller_ev_register_trigger()`、
     `init_hook()` 都声明为 `int` 却从函数末尾掉出——C11 6.9.1p12 是 UB（返回值取自返回寄存器，未定义）。
     上游不开 `-Wall` 故从未暴露；三个调用点目前均忽略返回值，所以**此前无实际故障表现**。
     **只改了本目录 `ntyco/`**。`kvstore` 子模块里那份 NtyCo 有完全相同的 3 处漏 return
     （两份逐字节一致），但它是 `wangbojing/NtyCo` 的**上游 submodule**——pocket-kv 的 `.gitmodules`
     将该路径指向该第三方仓库并 pin 在 `v1.0.4-55-g72ab5fd`，并非 pocket-kv 自有代码，本仓库无法就地提交修复。
     **决定不动它**：pocket-kv 编译 NtyCo 用 `FLAG = -lpthread -O3 -ldl`（不带 `-Wall`），
     那 3 处连警告都不会出现；且其全部调用点（`nty_schedule.c:220` 与 `nty_socket.c:362,387,412,439,467,502,537,566,612`）
     同样忽略返回值，与 zrpc 侧一样无实际故障表现。待上游修，或日后若 pocket-kv 改为 vendored 该源码再一并处理。
  2. **`Makefile` 为 ntyco 对象单独加 `-Wno-sign-compare -Wno-unused-variable -Wno-unused-function`**：
     压制的是上游风格噪音（int 计数器 vs size_t 长度、遗留 `co` 变量、死函数 `nty_epollevent_2poll`）。
     **`-Wreturn-type` 特意不压制**，以便继续兜住上游同类新缺陷——正是它抓出了上面那 3 处。

## 5. 授权记录

2026-09-04 项目方口头确认：zrpc 与 NtyCo 均可用于本项目；NtyCo 已在 AnswerMesh 仓库的 kvstore
子模块服务中实际使用。本说明记录该授权以完成实施计划 Task 0 的"授权明确"退出条件。
