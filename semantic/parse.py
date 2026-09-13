"""parse.py —— 槽位抽取（semantic Phase1 Task2）

从一阶问句里结构化抽取 主题/意图/语言/操作/上下文标志，供给式 parse()。
两个核心接口：
    normalize(text) -> str                      归一化（NFKC/小写/符号保护/空白）
    parse(text) -> ParsedQuery   槽位结果数据类

纯逻辑模块，不 import nuxt/ontology 之外的外部服务。
接口契约（Task 1 已交付）：
    from ontology import lookup_subject_id, ONTOLOGY_VERSION
    lookup_subject_id(text): 全句最长别名命中（latin 词边界 / 中文受控子串）。

Task 3 才接入 residual_words / fingerprint / fingerprint_eligible；本任务 parse()
对这三者一律置空/False。
"""
try:  # py3.8 兼容：'str | None' 要到 3.10，故用 typing.Optional
    from typing import Optional  # noqa: F401
except ImportError:  # pragma: no cover
    pass
from dataclasses import dataclass
import hashlib
import json
import re
import unicodedata
import jieba  # noqa: F401  —— residual 词面统计在 Task3 使用，此处保留 import

# 约束/描述性多音节词注册进 jieba，避免被拆成"线程/安全"而丢失"未解析约束"残差语义。
# 只 add_word 不改写停用表，幂等且仅影响这些词本身的切分原子性（模块只读语义仍纯逻辑）。
for _phrase in ("线程安全", "持久化", "持久型", "重复键", "父指针", "无递归", "关联数组"):
    if _phrase and _phrase not in jieba.dt.FREQ:
        jieba.add_word(_phrase, freq=50000)

from ontology import (
    lookup_subject_id, ONTOLOGY_VERSION,
    lookup_lang_entity, LANG_TERMS_VERSION, TermMatch,
)

# Task3：术语实体化 + 指纹安全 + 语言门控。PARSER_VERSION v1→v3：
# fingerprint payload 含 parser/ontology/lang_terms 版本 → 同文本新值不同、旧值成孤儿，无跨版混用。
PARSER_VERSION = "v3"
# lang_terms 实体 kind 属库/内建/类 → 非 alias_of 概念，指纹不可直命（仅向量+decision）。
_LANG_ENTITY_KINDS = {"library_type", "builtin_type", "class"}

# fmt: off
# ============ 主题句式（另有新增中英句式见 _SUBJECT_EN，均在 _extract_subject_pair 汇总） ============
SUBJECT_PATTERNS = [
    r"^什么是(.+?)[？?]?$",
    r"^(.+?)是什么[？?]?$",
    r"^(.+)指的是什么[？?]?$",
    r"^(.+)是什么意思[？?]?$",
    r"^请介绍一下(.+?)[？?]?$",
    r"^实现(?:一个)?(?:简单|简易|完整|基础)?(?:版本的)?(.+?)[。！!?？]?$",
    r"^写(?:一个|出)?(?:简单|简易|完整|基础)?(?:版本的)?(.+?)[。！!?？]?$",
    r"^生成(?:一个)?(?:简单|简易|完整|基础)?(?:版本的)?(.+?)[。！!?？]?$",
    r"^构建(?:一个)?(?:简单|简易|完整|基础)?(?:版本的)?(.+?)[。！!?？]?$",
    r"^之前实现过(.+?)吗[？?]?$",
    r"^以前写过(.+?)吗[？?]?$",
    r"^(.+?)怎么(写|实现)[？?]?$",              # "冒泡排序怎么写/怎么实现" → 冒泡排序
    r"^怎么(?:写|实现)(.+?)[。！!?？]?$",          # "怎么写冒泡排序" → 冒泡排序
    r"^(.+?)的(插入|删除|遍历|查询|添加|更新)[。！!?？]?$",  # "红黑树的插入" → 红黑树
    r"^用.+?(?:写|实现|生成|构建)(.+?)[。！!?？]?$",  # "用Go实现红黑树" → 红黑树（语言另由 extract_language 处理）
]

# 新增中英句式：英文定义/实现句式。theme 捕获组统一放 group(1)。
_EN = {
    "definition": [r"^what is(?: an?| the)? (.+?)[?]?$", r"^define (.+?)[.]?$",
                   r"^explain(?: the concept of)? (.+?)[?]?$", r"^what does (.+?) (?:mean|do)[?]?$"],
    "implementation": [r"^(?:implement|write|build|create|code) (?:an? |the )?(.+?)[.]?$",
                       r"^implement (.+?) in (.+?)$",            # group2=语言
                       r"^write (.+?) using (.+?)$",
                       r"^(.+?) (?:implementation|implementation in) (.+?)$"],
}
# 英文主题句式扁平化用于主题抽取：全部取 group(1) 为主题。
SUBJECT_EN_PATTERNS = [pat for pats in _EN.values() for pat in pats]


def normalize_subject(subject):
    subject = re.sub(r"^(一个|简单的|简易的|完整的|基础的|版本的)+", "", subject)
    # 剥尾部后缀/操作词：写一个红黑树的实现 → 红黑树；红黑树的删除 → 红黑树。
    # 否则 SUBJECT:红黑树的实现 与 SUBJECT:红黑树 标记桶不匹配、余弦被压低。
    # 删除/插入等操作区分由 rerank 的 extract_operation 单独负责。
    subject = re.sub(r"(的实现(方法|方式)?|的代码|的方法|的程序|的demo|的示例|的删除|的插入|的遍历|的查询|的添加|的更新)$", "", subject)
    return subject.strip()


def _cn_subject_on(config_text):
    """在中英句式中文句式（SUBJECT_PATTERNS，含中文头配英文宾语）上跑一遍并返回命中的 subject_text。"""
    for p in SUBJECT_PATTERNS:
        m = re.match(p, config_text)
        if m:
            return normalize_subject(m.group(1).strip())
    return None


