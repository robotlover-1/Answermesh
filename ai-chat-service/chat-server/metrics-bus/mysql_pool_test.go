package metrics_bus

import (
	"database/sql"
	"testing"

	"ai-chat-service/chat-server/data"
	"ai-chat-service/chat-server/semcache"
	_ "github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
)

// 编译期确认注入点契约。
var (
	_ data.RecordsObserver = (*BusMetrics)(nil)
	_ semcache.Observer    = (*BusMetrics)(nil)
)

func TestMySQLPoolCollectorNilDB(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewMySQLPoolCollector(func() *sql.DB { return nil }))
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather with nil db: %v", err)
	}
}

func TestMySQLPoolCollectorStats(t *testing.T) {
	// sql.Open 是惰性的，不建立连接也能读取 DBStats。
	db, err := sql.Open("mysql", "user:pass@tcp(127.0.0.1:1)/none")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(7)

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewMySQLPoolCollector(func() *sql.DB { return db }))
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := map[string]bool{}
	for _, f := range fams {
		names[f.GetName()] = true
	}
	for _, want := range []string{
		"ai_chat_chat_service_mysql_pool_open_connections",
		"ai_chat_chat_service_mysql_pool_in_use_connections",
		"ai_chat_chat_service_mysql_pool_idle_connections",
		"ai_chat_chat_service_mysql_pool_max_open_connections",
		"ai_chat_chat_service_mysql_pool_wait_count_total",
		"ai_chat_chat_service_mysql_pool_wait_duration_seconds_total",
	} {
		if !names[want] {
			t.Fatalf("metric %s missing", want)
		}
	}
}
