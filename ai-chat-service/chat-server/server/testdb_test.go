package server

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"ai-chat-service/db/migrations"
	"ai-chat-service/pkg/db/migrate"

	_ "github.com/go-sql-driver/mysql"
)

// setupServerDB 打开真实 MySQL、应用迁移并分配唯一 tenant；清理该租户的会话/知识/用量。
func setupServerDB(t *testing.T) (*sql.DB, uint64) {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(ctx, db, migs); err != nil {
		t.Fatal(err)
	}
	tenant := uint64(time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM cache_outbox WHERE entry_id IN (SELECT id FROM knowledge_entries WHERE tenant_id=?)", tenant)
		_, _ = db.Exec("DELETE FROM knowledge_entries WHERE tenant_id=?", tenant)
		_, _ = db.Exec("DELETE FROM chat_sessions WHERE tenant_id=?", tenant)
		_, _ = db.Exec("DELETE FROM llm_usage_events WHERE tenant_id=?", tenant)
		_, _ = db.Exec("DELETE FROM usage_daily WHERE tenant_id=?", tenant)
		_ = db.Close()
	})
	return db, tenant
}
