#!/usr/bin/env python3
"""向量检索对比基准的公共基础设施。

被 run_vector_bench.py 与 backends/* 复用：RESP 客户端、数据生成、延迟统计、
结果落盘、kvstore server 生命周期与 RSS 采样。

口径说明（与 kvstore/kvstore/tools/bench/bench_common.py 保持一致）：
  - server 绑核 SERVER_CORES（默认 2,3），client 绑核 CLIENT_CORE（默认 0）
  - 结果落结果目录 results/，文件名带日期
"""
from __future__ import annotations

import json
import math
import os
import socket
import struct
import subprocess
import sys
import time
from typing import Dict, List, Optional, Sequence, Tuple

import numpy as np

# ---------------------------------------------------------------- 路径与常量

HERE = os.path.dirname(os.path.abspath(__file__))
PROJ = os.path.dirname(os.path.dirname(HERE))            # Answermesh/
KVSTORE_PROJ = os.path.join(PROJ, "kvstore", "kvstore")  # 子模块
KVSTORE_BIN = os.path.join(KVSTORE_PROJ, "kvstore")
RESULTS_DIR = os.path.join(HERE, "results")

KV_PORT = int(os.environ.get("KV_PORT", "5190"))
SERVER_CORES = os.environ.get("SERVER_CORES", "2,3")
CLIENT_CORE = os.environ.get("CLIENT_CORE", "0")

# 生产口径：ai-chat-service/dev.config.yaml semantic_cache
PROD_DIM = 384
PROD_TOP_K = 30
PROD_NS = "semd:e5s:v1:"

# kvstore VSEARCH 白名单（kvs_vector.c），超出即 "vsearch bad args"
KVSTORE_ALLOWED_DIMS = (256, 384, 1024)


def sh(cmd: str, check: bool = False) -> subprocess.CompletedProcess:
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise RuntimeError("cmd failed: %s\n%s\n%s" % (cmd, r.stderr, r.stdout))
    return r


# ---------------------------------------------------------------- RESP 客户端

class RespError(Exception):
    pass


class RespClient:
    """最小 RESP2 客户端。支持 pipeline（批量写 + 批量读）。

    null bulk（`$-1`）解析为 None —— 注意别把它当成空 bytes，否则 GET miss 会挂。
    """

    def __init__(self, host: str = "127.0.0.1", port: int = KV_PORT, timeout: float = 30.0):
        self.sock = socket.create_connection((host, port), timeout=timeout)
        self.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        self._buf = b""

    # -- 底层读 ---------------------------------------------------------
    def _read_line(self) -> bytes:
        while b"\r\n" not in self._buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise EOFError("连接被对端关闭")
            self._buf += chunk
        line, self._buf = self._buf.split(b"\r\n", 1)
        return line

    def _read_exact(self, n: int) -> bytes:
        while len(self._buf) < n:
            chunk = self.sock.recv(max(65536, n - len(self._buf)))
            if not chunk:
                raise EOFError("连接被对端关闭")
            self._buf += chunk
        out, self._buf = self._buf[:n], self._buf[n:]
        return out

    def _read_reply(self):
        line = self._read_line()
        t = line[:1]
        if t == b"+":
            return line[1:].decode()
        if t == b"-":
            raise RespError(line[1:].decode())
        if t == b":":
            return int(line[1:])
        if t == b"$":
            n = int(line[1:])
            if n < 0:
                return None
            data = self._read_exact(n + 2)
            return data[:-2]
        if t == b"*":
            n = int(line[1:])
            if n < 0:
                return None
            return [self._read_reply() for _ in range(n)]
        raise RespError("无法解析的回复: %r" % line)

    # -- 编码 -----------------------------------------------------------
    @staticmethod
    def _encode(args: Sequence[bytes]) -> bytes:
        out = [b"*%d\r\n" % len(args)]
        for a in args:
            out.append(b"$%d\r\n" % len(a))
            out.append(a)
            out.append(b"\r\n")
        return b"".join(out)

    # -- 公开 API -------------------------------------------------------
    def cmd(self, *args):
        parts = [a if isinstance(a, bytes) else str(a).encode() for a in args]
        self.sock.sendall(self._encode(parts))
        return self._read_reply()

    def pipeline(self, cmds: List[List[bytes]]):
        """一次发多条命令再一次收全部回复（population 用，避免逐条 RTT）。"""
        self.sock.sendall(b"".join(self._encode(c) for c in cmds))
        return [self._read_reply() for _ in cmds]

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()


# ---------------------------------------------------------------- 数据生成

def gen_vectors(n: int, dim: int, seed: int = 42, n_clusters: int = 64,
                noise: float = 0.25) -> np.ndarray:
    """生成 L2 归一化 float32 向量（单位化后内积 == 余弦）。

    用**聚类分布**而非均匀随机：真实 e5 嵌入是同主题聚集、跨主题余弦饱和
    （dev.config.yaml 记 0.84~0.9，同主题 0.776+），均匀随机的近邻是退化的，
    会让 ANN 的 recall 虚高/虚低、也不像真实负载。
    """
    rng = np.random.default_rng(seed)
    n_clusters = max(1, min(n_clusters, n))
    centers = rng.normal(size=(n_clusters, dim)).astype(np.float32)
    centers /= np.linalg.norm(centers, axis=1, keepdims=True)
    assign = rng.integers(0, n_clusters, size=n)
    vecs = centers[assign] + (rng.normal(size=(n, dim)) * noise).astype(np.float32)
    vecs /= np.linalg.norm(vecs, axis=1, keepdims=True)
    return vecs.astype(np.float32)


