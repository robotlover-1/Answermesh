#!/usr/bin/env python3
"""后端公共接口 + 并发 QPS 测量。

统一契约（三个系统都实现它，保证同口径）：
  build(vectors, keys) -> dict        建索引/灌数据，返回 {"build_s", "mem_mb", ...}
  search_one(vec, topk) -> List[int]  单条查询，返回 topk 的 int id（降序）
  close()

id 约定：向量 i 的 id 就是 i，key 由各后端自行映射（kvstore 存成 `NS + <id>`）。
"""
from __future__ import annotations

import threading
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Dict, List, Sequence, Tuple

import numpy as np


class Backend:
    name = "base"
    index_name = ""

    def build(self, vectors: np.ndarray, keys: Sequence[str]) -> Dict:
        raise NotImplementedError

    def search_one(self, vec: np.ndarray, topk: int) -> List[int]:
        raise NotImplementedError

    def close(self):
        pass

    # -- ANN 召回旋钮 ---------------------------------------------------
    # 近似索引的延迟**必须**和它自己的 recall 一起看：nprobe=1 的 IVF 比精确检索
    # 快是废话。所以每个近似索引都要扫自己的召回参数，报告里在**等 recall** 下比延迟。
    # 精确索引（kvstore / faiss-flat / milvus-flat）只有一个空参数点，recall 恒为 1。
    def param_grid(self) -> List[Dict]:
        return [{}]

    def set_param(self, p: Dict):
        pass

    @staticmethod
    def param_label(p: Dict) -> str:
        return ",".join("%s=%s" % (k, v) for k, v in p.items()) or "default"

    # -- 通用：单线程逐条测延迟 -----------------------------------------
    def measure_latency(self, queries: np.ndarray, topk: int,
                        warmup: int = 20) -> Tuple[List[List[int]], List[float]]:
        for i in range(min(warmup, len(queries))):
            self.search_one(queries[i], topk)
        lat, ids = [], []
        for q in queries:
            t0 = time.perf_counter()
            got = self.search_one(q, topk)
            lat.append((time.perf_counter() - t0) * 1000.0)
            ids.append(got)
        return ids, lat

    # -- 通用：C 线程并发测吞吐 -----------------------------------------
    def measure_concurrency(self, queries: np.ndarray, topk: int, concurrency: int,
                            warmup: int = 20) -> Dict:
        """C 个线程各持一条连接打同一批查询，返回 QPS + 逐次延迟。

        注意：QPS 是**客户端侧**吞吐。kvstore 是单线程 reactor，VSEARCH 期间会
        阻塞事件循环，所以它的 QPS 不会随 C 提升 —— 这正是要测出来的结论之一。
        """
        for i in range(min(warmup, len(queries))):
            self.search_one(queries[i], topk)

        n = len(queries)
        lat_all: List[float] = []
        lock = threading.Lock()
        errs: List[str] = []
        barrier = threading.Barrier(concurrency)

        def worker(tid: int):
            my_lat = []
            try:
                barrier.wait(timeout=60)          # 对齐起跑，避免先启动的线程多吃负载
                for i in range(tid, n, concurrency):
                    t0 = time.perf_counter()
                    self.search_one(queries[i], topk)
                    my_lat.append((time.perf_counter() - t0) * 1000.0)
            except Exception as e:                # noqa: BLE001
                with lock:
                    errs.append("%s: %s" % (type(e).__name__, e))
            with lock:
                lat_all.extend(my_lat)

        t0 = time.perf_counter()
        with ThreadPoolExecutor(max_workers=concurrency) as ex:
            list(ex.map(worker, range(concurrency)))
        wall = time.perf_counter() - t0

        done = len(lat_all)
        return {
            "concurrency": concurrency,
            "ops": done,
            "wall_s": wall,
            "qps": (done / wall) if wall > 0 else float("nan"),
            "latency_ms": lat_all,
            "errors": errs,
        }
