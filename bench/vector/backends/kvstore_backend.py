#!/usr/bin/env python3
"""kvstore 后端：VSEARCH 暴力检索（+ 生产读路径的 GET 取回）。

严格照 semcache.go 的真实用法：
  - 向量写 hash 引擎：HSET <ns><id>  value=[u32 dim][float32 vec]
  - 明文回答写 array 引擎：SET <id>  value=回答文本（生产里 key 是问题原文，
    VSEARCH 命中后剥掉 ns 前缀再 GET —— 这里用 <id> 等价替代）
  - 检索：VSEARCH <dim> <裸 float32 bytes> <topk> <ns>   （查询向量不带 dim 头）
"""
from __future__ import annotations

import os
import sys
import threading
import time
from typing import Dict, List, Sequence, Tuple

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from harness import (PROD_NS, RespClient, RespError, kvstore_pids,  # noqa: E402
                     pack_query, pack_record, rss_mb)
from backends.base import Backend  # noqa: E402

POPULATE_BATCH = 512          # pipeline 深度，避免一次塞爆 socket 缓冲
ANSWER_LEN = 64               # 回答文本长度（生产是 100~500 字的中文回答，这里量级保守）


class KvstoreBackend(Backend):
    name = "kvstore"

    def __init__(self, ns: str = PROD_NS, pipelined: bool = True):
        self.ns = ns
        self.pipelined = pipelined
        self.dim = 0
        self.n = 0
        self.keys: List[str] = []
        self._local = threading.local()
        self._conns: List[RespClient] = []
        self.server_pid = None

    # -- 连接管理：每线程一条，连接不是线程安全的 -----------------------
    def _conn(self) -> RespClient:
        c = getattr(self._local, "conn", None)
        if c is None:
            c = RespClient()
            self._local.conn = c
            self._conns.append(c)
        return c

    # -- 灌数据 ---------------------------------------------------------
    def build(self, vectors: np.ndarray, keys: Sequence[str]) -> Dict:
        self.dim = int(vectors.shape[1])
        self.n = int(vectors.shape[0])
        self.keys = list(keys)
        pids = kvstore_pids()
        self.server_pid = pids[0] if pids else None
        rss_before = rss_mb(self.server_pid) if self.server_pid else None

        c = self._conn()
        t0 = time.perf_counter()
        cmds: List[List[bytes]] = []
        for i, v in enumerate(vectors):
            k = ("%s%d" % (self.ns, i)).encode()
            cmds.append([b"HSET", k, pack_record(v)])
            cmds.append([b"SET", str(i).encode(), b"A" * ANSWER_LEN])
            if len(cmds) >= POPULATE_BATCH:
                c.pipeline(cmds)
                cmds = []
        if cmds:
            c.pipeline(cmds)
        build_s = time.perf_counter() - t0

        rss_after = rss_mb(self.server_pid) if self.server_pid else None
        return {
            "build_s": build_s,
            "mem_mb": (rss_after - rss_before) if (rss_before and rss_after) else None,
            "server_rss_mb": rss_after,
            "pipelined": self.pipelined,
        }

    # -- 检索 -----------------------------------------------------------
    def search_one(self, vec: np.ndarray, topk: int) -> List[int]:
        return self.search_one_full(vec, topk)[0]

    def search_one_full(self, vec: np.ndarray, topk: int) -> Tuple[List[int], float]:
        """返回 (ids, vsearch_ms)。检索延迟单独计时，便于把 GET 拆出去。"""
        c = self._conn()
        qb = pack_query(vec)
        t0 = time.perf_counter()
        res = c.cmd(b"VSEARCH", str(self.dim).encode(), qb, str(topk).encode(), self.ns.encode())
        t_search = (time.perf_counter() - t0) * 1000.0
        if res is None:
            return [], t_search
        ids = []
        for i in range(0, len(res) - 1, 2):
            k = res[i]
            if k is None:
                continue
            s = k.decode() if isinstance(k, bytes) else str(k)
            if not s.startswith(self.ns):
                continue
            try:
                ids.append(int(s[len(self.ns):]))
            except ValueError:
                continue
        return ids, t_search

    def search_and_fetch(self, vec: np.ndarray, topk: int) -> Tuple[List[int], float, bool]:
        """生产读路径的一跳：VSEARCH topk → GET 命中候选的明文回答。

        返回 (ids, total_ms, fetched_ok)。total_ms 含 VSEARCH + 1 次 GET。
        """
        ids, t_search = self.search_one_full(vec, topk)
        c = self._conn()
        t0 = time.perf_counter()
        ok = False
        if ids:
            ans = c.cmd(b"GET", str(ids[0]).encode())     # 生产只取第一个被接受的候选
            ok = ans is not None
        t_get = (time.perf_counter() - t0) * 1000.0
        return ids, t_search + t_get, ok

    def measure_fetch_path(self, queries: np.ndarray, topk: int, warmup: int = 20) -> Dict:
        """分别测 VSEARCH-only 与 VSEARCH+GET，量化 GET 那一跳的代价。"""
        for i in range(min(warmup, len(queries))):
            self.search_and_fetch(queries[i], topk)
        # 分两轮测，避免第二轮吃第一轮的缓存预热（HSET 路径无缓存，但保持口径干净）
        only = [self.search_one_full(q, topk)[1] for q in queries]
        both = [self.search_and_fetch(q, topk)[1] for q in queries]
        return {"vsearch_ms": only, "vsearch_plus_get_ms": both}

    def close(self):
        for c in self._conns:
            c.close()
        self._conns = []
