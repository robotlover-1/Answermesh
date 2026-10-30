package config

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Http struct {
		IP   string
		Port int
		// 可信代理；空=回环默认，见 middlewares.NewEngine
		TrustedProxies []string `mapstructure:"trusted_proxies"`
	}
	Log struct {
		Level   string
		LogPath string `mapstructure:"logPath"`
	} `mapstructure:"log"`
	Chat struct {
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
	DependOn struct {
		AiChatService struct {
			Address     string // gRPC address (50055)
			ZrpcAddress string // zrpc v2 address (50065)
			AccessToken string
			Transport   string // "grpc" (default) | "zrpc"
		} `mapstructure:"ai-chat-service"`
	}
	Redis struct {
		Host string
		Port int
		Pwd  string `mapstructure:"pwd"`
	}
	Mysql struct {
		DSN         string
		MaxOpenConn int
		MaxIdleConn int
	}
	Auth struct {
		Enabled   bool `mapstructure:"enabled"`
		InitQuota int  `mapstructure:"init_quota"`
	}
	// Identity 用于给 ai-chat-service 签发短时 tenant/user 身份令牌（HMAC）。
	Identity struct {
		Secret   string `mapstructure:"secret"`
		Issuer   string `mapstructure:"issuer"`
		TenantID uint64 `mapstructure:"tenant_id"`
		// AdminUserIDs 中的用户签发 role=admin，其余为 user。
		AdminUserIDs []uint64 `mapstructure:"admin_user_ids"`
	}
	Tokenizer struct {
		Address string
	}
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
	if v := os.Getenv(name); v != "" {
		return v
	}
	return cur
}

// 敏感配置不入库：支持 ${VAR} 占位符与 env 覆盖（MySQL DSN / 服务 token / Redis 密码）。
func applyEnvOverrides(c *Config) {
	if c == nil {
		return
	}
	c.Mysql.DSN = envOverride("AI_CHAT_MYSQL_DSN", expandEnvValue(c.Mysql.DSN))
	c.DependOn.AiChatService.AccessToken = envOverride(
		"AI_CHAT_SERVER_ACCESS_TOKEN",
		expandEnvValue(c.DependOn.AiChatService.AccessToken))
	c.Redis.Pwd = envOverride("AI_CHAT_REDIS_PWD", expandEnvValue(c.Redis.Pwd))
	c.Identity.Secret = envOverride("AI_CHAT_IDENTITY_SECRET",
		expandEnvValue(c.Identity.Secret))
	c.Identity.Issuer = envOverride("AI_CHAT_IDENTITY_ISSUER",
		expandEnvValue(c.Identity.Issuer))
	if c.Identity.TenantID == 0 {
		c.Identity.TenantID = 1
	}
	if c.Identity.Issuer == "" {
		c.Identity.Issuer = "answermesh-backend"
	}
	if v := os.Getenv("AI_CHAT_IDENTITY_ADMIN_USER_IDS"); v != "" {
		ids := []uint64{}
		for _, part := range strings.Split(v, ",") {
			if id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
				ids = append(ids, id)
			}
		}
		c.Identity.AdminUserIDs = ids
	}
}

func GetConfig() *Config {
	return conf
}
