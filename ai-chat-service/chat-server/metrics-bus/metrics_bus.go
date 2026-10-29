package metrics_bus

import "github.com/prometheus/client_golang/prometheus"

type BusMetrics struct {
	QuestionsTotalCounter          prometheus.Counter
	KeywordsQuestionsTotalCounter  prometheus.Counter
	SensitiveQuestionsTotalCounter prometheus.Counter
	ErrQuestionsTotalCounter       prometheus.Counter

	// chat_records 异步入库：入队失败与 worker 内 SQL 失败分开统计，便于区分
	// “队列/背压问题” 和 “数据库问题”。
	RecordsEnqueueFailures  *prometheus.CounterVec
	RecordsDBInsertFailures *prometheus.CounterVec
	RecordsDBInsertDuration prometheus.Histogram
	RecordsQueueLength      prometheus.Gauge
	RecordsPersisted        prometheus.Counter

	// zRPC 业务入口（当前主要传输；gRPC interceptor 指标不再代表新流量）。
	ZrpcRequests           *prometheus.CounterVec
	ZrpcUnaryDuration      *prometheus.HistogramVec
	ZrpcStreamDuration     *prometheus.HistogramVec
	ZrpcStreamActive       *prometheus.GaugeVec
	ZrpcFirstTokenDuration *prometheus.HistogramVec

	// 语义缓存（stage/result 均为固定枚举）。
	CacheRequests *prometheus.CounterVec
	CacheWrites   *prometheus.CounterVec

	// LLM 调用与 token（actual 只来自模型返回的 usage；缓存命中只记 estimated_saved）。
	LLMRequests *prometheus.CounterVec
	LLMTokens   *prometheus.CounterVec

	// MySQL 逻辑查询（operation 为硬编码逻辑名，禁止 SQL 文本/主键作标签）。
	MySQLQueries       *prometheus.CounterVec
	MySQLQueryDuration *prometheus.HistogramVec
	MySQLTxRetries     *prometheus.CounterVec

	// 知识发布 Outbox 与 PocketKV Cache Aside。
	OutboxPending   prometheus.Gauge
	OutboxOldestAge prometheus.Gauge
	OutboxEvents    *prometheus.CounterVec
	KBCacheLookups  *prometheus.CounterVec

	// LLM 用量账本写入。
	UsageEnqueueFailures *prometheus.CounterVec
	UsageWriteFailures   *prometheus.CounterVec
	UsageEvents          *prometheus.CounterVec
	UsageQueueDepth      prometheus.Gauge
}

const (
	NAMESPACE = "ai_chat"
	SUBSYSTEM = "chat_service"
)

