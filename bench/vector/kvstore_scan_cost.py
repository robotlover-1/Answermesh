#!/usr/bin/env python3
"""量化 VSEARCH 的「全表扫描」成本：hash 引擎里**与本命名空间无关**的键也会拖慢它。

kvs_vector.c 的实现是遍历 global_hash 的每个槽/节点，对每个节点 memcmp 前缀，命中才
算余弦。所以扫描成本 ∝ **整张 hash 表**的节点数，而不是 `semd:e5s:v1:` 前缀下的条数。

这在生产里是有意义的：hash 引擎是通用 KV，语义缓存的向量和其他任何 HSET 数据共享同一张表。
本脚本测「只有向量」和「向量 + M 条无关键」两种情况下同一条 VSEARCH 的延迟差。

    python3 kvstore_scan_cost.py --n 10000 --extra 10000,50000,100000
"""
from __future__ import annotations

import argparse
import os
import sys
import time
from typing import Dict, List

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from harness import (PROD_NS, PROD_TOP_K, dump_result, gen_queries, gen_vectors,  # noqa: E402
                     kvstore_cleanup, kvstore_start, latency_stats, pack_record)
from backends.kvstore_backend import KvstoreBackend  # noqa: E402

POPULATE_BATCH = 512


def add_unrelated(c, m: int, start: int = 0):
    """往 hash 引擎塞 m 条与向量命名空间无关的普通键值（模拟同一张表里的其它业务数据）。"""
    cmds: List[List[bytes]] = []
    for i in range(start, start + m):
        cmds.append([b"HSET", b"other:key:%d" % i, b"x" * 32])
        if len(cmds) >= POPULATE_BATCH:
            c.pipeline(cmds)
            cmds = []
    if cmds:
        c.pipeline(cmds)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--n", type=int, default=10000)
    p.add_argument("--dim", type=int, default=384)
    p.add_argument("--topk", type=int, default=PROD_TOP_K)
    p.add_argument("--queries", type=int, default=100)
    p.add_argument("--extra", default="10000,50000,100000")
    p.add_argument("--seed", type=int, default=42)
    p.add_argument("--name", default="kvstore_scan_cost")
    args = p.parse_args()

    extras = [int(x) for x in args.extra.split(",")]
    kvstore_start()
    records: List[Dict] = []
    try:
        vecs = gen_vectors(args.n, args.dim, seed=args.seed)
        qs = gen_queries(args.queries, args.dim, vecs, seed=args.seed + 1)
        be = KvstoreBackend()
        info = be.build(vecs, ["%s%d" % (PROD_NS, i) for i in range(args.n)])
        print("灌入 %d 条向量，耗时 %.2fs" % (args.n, info["build_s"]))

        base = be.measure_latency(qs, args.topk, warmup=10)[1]
        st = latency_stats(base)
        print("  [表内只有向量]        p50=%.3fms p99=%.3fms" % (st["p50"], st["p99"]))
        records.append({"extra_keys": 0, "table_keys": args.n, "latency_ms": st, "ratio_p50": 1.0})

        total = 0
        for m in extras:
            t0 = time.perf_counter()
            add_unrelated(be._conn(), m, start=total)
            total += m
            lat = be.measure_latency(qs, args.topk, warmup=10)[1]
            st = latency_stats(lat)
            ratio = st["p50"] / records[0]["latency_ms"]["p50"]
            print("  [再加 %6d 条无关键] p50=%.3fms p99=%.3fms  （%.2fx）"
                  % (total, st["p50"], st["p99"], ratio))
            records.append({"extra_keys": total, "table_keys": args.n + total,
                            "latency_ms": st, "ratio_p50": ratio,
                            "add_s": time.perf_counter() - t0})
        be.close()
    finally:
        kvstore_cleanup()

    meta = {"n_vectors": args.n, "dim": args.dim, "topk": args.topk,
            "queries": args.queries, "seed": args.seed,
            "note": "VSEARCH 扫描整张 global_hash，无关键也计入成本"}
    dump_result(args.name, meta, records)


if __name__ == "__main__":
    sys.exit(main())
