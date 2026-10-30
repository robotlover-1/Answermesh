package semcache

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-chat-service/pkg/config"
	kredis "ai-chat-service/pkg/db/redis"
	"ai-chat-service/pkg/log"
	"github.com/redis/go-redis/v9"
)

// Observer 观测缓存查询/写入。stage=gate|exact|semantic，result=hit|miss|bypass|error，
// 均为固定枚举；实现方不得把问题/答案文本放进标签。
type Observer interface {
	CacheRequest(stage, result string)
	CacheWrite(result string)
}

type nopObserver struct{}

func (nopObserver) CacheRequest(string, string) {}
func (nopObserver) CacheWrite(string)           {}

var observer Observer = nopObserver{}

// SetObserver 在 main 启动时注入；nil 表示不采集。
func SetObserver(o Observer) {
	if o == nil {
		o = nopObserver{}
	}
	observer = o
}

func noteCache(stage, result string) {
	observer.CacheRequest(stage, result)
}

// 语义服务专用 HTTP client：整体超时兜底，防 semantic 卡死拖住聊天。
const semanticTimeout = 1500 * time.Millisecond

var semanticClient = &http.Client{Timeout: semanticTimeout}

// 明文 Q-A 存储在 array 引擎（SET/GET），key 直接用问题原文（便于 redis-cli 直接读）；
// 向量索引在 hash 引擎（HSET，VSEARCH 只扫 hash），key = <命名空间前缀><问题原文>。
const semPrefix = "semcache:" // 旧 256 命名空间（Phase-1；vector_write_mode=dual_write 灰度期补写）

// 指纹缓存 key 前缀：semfp:v1:<fingerprint> → <候选问题(query 文本)>
// 只存候选问题，不存答案；命中后再 GET <candidate> 取答（值即 candidate=<query>）。
const semfpPrefix = "semfp:v1:"

// TopK 默认值，config 给定 top_k < 1 时回退。
const defaultTopK = 30

// ============ 包级向量 schema 状态（/model-info 一致性校验结果） ============
// vectorEnabled：映射 `semd:e5s:v1:`（及匹配的 dim/namespace）是否启用。启动/首用经
// /model-info 与 config 一致性校验；任一不符 → false。false 仅禁向量读(VSEARCH)/写/回填，
// fp 纯规则路径仍可用（保守可用）。
var (
	_vectOnce        sync.Once
	vectorEnabled    = true
	embedModelInfoOK bool // 已成功比对过
)

// modelInfoResp：semantic /model-info 返回 `{code, model, revision, dimension, vector_namespace, export_version}`。
type modelInfoResp struct {
	Code          int    `json:"code"`
	Model         string `json:"model"`
	Revision      string `json:"revision"`
	Dimension     int    `json:"dimension"`
	Ns            string `json:"vector_namespace"`
	ExportVersion string `json:"export_version"`
}

// semanticDimOf：配置的语义向量维度；config.dimension 缺省（<1）回退 384（与 e5 导出一致）。
func semanticDimOf(cnf *config.Config) int {
	if cnf.SemanticCache.Dimension > 0 {
		return cnf.SemanticCache.Dimension
	}
	return 384
}

