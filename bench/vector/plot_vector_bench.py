#!/usr/bin/env python3
"""把 run_vector_bench.py 的结果 JSON 画成图。

用法：
    python3 plot_vector_bench.py --input results/vector_bench_2026-09-14.json
    python3 plot_vector_bench.py                 # 自动取 results/ 下最新的一个

产出（默认 results/plots/）：
  latency_vs_n.png        精确检索 p50/p99 随 N 的增长（log-log）
  recall_vs_latency.png   各近似索引的 recall@k — 延迟权衡曲线（按 N 分面）
  build_time_vs_n.png     建索引/灌数据耗时
  kvstore_read_path.png   VSEARCH 单独 vs 加上生产读路径那跳 GET
  qps_vs_concurrency.png  并发吞吐（kvstore 单线程 reactor 的墙）
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import sys
from collections import defaultdict
from pathlib import Path

try:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
except Exception as e:  # noqa: BLE001
    raise SystemExit("matplotlib is required: %s" % e)

# 图上要写中文：显式挑一个系统里有的 CJK 字体，否则全是豆腐块
matplotlib.rcParams["font.sans-serif"] = [
    "Noto Sans CJK SC", "Noto Sans CJK JP", "WenQuanYi Zen Hei",
    "AR PL UMing CN", "DejaVu Sans",
]
matplotlib.rcParams["axes.unicode_minus"] = False

HERE = Path(__file__).resolve().parent

# 每个 (system,index) 一套固定样式：颜色按系统分，线型按索引分
COLOR = {"kvstore": "#c0392b", "faiss": "#2471a3", "milvus": "#1e8449"}
LINESTYLE = {"bruteforce": "-", "flat": "-", "hnsw": "--", "ivf": ":", "auto": "-."}
MARKER = {"bruteforce": "o", "flat": "o", "hnsw": "s", "ivf": "^", "auto": "D"}
LABEL = {"bruteforce": "VSEARCH (exact)", "flat": "FlatIP (exact)",
         "ivf": "IVF (ANN)", "hnsw": "HNSW (ANN)", "auto": "AUTOINDEX (ANN)"}


def label_of(r):
    return "%s %s" % (r["system"], LABEL.get(r["index"], r["index"]))


class LegendOnce:
    """同一个 (system,index) 在多个 N 上各画一次，但图例只出现一次。"""

    def __init__(self):
        self.seen = set()

    def kw(self, r, **extra):
        key = (r["system"], r["index"])
        d = dict(style_of(r))
        if key not in self.seen:
            self.seen.add(key)
            d["label"] = label_of(r)
        d.update(extra)
        return d


def style_of(r):
    return {"color": COLOR.get(r["system"], "gray"),
            "linestyle": LINESTYLE.get(r["index"], "-"),
            "marker": MARKER.get(r["index"], "o")}


def load(path: Path):
    with path.open() as f:
        return json.load(f)


def ok_records(data):
    return [r for r in data["records"] if r.get("ok")]


def latest_input() -> Path:
    cands = sorted(glob.glob(str(HERE / "results" / "vector_bench_*.json")))
    if not cands:
        raise SystemExit("results/ 下没有 __vector_bench_*.json，先跑 run_vector_bench.py")
    return Path(cands[-1])


# ------------------------------------------------------------------ 图 1
def plot_latency_vs_n(recs, out: Path, dim, topk):
    """精确检索：p50/p99 随 N 的增长。ANN 单独画在 recall-latency 图里，不混进来。"""
    exact = [r for r in recs if r["index"] in ("bruteforce", "flat")]
    if not exact:
        return
    fig, ax = plt.subplots(figsize=(8.5, 5.5))
    once = LegendOnce()
    for r in exact:
        if not r["ann_points"]:
            continue
        p50 = r["ann_points"][0]["latency_ms"]["p50"]
        # Milvus Lite 在 ~9 万条以上会退化成近似检索（连 FLAT 都不搜全），那个点不能当成
        # "性能变好"——画成空心并标注，且不参与连线。
        degraded = r["system"] == "milvus" and r["n"] >= 90000
        if degraded:
            ax.plot(r["n"], p50, marker="o", ls="none", mfc="none", mec=COLOR[r["system"]],
                    ms=10, mew=2)
            ax.annotate("%.2fms\n⚠ Lite 未搜全，不可比" % p50, (r["n"], p50),
                        textcoords="offset points", xytext=(-14, -34), fontsize=7.5,
                        color=COLOR[r["system"]], ha="center")
            continue
        ax.plot(r["n"], p50, **once.kw(r))
        ax.annotate("%.2fms" % p50, (r["n"], p50),
                    textcoords="offset points", xytext=(6, 4), fontsize=8)
    ax.annotate("↔ kvstore 与 Milvus 在 1k~10k 之间交叉：小规模 kvstore 快，约 2~5k 条起 Milvus 反超",
                (0.30, 0.04), xycoords="axes fraction", fontsize=8.5, color="#555")
    ax.set_xscale("log")
    ax.set_yscale("log")
    ax.set_xlabel("N (库内向量条数)")
    ax.set_ylabel("单条查询延迟 p50 (ms, log)")
    ax.set_title("精确检索延迟 vs 规模（dim=%d, topk=%d, 单线程）" % (dim, topk))
    ax.grid(True, which="both", alpha=0.25)
    ax.legend(fontsize=9)
    fig.tight_layout()
    fig.savefig(out, dpi=160, bbox_inches="tight")
    plt.close(fig)


# ------------------------------------------------------------------ 图 2
def plot_recall_vs_latency(recs, out: Path, topk):
    """ANN 的核心图：每个参数点一个 (延迟, recall)，曲线左下角最优。"""
    anns = [r for r in recs if r["index"] in ("hnsw", "ivf", "auto")]
    if not anns:
        return
    ns = sorted({r["n"] for r in anns})
    # 精确检索基准线：kvstore(VSEARCH) 与 faiss(FlatIP) 都画出来，同 N 下直接可比
    exact = [r for r in recs if r["index"] in ("bruteforce", "flat")]
    fig, axes = plt.subplots(1, len(ns), figsize=(5.4 * len(ns), 5), squeeze=False)
    for ax, n in zip(axes[0], ns):
        once = LegendOnce()          # 每个分面都要有完整图例
        for r in [x for x in anns if x["n"] == n]:
            xs = [p["latency_ms"]["p50"] for p in r["ann_points"]]
            ys = [p.get("recall_at_k", float("nan")) for p in r["ann_points"]]
            order = sorted(range(len(xs)), key=lambda i: xs[i])
            ax.plot([xs[i] for i in order], [ys[i] for i in order], **once.kw(r))
        ax.axhline(1.0, color="gray", lw=0.8, ls="--", alpha=0.6)
        for i, e in enumerate([x for x in exact if x["n"] == n and x["ann_points"]]):
            p50 = e["ann_points"][0]["latency_ms"]["p50"]
            col = COLOR.get(e["system"], "gray")
            ax.axvline(p50, color=col, lw=1.4, ls=":", alpha=0.8)
            ax.annotate("精确 %s %.2fms" % (e["system"], p50), (p50, 1.045 - i * 0.075),
                        fontsize=7.5, color=col, ha="center", va="bottom")
        ax.set_xscale("log")
        ax.set_xlabel("p50 延迟 (ms, log)")
        ax.set_ylabel("recall@%d" % topk)
        ax.set_ylim(0, 1.20)
        ax.set_title("N = %s" % f"{n:,}")
        ax.grid(True, which="both", alpha=0.25)
        ax.legend(fontsize=8, loc="lower right", framealpha=0.9)
    fig.suptitle("近似检索的召回—延迟权衡（越靠左上越好；灰线=精确检索基准）", y=1.02)
    fig.tight_layout()
    fig.savefig(out, dpi=160, bbox_inches="tight")
    plt.close(fig)


# ------------------------------------------------------------------ 图 3
def plot_build_time(recs, out: Path):
    fig, ax = plt.subplots(figsize=(8, 5))
    once = LegendOnce()
    for r in recs:
        ax.plot(r["n"], r["build_s"], **once.kw(r))
    ax.set_xscale("log")
    ax.set_yscale("log")
    ax.set_xlabel("N (库内向量条数)")
    ax.set_ylabel("空库 → 可查询 耗时 (s, log)")
    ax.set_title("灌数据 / 建索引耗时")
    ax.grid(True, which="both", alpha=0.25)
    ax.legend(fontsize=9)
    fig.tight_layout()
    fig.savefig(out, dpi=160, bbox_inches="tight")
    plt.close(fig)


# ------------------------------------------------------------------ 图 4
def plot_kvstore_read_path(recs, out: Path):
    rs = [r for r in recs if r.get("read_path")]
    if not rs:
        return
    rs = sorted(rs, key=lambda r: r["n"])
    labels = ["%s" % f"{r['n']:,}" for r in rs]
    x = range(len(rs))
    v = [r["read_path"]["vsearch_only_ms"]["p50"] for r in rs]
    vg = [r["read_path"]["vsearch_plus_get_ms"]["p50"] for r in rs]
    width = 0.36
    fig, ax = plt.subplots(figsize=(7.5, 5))
    b1 = ax.bar([i - width / 2 for i in x], v, width, label="VSEARCH only", color="#c0392b")
    b2 = ax.bar([i + width / 2 for i in x], vg, width, label="VSEARCH + GET (生产读路径)",
                color="#e59866")
    for bars in (b1, b2):
        for b in bars:
            ax.text(b.get_x() + b.get_width() / 2, b.get_height() * 1.02,
                    "%.2f" % b.get_height(), ha="center", va="bottom", fontsize=8)
    ax.set_xticks(list(x))
    ax.set_xticklabels(labels)
    ax.set_xlabel("N")
    ax.set_ylabel("p50 延迟 (ms)")
    ax.set_title("kvstore 读路径：VSEARCH vs 再加一跳 GET")
    ax.legend()
    fig.tight_layout()
    fig.savefig(out, dpi=160, bbox_inches="tight")
    plt.close(fig)


# ------------------------------------------------------------------ 图 5
def plot_qps_vs_concurrency(recs, out: Path, dim):
    fig, ax = plt.subplots(figsize=(8, 5))
    once = LegendOnce()
    for r in recs:
        cs = sorted(r["concurrency"], key=lambda c: c["concurrency"])
        xs = [c["concurrency"] for c in cs]
        ys = [c["qps"] for c in cs]
        ax.plot(xs, ys, **once.kw(r))
    ax.set_xscale("log")
    ax.set_yscale("log")
    ax.set_xlabel("并发客户端数 (log)")
    ax.set_ylabel("吞吐 QPS (log)")
    ax.set_title("并发吞吐（dim=%d；注意进程内后端受 Python GIL 限制）" % dim)
    ax.grid(True, which="both", alpha=0.25)
    ax.legend(fontsize=8)
    fig.tight_layout()
    fig.savefig(out, dpi=160, bbox_inches="tight")
    plt.close(fig)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--input", default="")
    p.add_argument("--outdir", default=str(HERE / "results" / "plots"))
    args = p.parse_args()

    path = Path(args.input) if args.input else latest_input()
    data = load(path)
    recs = ok_records(data)
    meta = data.get("meta", {})
    dim, topk = meta.get("dim", 0), meta.get("topk", 0)
    outdir = Path(args.outdir)
    outdir.mkdir(parents=True, exist_ok=True)

    print("输入: %s（%d 条有效记录）" % (path, len(recs)))
    plot_latency_vs_n(recs, outdir / "latency_vs_n.png", dim, topk)
    plot_recall_vs_latency(recs, outdir / "recall_vs_latency.png", topk)
    plot_build_time(recs, outdir / "build_time_vs_n.png")
    plot_kvstore_read_path(recs, outdir / "kvstore_read_path.png")
    plot_qps_vs_concurrency(recs, outdir / "qps_vs_concurrency.png", dim)
    for f in sorted(outdir.iterdir()):
        print("  %s" % f.resolve())


if __name__ == "__main__":
    sys.exit(main())