func NewBusMetrics(registry *prometheus.Registry) *BusMetrics {
	questionsTotalCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "questions_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "历史语义：仅 stream 成功返回并尝试入库的问题数，不含 unary/缓存命中；新看板请使用 zrpc_requests_total 与 records_persisted_total",
	})
	keywordsQuestionsTotalCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "keywords_questions_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "记录用户提交的包含关键词的问题总数",
	})
	sensitiveQuestionsTotalCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "sensitive_questions_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "记录用户提交的触发敏感词的问题总数",
	})
	errQuestionsTotalCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "err_questions_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "记录用户提交问题时报错的总数",
	})
	// reason 只使用固定枚举：closed/timeout/canceled/other（入队），
	// timeout/canceled/db_error（SQL）。禁止用户内容进入标签。
	recordsEnqueueFailures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "records_enqueue_failures_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "chat_records 异步入队失败次数（按 reason 分类，不包含用户内容）",
	}, []string{"reason"})
	recordsDBInsertFailures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "records_db_insert_failures_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "chat_records worker 内数据库写入失败次数（按 reason 分类，不包含用户内容）",
	}, []string{"reason"})
	recordsDBInsertDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "records_db_insert_duration_seconds",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "chat_records 单条数据库写入耗时",
		Buckets:     []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	})
	recordsQueueLength := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "records_queue_length",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "chat_records 异步入库队列当前积压条数",
	})
	recordsPersisted := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "records_persisted_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "成功写入 chat_records 的记录数（与入口 questions_total 区分）",
	})
	// 只使用 method/type/result 等固定枚举，禁止把问题文本/答案/token 放进标签。
	zrpcRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "zrpc_requests_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "zRPC 请求数（type=unary|stream，result=ok|error）",
	}, []string{"method", "type", "result"})
	zrpcUnaryDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "zrpc_unary_duration_seconds",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "zRPC unary 请求耗时",
		Buckets:     prometheus.DefBuckets,
	}, []string{"method", "status"})
	zrpcStreamDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "zrpc_stream_duration_seconds",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "zRPC stream 请求总耗时",
		Buckets:     prometheus.DefBuckets,
	}, []string{"method", "status"})
	zrpcStreamActive := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "zrpc_stream_active",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "当前进行中的 zRPC stream 数",
	}, []string{"method"})
	zrpcFirstToken := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "zrpc_first_token_seconds",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "zRPC stream 首 token 耗时（TTFT；重试成功后才记录）",
		Buckets:     []float64{0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 20, 30, 60},
	}, []string{"method"})

	// stage=gate|exact|semantic；result=hit|miss|bypass|error。
	cacheRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "cache_requests_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "语义缓存查询次数（stage=gate|exact|semantic，result=hit|miss|bypass|error）",
	}, []string{"stage", "result"})
	cacheWrites := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "cache_writes_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "语义缓存写入结果（result=ok|error|bypass）",
	}, []string{"result"})

	llmRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "llm_requests_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "LLM 调用次数（provider=openai_compatible，result=ok|error）",
	}, []string{"provider", "result"})
	llmTokens := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "llm_tokens_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "Token 计数；kind=input|output 为模型 usage 实际值，kind=estimated_saved 为缓存命中估算节省量（不可用于计费）",
	}, []string{"kind"})

	mysqlQueries := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "mysql_queries_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "MySQL 逻辑查询次数（operation 为硬编码逻辑名，result=ok|error）",
	}, []string{"operation", "result"})
	mysqlQueryDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "mysql_query_duration_seconds",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "MySQL 逻辑查询耗时（operation 为硬编码逻辑名）",
		Buckets:     []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"operation"})
	mysqlTxRetries := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "mysql_tx_retries_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "事务整体重试次数（reason=deadlock|lock_timeout）",
	}, []string{"reason"})

	outboxPending := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "outbox_pending_jobs",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "cache_outbox 待处理/处理中/失败待重试任务数",
	})
	outboxOldestAge := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "outbox_oldest_pending_age_seconds",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "最老一条未完成 Outbox 任务的存在时长（秒）",
	})
	outboxEvents := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "outbox_events_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "Outbox 事件处理结果（result=done|retry|dead|error）",
	}, []string{"result"})
	kbCacheLookups := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "kb_cache_lookups_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "已发布知识缓存查询结果（outcome=hit|miss|error；版本始终以 MySQL 为准）",
	}, []string{"outcome"})

	usageEnqueueFailures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "usage_enqueue_failures_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "用量账本入队失败次数（reason=closed|timeout|canceled|other）",
	}, []string{"reason"})
	usageWriteFailures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "usage_write_failures_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "用量账本数据库写入失败次数（reason=timeout|canceled|db_error）",
	}, []string{"reason"})
	usageEvents := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "usage_events_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "用量事件写入结果（source=llm|cache_exact|cache_semantic|blocked|unknown，duplicate=yes|no）",
	}, []string{"source", "duplicate"})
	usageQueueLength := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "usage_queue_length",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "用量账本异步入库队列积压条数",
	})

	registry.MustRegister(questionsTotalCounter, keywordsQuestionsTotalCounter,
		sensitiveQuestionsTotalCounter, errQuestionsTotalCounter,
		recordsEnqueueFailures, recordsDBInsertFailures,
		recordsDBInsertDuration, recordsQueueLength, recordsPersisted,
		zrpcRequests, zrpcUnaryDuration, zrpcStreamDuration, zrpcStreamActive, zrpcFirstToken,
		cacheRequests, cacheWrites, llmRequests, llmTokens,
		mysqlQueries, mysqlQueryDuration, mysqlTxRetries,
		outboxPending, outboxOldestAge, outboxEvents, kbCacheLookups,
		usageEnqueueFailures, usageWriteFailures, usageEvents, usageQueueLength)
	return &BusMetrics{
		QuestionsTotalCounter:          questionsTotalCounter,
		KeywordsQuestionsTotalCounter:  keywordsQuestionsTotalCounter,
		SensitiveQuestionsTotalCounter: sensitiveQuestionsTotalCounter,
		ErrQuestionsTotalCounter:       errQuestionsTotalCounter,
		RecordsEnqueueFailures:         recordsEnqueueFailures,
		RecordsDBInsertFailures:        recordsDBInsertFailures,
		RecordsDBInsertDuration:        recordsDBInsertDuration,
		RecordsQueueLength:             recordsQueueLength,
		RecordsPersisted:               recordsPersisted,
		ZrpcRequests:                   zrpcRequests,
		ZrpcUnaryDuration:              zrpcUnaryDuration,
		ZrpcStreamDuration:             zrpcStreamDuration,
		ZrpcStreamActive:               zrpcStreamActive,
		ZrpcFirstTokenDuration:         zrpcFirstToken,
		CacheRequests:                  cacheRequests,
		CacheWrites:                    cacheWrites,
		LLMRequests:                    llmRequests,
		LLMTokens:                      llmTokens,
		MySQLQueries:                   mysqlQueries,
		MySQLQueryDuration:             mysqlQueryDuration,
		MySQLTxRetries:                 mysqlTxRetries,
		OutboxPending:                  outboxPending,
		OutboxOldestAge:                outboxOldestAge,
		OutboxEvents:                   outboxEvents,
		KBCacheLookups:                 kbCacheLookups,
		UsageEnqueueFailures:           usageEnqueueFailures,
		UsageWriteFailures:             usageWriteFailures,
		UsageEvents:                    usageEvents,
		UsageQueueDepth:                usageQueueLength,
	}
}

