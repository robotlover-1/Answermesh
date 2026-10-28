# 向量检索对比基准：kvstore VSEARCH vs faiss vs Milvus

测的是语义缓存那条读路径上的向量检索：kvstore 自研的 `VSEARCH` 命令，和两个业界实现
（faiss 进程内库、Milvus Lite 嵌入式）在**同一批数据、同一批查询、同一 topk** 下的
延迟 / 吞吐 / 召回 / 内存 / 建索引耗时。

结论与口径说明见 [REPORT.md](REPORT.md)。这里只讲怎么跑。

## 依赖

```bash
# 1. kvstore 二进制（本基准的 VSEARCH 来自这里）
cd ../../kvstore/kvstore && make

# 2. Python 侧依赖
pip install --user faiss-cpu pymilvus milvus-lite numpy matplotlib
```

`milvus-lite` 随 `pymilvus` 一起装；不需要 Docker（本机 docker daemon 未启动，
Milvus Lite 是 pip 内嵌的单进程实现）。

## 跑

```bash
cd bench/vector

# 完整矩阵：3 个系统 × {1k,10k,100k} × dim=384 × topk=30
python3 run_vector_bench.py

# 只跑 kvstore，快速冒烟
python3 run_vector_bench.py --systems kvstore --n 1000 --queries 50 --concurrency 1,4

# 换规模 / 换 topk
python3 run_vector_bench.py --n 1000,10000,100000 --topk 10
python3 run_vector_bench.py --dim 256 --systems kvstore,faiss

# 只跑某几个索引
python3 run_vector_bench.py --systems faiss --indexes flat,hnsw
```

结果落到 `results/vector_bench_<日期>.json`（原始数据）与 `results/tmp/`（每个组合的
中间结果，出错的组合也留着，便于复现）。

## 画图

```bash
python3 plot_vector_bench.py                          # 自动取最新结果
python3 plot_vector_bench.py --input results/vector_bench_2026-09-14.json
```

产出在 `results/plots/`：延迟随规模增长、召回—延迟权衡曲线、建索引耗时、
kvstore 读路径拆解、并发吞吐。

## 设计要点（为什么这么测）

- **每个组合跑在独立进程里**。faiss / Milvus Lite 都在进程内，kvstore 的内存要看
  server 进程的 RSS —— 同一个进程连跑不同规模会把内存数字搅在一起。
- **近似索引必须扫自己的召回旋钮**（HNSW 的 `efSearch`/`ef`、IVF 的 `nprobe`）。
  拿 `nprobe=1` 的 IVF 去比精确检索的延迟是没意义的，所以每个近似索引都出一整条
  recall—延迟曲线，报告里在**等 recall** 下比。
- **ground truth 用 faiss `IndexFlatIP` 独立算**，不依赖被测后端。kvstore 也是暴力
  精确检索，所以它的 recall 应当恒等于 1.0 —— 这同时是对 VSEARCH 正确性的一道校验
  （结果里记录了 `exact_match`）。
- **绑核**沿用 `kvstore/kvstore/tools/bench/bench_common.py` 的口径：server 绑
  `SERVER_CORES`（默认 2,3），client 在核 0。可用环境变量覆盖。

## 数据分布

合成向量是**聚类分布**（`gen_vectors`，默认 64 个簇 + 噪声），不是均匀随机。真实 e5
嵌入是同主题聚集、跨主题余弦饱和（`ai-chat-service/dev.config.yaml` 里记 0.84~0.9），
均匀随机的近邻是退化的，会让 ANN 的 recall 失真。查询向量由库内点加小扰动生成
（模拟「同一问题的改写」这种真实命中场景）。

## 目录

```
harness.py              RESP 客户端 / 数据生成 / 统计 / server 生命周期
run_vector_bench.py     矩阵编排（driver）+ 单组合执行（--single）
plot_vector_bench.py    画图
backends/base.py        后端接口 + 并发 QPS 测量
backends/kvstore_backend.py
backends/faiss_backend.py
backends/milvus_backend.py
results/                原始 JSON 与图
```
