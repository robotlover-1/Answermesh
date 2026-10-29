package migrate_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"ai-chat-service/db/migrations"
	"ai-chat-service/pkg/db/migrate"

	_ "github.com/go-sql-driver/mysql"
)

func TestSplitStatements(t *testing.T) {
	sqlText := `
-- comment; with semicolon
CREATE TABLE a (id INT, note VARCHAR(10) DEFAULT 'x;y');
/* block ; comment */
# hash ; comment
INSERT INTO a VALUES (1, "q\"r;z");
CREATE TABLE ` + "`b;c`" + ` (id INT)
`
	got := migrate.SplitStatements(sqlText)
	if len(got) != 3 {
		t.Fatalf("statements = %d (%v), want 3", len(got), got)
	}
	if !strings.Contains(got[0], "'x;y'") {
		t.Fatalf("quoted semicolon lost: %q", got[0])
	}
	if !strings.Contains(got[1], `"q\"r;z"`) {
		t.Fatalf("escaped quote lost: %q", got[1])
	}
	if !strings.Contains(got[2], "`b;c`") {
		t.Fatalf("backtick identifier lost: %q", got[2])
	}
}

func TestLoadEmbedded(t *testing.T) {
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(migs) != 5 {
		t.Fatalf("migrations = %d, want 5", len(migs))
	}
	for i, m := range migs {
		if m.Version != int64(i+1) || m.UpSQL == "" || m.DownSQL == "" || len(m.Checksum) != 64 {
			t.Fatalf("bad migration %d: %+v", i, m)
		}
	}
}

// 集成测试需要真实 MySQL：MYSQL_TEST_DSN=user:pass@tcp(127.0.0.1:3306)/test_db?...
// 会创建并回滚 0001..0004 的表，最后删除 schema_migrations。
func TestApplyAndRollbackMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS schema_migrations")
	}()

	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 清理上一次失败留下的表，保证从干净状态开始。
	if _, err := migrate.RollbackLast(ctx, db, migs, len(migs)+8); err != nil {
		t.Logf("pre-clean rollback: %v", err)
	}

	done, err := migrate.Apply(ctx, db, migs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(done) != len(migs) {
		t.Fatalf("applied = %v, want all %d", done, len(migs))
	}
	done, err = migrate.Apply(ctx, db, migs)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(done) != 0 {
		t.Fatalf("second Apply applied %v, want none", done)
	}

	// 已应用版本内容变化必须被检出。
	tampered := append([]migrate.Migration(nil), migs...)
	tampered[0].Checksum = strings.Repeat("0", 64)
	if _, err := migrate.Apply(ctx, db, tampered); err == nil {
		t.Fatal("expected checksum mismatch error")
	}

	rolled, err := migrate.RollbackLast(ctx, db, migs, len(migs))
	if err != nil {
		t.Fatalf("RollbackLast: %v", err)
	}
	if len(rolled) != len(migs) {
		t.Fatalf("rolled back %v, want %d", rolled, len(migs))
	}
	for _, table := range []string{"chat_messages", "chat_sessions", "knowledge_versions",
		"knowledge_entries", "cache_outbox", "llm_usage_events", "usage_daily"} {
		var n int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?",
			table).Scan(&n); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("table %s still exists after rollback", table)
		}
	}
}