def _extract_subject_pair(text):
    """双路主题：先跑原中文单条主题句式、再跑新增中英句式；返回 (subject_text, subject_id)。

    subject_text=首个句式命中主题，否则 None；
    subject_id = lookup(subject_text) 或 全体归一化文本兜底 lookup。

    carry-over(R3, Task4 评审)：中文句式内嵌英文宾语（"实现一个 red-black tree"）在把整句
    压空格/小写后，英文 token 间隙被吞，subject_text 变成 'red-blacktree' 型残缺形态，无法命中
    本体（本体存 'red black tree'/'red-black tree'）→ subject_id=None → decision 只能给
    unknown_subject 而非 intent_conflict。修法：当紧凑文本跑中文句式得到的 subject_text 经
    lookup_subject_id 未命中时，用「保空格」normalized 文本重跑中文句式（英文保留 token 间隙），
    若命中本体则采用；纯无空格中文不受影响（紧凑=保空格）。仅命中时覆盖，否则维持紧凑结果。
    """
    n = re.sub(r"\s+", "", text.strip().lower())
    subject_text = _cn_subject_on(n)
    if subject_text is not None and lookup_subject_id(subject_text) is None:
        space_retry = _cn_subject_on(normalize(text))
        if space_retry is not None and lookup_subject_id(space_retry) is not None:
            subject_text = space_retry
    if subject_text is None:
        # 英文句式在保留空格的文本上匹配（原中文句式已在上面无空格 n 上跑过）
        for p in SUBJECT_EN_PATTERNS:
            m = re.match(p, text.strip())
            if m:
                subject_text = normalize_subject(m.group(1).strip())
                break
    subject_id = lookup_subject_id(subject_text) if subject_text else None
    if subject_id is None:
        subject_id = lookup_subject_id(n)
    return subject_text, subject_id


# ---- Task3：语言实体化回退 ----
# 句式抽出的 subject 未被全局本体(alias_of)认出、但语言已知时，尝试语言受限实体表
# (lang_terms cpp/python/java…)。返回 (kind/family/namespace/type_args/surface) 汇总的 dict 或 None；
# ≥2 个不同实体 → 返回 sentinel dict(multi_subject=True)。供 parse() 决策 subject_id/source。
_MULTI_SENTINEL = {
    "multi_subject": True, "entity_id": None, "kind": None, "family": None,
    "namespace": None, "type_args": (), "surface": None,
}


def _resolve_lang_entity(subject_text, full_normalized, lang):
    """subject_text 部分未被概念解出 → 尝试 lang 受限实体的**单实体**、**多实体**或空。
    参数 span_text：句式命中子句优先，否则保守整句扫描（澄清：句式空→全句）。
    返回 dicts：单/multi 或 None（0 实体）。"""
    span = subject_text if subject_text and subject_text.strip() else full_normalized.strip()
    matches = lookup_lang_entity(lang, span)
    if not matches:
        # 子句是语言无关但整句含的容器词非类别词(如"写一个 list")→ 子句已覆盖；此处不兜底整句再查，
        # 保持 0→None 语义(避免英文无关 "in" 误撞)。空 span 不会发生(上面已回退 full)。
        return None
    uniq = {}
    for m in matches:
        uniq.setdefault(m.entity_id, m)
    if len(uniq) > 1:
        return dict(_MULTI_SENTINEL)
    ent = next(iter(uniq.values()))
    return {
        "multi_subject": False, "entity_id": ent.entity_id, "kind": ent.kind,
        "family": ent.family, "namespace": ent.namespace,
        "type_args": tuple(ent.type_args), "surface": ent.surface,
    }


# ============ 意图分类（definition 组排在 implementation 之前，兼顾英文定义不误撞实现） ============
# 中文沿用 semantic.py 句式语义；追加"使用/编写/用X写…"等 → implementation 的句式，以及英文触发词。
# 各 intent 的 cn 词集（在无空格文本上搜索）：
INTENT_CN = {
    "definition":   [r"是什么", r"什么是", r"什么意思", r"介绍一下", r"概念", r"定义"],
    "comparison":   [r"区别", r"差异", r"对比", r"哪个好", r"和.*区别"],
    "reason":       [r"为什么", r"原因", r"为何"],
    "troubleshooting": [r"报错", r"错误", r"异常", r"失败", r"怎么解决", r"无法"],
    "implementation":  [r"实现", r"写一个", r"写出", r"给.*代码", r"代码示例", r"完整代码",
                        r"生成", r"构建", r"做一个", r"使用", r"编写",
                        r"^(?:用|使用).{0,16}?(?:写|编写|实现|生成|构建|做)"],
    "operation":       [r"删除", r"插入", r"添加", r"遍历", r"查询", r"怎么(增|删|改|查|插)"],
    "state_update":    [r"记住", r"以后.*回答", r"后面.*会问", r"从现在开始", r"你是一个",
                        r"你是一名", r"扮演", r"我的名字是"],
    "history_query":   [r"之前.*过.*吗", r"以前.*过.*吗", r"刚才.*过.*吗", r"是否.*过",
                        r"还记得", r"上次", r"前面"],
}
# 各 intent 的英文触发词（在保留空格文本上搜索，词边界分隔）。
INTENT_EN = {
    "definition":     [r"what is", r"what are", r"what does", r"what do", r"\bdefine",
                       r"\bexplain(?!.*implement)", r"\bmeaning\b", r"\bdefinition\b"],
    "comparison":     [r"\bdifference\b", r"\bcompare\b", r"\bversus\b", r"\bvs\b", r"\bcontrast\b"],
    "reason":         [r"\bwhy\b", r"\breason\b"],
    "troubleshooting":[r"\berror\b", r"\bexception\b", r"\bfailed\b", r"\bfail\b", r"unable to"],
    "implementation": [r"\bimplement", r"\bwrite\b", r"\bbuild\b", r"\bcreate\b", r"\bcode\b"],
    "operation":      [r"\binsert(?:ion)?\b", r"\bdelete\b", r"\bremove\b", r"\btraverse", r"\bsearch\b", r"\bupdate\b"],
}

