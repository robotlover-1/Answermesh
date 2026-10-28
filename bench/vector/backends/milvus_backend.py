#!/usr/bin/env python3
"""Milvus Lite 后端：FLAT（精确）/ HNSW / IVF_FLAT（近似）。

用 Milvus Lite（pip 内嵌，`MilvusClient(<本地 .db 路径>)`）而不是 Docker Standalone：
本机 docker daemon 未启动、内存 7.7GiB 也不宽裕。Lite 是**单进程嵌入式**，
没有独立 server 进程和网络跳数，所以数字是真实 Milvus 部署的**下界**，报告需注明。

注意 pymilvus 3.0 的 API 与 2.x 不同：create_index 收 IndexParams 对象而不是
(collection, field_name, params) 三元组。
"""
from __future__ import annotations

import os
import shutil
import sys
import time
from typing import Dict, List, Sequence

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from harness import dir_size_mb  # noqa: E402
from backends.base import Backend  # noqa: E402
from backends.faiss_backend import rss_mb_proc  # noqa: E402

INSERT_BATCH = 1000


class MilvusBackend(Backend):
    name = "milvus"

    def __init__(self, index_name: str = "flat", m: int = 16, ef_construction: int = 200,
                 ef_search: int = 64, nlist: int = 0, nprobe: int = 0,
                 db_path: str = "/tmp/milvus_vec_bench.db"):
        self.index_name = index_name
        self.m = m
        self.ef_construction = ef_construction
        self.ef_search = ef_search
        self.nlist = nlist
        self.nprobe = nprobe
        self.db_path = db_path
        self.client = None
        self.coll = "vecbench"
        self.dim = 0
        self.n = 0
        self.cur_param: Dict = {}

    def _index_params(self):
        from pymilvus.milvus_client.index import IndexParams
        ip = IndexParams()
        if self.index_name == "flat":
            ip.add_index(field_name="vector", index_type="FLAT", metric_type="COSINE")
        elif self.index_name == "auto":
            # Milvus Lite **不支持 HNSW**（服务端报 "local mode only support FLAT
            # IVF_FLAT AUTOINDEX"）。AUTOINDEX 是它自带的 ANN，内部走 HNSW 一类图索引，
            # 用 ef 调召回 —— 所以 Milvus 侧的近似代表就是它。
            ip.add_index(field_name="vector", index_type="AUTOINDEX", metric_type="COSINE")
        elif self.index_name == "hnsw":
            ip.add_index(field_name="vector", index_type="HNSW", metric_type="COSINE",
                         params={"M": self.m, "efConstruction": self.ef_construction})
        elif self.index_name == "ivf":
            nlist = self.nlist or max(16, int(4 * np.sqrt(self.n)))
            self.nlist = nlist
            ip.add_index(field_name="vector", index_type="IVF_FLAT", metric_type="COSINE",
                         params={"nlist": nlist})
        else:
            raise ValueError("未知 milvus 索引: %s" % self.index_name)
        return ip

    def param_grid(self) -> List[Dict]:
        if self.index_name == "ivf":
            maxp = max(1, self.nlist)
            return [{"nprobe": v} for v in (1, 2, 4, 8, 16, 32, 64, 128, 256) if v <= maxp]
        if self.index_name in ("auto", "hnsw"):
            return [{"ef": v} for v in (16, 32, 64, 128, 256, 512)]
        return [{}]

    def set_param(self, p: Dict):
        self.cur_param = dict(p or {})

    def _search_params(self):
        params = dict(getattr(self, "cur_param", {}) or {})
        if self.index_name == "hnsw" and "ef" not in params:
            params["ef"] = self.ef_search
        if self.index_name == "ivf" and "nprobe" not in params:
            params["nprobe"] = self.nprobe or max(1, (self.nlist or 16) // 16)
        self.nprobe = params.get("nprobe", self.nprobe)
        return {"metric_type": "COSINE", "params": params}

    def build(self, vectors: np.ndarray, keys: Sequence[str]) -> Dict:
        from pymilvus import MilvusClient

        self.dim = int(vectors.shape[1])
        self.n = int(vectors.shape[0])

        # 干净起库：删掉上次的 .db（Milvus Lite 单文件）
        for p in (self.db_path, self.db_path + ".lock"):
            if os.path.isdir(p):
                shutil.rmtree(p, ignore_errors=True)
            elif os.path.exists(p):
                os.remove(p)

        mem_before = rss_mb_proc()
        t0 = time.perf_counter()
        self.client = MilvusClient(self.db_path)
        self.client.create_collection(self.coll, dimension=self.dim,
                                      metric_type="COSINE", auto_id=False)
        for i in range(0, self.n, INSERT_BATCH):
            rows = [{"id": j, "vector": vectors[j].tolist()}
                    for j in range(i, min(i + INSERT_BATCH, self.n))]
            self.client.insert(self.coll, rows)
        self.client.create_index(self.coll, self._index_params())
        self.client.load_collection(self.coll)
        build_s = time.perf_counter() - t0
        mem_after = rss_mb_proc()
        return {
            "build_s": build_s,
            "mem_mb": mem_after - mem_before,
            "process_rss_mb": mem_after,
            "db_mb": dir_size_mb(self.db_path) if os.path.isdir(self.db_path)
                     else (os.path.getsize(self.db_path) / 1048576.0 if os.path.exists(self.db_path) else None),
            "nlist": self.nlist or None,
            "nprobe": self.nprobe or None,
            "M": self.m if self.index_name == "hnsw" else None,
            "efConstruction": self.ef_construction if self.index_name == "hnsw" else None,
            "ef_search": self.ef_search if self.index_name == "hnsw" else None,
        }

    def search_one(self, vec: np.ndarray, topk: int) -> List[int]:
        res = self.client.search(self.coll, data=[vec.astype(np.float32).tolist()],
                                 limit=topk, search_params=self._search_params())
        if not res:
            return []
        return [int(h["id"]) for h in res[0]]

    def close(self):
        try:
            if self.client is not None:
                self.client.close()
        except Exception:  # noqa: BLE001
            pass
        self.client = None
