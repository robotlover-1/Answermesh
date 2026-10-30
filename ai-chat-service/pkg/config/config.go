package config

import (
	"log"
	"os"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server struct {
		IP          string
		Port        int // gRPC business port (kept during the observation period)
		ZrpcPort    int // zrpc v2 business port (double-stack)
		AccessToken string
		// MetricsAddr 健康/指标 HTTP 监听地址。默认仅本机 127.0.0.1:8080；
		// 容器/内网抓取需显式改成 0.0.0.0:8080，并由网络策略限制来源。
		MetricsAddr string `mapstructure:"metrics_addr"`
		// MetricsAccessToken 非空时 /metrics 要求 Authorization: Bearer <token>。
		MetricsAccessToken string `mapstructure:"metrics_access_token"`
	}
	Log struct {
		Level   string
		LogPath string `mapstructure:"logPath"`
	} `mapstructure:"log"`
	Chat struct {
		ApiKey            string  `mapstructure:"api_key"`
		BaseUrl           string  `mapstructure:"base_url"`
		Model             string  `mapstructure:"model"`
		MaxTokens         int     `mapstructure:"max_tokens"`
		Temperature       float32 `mapstructure:"temperature"`
		TopP              float32 `mapstructure:"top_p"`
		PresencePenalty   float32 `mapstructure:"presence_penalty"`
		FrequencyPenalty  float32 `mapstructure:"frequency_penalty"`
		BotDesc           string  `mapstructure:"bot_desc"`
		MinResponseTokens int     `mapstructure:"min_response_tokens"`
		ContextTTL        int     `mapstructure:"context_ttl"`
		ContextLen        int     `mapstructure:"context_len"`
	}
	Mysql struct {
		DSN         string
		MaxLifeTime int
		MaxOpenConn int
		MaxIdleConn int
	}
	// Identity 校验 backend 签发的短时用户身份令牌；secret 支持逗号分隔轮换。
	Identity struct {
		Secret string `mapstructure:"secret"`
		Issuer string `mapstructure:"issuer"`
	} `mapstructure:"identity"`
	// KnowledgeOutbox 控制知识发布 Outbox Worker（PR-M2/M3）。
	KnowledgeOutbox struct {
		Enabled         bool `mapstructure:"enabled"`
		IntervalSeconds int  `mapstructure:"interval_seconds"`
		BatchSize       int  `mapstructure:"batch_size"`
		MaxAttempts     int  `mapstructure:"max_attempts"`
		LeaseSeconds    int  `mapstructure:"lease_seconds"`
		CacheTTLSeconds int  `mapstructure:"cache_ttl_seconds"`
	} `mapstructure:"knowledge_outbox"`
	Redis struct {
		Host string
		Port int
		Pwd  string `mapstructure:"pwd"`
	}
	DependOn struct {
		Sensitive struct {
			Address     string
			AccessToken string
			Transport   string // "grpc" (default) | "zrpc"
		}
		Keywords struct {
			Address     string
			AccessToken string
			Transport   string // "grpc" (default) | "zrpc"
		}
		Tokenizer struct {
			Address string
		}
		Semantic struct {
			Address string
		}
	}
	VectorDB struct {
		Url                string
		Username           string
		Pwd                string
		Database           string
		Timeout            int
		MaxIdleConnPerHost int
		ReadConsistency    string
		IdleConnTimeout    int
	}
	SemanticCache struct {
		Enabled bool `mapstructure:"enabled"`
		// Deprecated: Phase-1 阈值,仅兼容读取,不再参与新(e5/批决策)链路。
		Threshold float32 `mapstructure:"threshold"`
		// Deprecated: Phase-1 rerank 阈值,仅兼容读取,不再参与新链路。/rerank 已废弃且 score 恒 0。
		RerankThreshold         float32 `mapstructure:"rerank_threshold"`
		ExactFingerprintEnabled bool    `mapstructure:"exact_fingerprint_enabled"`
		TopK                    int     `mapstructure:"top_k"`

		// e5 向量命名空间与一致性(e5s:v1)。
		Dimension         int    `mapstructure:"dimension"`          // 384
		VectorNamespace   string `mapstructure:"vector_namespace"`   // "semd:e5s:v1:"
		EmbeddingModel    string `mapstructure:"embedding_model"`    // "intfloat/multilingual-e5-small"
		EmbeddingRevision string `mapstructure:"embedding_revision"` // 固定 commit SHA
		ExportVersion     string `mapstructure:"export_version"`     // "onnx-int8-v1"

		// 检索阈值:acceptance_threshold + min_margin(TrimFlow 靠 VSEARCH Top-K)。
		AcceptanceThreshold  float32 `mapstructure:"acceptance_threshold"`
		MinMargin            float32 `mapstructure:"min_margin"`
		SoftSemanticFallback bool    `mapstructure:"soft_semantic_fallback"` // soft 通道兜底,默认 false

		// 灰度读写模式与异步回填。
		VectorReadMode  string `mapstructure:"vector_read_mode"`  // "new_only" | "dual_read"
		VectorWriteMode string `mapstructure:"vector_write_mode"` // "new_only" | "dual_write"
		AsyncBackfill   bool   `mapstructure:"async_backfill"`    // 默认 true
	} `mapstructure:"semantic_cache"`
}