# 判定优先级：definition→comparison→reason→troubleshooting→implementation→operation→state_update→history_query
_INTENT_ORDER = ["definition", "comparison", "reason", "troubleshooting", "implementation",
                 "operation", "state_update", "history_query"]


def extract_intent(text):
    """规则型意图分类：definition/comparison/reason/troubleshooting/implementation/operation/
    state_update/history_query/unknown。

    definition 组先于 implementation，故中文/英文定义句不会误撞 implementation。
    中文词在无空格文本上搜索；英文词在原保留空格文本上搜索（词边界）。
    """
    n = re.sub(r"\s+", "", text.lower())
    for intent in _INTENT_ORDER:
        if any(re.search(p, n) for p in INTENT_CN[intent]):
            return intent
        if any(re.search(p, text) for p in INTENT_EN.get(intent, [])):
            return intent
    return "unknown"


CONTEXT_PATTERNS = [
    r"之前", r"刚才", r"前面", r"上次", r"上一", r"继续", r"还是",
    r"再改", r"刚刚", r"还记得", r"基于前", r"按照前", r"接着",
]


def is_context_dependent(text):
    """依赖当前会话上下文的问题（之前/刚才/继续…）→ 不适合全局语义缓存。"""
    n = re.sub(r"\s+", "", text.lower())
    return any(re.search(p, n) for p in CONTEXT_PATTERNS)


# 状态修改指令：会改变会话状态（身份/偏好/记忆/输出规则）→ 不可缓存、不可命中缓存。
# 这类内容即使 Embedding 再强也会与"无状态问答"混淆（你是一个工程师 vs 你是一个艺术家），
# 必须在准入层直接绕过。
STATEFUL_PATTERNS = [
    # 记忆与偏好
    r"记住", r"请记得", r"不要忘记", r"以后.*回答", r"后面.*会问", r"接下来.*会问",
    # 角色与身份设置
    r"你是一个", r"你是一名", r"你现在是", r"从现在开始", r"扮演", r"作为.*回答", r"假设你是",
    # 用户信息设置
    r"我是一个", r"我是一名", r"我的名字是", r"我的职业是", r"我的偏好是",
    # 输出规则设置
    r"以后都", r"接下来都", r"后续都", r"回答时要", r"回答不要", r"统一使用",
]


def is_stateful_instruction(text):
    n = re.sub(r"\s+", "", text.strip().lower())
    return any(re.search(p, n) for p in STATEFUL_PATTERNS)


def should_bypass_semantic_cache(text):
    """缓存准入：上下文依赖 或 状态修改指令 → 不查不写全局语义缓存。"""
    return is_context_dependent(text) or is_stateful_instruction(text)


# ============ 语言抽取：长模式优先，避免 c 吞 c++ / java 吞 javascript ============
# CJK 属 Unicode \w，故词界一律用 ASCII 邻接 lookaround（对中英粘连"用Python"同样生效）。
# 顺序即优先级；命中 ≥2 个不同语言视为歧义 → None。
_LK = lambda body: "(?<![A-Za-z0-9])" + body + "(?![A-Za-z0-9])"
LANG_PATTERNS = [
    ("cpp",      re.compile(r"(?<![A-Za-z0-9+#:\.])c\+\+(?![A-Za-z0-9])|" + _LK(r"cxx") + r"|" + _LK(r"cpp") + r"|" + _LK(r"CPP"))),
    ("csharp",   re.compile(_LK(r"c") + r"#(?![A-Za-z0-9])|" + _LK(r"csharp") + r"|" + _LK(r"C#"))),
    ("dotnet",   re.compile(r"(?<![A-Za-z0-9])\.net(?![A-Za-z0-9])")),
    ("javascript", re.compile(_LK(r"javascript"))),
    ("node",     re.compile(r"node\.js|" + _LK(r"nodejs") + r"|" + _LK(r"node"))),
    ("typescript", re.compile(_LK(r"typescript"))),
    ("java",     re.compile(_LK(r"java(?!script)"))),
    ("python",   re.compile(_LK(r"python") + r"|" + _LK(r"\bpy\b"))),
    ("rust",     re.compile(_LK(r"rust"))),
    ("golang",   re.compile(_LK(r"golang"))),
]
# `c` 单独特殊：仅命中"c语言"或紧跟实现语境才判（去空白后匹配，"使用 C 编写 rbtree"）。
_C_IMPL_CTX = re.compile(r"(?<![a-z0-9+#])c(?![a-z0-9+#])(?=.*(实现|编写|生成|写|编程|代码|implementation))")

# carry-over(Task3评审)：英文尾式语言 "implement RB-tree insertion in Python / write X in Go"，
# 压缩后 in+lang 粘连丢失词界，故在保留空格的小写文本上用词界匹配（小写 text 预处理）。
_EN_IN_LANG = re.compile(r"\bin\s+(python|py|java|javascript|typescript|go|golang|rust|c\+\+|cpp|ruby|php|swift|kotlin|c#|csharp)\b", re.IGNORECASE)