// probeModelConsistency：首用/启动一次性调用 /model-info，与 config 比对
// model/revision/dimension/namespace。任一配置字段不一致 → 置 vectorEnabled=false
// 并 log.ErrorF("vector_index_mismatch …")（fp 是否可用取决于 embed 本身仍可用）。
// 并发安全（Once）。transport/临时错误仅记录一次，不改判（embed 不可达时 CacheQuery 本就 miss）。
func probeModelConsistency() {
	_vectOnce.Do(func() {
		cnf := config.GetConfig()
		sc := cnf.SemanticCache
		endpoint := cnf.DependOn.Semantic.Address + "/model-info"
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			log.ErrorF("vector_model_info_error_request: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), semanticTimeout)
		defer cancel()
		resp, err := semanticClient.Do(req.WithContext(ctx))
		if err != nil {
			log.ErrorF("vector_model_info_unreachable: %v", err)
			return
		}
		defer resp.Body.Close()
		var mi modelInfoResp
		if err := json.NewDecoder(resp.Body).Decode(&mi); err != nil {
			log.ErrorF("vector_model_info_bad_json: %v", err)
			return
		}
		if resp.StatusCode != http.StatusOK || mi.Code != 200 {
			log.ErrorF("vector_model_info_bad_status: status=%d code=%d", resp.StatusCode, mi.Code)
			return
		}
		mism := []string{}
		if sc.EmbeddingModel != "" && mi.Model != sc.EmbeddingModel {
			mism = append(mism, fmt.Sprintf("model %q != cfg %q", mi.Model, sc.EmbeddingModel))
		}
		if sc.EmbeddingRevision != "" && mi.Revision != sc.EmbeddingRevision {
			mism = append(mism, fmt.Sprintf("revision %q != cfg %q", mi.Revision, sc.EmbeddingRevision))
		}
		if sc.Dimension > 0 && mi.Dimension != sc.Dimension {
			mism = append(mism, fmt.Sprintf("dimension %d != cfg %d", mi.Dimension, sc.Dimension))
		}
		if sc.VectorNamespace != "" && mi.Ns != sc.VectorNamespace {
			mism = append(mism, fmt.Sprintf("namespace %q != cfg %q", mi.Ns, sc.VectorNamespace))
		}
		if len(mism) == 0 {
			embedModelInfoOK = true
			return
		}
		vectorEnabled = false
		log.ErrorF("vector_index_mismatch name=%s ns=%s export=%s: %s",
			sc.EmbeddingModel, sc.VectorNamespace, sc.ExportVersion, strings.Join(mism, "; "))
	})
}

func vectorNamespaceOf(cnf *config.Config) string {
	if cnf.SemanticCache.VectorNamespace != "" {
		return cnf.SemanticCache.VectorNamespace
	}
	return "semd:e5s:v1:"
}

// ============ vector 读写（semd:e5s:v1:，与 config 对齐） ============

type embedResp struct {
	Code                int       `json:"code"`
	Embedding           []float64 `json:"embedding"`
	BypassCache         bool      `json:"bypass_cache"`
	Subject             string    `json:"subject"`
	SubjectID           string    `json:"subject_id"`
	Language            string    `json:"language"`
	Operation           string    `json:"operation"`
	Intent              string    `json:"intent"`
	OutputType          string    `json:"output_type"`
	Fingerprint         string    `json:"fingerprint"`
	FingerprintEligible bool      `json:"fingerprint_eligible"`
	Msg                 string    `json:"msg"`
}

// embedMeta 汇集 /embed 语义指纹/向量召回决策所需字段。
type embedMeta struct {
	Vec                 []float32
	Bypass              bool
	Subject             string // subject_text：可为空（主题只靠全句本体兜底命中）；空不完全拦，见 CacheQuery/CacheWrite
	SubjectID           string // subject_id：fp/VSEARCH 的真正钥匙；与 Subject 皆空才整体 miss / 不写 (仅此禁入闸)
	Fingerprint         string
	FingerprintEligible bool
}

// poolClient 复用 ai-chat-service 已有的 redis 连接池：借出 *redis.Client，
// 单条命令执行后立即归还（避免连接得不到释放）。本包不保留跨请求连接。
type poolClient struct {
	pool   kredis.RedisPool
	client *redis.Client
}

func (c *poolClient) Do(ctx context.Context, args ...interface{}) (interface{}, error) {
	defer c.pool.Put(c.client)
	return c.client.Do(ctx, args...).Result()
}

func getClient() *poolClient {
	pool := kredis.GetPool()
	client := pool.Get()
	return &poolClient{pool: pool, client: client}
}

// embedMetaOf：对 text 做一次 /embed，校验返回维度与 config 对齐。Query 每次请求只编码一次。
func embedMetaOf(ctx context.Context, text string) (*embedMeta, error) {
	cnf := config.GetConfig()
	dim := semanticDimOf(cnf)
	body, _ := json.Marshal(map[string]string{"text": text})
	endpoint := cnf.DependOn.Semantic.Address + "/embed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := semanticClient.Do(req)
	if err != nil {
		log.ErrorF("semantic_unavailable: %v", err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.ErrorF("semantic_bad_status: %d", resp.StatusCode)
		return nil, fmt.Errorf("embed status=%d", resp.StatusCode)
	}
	var r embedResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		log.ErrorF("invalid_json: %v", err)
		return nil, err
	}
	if r.Code != 200 || len(r.Embedding) != dim {
		log.ErrorF("invalid_embedding_dimension: code=%d dim=%d expect=%d", r.Code, len(r.Embedding), dim)
		return nil, fmt.Errorf("embed failed: code=%d dim=%d", r.Code, len(r.Embedding))
	}
	m := &embedMeta{
		Bypass:              r.BypassCache,
		Subject:             r.Subject,
		SubjectID:           r.SubjectID,
		Fingerprint:         r.Fingerprint,
		FingerprintEligible: r.FingerprintEligible,
	}
	m.Vec = make([]float32, dim)
	for i, v := range r.Embedding {
		m.Vec[i] = float32(v)
	}
	return m, nil
}

// ============ decision（纯规则，复用 semantic /v1/decision[/batch]，不再逐候选 /rerank） ============

type decisionResp struct {
	Code   int    `json:"code"`
	Shared bool   `json:"shared"`
	Reason string `json:"reason"`
	Soft   bool   `json:"soft"`
	Msg    string `json:"msg"`
}

type batchResult struct {
	CachedQuery string `json:"cached_query"`
	Shared      bool   `json:"shared"`
	Reason      string `json:"reason"`
	Soft        bool   `json:"soft"`
}

type batchResp struct {
	Code    int           `json:"code"`
	Results []batchResult `json:"results"`
	Msg     string        `json:"msg"`
}

// decisionOf：单候选纯规则复核（fp 路径用），返回 (shared, soft, reason)。
func decisionOf(ctx context.Context, query, cached string) (bool, bool, string, error) {
	body, _ := json.Marshal(map[string]string{"query": query, "cached_query": cached})
	endpoint := config.GetConfig().DependOn.Semantic.Address + "/v1/decision"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, false, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := semanticClient.Do(req)
	if err != nil {
		log.ErrorF("decision_semantic_unavailable: %v", err)
		return false, false, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.ErrorF("decision_bad_status: %d", resp.StatusCode)
		return false, false, "", fmt.Errorf("decision status=%d", resp.StatusCode)
	}
	var r decisionResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		log.ErrorF("decision_invalid_json: %v", err)
		return false, false, "", err
	}
	if r.Code != 200 {
		log.ErrorF("decision_failed: %s", r.Msg)
		return false, false, r.Reason, fmt.Errorf("decision failed: %s", r.Msg)
	}
	return r.Shared, r.Soft, r.Reason, nil
}

