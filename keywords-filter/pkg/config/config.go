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
		Port        int // zrpc v2 business port（gRPC 已删）
		AccessToken string
		HealthPort  int // 0 disables the HTTP healthz/readyz listener
	}
	Log struct {
		Level   string
		LogPath string `mapstructure:"logPath"`
	} `mapstructure:"log"`
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
	if strings.Contains(conf.Server.AccessToken, "${") {
		conf.Server.AccessToken = os.ExpandEnv(conf.Server.AccessToken)
	}
	if v := os.Getenv("AI_CHAT_KEYWORDS_ACCESS_TOKEN"); v != "" {
		conf.Server.AccessToken = v
	}
}

func GetConfig() *Config {
	return conf
}
