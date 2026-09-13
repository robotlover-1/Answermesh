import sys, os
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from parse import parse, critical_constraints
from decision import hard_decide
# Task3：decision 纯规则化——返回 (shared, reason, soft)，永不含模型分。
# 本文件锁定：无模型依赖、hard reject 集与 reason 逐一与 Phase1 一致、soft 默认关闭保守拒。

def H(q, c):
    return hard_decide(parse(q), parse(c))

def test_pure_no_model():
    # ok 命中（同桶别名）→ shared/reason/soft；soft 恒 False（不是软通道）
    s, r, soft = H("生成一个红黑树", "生成一个 rbtree")
    assert s is True and r == "ok" and soft is False
    # decision 不再产 score：三元组里没有可分（对比旧 decide 四元/元组首位 score）
    assert len(H("生成一个红黑树", "生成一个 rbtree")) == 3

def test_hard_rejects_unchanged():
    # reject reason 与 Phase1 逐一相同（subject/lang/op/intent/constraint 轴）
    assert H("C 实现红黑树", "C++ 实现 rbtree")[:2] == (False, "language_conflict")
    assert H("红黑树是什么", "实现一个 red-black tree")[:2] == (False, "intent_conflict")
    assert H("用 Python 实现红黑树插入", "用 Python 实现 rbtree 删除")[:2] == (False, "operation_conflict")
    assert H("实现线程安全的 C++ 红黑树", "实现持久化的 C++ 红黑树")[:2] == (False, "constraint_conflict")

def test_soft_not_triggered_when_disabled():
    # "LRU 类无 subject 样本"（两侧均不在本体、subject_id 均为 None）→ subject 硬门"缺 id"拟拒；
    # soft 默认关闭 → 保守拒 unknown_subject（零回归也在此锁定）。
    # 注：real 本体已把 "LRU" 归 memory_cache，故用同类的真缺-id pair（bloom filter）做为确定样例；
    # "实现布隆过滤器" 双侧 subject_id 实测都 None。
    s, r, soft = H("实现布隆过滤器", "写一个 bloom filter")
    assert s is False and r == "unknown_subject" and soft is False


# ===== critical_constraints 纯规则 =====
def test_critical_constraints_mapping():
    assert critical_constraints(parse("实现一个线程安全的红黑树")) == frozenset({"concurrency"})
    assert critical_constraints(parse("写一个持久化的红黑树")) == frozenset({"persistence"})
    assert critical_constraints(parse("只实现红黑树的插入")) == frozenset({"scope_limitation"})
    assert critical_constraints(parse("生成一个红黑树")) == frozenset()  # 无硬约束关键词


# ===== soft 通道（显式开启）专项：语义软命中给独立 reason semantic_soft_match =====
def test_soft_hit_when_enabled():
    # 双侧缺 id 的兼容 pair（bloom filter）：soft 开 → 语义软匹配命中（独立 reason semantic_soft_match）
    # decision.hard_decide 每次调用都读 SEMANTIC_SOFT_FALLBACK，无模块级缓存，故直接切 env 即可。
    old = os.environ.get("SEMANTIC_SOFT_FALLBACK", "0")
    try:
        os.environ["SEMANTIC_SOFT_FALLBACK"] = "1"
        s, r, soft = H("实现布隆过滤器", "写一个 bloom filter")
        assert s is True and r == "semantic_soft_match" and soft is True, (s, r, soft)
    finally:
        os.environ["SEMANTIC_SOFT_FALLBACK"] = old


def test_soft_rejects_conflicting_when_enabled():
    # soft 开但条件不满足：语言冲突（双侧同 id 不同语言）→ 普通硬 language_conflict，soft 不放行。
    old = os.environ.get("SEMANTIC_SOFT_FALLBACK", "0")
    try:
        os.environ["SEMANTIC_SOFT_FALLBACK"] = "1"
        s, r, soft = H("用 Go 构建 hash map", "用 Python 实现 hash table")
        assert (s, r, soft) == (False, "language_conflict", False), (s, r, soft)
    finally:
        os.environ["SEMANTIC_SOFT_FALLBACK"] = old


def test_residual_is_case_insensitive():
    """回归：残差词曾按原文大小写存入，导致「只差一个字母大小写」的问句被判成
    constraint_conflict 而永远不命中缓存（两问本应共用一条缓存）。

    residual 是"内容约束"的比较基准，大小写不构成约束差异 —— 归一化后再比较。
    """
    for q, c in [
        ("生成一个Linklist", "生成一个linklist"),
        ("生成一个LINKLIST", "生成一个linklist"),
        ("用Python写个LRU缓存", "用python写个lru缓存"),
    ]:
        s, r, _ = H(q, c)
        assert (s, r) == (True, "ok"), f"{q!r} vs {c!r} → {(s, r)}"

    # 大小写之外的实质差异仍然要拒（别把这条修成"什么都放行"）
    assert H("用Go写Linklist", "用Python写linklist")[:2] == (False, "language_conflict")
    assert H("生成一个Linklist", "生成一个linklist并排序")[:2] != (True, "ok")
