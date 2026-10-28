#!/usr/bin/env python3
"""faiss 后端：IndexFlatIP（精确）/ HNSW / IVF-Flat（近似）。

向量已单位化 ⇒ 内积 == 余弦，统一用 METRIC_INNER_PRODUCT。
faiss 是**进程内零拷贝**调用：没有网络、没有序列化 —— 与 kvstore 的
RESP+TCP 路径不同口径，报告里必须注明（见 REPORT.md「口径与公平性」）。
"""
from __future__ import annotations

import os
import sys
import time
from typing import Dict, List, Sequence

import faiss
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from harness import rss_mb  # noqa: E402
from backends.base import Backend  # noqa: E402


class FaissBackend(Backend):
    name = "faiss"

    def __init__(self, index_name: str = "flat", m: int = 32, ef_search: int = 64,
                 nlist: int = 0, nprobe: int = 0):
        self.index_name = index_name
        self.m = m
        self.ef_search = ef_search
        self.nlist = nlist
        self.nprobe = nprobe
        self.index = None
        self.dim = 0
        self.n = 0

    def _make(self, dim: int, n: int):
        if self.index_name == "flat":
            return faiss.IndexFlatIP(dim)
        if self.index_name == "hnsw":
            idx = faiss.IndexHNSWFlat(dim, self.m, faiss.METRIC_INNER_PRODUCT)
            idx.hnsw.efConstruction = 200
            idx.hnsw.efSearch = self.ef_search
            return idx
        if self.index_name == "ivf":
            # nlist 经验值 ~ 4*sqrt(n)（faiss wiki），下界 16
            nlist = self.nlist or max(16, int(4 * np.sqrt(n)))
            quant = faiss.IndexFlatIP(dim)
            idx = faiss.IndexIVFFlat(quant, dim, nlist, faiss.METRIC_INNER_PRODUCT)
            idx.nprobe = self.nprobe or max(1, nlist // 16)
            self.nlist, self.nprobe = nlist, idx.nprobe
            return idx
        raise ValueError("未知 faiss 索引: %s" % self.index_name)

    def build(self, vectors: np.ndarray, keys: Sequence[str]) -> Dict:
        self.dim = int(vectors.shape[1])
        self.n = int(vectors.shape[0])
        xb = np.ascontiguousarray(vectors, dtype=np.float32)
        mem_before = rss_mb_proc()
        t0 = time.perf_counter()
        self.index = self._make(self.dim, self.n)
        if isinstance(self.index, faiss.IndexIVFFlat):
            self.index.train(xb)
        self.index.add(xb)
        build_s = time.perf_counter() - t0
        mem_after = rss_mb_proc()
        return {
            "build_s": build_s,
            "mem_mb": mem_after - mem_before,
            "process_rss_mb": mem_after,
            "flat_bytes_est": self.n * self.dim * 4,
            "nlist": self.nlist or None,
            "nprobe": self.nprobe or None,
            "M": self.m if self.index_name == "hnsw" else None,
            "ef_search": self.ef_search if self.index_name == "hnsw" else None,
        }

    # -- 召回旋钮扫描 ---------------------------------------------------
    def param_grid(self) -> List[Dict]:
        if self.index_name == "hnsw":
            return [{"efSearch": v} for v in (16, 32, 64, 128, 256, 512)]
        if self.index_name == "ivf":
            # 网格要够到高 recall：nprobe 只扫到 64 时 N=10k 的 recall 才 0.43，
            # 那不是「IVF 不行」而是「没扫够」—— 等 recall 比较必须让曲线爬到接近 1。
            maxp = max(1, self.nlist)
            return [{"nprobe": v} for v in (1, 2, 4, 8, 16, 32, 64, 128, 256) if v <= maxp]
        return [{}]

    def set_param(self, p: Dict):
        if not p:
            return
        if "efSearch" in p:
            self.index.hnsw.efSearch = int(p["efSearch"])
        if "nprobe" in p:
            self.index.nprobe = int(p["nprobe"])

    def search_one(self, vec: np.ndarray, topk: int) -> List[int]:
        q = np.ascontiguousarray(vec.reshape(1, -1), dtype=np.float32)
        _d, i = self.index.search(q, topk)
        return [int(x) for x in i[0] if x >= 0]

    def search_batch(self, queries: np.ndarray, topk: int) -> List[List[int]]:
        q = np.ascontiguousarray(queries, dtype=np.float32)
        _d, i = self.index.search(q, topk)
        return [[int(x) for x in row if x >= 0] for row in i]

    def close(self):
        self.index = None


def rss_mb_proc() -> float:
    """本进程 RSS（faiss/milvus 都跑在进程内，用进程 RSS 代表索引内存）。

    注意：读的是 /proc/self，所以每个后端必须**独立进程**跑，否则互相污染。
    """
    try:
        with open("/proc/self/status") as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1]) / 1024.0
    except OSError:
        pass
    return float("nan")
