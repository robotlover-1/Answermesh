package metrics_bus

import "github.com/prometheus/client_golang/prometheus"

type BusMetrics struct {
	QuestionsTotalCounter          prometheus.Counter
	KeywordsQuestionsTotalCounter  prometheus.Counter
	SensitiveQuestionsTotalCounter prometheus.Counter
	ErrQuestionsTotalCounter       prometheus.Counter
	RecordsWriteErrorsCounter      prometheus.Counter

	// zRPC 业务入口耗时（当前主要传输；gRPC interceptor 指标不再代表新流量）。
	ZrpcUnaryDuration  *prometheus.HistogramVec
	ZrpcStreamDuration *prometheus.HistogramVec
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
		Help:        "记录用户提交问题的总数，仅包含记录到DB的问题数量",
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
	recordsWriteErrorsCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   NAMESPACE,
		Subsystem:   SUBSYSTEM,
		Name:        "records_write_errors_total",
		ConstLabels: map[string]string{"app": "ai_chat"},
		Help:        "chat_records 异步入库队列满/写入失败次数（不包含用户内容）",
	})
	// 只使用 method/status 标签，禁止把问题文本/答案/token 放进标签。
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
	registry.MustRegister(questionsTotalCounter, keywordsQuestionsTotalCounter,
		sensitiveQuestionsTotalCounter, errQuestionsTotalCounter,
		recordsWriteErrorsCounter, zrpcUnaryDuration, zrpcStreamDuration)
	return &BusMetrics{
		QuestionsTotalCounter:          questionsTotalCounter,
		KeywordsQuestionsTotalCounter:  keywordsQuestionsTotalCounter,
		SensitiveQuestionsTotalCounter: sensitiveQuestionsTotalCounter,
		ErrQuestionsTotalCounter:       errQuestionsTotalCounter,
		RecordsWriteErrorsCounter:      recordsWriteErrorsCounter,
		ZrpcUnaryDuration:              zrpcUnaryDuration,
		ZrpcStreamDuration:             zrpcStreamDuration,
	}
}
