package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"ai-chat-service/chat-server/data"
	metrics_bus "ai-chat-service/chat-server/metrics-bus"
	"ai-chat-service/chat-server/semcache"
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

const (
	defaultMetricsAddr = "127.0.0.1:8080"
	shutdownTimeout    = 10 * time.Second
)

// fatalStartup 记录启动期致命错误后终止进程。
// 注意：pkg/log 的 FatalF 只按 FatalLevel 记一条日志，并不会 os.Exit；
// 若直接调用，进程会带着空 DSN/空 AccessToken 继续对外服务。
func fatalStartup(format string, args ...interface{}) {
	log.FatalF(format, args...)
	os.Exit(1)
}

// metricsAuth 在配置了 token 时要求 Authorization: Bearer <token>，
// 避免 /metrics（含连接池/业务指标）无认证暴露到所有网卡。
func metricsAuth(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	flag.Parse()

	// 配置必须先加载：metrics 监听地址/抓取 token 都来自配置。
	config.InitConfig(*configFile)
	cnf := config.GetConfig()
	log.SetLevel(cnf.Log.Level)
	log.SetOutput(log.GetRotateWriter(cnf.Log.LogPath))
	log.SetPrintCaller(true)

	logger := log.NewLogger()
	logger.SetLevel(cnf.Log.Level)
	logger.SetOutput(log.GetRotateWriter(cnf.Log.LogPath))
	logger.SetPrintCaller(true)

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	busMetrics := metrics_bus.NewBusMetrics(registry)
	// DBStats 在 scrape 时通过函数取连接池；MySQL 初始化前返回 nil，不 panic。
	registry.MustRegister(metrics_bus.NewMySQLPoolCollector(mysql.GetDB))
	semcache.SetObserver(busMetrics)

	// 健康/指标 HTTP：默认只监听 loopback，容器/内网抓取需显式配置 metrics_addr，
	// 并配合网络策略或 metrics_access_token 限制来源。
	metricsAddr := cnf.Server.MetricsAddr
	if metricsAddr == "" {
		metricsAddr = defaultMetricsAddr
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsAuth(
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		cnf.Server.MetricsAccessToken))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
	})
	httpSrv := &http.Server{Addr: metricsAddr, Handler: mux}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(err)
		}
	}()

	if err := mysql.InitMysql(cnf); err != nil {
		fatalStartup("init mysql: %v", err)
	}
	if cnf.Server.AccessToken == "" {
		fatalStartup("server access token is empty; set AI_CHAT_SERVER_ACCESS_TOKEN")
	}
	redis.InitRedisPool(cnf)
	recordsData := data.NewChatRecordsData(mysql.GetDB(), busMetrics)

	// 单 zrpc 传输（gRPC 已删）：业务端口即 cnf.Server.Port（50055）
	service := server.NewChatService(recordsData, cnf, logger, busMetrics)
	zsrv, err := zrpc.NewServer(zrpc.ServerOptions{
		Address:     fmt.Sprintf("%s:%d", cnf.Server.IP, cnf.Server.Port),
		AccessToken: cnf.Server.AccessToken,
	})
	if err != nil {
		fatalStartup("%v", err)
	}
	if err := server.RegisterChatZRPC(zsrv, service, true, busMetrics); err != nil {
		fatalStartup("%v", err)
	}
	if err := zsrv.Serve(); err != nil {
		fatalStartup("%v", err)
	}
	ready.Store(true)
	fmt.Printf("[zrpc] listening on %s:%d\n", cnf.Server.IP, cnf.Server.Port)

	// 优雅退出：先停止接收新请求，再在 deadline 内排空入库队列，
	// 最后关闭 MySQL 与指标 HTTP。SIGKILL/断电仍会丢失内存队列中的记录，
	// 强持久化需求需数据库 Outbox 或同步关键写入。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	log.InfoF("received signal %s: stop accepting requests", sig)
	ready.Store(false)

	if err := zsrv.Close(); err != nil {
		log.ErrorF("zrpc close: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := recordsData.Close(shutdownCtx); err != nil {
		log.ErrorF("drain chat_records queue: %v", err)
	}
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.ErrorF("metrics http shutdown: %v", err)
	}
	if err := mysql.Close(); err != nil {
		log.ErrorF("mysql close: %v", err)
	}
	log.InfoF("shutdown complete")
}