# ---- Task3 语言门控 ----
# 强证据（长 token/带符号）：出现即判，无需编程锚点。order 内后者若为前者的前缀/变形不重复计。
# ja/ts/js/py 等短别名为次证据同列。剔除弱词 go/swift/ruby/c（单独处理）。
_STRONG_HINT_ORDER = [
    ("objective-c", r"objective-?c"),
    ("cpp",         r"c\+\+"),            # c++ 长于 c，先扫；随后 cpp/cxx/CPP
    ("cpp",         r"\bcxx\b"),
    ("cpp",         r"\bcpp\b"),
    ("cpp",         r"\bCPP\b"),
    ("csharp",      r"c#"),
    ("csharp",      r"\bcsharp\b"),
    ("dotnet",      r"\.net(?![A-Za-z0-9])"),
    ("golang",      r"\bgolang\b"),
    ("typescript",  r"\btypescript\b"),
    ("javascript",  r"\bjavascript\b"),
    ("java",        r"\bjava(?!script)\b"),
    ("python",      r"\bpython\b"),
    ("python",      r"\bpy\b"),
    ("rust",        r"\brust\b"),
    ("php",         r"\bphp\b"),
    ("kotlin",      r"\bkotlin\b"),
    ("node",        r"\bnode\.?js\b|\bnodejs\b"),
]
# 弱词：单现须有编程锚点/技术词共现或带"语言"限定；判中后 go/swift/ruby 归一为 golang/swift/ruby，
# c 仅 c语言/实现语境（沿用上部 _C_IMPL_CTX+“c语言”特判）。
_WEAK_RULE = [
    # (canon_id, 词界 pattern, 触发释义/别名)
    ("golang",  r"go"),
    ("swift",   r"swift"),
    ("ruby",    r"ruby"),
]
_WEAK_ANCHOR = re.compile(
    r"(语言|编程|代码|开发|实现|编写|类型|类|接口|函数|方法|数组|列表|实现|库|算法|struct|class|func|var|func|array|list|map|哈希)"
    r"|using\s+\w+|in\b|language|with\s+\w+|package|import|module|函数|学习|入门", re.I)


def _langs_present(low):
    """在保空格小写文本上扫强/中证据，返回命中语言规范 id 集（go 等弱词另行门控追加）。"""
    low2 = re.sub(r"([\s,，;；.。]+)", " ", low)  # 词隙统一便于词界
    hits = set()
    for canon, pat in LANG_PATTERNS:            # 复用现成强模式（cpp/c#/java/…，无 go）
        if pat.search(low2):
            hits.add(canon)
    # golang 已在 LANG_PATTERNS，js/ts/py 等短别名用 STRONG fallback（LANG_PATTERNS 未含，实走 in 尾式）
    for canon, pat in _STRONG_HINT_ORDER:
        if re.search(pat, low2):
            hits.add(canon)
    return hits


def detect_language(text):
    """语言门控：返回规范语言 id or None。
    强证据（长 token/符号：python/c++/rust/golang/…）直接判；中证据（js/ts/py/java）限定拼写；
    弱词 go/swift/ruby 单现须命中编程锚点（_WEAK_ANCHOR：语言/实现/代码/类型词族共性技术词）才判，
    否则保守放弃（如 "go to the next step"→ 与英文介词高频词 "go" 混淆 → None）。
    c 仅"c语言"或实现语境（_C_IMPL_CTX），由 extract_language 特判合并。
    多条不同语言并存仍按既有一致性 → None（防 C++/Go 同句双解被误标单语言）。
    """
    low = " ".join(text.lower().split())
    hits = _langs_present(low)
    if len(hits) > 1:
        return None
    if hits:
        return next(iter(hits))
    # 无强证据 → 弱词门控（go/swift/ruby 须编程锚点）
    low_wordy = re.sub(r"\s+", " ", low).strip()
    for canon, pat in _WEAK_RULE:
        if re.search(_LK(pat), low_wordy) and _WEAK_ANCHOR.search(low_wordy):
            return canon
    # 白名单命名空间 std::<ident> 是原生 concrete 容器引用(如 std::list<int>)→ 无口语语言词也定为 cpp。
    # 不进文词频大,避免英语副词 "std" 误撞：仅"std::"后紧跟拉丁标识符的**代码形态**。
    if re.search(r"\bstd::[A-Za-z_]\w*", low_wordy) and not re.search(
            r"[一-鿿]{2,}(标准|单位)", low_wordy):
        return "cpp"
    return None


def normalize_lang_tag(word):
    """英文尾式 ' in <lang>' 的语言词 → 规范 id。"""
    w = word.lower()
    for canon, tags in (
        ("cpp", {"c++", "cpp"}),
        ("csharp", {"c#", "csharp"}),
        ("javascript", {"javascript", "js"}),
        ("node", {"node.js", "nodejs", "node"}),
        ("typescript", {"typescript", "ts"}),
        ("python", {"python", "py"}),
        ("golang", {"go", "golang"}),
    ):
        if w in tags:
            return canon
    return w


def extract_language(text):
    """抽取实现目标语言：cpp/csharp/dotnet/javascript/node/typescript/java/python/rust/golang/go/c。
    复用门控 detect_language（强/中证据），再特判 c（c语言/实现语境）与英文尾式 in <lang>；
    命中多个不同语言返回 None。保留既有识别语义，调用方可安全替换原强模式扫描。"""
    low = " ".join(text.lower().split())
    n = re.sub(r"\s+", "", low)
    hits = detect_language(text)
    langs = set()
    if hits:
        langs.add(hits)
    # c 特判：仅"c语言"或紧跟实现语境（去空白后匹配）
    if "c语言" in n or _C_IMPL_CTX.search(n):
        langs.add("c")
    # 英文尾式 "… in python/…"（压缩后 inpython 粘连 → 带空格文本补救）
    in_lang = _EN_IN_LANG.search(low)
    if in_lang:
        langs.add(normalize_lang_tag(in_lang.group(1)))
    if len(langs) == 1:
        return next(iter(langs))
    return None


