package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"sync/atomic"

	"ai-chat-service/chat-server/data"
	metrics_bus "ai-chat-service/chat-server/metrics-bus"
	"ai-chat-service/chat-server/server"
	"ai-chat-service/pkg/config"
	"ai-chat-service/pkg/db/mysql"
	"ai-chat-service/pkg/db/redis"
	"ai-chat-service/pkg/log"

	zrpc "echo-zrpc-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	configFile = flag.String("config", "dev.config.yaml", "")
	// ready 置真表示 zrpc 监听已就绪，供 /readyz 探活。
	ready atomic.Bool
)

func main() {
	flag.Parse()

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	busMetrics := metrics_bus.NewBusMetrics(registry)

	// 健康/指标 HTTP（:8080；/readyz 供 compose 探活）
	http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	http.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
	})
	go func() {
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Error(err)
		}
	}()

	// 配置/日志
	config.InitConfig(*configFile)
	cnf := config.GetConfig()
	log.SetLevel(cnf.Log.Level)
	log.SetOutput(log.GetRotateWriter(cnf.Log.LogPath))
	log.SetPrintCaller(true)

	logger := log.NewLogger()
	logger.SetLevel(cnf.Log.Level)
	logger.SetOutput(log.GetRotateWriter(cnf.Log.LogPath))
	logger.SetPrintCaller(true)

	if err := mysql.InitMysql(cnf); err != nil {
		log.FatalF("init mysql: %v", err)
	}
	if cnf.Server.AccessToken == "" {
		log.Fatal("server access token is empty; set AI_CHAT_SERVER_ACCESS_TOKEN")
	}
	redis.InitRedisPool(cnf)
	recordsData := data.NewChatRecordsData(mysql.GetDB())
	defer recordsData.Close(context.Background())

	// 单 zrpc 传输（gRPC 已删）：业务端口即 cnf.Server.Port（50055）
	service := server.NewChatService(recordsData, cnf, logger, busMetrics)
	zsrv, err := zrpc.NewServer(zrpc.ServerOptions{
		Address:     fmt.Sprintf("%s:%d", cnf.Server.IP, cnf.Server.Port),
		AccessToken: cnf.Server.AccessToken,
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := server.RegisterChatZRPC(zsrv, service, true, busMetrics); err != nil {
		log.Fatal(err)
	}
	if err := zsrv.Serve(); err != nil {
		log.Fatal(err)
	}
	ready.Store(true)
	fmt.Printf("[zrpc] listening on %s:%d\n", cnf.Server.IP, cnf.Server.Port)

	select {} // zrpc 调度线程在独立线程，主线程保持存活
}