// 以下方法实现 data.RecordsObserver（worker/入队路径注入指标）。
func (m *BusMetrics) EnqueueFailure(reason string) {
	m.RecordsEnqueueFailures.WithLabelValues(reason).Inc()
}

func (m *BusMetrics) InsertFailure(reason string) {
	m.RecordsDBInsertFailures.WithLabelValues(reason).Inc()
}

func (m *BusMetrics) InsertDuration(seconds float64) {
	m.RecordsDBInsertDuration.Observe(seconds)
}

func (m *BusMetrics) QueueLength(n int) {
	m.RecordsQueueLength.Set(float64(n))
}

func (m *BusMetrics) RecordPersisted() {
	m.RecordsPersisted.Inc()
}

// DBQuery 由 data 层以硬编码 operation 调用；禁止传真实 SQL 文本。
func (m *BusMetrics) DBQuery(operation, result string, seconds float64) {
	m.MySQLQueries.WithLabelValues(operation, result).Inc()
	m.MySQLQueryDuration.WithLabelValues(operation).Observe(seconds)
}

func (m *BusMetrics) TxRetry(reason string) {
	m.MySQLTxRetries.WithLabelValues(reason).Inc()
}

func (m *BusMetrics) OutboxStats(pending int, oldestSeconds float64) {
	m.OutboxPending.Set(float64(pending))
	m.OutboxOldestAge.Set(oldestSeconds)
}

func (m *BusMetrics) OutboxEvent(result string) {
	m.OutboxEvents.WithLabelValues(result).Inc()
}

func (m *BusMetrics) KBCacheLookup(outcome string) {
	m.KBCacheLookups.WithLabelValues(outcome).Inc()
}

// 以下实现 data.UsageObserver。
func (m *BusMetrics) UsageEnqueueFailure(reason string) {
	m.UsageEnqueueFailures.WithLabelValues(reason).Inc()
}

func (m *BusMetrics) UsageWriteFailure(reason string) {
	m.UsageWriteFailures.WithLabelValues(reason).Inc()
}

func (m *BusMetrics) UsageWritten(source string, created bool) {
	dup := "yes"
	if created {
		dup = "no"
	}
	m.UsageEvents.WithLabelValues(source, dup).Inc()
}

func (m *BusMetrics) UsageQueueLength(n int) {
	m.UsageQueueDepth.Set(float64(n))
}

// 以下方法实现 semcache.Observer。
func (m *BusMetrics) CacheRequest(stage, result string) {
	m.CacheRequests.WithLabelValues(stage, result).Inc()
}

func (m *BusMetrics) CacheWrite(result string) {
	m.CacheWrites.WithLabelValues(result).Inc()
}

func (m *BusMetrics) LLMRequest(provider, result string) {
	m.LLMRequests.WithLabelValues(provider, result).Inc()
}

func (m *BusMetrics) LLMToken(kind string, n int) {
	if n <= 0 {
		return
	}
	m.LLMTokens.WithLabelValues(kind).Add(float64(n))
}