# ============ 操作抽取：中文→英文 id 映射 + 英文触发词 ============
# 无信息模板词：嵌入权重 0。注意"实现/简单"等是意图/主题词，不在此列（结构化保存）。
STOP_WORDS = {
    "一个", "一下", "请问", "帮我", "可以", "简单地", "请", "能否", "让我", "帮我",
    # 通用疑问/模板词：无内容信息，去掉避免"怎么减肥 vs 怎么学好英语"这类跨主题误命中
    "怎么", "如何", "为什么", "为何", "推荐", "哪些", "多少", "怎样", "为啥",
    "怎么办", "是不是", "有没有", "是否", "什么", "是", "吗", "呢", "吧", "啊",
    "应该", "需要", "想要", "请问一下", "一下",
}

OP_ZH2ID = {
    "插入": "insert", "添加": "insert", "插入节点": "insert",
    "删除": "delete", "移除": "delete",
    "遍历": "traverse",
    "查询": "find", "查找": "find", "搜索": "find",
    "更新": "update", "修改": "update",
    "替换": "replace",
}
# 英文操作词 → 规范 id（专有词按优先级在中文之后扫描）
OP_EN = [
    ("insert", [r"\binsert(?:ion)?s?\b"]),
    ("delete", [r"\bdelete\b|\bdeletion\b|deleting"]),
    ("traverse", [r"\btraverse|\btraversal"]),
    ("find", [r"\bfind\b|\bsearch\b|\blookup\b"]),
    ("update", [r"\bupdate\b|\bmodify\b"]),
    ("remove", [r"\bremove\b|\bremoval\b"]),   # → delete 需容器语境，见 extract_operation
    ("replace", [r"\breplace\b"]),
]
# remove → delete 仅限容器语境（按 subject_id）放宽，否则保守忽略。
_DELETE_CONTAINER_SUBJECTS = {"array", "linked_list", "vector"}


def _subject_erase_forms(subject_id):
    """本体中 subject 的全部别名 -> erase 串集（紧凑=去掉所有空白并小写；spaced=保留单空格小写）。
    优先长串（如 二叉搜索树 先于 二叉），避免短别名残留把操作词错误保留/吞并。"""
    if not subject_id:
        return []
    from ontology import load
    forms = []
    for c in load().get("concepts", []):
        if c.get("id") == subject_id:
            for a in (c.get("aliases") or []):
                low = unicodedata.normalize("NFKC", a).lower()
                compact = re.sub(r"\s+", "", low)
                forms.append(low.strip())
                forms.append(compact)
            break
    seen = set()
    out = []
    for f in forms:
        if f and f not in seen:
            seen.add(f)
            out.append(f)
    return sorted(out, key=len, reverse=True)


def _erase_subject(text, subject_id):
    """把 text 中命中的 subject 别名串删掉，返回 (spacedremain, compactremain)。
    使"二叉搜索树"里的'搜索'、"插入排序"里的'插入'等藏在概念名内的操作字不再被当作真正的数据操作；
    真正的操作(如词尾"插入/删除")因不以别名子串存在而保留。"""
    if not subject_id:
        spaced = re.sub(r"\s+", " ", text.lower()).strip()
        return spaced, re.sub(r"\s+", "", spaced)
    spaced = re.sub(r"\s+", " ", text.lower()).strip()
    comp = re.sub(r"\s+", "", spaced)
    for f in _subject_erase_forms(subject_id):
        spaced = spaced.replace(f, " ")
        comp = comp.replace(re.sub(r"\s+", "", f), "")
    return spaced.strip(), comp.strip()


def extract_operation(text, subject_id=None):
    """抽取具体操作（返回规范英文 id：insert/delete/traverse/find/update/replace）；
    先在文面删去 subject 别名（避免概念名里的'搜索/插入'如 二叉搜索树/插入排序 假当操作），
    再中文子串扫描、英文词界扫描（spaced 保留词隙使 \b 命中）；
    守旧不贪：英文 remove/removal 仅当 subject_id 为容器（array/linked_list/vector）才归 delete。"""
    spaced, n = _erase_subject(text, subject_id)
    # 中文优先（最长条目数序固定，先精确节词亦可）
    terms = sorted(OP_ZH2ID, key=len, reverse=True)
    for op_zh in terms:
        if op_zh in n:
            return OP_ZH2ID[op_zh]
    # 英文词界扫（在保留空格的文本上，才能让 \binsertion\b 这类命中由整句压空格吞掉的英文操作）
    for op_id, pats in OP_EN:
        for p in pats:
            if re.search(p, spaced):
                if op_id == "remove":
                    if subject_id in _DELETE_CONTAINER_SUBJECTS:
                        return "delete"
                    continue  # 非容器语境保守忽略
                return op_id
    return None


# ================= Task 3：residual 残差 + fingerprint_eligible + 版本化指纹 =================
# 模板/句式词白名单：这些词只贡献 content（嵌入用），不构成"未解析约束"（不阻塞 eligible）。
# 首列中文为 brief 给定集；另补少量英文高停用词，使"what is a red-black tree"式英文句
# 残差为空 → definition/implementation 英文句可被真实地归并可缓（而非 None==None 弱通过）。
_RESIDUAL_WHITELIST = {
    "的", "个", "一个", "一下", "请", "帮我", "怎么", "如何", "什么",
    "是", "吗", "呢", "实现", "生成", "编写", "写", "使用", "用",
    "语言", "代码", "树", "节点", "中", "里", "进行",
    # —— 英文功能词（低信息，仅词面占用，不含义）——
    "a", "an", "the", "is", "are", "was", "were", "do", "does", "what",
    "how", "why", "of", "in", "to", "for", "and", "or", "it", "its",
    "my", "please", "write", "make", "show", "give", "create", "that", "this",
}

