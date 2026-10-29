package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version 由 -ldflags "-X main.version=<short-sha>" 注入；只允许发布版本/短 SHA。
var version = "dev"

func main() {
	var (
		target        = flag.String("target", "127.0.0.1:5160", "PocketKV RESP 地址")
		listen        = flag.String("listen", "127.0.0.1:9401", "Exporter 监听地址（默认仅本机）")
		scrapeTimeout = flag.Duration("scrape-timeout", 1500*time.Millisecond, "单条 INFO/MEMSTAT 命令超时")
		role          = flag.String("role", "auto", "role 标签：master|slave|auto（auto 取 INFO role）")
		showVersion   = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *scrapeTimeout <= 0 || *scrapeTimeout > 10*time.Second {
		log.Fatalf("invalid -scrape-timeout %v (must be in (0,10s])", *scrapeTimeout)
	}
	if *role != "master" && *role != "slave" && *role != "auto" && *role != "" {
		log.Fatalf("invalid -role %q (master|slave|auto)", *role)
	}

	client := newRESPClient(*target, *scrapeTimeout)
	defer client.Close()
	exp := NewExporter(client, version, *role)

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Exporter 自身存活即可；目标可用性看 pocketkv_exporter_scrape_success。
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("pocketkv-exporter %s: target=%s listen=%s timeout=%s role=%s",
		version, *target, *listen, *scrapeTimeout, *role)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("listen: %v", err)
		os.Exit(1)
	}
}