def gen_queries(n: int, dim: int, base: np.ndarray, seed: int = 1234,
                noise: float = 0.15) -> np.ndarray:
    """查询向量：围绕库中已有点做小扰动（模拟「同一问题的改写」这种真实命中场景）。"""
    rng = np.random.default_rng(seed)
    idx = rng.integers(0, len(base), size=n)
    q = base[idx] + (rng.normal(size=(n, dim)) * noise).astype(np.float32)
    q /= np.linalg.norm(q, axis=1, keepdims=True)
    return q.astype(np.float32)


def pack_record(vec: np.ndarray) -> bytes:
    """kvstore 存储值布局：[u32 dim][float32 vec]（小端）—— 与 kvs_vector.c parse_vec 一致。"""
    return struct.pack("<I", len(vec)) + vec.astype("<f4").tobytes()


def pack_query(vec: np.ndarray) -> bytes:
    """VSEARCH 查询向量：裸 float32，**不带** [u32 dim] 头（见 semcache.go vsearch）。"""
    return vec.astype("<f4").tobytes()


# ---------------------------------------------------------------- 统计

def latency_stats(samples_ms: Sequence[float]) -> Dict[str, float]:
    a = np.asarray(samples_ms, dtype=np.float64)
    if a.size == 0:
        return {}
    return {
        "n": int(a.size),
        "mean": float(a.mean()),
        "p50": float(np.percentile(a, 50)),
        "p90": float(np.percentile(a, 90)),
        "p99": float(np.percentile(a, 99)),
        "max": float(a.max()),
        "min": float(a.min()),
    }


def recall_at_k(approx_ids: Sequence[Sequence[int]], truth_ids: Sequence[Sequence[int]]) -> float:
    """平均 recall@k：|近似 topk ∩ 精确 topk| / k。"""
    if not truth_ids:
        return float("nan")
    tot = 0.0
    for a, t in zip(approx_ids, truth_ids):
        tot += len(set(a) & set(t)) / float(len(t))
    return tot / len(truth_ids)


def rss_mb(pid: int) -> Optional[float]:
    try:
        with open("/proc/%d/status" % pid) as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1]) / 1024.0
    except OSError:
        return None
    return None


def dir_size_mb(path: str) -> float:
    tot = 0
    for root, _dirs, files in os.walk(path):
        for fn in files:
            try:
                tot += os.path.getsize(os.path.join(root, fn))
            except OSError:
                pass
    return tot / (1024.0 * 1024.0)


# ---------------------------------------------------------------- kvstore server

def _self_safe_pattern() -> str:
    """`--port 5190` → `--port 519[0]`：防止 pgrep/pkill 匹配到执行它的那个 shell 自身。

    （不这么做的话 `pkill -f 'kvstore --port 5190'` 会连跑它的 shell 一起杀 —— 且
    pgrep 会把该 shell 的 pid 当成 kvstore 的 pid 返回。）
    """
    p = str(KV_PORT)
    return "kvstore --port %s[%s]" % (p[:-1], p[-1])


def kvstore_pids() -> List[int]:
    """只返回**确实是 kvstore 可执行文件**的 pid（用 /proc/<pid>/comm 二次确认）。"""
    r = sh("pgrep -f '%s'" % _self_safe_pattern())
    out = []
    for x in r.stdout.split():
        if not x.strip().isdigit():
            continue
        pid = int(x)
        try:
            with open("/proc/%d/comm" % pid) as f:
                if f.read().strip() != "kvstore":
                    continue
        except OSError:
            continue
        out.append(pid)
    return out


def kvstore_cleanup():
    sh("pkill -f '%s'" % _self_safe_pattern())
    time.sleep(0.8)
    for f in ("kvstore.dump", "kvstore.aof"):
        p = os.path.join(KVSTORE_PROJ, f)
        if os.path.exists(p):
            os.remove(p)


def kvstore_start(timeout: float = 60.0, extra: str = "") -> int:
    """启动 kvstore master（绑核 SERVER_CORES），返回 pid。空库启动。

    用 setsid 脱离本进程会话：否则 Bash 工具的会话结束时 server 会被一并杀掉。
    """
    if not os.path.exists(KVSTORE_BIN):
        raise RuntimeError("未找到 kvstore 二进制：%s（先 cd kvstore/kvstore && make）" % KVSTORE_BIN)
    kvstore_cleanup()
    cmd = ("cd %s && setsid nohup taskset -c %s %s --port %d --role master "
           "--mem libc --net reactor %s > /tmp/kv_bench_vsearch.log 2>&1 < /dev/null &"
           % (KVSTORE_PROJ, SERVER_CORES, KVSTORE_BIN, KV_PORT, extra))
    sh(cmd)
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            with RespClient(port=KV_PORT, timeout=2.0) as c:
                if c.cmd("PING") is not None:
                    time.sleep(1.0)          # settle
                    pids = kvstore_pids()
                    if pids:
                        return pids[0]
                    raise RuntimeError("server 起来了但没找到 pid")
        except (OSError, EOFError, RespError, socket.timeout):
            time.sleep(0.4)
    log = ""
    try:
        log = open("/tmp/kv_bench_vsearch.log").read()[-500:]
    except OSError:
        pass
    raise RuntimeError("kvstore 启动失败：\n%s" % log)


# ---------------------------------------------------------------- 结果落盘

def dump_result(name: str, meta: Dict, records: List[Dict]) -> str:
    os.makedirs(RESULTS_DIR, exist_ok=True)
    date = time.strftime("%Y-%m-%d")
    path = os.path.join(RESULTS_DIR, "%s_%s.json" % (name, date))
    payload = {"meta": meta, "records": records}
    with open(path, "w") as f:
        json.dump(payload, f, indent=2, ensure_ascii=False, default=str)
    print("\n结果: %s" % path)
    return path