# residual = 内容 token 中未被以下任一消费的：
#   停用词 / 白名单 / 命中语言的别名 / 中文操作词 / 英文操作词 / subject alias 组成词
LANG_TERMS = {"python", "py", "java", "javascript", "js", "typescript", "ts",
              "go", "golang", "rust", "c", "c++", "cpp", "cxx", "c#", "csharp",
              ".net", "node.js", "nodejs"}
OP_TERMS_ZH = set(OP_ZH2ID.keys())            # 中文操作词（Task 2 定义）
OP_TERMS_EN = {"insert", "insertion", "delete", "deletion", "removal", "remove", "traverse",
               "traversal", "find", "search", "lookup", "update", "modify", "modify",
               "replace", "add", "implement", "implementation", "writing", "written"}


def _tokenize_words(text):
    """jieba 分词 → 去空白 token 集；TextRank 无关，纯词面集合。"""
    return set(w for w in jieba.cut(text) if w.strip() and not w.isspace())


def _concept_aliases_for(subject_id):
    """当前本体中 subject 的全部别名（小写化去空白）。"""
    from ontology import load
    if not subject_id:
        return []
    aliases = []
    for c in load().get("concepts", []):
        if c.get("id") == subject_id:
            aliases = c.get("aliases") or []
            break
    norm = []
    for a in aliases:
        norm.append(a.lower().replace(" ", "").replace("-", ""))
    return norm


def _is_alias_piece(word, subject_id):
    """word 是否为 subject alias 的组成词：
    等价于 alias 整段被 word 覆盖（word 含 alias）或 word 是 alias 的子串（如 '红黑' / 'tree' 拆段）。"""
    w = word.lower().strip()
    if not w or not subject_id:
        return False
    try:
        return any(w in al or al in w for al in _concept_aliases_for(subject_id))
    except Exception:
        return False


def _consumed_cover(_qp):
    """把所有“已消费词”(主题别名/停用/白名单/语言/操作词/命中 surface)的字面拼起来，
    用于识别 jieba 跨界碎片。命中 surface 及其实体概念别名族一并入镜(消 std::list/cpp_std_list 等)：
    语素若落于此总字面 → 非真实内容约束。type_args(int/string…) **不**入镜 → 保留为约束。
    """
    parts = []
    if _qp.subject_id:
        from ontology import load
        for c in load()["concepts"]:
            if c["id"] == _qp.subject_id:
                parts += c["aliases"]
                break
    # 仅库/内建/类实体(非 alias_of)才额外把命中 surface 入镜：它们的别名族不在概念正排表里，
    # 须靠 surface(如 std::list/cpp_std_list)消其残余 token。概念型主体已由其 alias 覆盖，勿再吞宽。
    if (getattr(_qp, "subject_kind", None) in _LANG_ENTITY_KINDS
            and getattr(_qp, "matched_surface", None)):
        parts.append(getattr(_qp, "matched_surface", None))
    parts += list(STOP_WORDS) + list(_RESIDUAL_WHITELIST) + list(LANG_TERMS) + list(OP_TERMS_EN) + list(OP_TERMS_ZH)
    return "".join(parts).lower()


def _residual_words(_qp):
    """未被消费的内容词 → frozenset。空 = 无未解析约束 => eligible 前提之一。"""
    toks = _tokenize_words(_qp.raw_text)
    cover = _consumed_cover(_qp)
    keep = set()
    for w in toks:
        wl = w.lower().strip()
        if not wl:
            continue
        if not re.search(r"[0-9A-Za-z一-鿿]", w):  # 纯标点/分隔 token 无内容信息
            continue
        if w in STOP_WORDS or w in _RESIDUAL_WHITELIST:
            continue
        if (wl in LANG_TERMS or wl in OP_TERMS_EN or w in OP_TERMS_ZH):
            continue
        if _is_alias_piece(w, _qp.subject_id):
            continue
        # jieba 会从别名/模板词边界“借字”成跨界碎片(如“写红黑树”→“写红”)。这类 token 非真实内容约束：
        # 若它含中文且每个字符都已落在已消费词的字面里(如 写∈白名单、红∈别名“红黑树”)，判为碎片丢弃。
        if cover and re.search(r"[一-鿿]", w) and all(ch in cover for ch in wl):
            continue
        # 存归一化后的小写形式，不存原文——残差是"内容约束"的比较基准，
        # 大小写不构成约束差异。存原文会让「生成一个Linklist」与「生成一个linklist」
        # 得到不同的 frozenset，被 decision.py 的残差复核判成 constraint_conflict
        # 从而永远不命中缓存（两者本应共用一条缓存）。
        # 不影响指纹：build_fingerprint 仅在残差为空时才产出，此时这里恒为 no-op。
        keep.add(wl)
    return frozenset(keep)


def _alias_of_only(qp):
    """是否 alias_of 概念（非库/内建/类实体）:概念路径的 subject 未标 kind。"""
    return getattr(qp, "subject_kind", None) is None


def _concept_family(concept_id):
    """alias_of 抽象 family 概念 → 其 canonical implementation-family token（实施族标签）。
    缺省=自身 id（linked_list/hash_table…：概念即族）。唯一特例：抽象"数组/动态数组"族概念 id 为
    `array`，而 lang 实体实现族 token 是 `dynamic_array`（concepts.json 无 dynamic_array 概念标签，
    lang_terms family 用 dynamic_array）。family_compat 需两侧同值 → 此处把抽象族规范到实体侧 family。
    该字段推进 key 不进 fp payload、不参与 eligible，仅决策族比较用；lang 实体路径在 parse() 末尾
    整体覆盖为 ent['family']，故本函数只作用于 concept 命中(subject 未标 kind)路径。"""
    return {"array": "dynamic_array"}.get(concept_id, concept_id)