var conf *Config

func InitConfig(filePath string, typ ...string) {
	v := viper.New()
	v.SetConfigFile(filePath)
	if len(typ) > 0 {
		v.SetConfigType(typ[0])
	}
	err := v.ReadInConfig()
	if err != nil {
		log.Fatal(err)
	}
	conf = &Config{}
	err = v.Unmarshal(conf)
	if err != nil {
		log.Fatal(err)
	}
	applyEnvOverrides(conf)
}

func expandEnvValue(v string) string {
	if strings.Contains(v, "${") {
		return os.ExpandEnv(v)
	}
	return v
}

func envOverride(name, cur string) string {
	if val := os.Getenv(name); val != "" {
		return val
	}
	return cur
}

// applyEnvOverrides 把敏感配置从 YAML/仓库移出：支持 ${VAR} 占位符和显式 env 覆盖。
// 不把 DSN/token 写入日志或 Prometheus 标签。
func applyEnvOverrides(c *Config) {
	if c == nil {
		return
	}
	c.Mysql.DSN = envOverride("AI_CHAT_MYSQL_DSN", expandEnvValue(c.Mysql.DSN))
	c.Server.AccessToken = envOverride("AI_CHAT_SERVER_ACCESS_TOKEN",
		expandEnvValue(c.Server.AccessToken))
	c.Server.MetricsAddr = envOverride("AI_CHAT_METRICS_ADDR",
		expandEnvValue(c.Server.MetricsAddr))
	c.Server.MetricsAccessToken = envOverride("AI_CHAT_METRICS_TOKEN",
		expandEnvValue(c.Server.MetricsAccessToken))
	c.Identity.Secret = envOverride("AI_CHAT_IDENTITY_SECRET",
		expandEnvValue(c.Identity.Secret))
	c.Identity.Issuer = envOverride("AI_CHAT_IDENTITY_ISSUER",
		expandEnvValue(c.Identity.Issuer))
	c.Chat.ApiKey = envOverride("AI_CHAT_API_KEY", expandEnvValue(c.Chat.ApiKey))
	if c.Chat.ApiKey == "" {
		c.Chat.ApiKey = envOverride("OPENAI_API_KEY", "")
	}
	c.Redis.Pwd = envOverride("AI_CHAT_REDIS_PWD", expandEnvValue(c.Redis.Pwd))
	c.DependOn.Sensitive.AccessToken = envOverride("AI_CHAT_SENSITIVE_ACCESS_TOKEN",
		expandEnvValue(c.DependOn.Sensitive.AccessToken))
	c.DependOn.Keywords.AccessToken = envOverride("AI_CHAT_KEYWORDS_ACCESS_TOKEN",
		expandEnvValue(c.DependOn.Keywords.AccessToken))
	c.VectorDB.Username = envOverride("AI_CHAT_VECTORDB_USERNAME",
		expandEnvValue(c.VectorDB.Username))
	c.VectorDB.Pwd = envOverride("AI_CHAT_VECTORDB_PWD",
		expandEnvValue(c.VectorDB.Pwd))
}

func GetConfig() *Config {
	return conf
}