// decisionBatch：一次 HTTP 对全部候选纯规则复核（VSEARCH 后批量），返回按 candidates 顺序对齐。
func decisionBatch(ctx context.Context, query string, candidates []string) []batchResult {
	if len(candidates) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]interface{}{"query": query, "candidates": candidates})
	endpoint := config.GetConfig().DependOn.Semantic.Address + "/v1/decision/batch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return make([]batchResult, len(candidates))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := semanticClient.Do(req)
	if err != nil {
		log.ErrorF("batch_semantic_unavailable: %v", err)
		return make([]batchResult, len(candidates))
	}
	defer resp.Body.Close()
	var r batchResp
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&r) != nil || r.Code != 200 {
		return make([]batchResult, len(candidates))
	}
	// 对齐：不足则补空条目，多则截断。
	out := make([]batchResult, len(candidates))
	for i, c := range candidates {
		if i < len(r.Results) {
			out[i] = r.Results[i]
			out[i].CachedQuery = c
		} else {
			out[i] = batchResult{CachedQuery: c, Shared: false}
		}
	}
	return out
}

// ============ kvstore / 值存取 ============

// nsVecKey：semd 向量索引 key = <ns><query>。
func nsVecKey(ns, query string) string { return ns + query }

// 向量索引值: [u32 dim][float vec[dim]]（小端），与 kvstore parse_vec 匹配
func encodeVec(vec []float32) []byte {
	buf := make([]byte, 4+len(vec)*4)
	binary.LittleEndian.PutUint32(buf, uint32(len(vec)))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[4+i*4:], math.Float32bits(v))
	}
	return buf
}

// fetchAnswer: 读明文回答（array 引擎），key 就是问题原文（cachedQ）。
// 指纹/向量命中只是拿到候选问题文本，真正答案仍要 GET <cachedQ> 取 —— semfp 不存答案。
func fetchAnswer(ctx context.Context, cachedQ string) (string, bool) {
	answer, err := getClient().Do(ctx, "GET", cachedQ)
	if err != nil {
		return "", false
	}
	ans, ok := answer.(string)
	if !ok || ans == "" {
		return "", false
	}
	return ans, true
}