# Task3 指纹安全：语义指纹只对「alias_of 同实体 + 无 type_args + 白名单/无 namespace + 单主题且干净」
# 的查询可直命。库/内建实体(entity_kind∈{library_type,builtin_type,class})、type_args 非空、自定义
# namespace、multi_subject、无法解析实体 → 一律 eligible=False（只向量召回 + decision 判）。
def _fingerprint_eligible(qp) -> bool:
    if not getattr(qp, "subject_id", None):
        return False
    if getattr(qp, "multi_subject", False):
        return False
    # 库/内建/类实体：implementation_family 仅召回，不直接共享指纹
    if not _alias_of_only(qp):
        return False
    if getattr(qp, "type_args", ()):        # type_args 非空 → 模板参数是硬约束，不可直命
        return False
    ns = getattr(qp, "namespace", None)
    if ns is not None and ns != "std":
        return False
    return (qp.intent not in ("unknown",)
            and not qp.bypass_cache
            and not qp.context_dependent
            and not qp.stateful
            and not _residual_words(qp))


def build_fingerprint(qp):
    """保守准入 + 版本化指纹：格式 sha256(json sort_keys, separators=(',',':'), ensure_ascii=False)。
    意图/语言/操作/主题任一变（do 时清残差归并、跨主题不同指纹）；同措辞即便表达不同 →
    （subject,intent,language,operation,output_type,residual=[] 全同）→ 同指纹。"""
    if not _fingerprint_eligible(qp):
        return None
    payload = {
        "schema": "v1",
        "parser_version": qp.parser_version,
        "ontology_version": qp.ontology_version,
        "lang_terms_version": qp.lang_terms_version,
        "subject_id": qp.subject_id,
        "intent": qp.intent,
        "language": qp.language,
        "operation": qp.operation,
        "output_type": qp.output_type,
        "residual": sorted(qp.residual_words),
    }
    s = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(s.encode("utf-8")).hexdigest()


# ============ Task 3(P2/3)：critical 约束识别（纯规则；供 decision.soft/其他渠道） ============
# 由已知关键词短语映射到约束类别，作为语义软匹配通道（soft）里的"硬约束安全网"：
# 一侧宣称的关键约束若另一侧完全未满足（critical_constraints 之差非空），soft 不放行。
CRITICAL_ZH = {
    "concurrency": ["线程安全", "并发安全", "加锁", "锁"],
    "persistence": ["持久化", "落盘"],
    "dup_keys": ["支持重复键", "可重复键"],
    "parent_ptr": ["带父指针", "父指针"],
    "recursion_mode": ["无递归", "非递归"],
    "scope_limitation": ["只实现", "仅实现", "只写", "只做", "完整实现", "简化", "最小"],
    "capacity": ["容量", "N个", "有限", "上限"],
    "space_complexity": ["O(1)空间", "空间复杂度"],
    "time_complexity": ["O(n)", "O(logn)", "时间复杂度", "O(1)"],
    "return_format": ["返回类型", "输出格式", "JSON格式", "格式为"],
}


def critical_constraints(qp) -> frozenset:
    """输入已 parse 的 ParsedQuery，把命中关键词的约束类别汇总为 frozenset（空=无硬约束）。

    纯规则、无模型：任类别下任关键词出现在 normalized (raw 兜底) 文本即计入。可散列安全。
    """
    if getattr(qp, "raw_text", None) is None:
        return frozenset()
    n = qp.normalized_text or qp.raw_text
    if not n:
        return frozenset()
    cats = set()
    for cat, kws in CRITICAL_ZH.items():
        for kw in kws:
            if kw in n:
                cats.add(cat)
                break   # 每类只记一次
    return frozenset(cats)


def normalize(text):
    """归一化问句：NFKC 统一、小写、c++/c#/.net/node.js 符号占位保护、统一空白。"""
    s = unicodedata.normalize("NFKC", text)
    s = s.lower()
    # 保留 c++/c#/.net 等符号：先保护占位再转半角
    for sym in ["c++", "c#", ".net", "node.js"]:
        s = s.replace(sym, sym.replace("+", "P").replace("#", "H").replace(".", "D"))
    # 全角→半角已在 NFKC 阶段完成；这里仅统一空白与常见全角标点
    s = re.sub(r"[\s　]+", " ", s)
    for sym in ["c++", "c#", ".net", "node.js"]:
        s = s.replace(sym.replace("+", "P").replace("#", "H").replace(".", "D"), sym)
    return s.strip()


@dataclass(frozen=True)
class ParsedQuery:
    raw_text: str
    normalized_text: str
    subject_text: "Optional[str]"
    subject_id: "Optional[str]"
    intent: str
    language: "Optional[str]"
    operation: "Optional[str]"
    output_type: "Optional[str]"
    residual_words: "frozenset" = frozenset()
    context_dependent: bool = False
    stateful: bool = False
    bypass_cache: bool = False
    fingerprint: "Optional[str]" = None
    fingerprint_eligible: bool = False
    parser_version: str = PARSER_VERSION
    ontology_version: str = ONTOLOGY_VERSION
    lang_terms_version: "Optional[str]" = None
    # ---- Task3 实体化元数据（无则 None/False；旧字段名与语义不变） ----
    subject_kind: "Optional[str]" = None
    implementation_family: "Optional[str]" = None
    namespace: "Optional[str]" = None
    type_args: "Optional[tuple]" = ()
    matched_surface: "Optional[str]" = None
    multi_subject: bool = False
    reason: "Optional[str]" = None
    def __post_init__(self):
        # residual_words 恒为可散列集合（容错 mutable 默认入参的调用方）
        object.__setattr__(self, "residual_words", frozenset(self.residual_words))
        # type_args 恒 tuple（语义=结构化模板实参，可哈希供决策读）
        object.__setattr__(self, "type_args", tuple(self.type_args or ()))


