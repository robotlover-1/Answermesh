#!/usr/bin/env python3
"""向量检索对比基准：kvstore VSEARCH vs faiss vs Milvus Lite。

用法（driver 模式，自动为每个组合起独立子进程）：
    python3 run_vector_bench.py                       # 默认矩阵
    python3 run_vector_bench.py --n 1000,10000 --dim 384 --systems kvstore,faiss,milvus
    python3 run_vector_bench.py --systems kvstore --indexes bruteforce --n 1000

每个 (system, index, N) 组合跑在**独立进程**里：faiss/milvus 在进程内，
kvstore 靠 server 的 RSS —— 同进程连跑会把不同规模的 RSS 搅在一起。

口径（三边统一）：
  - 同一批向量/查询（同 seed 生成），同一 topk
  - build_s  = 空库 → 可查询（kvstore: 灌 HSET；faiss: add；milvus: insert+index+load）
  - 延迟     = 单线程逐条打 200 条查询的墙钟（丢前 20 条预热）
  - QPS      = C 线程各自持连接并发打同一批查询
  - recall@k = 与 faiss FlatIP 精确结果的交集比（kvstore 也是暴力精确，理论上应 == 1.0）
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from typing import Dict, List

import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from harness import (PROD_DIM, PROD_NS, PROD_TOP_K, RESULTS_DIR,  # noqa: E402
                     dump_result, gen_queries, gen_vectors, latency_stats,
                     recall_at_k)
from backends.base import Backend  # noqa: E402

TMP_DIR = os.path.join(RESULTS_DIR, "tmp")

DEFAULT_SYSTEMS = "kvstore,faiss,milvus"
# milvus 用 auto 而不是 hnsw：Milvus Lite 只支持 FLAT/IVF_FLAT/AUTOINDEX，
# 显式建 HNSW 会被服务端拒（"invalid index type: HNSW"）。
DEFAULT_INDEXES = {"kvstore": "bruteforce", "faiss": "flat,hnsw,ivf",
                   "milvus": "flat,ivf,auto"}


# --------------------------------------------------------------- 建后端

def make_backend(system: str, index: str, **kw):
    if system == "kvstore":
        from backends.kvstore_backend import KvstoreBackend
        return KvstoreBackend()
    if system == "faiss":
        from backends.faiss_backend import FaissBackend
        return FaissBackend(index_name=index, m=kw.get("m", 32),
                            ef_search=kw.get("ef_search", 64),
                            nlist=kw.get("nlist", 0), nprobe=kw.get("nprobe", 0))
    if system == "milvus":
        from backends.milvus_backend import MilvusBackend
        return MilvusBackend(index_name=index, m=kw.get("m", 16),
                             ef_construction=kw.get("ef_construction", 200),
                             ef_search=kw.get("ef_search", 64),
                             nlist=kw.get("nlist", 0), nprobe=kw.get("nprobe", 0),
                             db_path=kw["db_path"])
    raise ValueError("未知系统: %s" % system)


# --------------------------------------------------------------- 单个组合

def run_one(args) -> Dict:
    from backends.kvstore_backend import KvstoreBackend

    n, dim, topk = args.n, args.dim, args.topk
    print("=" * 72)
    print("[%s/%s] N=%d dim=%d topk=%d queries=%d conc=%s"
          % (args.system, args.index, n, dim, topk, args.queries, args.concurrency))

    vectors = gen_vectors(n, dim, seed=args.seed, n_clusters=args.clusters, noise=args.noise)
    queries = gen_queries(args.queries, dim, vectors, seed=args.seed + 1, noise=0.15)

    # 精确 ground truth：独立用 faiss FlatIP 算，不受被测后端影响
    gt_ids = None
    if args.recall:
        import faiss
        gt_idx = faiss.IndexFlatIP(dim)
        gt_idx.add(np.ascontiguousarray(vectors, dtype=np.float32))
        _d, I = gt_idx.search(np.ascontiguousarray(queries, dtype=np.float32), topk)
        gt_ids = [[int(x) for x in row if x >= 0] for row in I]

    started_server = False
    if args.system == "kvstore":
        from harness import kvstore_start
        kvstore_start()
        started_server = True

    backend = make_backend(args.system, args.index, db_path=os.path.join(TMP_DIR, "milvus_%d_%d.db" % (n, dim)))
    rec: Dict = {"system": args.system, "index": args.index, "n": n, "dim": dim,
                 "topk": topk, "n_queries": args.queries, "seed": args.seed,
                 "clusters": args.clusters, "noise": args.noise}
    try:
        rec.update(backend.build(vectors, ["%s%d" % (PROD_NS, i) for i in range(n)]))

        # ① 召回旋钮扫描：近似索引逐个参数点测 (recall, latency)，供等 recall 比较。
        #    精确索引只有一个空参数点，recall 应恒为 1.0。
        points = []
        ids = lat = None
        for p in backend.param_grid():
            backend.set_param(p)
            ids, lat = backend.measure_latency(queries, topk, warmup=args.warmup)
            pt = {"param": p, "param_label": Backend.param_label(p),
                  "latency_ms": latency_stats(lat)}
            if args.recall and gt_ids is not None:
                pt["recall_at_k"] = recall_at_k(ids, gt_ids)
                pt["exact_match"] = bool(all(set(a) == set(b) for a, b in zip(ids, gt_ids)))
            points.append(pt)
            print("  %-16s p50=%7.3fms p99=%7.3fms recall=%s"
                  % (pt["param_label"], pt["latency_ms"]["p50"], pt["latency_ms"]["p99"],
                     ("%.4f" % pt["recall_at_k"]) if "recall_at_k" in pt else "n/a"))
        rec["ann_points"] = points

        # 汇总口径：取**默认参数点**（grid 首项）作为该索引的代表，其余进 ann_points
        rec["param"] = points[0]["param"]
        rec["latency_ms"] = points[0]["latency_ms"]
        if "recall_at_k" in points[0]:
            rec["recall_at_k"] = points[0]["recall_at_k"]
            rec["exact_match"] = points[0]["exact_match"]

        # ② 并发吞吐：在默认参数点上测（近似索引的并发也要在同 recall 下看）
        backend.set_param(points[0]["param"])
        conc_recs = []
        for c in args.concurrency:
            r = backend.measure_concurrency(queries, topk, concurrency=c, warmup=args.warmup)
            conc_recs.append({"concurrency": c, "qps": r["qps"], "ops": r["ops"],
                              "wall_s": r["wall_s"], "errors": r["errors"],
                              "latency_ms": latency_stats(r["latency_ms"])})
            print("  conc=%-3d qps=%10.1f  p50=%.2fms p99=%.2fms%s"
                  % (c, r["qps"], conc_recs[-1]["latency_ms"]["p50"],
                     conc_recs[-1]["latency_ms"]["p99"],
                     ("  ERR=%s" % r["errors"]) if r["errors"] else ""))
        rec["concurrency"] = conc_recs

        # kvstore 专有：把生产读路径的 GET 那一跳单独量出来
        if isinstance(backend, KvstoreBackend):
            fp = backend.measure_fetch_path(queries, topk, warmup=args.warmup)
            rec["read_path"] = {
                "vsearch_only_ms": latency_stats(fp["vsearch_ms"]),
                "vsearch_plus_get_ms": latency_stats(fp["vsearch_plus_get_ms"]),
            }

        rec["ok"] = True
        rec["latency_ms_samples"] = [round(x, 4) for x in lat]
    except Exception as e:  # noqa: BLE001
        rec["ok"] = False
        rec["error"] = "%s: %s" % (type(e).__name__, e)
        print("  失败: %s" % rec["error"])
    finally:
        backend.close()
        if started_server:
            from harness import kvstore_cleanup
            kvstore_cleanup()

    print("  build=%.2fs mem=%sMB recall=%s"
          % (rec.get("build_s", float("nan")), rec.get("mem_mb"),
             rec.get("recall_at_k")))
    if rec.get("latency_ms"):
        print("  latency p50=%.2f p90=%.2f p99=%.2f max=%.2f ms"
              % (rec["latency_ms"]["p50"], rec["latency_ms"]["p90"],
                 rec["latency_ms"]["p99"], rec["latency_ms"]["max"]))
    return rec


# --------------------------------------------------------------- driver

def build_matrix(args) -> List[Dict]:
    systems = args.systems.split(",")
    jobs = []
    for n in args.n:
        for system in systems:
            indexes = (args.indexes.split(",") if args.indexes
                       else DEFAULT_INDEXES[system].split(","))
            if system == "kvstore":
                indexes = ["bruteforce"]
            for idx in indexes:
                jobs.append({"system": system, "index": idx, "n": n, "dim": args.dim,
                             "topk": args.topk, "queries": args.queries, "seed": args.seed,
                             "clusters": args.clusters, "noise": args.noise,
                             "warmup": args.warmup, "concurrency": args.concurrency})
    return jobs


def spawn(job: Dict) -> Dict:
    os.makedirs(TMP_DIR, exist_ok=True)
    tag = "%s_%s_n%d_d%d_k%d" % (job["system"], job["index"], job["n"], job["dim"], job["topk"])
    out = os.path.join(TMP_DIR, tag + ".json")
    cmd = [sys.executable, os.path.join(HERE, "run_vector_bench.py"), "--single",
           "--system", job["system"], "--index", job["index"], "--n", str(job["n"]),
           "--dim", str(job["dim"]), "--topk", str(job["topk"]),
           "--queries", str(job["queries"]), "--seed", str(job["seed"]),
           "--clusters", str(job["clusters"]), "--noise", str(job["noise"]),
           "--warmup", str(job["warmup"]), "--out", out,
           "--concurrency", ",".join(str(c) for c in job["concurrency"])]
    print("\n>>> %s" % " ".join(cmd[2:]), flush=True)
    r = subprocess.run(cmd, cwd=HERE)
    if r.returncode != 0 or not os.path.exists(out):
        return {"system": job["system"], "index": job["index"], "n": job["n"],
                "ok": False, "error": "worker exit=%d" % r.returncode}
    with open(out) as f:
        return json.load(f)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--systems", default=DEFAULT_SYSTEMS)
    p.add_argument("--indexes", default="", help="覆盖默认索引列表（逗号分隔）")
    p.add_argument("--n", default="1000,10000,100000")
    p.add_argument("--dim", type=int, default=PROD_DIM)
    p.add_argument("--topk", type=int, default=PROD_TOP_K)
    p.add_argument("--queries", type=int, default=200)
    p.add_argument("--warmup", type=int, default=20)
    p.add_argument("--concurrency", default="1,8")
    p.add_argument("--seed", type=int, default=42)
    p.add_argument("--clusters", type=int, default=64)
    p.add_argument("--noise", type=float, default=0.25)
    p.add_argument("--recall", type=int, default=1)
    p.add_argument("--name", default="vector_bench")
    # worker 模式
    p.add_argument("--single", action="store_true")
    p.add_argument("--system", default="")
    p.add_argument("--index", default="")
    p.add_argument("--out", default="")
    args = p.parse_args()

    if args.single:
        # worker 模式：单组合，n 取标量
        args.n = int(str(args.n).split(",")[0])
        args.concurrency = [int(x) for x in str(args.concurrency).split(",")]
        os.makedirs(TMP_DIR, exist_ok=True)
        rec = run_one(args)
        with open(args.out, "w") as f:
            json.dump(rec, f, ensure_ascii=False, default=str)
        return 0

    args.n = [int(x) for x in str(args.n).split(",")]
    args.concurrency = [int(x) for x in str(args.concurrency).split(",")]

    jobs = build_matrix(args)
    print("矩阵：%d 个组合" % len(jobs))
    t0 = time.time()
    records = [spawn(j) for j in jobs]

    meta = {
        "date": time.strftime("%Y-%m-%d %H:%M:%S"),
        "host": os.uname().nodename,
        "nproc": os.cpu_count(),
        "cpu": [l.split(":", 1)[1].strip() for l in open("/proc/cpuinfo")
                if l.startswith("model name")][:1],
        "systems": args.systems, "n_list": args.n, "dim": args.dim, "topk": args.topk,
        "queries": args.queries, "concurrency": args.concurrency,
        "seed": args.seed, "clusters": args.clusters, "noise": args.noise,
        "elapsed_s": time.time() - t0,
        "notes": ("clustered synthetic vectors, L2-normalized (cosine==IP); "
                  "ground truth = faiss IndexFlatIP exact; "
                  "kvstore server pinned to cores %s via taskset" % os.environ.get("SERVER_CORES", "2,3")),
    }
    dump_result(args.name, meta, records)
    print("总耗时 %.1fs" % meta["elapsed_s"])
    return 0


if __name__ == "__main__":
    sys.exit(main())
