package config

import (
	"log"
	"os"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Http struct {
		Host        string
		Port        int
		AccessToken string `mapstructure:"access_token"`
		Mode        string
	}
	Chat struct {
		APIKeys []string `mapstructure:"api_keys"`
		BaseURL string   `mapstructure:"base_url"`
	}
	Log struct {
		Level   string
		LogPath string `mapstructure:"log_path"`
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
	// DeepSeek key 从环境变量注入，覆盖配置文件（不把密钥写进 git）
	if k := os.Getenv("DEEPSEEK_API_KEY"); k != "" {
		conf.Chat.APIKeys = []string{k}
	}
	if strings.Contains(conf.Http.AccessToken, "${") {
		conf.Http.AccessToken = os.ExpandEnv(conf.Http.AccessToken)
	}
	if v := os.Getenv("OPENAI_PROXY_ACCESS_TOKEN"); v != "" {
		conf.Http.AccessToken = v
	}

}

func GetConfig() *Config {
	return conf
}