def output_type_for(intent):
    """意图→输出类型：实现/排障给代码，定义/对比/原因给讲解。"""
    if intent in {"implementation", "troubleshooting"}:
        return "code"
    if intent in {"definition", "comparison", "reason"}:
        return "explanation"
    return None


def parse(text):
    """槽位抽取主入口：normalize → 双路 subject → intent/language/operation → output_type
    → context/stateful/bypass（对原文本）→ residual → fingerprint_eligible → build_fingerprint。

    构造后临时算好各约束字段，再用 dataclasses.replace 落到 frozen 结构。
    residual 面统计针对原文本（含空格，保证 latin 别名可整体作为 token 消解）。
    """
    from dataclasses import replace

    raw = text if isinstance(text, str) else str(text)
    normalized_text = normalize(raw)
    raw_lower = raw.strip()
    intent = extract_intent(normalized_text)
    language = extract_language(normalized_text)
    context_dependent = is_context_dependent(raw_lower)
    stateful = is_stateful_instruction(raw_lower)

    # ---- subject 解析：句式→span → 全局 alias_of 概念；空且语言已判 → 语言受限实体 ----
    # subject_text=句式命中子句原文(无则 None)；概念(alias_of)优先，落到 concepts.json 顶层 id。
    clause_text, concept_id = _extract_subject_pair(normalized_text)
    subject_meta = {
        "subject_text": clause_text, "subject_id": concept_id,
        # alias_of 抽象 family 概念 → implementation_family。语义本为"每个 alias_of 概念即其自身
        # 实现族"，使 cpp_std_list(family=linked_list) 与概念 linked_list(family=linked_list) 同 family
        # 可在受控 family_compat 下兼容（决策层直接比较两侧 implementation_family，无补丁表）。
        # 但 ontology 对"动态数组"命名存在冗余：lang 实体 family token=dynamic_array，而其族之抽象
        # 概念 canonical id=array（concepts.json 无 dynamic_array 概念）。为让两侧同 family，抽象
        # family 概念按 canonical「实现族 token」取：linked_list/hash_table 为自身 id（一致），
        # array→其实现族 token dynamic_array。仅影响该字段：implementation_family 不进 fp payload、
        # eligible 判定不变（lang_terms 实体路径随后整体覆盖本 dict 的 family=ent['family']）。
        "subject_kind": None,
        "implementation_family": _concept_family(concept_id) if concept_id else None,
        "namespace": None, "type_args": (),
        "matched_surface": clause_text, "multi_subject": False,
        "reason": None, "source": "concept" if concept_id else "none",
    }
    if concept_id is None and language:
        ent = _resolve_lang_entity(clause_text, normalized_text, language)
        if ent is not None:
            if ent.get("multi_subject"):
                subject_meta = {
                    "subject_text": clause_text, "subject_id": None,
                    "subject_kind": None, "implementation_family": None,
                    "namespace": None, "type_args": (), "matched_surface": None,
                    "multi_subject": True, "reason": "multiple_subjects",
                    "source": "lang_terms",
                }
            else:
                subject_meta = {
                    "subject_text": ent["surface"], "subject_id": ent["entity_id"],
                    "subject_kind": ent["kind"], "implementation_family": ent["family"],
                    "namespace": ent["namespace"], "type_args": ent["type_args"],
                    "matched_surface": ent["surface"], "multi_subject": False,
                    "reason": None, "source": "lang_terms",
                }
        else:
            # 语言已知但 lang 受限实体 0 命中（未落库/库未收录该实体）→ 主题仍无法映射。
            # 与下方 language 未知/概念未解出路径同设 subject_unresolved，reason 场对称：
            # 仅供 decision 读诊断，hard_decide 已由 subject_id=None 兜 unknown_subject，
            # 不读该 reason，决策语义不变。
            subject_meta["reason"] = "subject_unresolved"
    elif subject_meta["subject_id"] is None:
        # 概念/实体都未解出：无法映射主题(含 unknown 语言弱词)→ 非 eligible；供 decision 走 unknown。
        subject_meta["reason"] = "subject_unresolved"

    subject_id = subject_meta["subject_id"]
    operation = extract_operation(normalized_text, subject_id)

    base = ParsedQuery(
        raw_text=raw,
        normalized_text=normalized_text,
        subject_text=subject_meta["subject_text"],
        subject_id=subject_id,
        intent=intent,
        language=language,
        operation=operation,
        output_type=output_type_for(intent),
        context_dependent=context_dependent,
        stateful=stateful,
        bypass_cache=context_dependent or stateful,
        subject_kind=subject_meta["subject_kind"],
        implementation_family=subject_meta["implementation_family"],
        namespace=subject_meta["namespace"],
        type_args=subject_meta["type_args"],
        matched_surface=subject_meta["matched_surface"],
        multi_subject=subject_meta["multi_subject"],
        reason=subject_meta["reason"],
        lang_terms_version=LANG_TERMS_VERSION,
    )
    residual = _residual_words(base)
    eligible = _fingerprint_eligible(base)  # 内部复算 residual，但二者一致（residual 只依赖槽位+原文）
    # build_fingerprint 需先看到 residual/fingerprint_eligible（payload 用 residual）
    with_residual = replace(base, residual_words=residual,
                           fingerprint_eligible=eligible,
                           fingerprint=None)
    fp = build_fingerprint(with_residual)
    return replace(with_residual, fingerprint=fp)