// vecExists：判断 semd 命名空间下 query 是否已有向量（幂等写前判存在）。HGET 读 hash 记录，
// 键即 nsVecKey ⇒ `semd:e5s:v1:<query>`；不存/被删返回 redis.Nil。
func vecExists(ctx context.Context, query string) bool {
	ns := vectorNamespaceOf(config.GetConfig())
	_, err := getClient().Do(ctx, "HGET", nsVecKey(ns, query))
	if err == redis.Nil {
		return false
	}
	return err == nil
}

// topKOf: VSEARCH 返回条数上限；config top_k < 1 时回退默认 30。
func topKOf(cnf *config.Config) int {
	k := cnf.SemanticCache.TopK
	if k < 1 {
		k = defaultTopK
	}
	return k
}

// vsearch: 一次 VSEARCH，返回剥前缀后的候选 query 列表及其 cosine（按命中序）。
func vsearch(ctx context.Context, vec []float32, topK int, ns string) (queries []string, scores []float64) {
	dim := len(vec)
	binVec := make([]byte, dim*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(binVec[i*4:], math.Float32bits(v))
	}
	res, err := getClient().Do(ctx, "VSEARCH", strconv.Itoa(dim), binVec, strconv.Itoa(topK), ns)
	if err != nil {
		log.ErrorF("vsearch_error: %v", err)
		return nil, nil
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 2 {
		return nil, nil
	}
	if len(arr) > 2*topK {
		arr = arr[:2*topK] // VSEARCH 返回数组长度防御上限
	}
	for i := 0; i+1 < len(arr); i += 2 {
		key := fmt.Sprintf("%v", arr[i])
		q := strings.TrimPrefix(key, ns)
		if q == key {
			continue // 非本 ns 前缀，忽略（防御）
		}
		score, err := strconv.ParseFloat(fmt.Sprintf("%v", arr[i+1]), 64)
		if err != nil {
			continue
		}
		queries = append(queries, q)
		scores = append(scores, score)
	}
	return queries, scores
}

// ============ 异步 E5 回填 ============

var (
	backfillMu   sync.Mutex
	backfillWin  time.Time // 当前秒窗口起点（回填限速）
	backfillUsed int
)

// backfillLimit：每秒最多允许的指纹命中回填数。
const backfillLimit = 4

// backfillGate：限速（每秒 ≤ backfillLimit）。返回 false 表示本轮放弃（下个请求再试）。
func backfillGate() bool {
	backfillMu.Lock()
	defer backfillMu.Unlock()
	now := time.Now()
	if backfillWin.IsZero() || now.Sub(backfillWin) >= time.Second {
		backfillWin = now
		backfillUsed = 0
	}
	if backfillUsed >= backfillLimit {
		return false
	}
	backfillUsed++
	return true
}

// backfillVec：异步 goroutine；只在向量 schema 启用时进行。幂等（先判 semd 已存在则返回）、
// /embed(mode query) 编 cachedQ 并写 semd、失败重试 ≤1、全程 context.Background + 短超时
// （不随请求结束被取消）、有限速（每秒 ≤backfillLimit）。
func backfillVec(cachedQ string) {
	if !vectorEnabled || cachedQ == "" {
		return
	}
	ns := vectorNamespaceOf(config.GetConfig())
	if !backfillGate() {
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		bctx, cancel := context.WithTimeout(context.Background(), semanticTimeout)
		m, err := embedMetaOf(bctx, cachedQ) // encode_query(cachedQ)
		cancel()
		if err != nil {
			log.ErrorF("backfill_encode_failed q=%q attempt=%d: %v", cachedQ, attempt, err)
			if attempt == 0 {
				continue
			}
			return
		}
		// 幂等写：写前再判存在，防并发重复
		exists, err2 := getClient().Do(context.Background(), "HGET", nsVecKey(ns, cachedQ))
		if err2 == nil && exists != nil {
			log.InfoF("backfill_already q=%q", cachedQ)
			return
		}
		if _, err := getClient().Do(context.Background(), "HSET", nsVecKey(ns, cachedQ), encodeVec(m.Vec)); err != nil {
			log.ErrorF("backfill_write_failed q=%q attempt=%d: %v", cachedQ, attempt, err)
			if attempt == 0 {
				continue
			}
			return
		}
		log.InfoF("backfill_done q=%q ns=%s dim=%d", cachedQ, ns, len(m.Vec))
		return
	}
}

// ============ CacheQuery：一次编码 → fp → VSEARCH → 批决策 → acceptance+margin ============

// CacheQuery：返回 (answer, hit)。
// 链路（Query 每请求恰好一次 /embed）：
//
//	① bypass/双空门；
//	② fp(ExactFingerprintEnabled) 命中且候选纯规则通过 → 返答；若 AsyncBackfill 且该候选在
//	   semd 无向量 → 异步回填。
//	③ VSEARCH <dim> vec topK <VectorNamespace>（复用其 cosine）；
//	④ 一次 /v1/decision/batch：通过者(share)按 cosine 排序，cos≥acceptance_threshold
//	   （soft 命中还需 SoftSemanticFallback），Top1-Top2 margin < min_margin → miss；命中返答。
//
// CacheQuery 兼容旧调用方；需要区分 exact/semantic 的阶段时用 CacheQueryMeta。
func CacheQuery(ctx context.Context, query string) (string, bool) {
	ans, hit, _ := CacheQueryMeta(ctx, query)
	return ans, hit
}

func CacheQueryMeta(ctx context.Context, query string) (string, bool, string) {
	cnf := config.GetConfig()
	if !cnf.SemanticCache.Enabled {
		noteCache("gate", "bypass")
		return "", false, ""
	}
	probeModelConsistency()
	sc := cnf.SemanticCache
	// ① 一次 /embed（Query 只编码一次）
	m, err := embedMetaOf(ctx, query)
	if err != nil {
		noteCache("gate", "error")
		return "", false, ""
	}
	log.InfoF("semcache_query_encodes=1 query_len=%d", len(query))
	if m.Bypass || (m.Subject == "" && m.SubjectID == "") {
		// 缓存准入（安全收窄）：状态修改/上下文依赖 → 不查；Subject 与 SubjectID 皆空
		// （通用 how-to 如“怎么减肥”抽不到本体链路）→ 不查全局缓存。
		noteCache("gate", "bypass")
		return "", false, ""
	}
	// ② fp 快路径
	fpChecked := sc.ExactFingerprintEnabled && m.Fingerprint != "" && m.FingerprintEligible
	fpResult := "miss"
	if fpChecked {
		if cand, err := getClient().Do(ctx, "GET", semfpPrefix+m.Fingerprint); err == nil {
			if cq, ok := cand.(string); ok && cq != "" {
				shared, soft, reason, err := decisionOf(ctx, query, cq)
				if err == nil && shared && reason == "ok" && !soft {
					if ans, ok2 := fetchAnswer(ctx, cq); ok2 {
						log.InfoF("semcache_fingerprint_hit subject=%s", m.SubjectID)
						// fp 命中，但候选在 semd 无向量 → 异步回填（幂等/限速/不阻塞）
						if sc.AsyncBackfill && vectorEnabled && !vecExists(ctx, cq) {
							go backfillVec(cq)
						}
						noteCache("exact", "hit")
						return ans, true, "exact"
					}
				} else if err == nil && !shared {
					log.InfoF("semcache_fingerprint_collision subject=%s reason=%s", m.SubjectID, reason)
				} else if err == nil && shared {
					log.InfoF("semcache_fingerprint_soft_skip subject=%s reason=%s", m.SubjectID, reason)
				} else if err != nil {
					fpResult = "error"
				}
			}
		} else {
			fpResult = "error"
		}
		noteCache("exact", fpResult)
	}
	// ③④ 向量召回 + 批量决策（vectorEnabled 关闭时跳过 → miss，fp 路径仍可命中）
	if !vectorEnabled {
		noteCache("semantic", "bypass")
		return "", false, ""
	}
	ns := vectorNamespaceOf(cnf)
	k := topKOf(cnf)
	qlist, scores := vsearch(ctx, m.Vec, k, ns)
	if len(qlist) == 0 {
		noteCache("semantic", "miss")
		return "", false, ""
	}
	decs := decisionBatch(ctx, query, qlist)
	// 收集通过候选（按 condition 过滤），记录 reason 分布便于观测
	reasonCtr := map[string]int{}
	accepted := make([]struct {
		q     string
		score float64
	}, 0, len(qlist))
	for i, dq := range qlist {
		var d batchResult
		if i < len(decs) {
			d = decs[i]
		}
		reasonCtr[d.Reason]++
		if !d.Shared {
			continue
		}
		if i >= len(scores) || scores[i] < float64(sc.AcceptanceThreshold) {
			continue
		}
		// soft 命中需开关 SoftSemanticFallback
		if d.Soft && !sc.SoftSemanticFallback {
			continue
		}
		accepted = append(accepted, struct {
			q     string
			score float64
		}{q: dq, score: scores[i]})
	}
	if sc.SoftSemanticFallback && reasonCtr["semantic_soft_match"] > 0 {
		log.InfoF("semcache_soft_fallback_enabled soft=%d", reasonCtr["semantic_soft_match"])
	}
	if len(accepted) == 0 {
		noteCache("semantic", "miss")
		return "", false, ""
	}
	// 按 cosine 降序
	sort.SliceStable(accepted, func(a, b int) bool {
		if accepted[a].score != accepted[b].score {
			return accepted[a].score > accepted[b].score
		}
		return accepted[a].q < accepted[b].q
	})
	top1 := accepted[0]
	// Top1-Top2 过近 → miss（区分度保护）
	if len(accepted) > 1 && top1.score-accepted[1].score < float64(sc.MinMargin) {
		noteCache("semantic", "miss")
		return "", false, ""
	}
	ans, ok := fetchAnswer(ctx, top1.q)
	if !ok {
		log.InfoF("semcache_vector_hit_no_answer top=%.3f q=%q", top1.score, top1.q)
		noteCache("semantic", "miss")
		return "", false, ""
	}
	log.InfoF("semcache_vector_hit subject=%s top=%.3f candidates=%d", m.SubjectID, top1.score, len(qlist))
	noteCache("semantic", "hit")
	return ans, true, "semantic"
}

// ============ CacheWrite：只写新 ns(semd:e5s:v1:) + 版本化 + fp；dual_write 灰度补旧 ============

// CacheWrite: 嵌入 → SET <问题> <回答>（明文，key=问题原文）
// + HSET <cfg.VectorNamespace><问题> = [u32 dim][vec]（dim 384）
// + 若指纹可入则 SET semfp:v1:<fp> <问题>（指纹只存候选问题，不存答案）
// write_mode==dual_write 时额外补写旧 semcache:256(灰度期)。
func CacheWrite(ctx context.Context, query, answer string) error {
	cnf := config.GetConfig()
	sc := cnf.SemanticCache
	if !sc.Enabled {
		observer.CacheWrite("bypass")
		return nil
	}
	probeModelConsistency()
	m, err := embedMetaOf(ctx, query)
	if err != nil {
		observer.CacheWrite("error")
		return err
	}
	if m.Bypass || (m.Subject == "" && m.SubjectID == "") {
		// 缓存准入（安全收窄）：状态修改/上下文依赖 → 不写；Subject 与 SubjectID 皆空
		// （通用 how-to 抽不到本体链路）→ 不写全局缓存。
		observer.CacheWrite("bypass")
		return nil
	}
	if _, err := getClient().Do(ctx, "SET", query, answer); err != nil {
		observer.CacheWrite("error")
		return err
	}
	// 写描述容：vectorEnabled 由 model-info 一致决定；禁用则只写答案+fp，不写向量。
	if vectorEnabled {
		writeVec(ctx, query, m.Vec)
		// dual_write 灰度：额外写旧 semcache:256 前缀（维度仍为当前 384；仅灰度对照不参与 256 read）
		if sc.VectorWriteMode == "dual_write" {
			if ns := vectorNamespaceOf(cnf); ns != semPrefix {
				_ = writeVecHSET(semPrefix+query, m.Vec)
			}
		}
	}
	if sc.ExactFingerprintEnabled && m.Fingerprint != "" && m.FingerprintEligible {
		if _, err := getClient().Do(ctx, "SET", semfpPrefix+m.Fingerprint, query); err != nil {
			log.ErrorF("semcache_fingerprint_write_failed: %v", err)
		}
	}
	observer.CacheWrite("ok")
	return nil
}

// writeVec：写 e5s 向量命名空间字段。
func writeVec(ctx context.Context, query string, vec []float32) error {
	ns := vectorNamespaceOf(config.GetConfig())
	if _, err := getClient().Do(ctx, "HSET", nsVecKey(ns, query), encodeVec(vec)); err != nil {
		log.ErrorF("semcache_vector_write_failed: ns=%s q=%q err=%v", ns, query, err)
		return err
	}
	return nil
}

// writeVecHSET：直接对指定 key HSLT 写（dual_write 灰度用），错误仅日志不返回（非关键路径）。
func writeVecHSET(key string, vec []float32) error {
	if _, err := getClient().Do(context.Background(), "HSET", key, encodeVec(vec)); err != nil {
		log.ErrorF("semcache_dual_write_failed: key=%q err=%v", key, err)
		return err
	}
	return nil
}
