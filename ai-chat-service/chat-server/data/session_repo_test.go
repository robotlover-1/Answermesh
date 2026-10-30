package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"ai-chat-service/db/migrations"
	"ai-chat-service/pkg/db/migrate"

	_ "github.com/go-sql-driver/mysql"
)

// 集成测试需要真实 MySQL（会创建 chat_sessions/chat_messages 等迁移表）：
//
//	MYSQL_TEST_DSN='user:pass@tcp(127.0.0.1:3306)/test_db?parseTime=true&charset=utf8mb4' \
//	  go test ./chat-server/data/
//
// setupDB 打开真实 MySQL、应用迁移，并为每个测试分配唯一 tenant；测试结束清理该租户数据。
func setupDB(t *testing.T) (*sql.DB, uint64) {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Apply(ctx, db, migs); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	tenant := uint64(time.Now().UnixNano())
	if tenant == 0 {
		tenant = 1
	}
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM cache_outbox WHERE entry_id IN (SELECT id FROM knowledge_entries WHERE tenant_id=?)", tenant)
		_, _ = db.Exec("DELETE FROM knowledge_entries WHERE tenant_id=?", tenant)
		_, _ = db.Exec("DELETE FROM chat_sessions WHERE tenant_id=?", tenant)
		_, _ = db.Exec("DELETE FROM usage_daily WHERE tenant_id=?", tenant)
		_, _ = db.Exec("DELETE FROM llm_usage_events WHERE tenant_id=?", tenant)
		_ = db.Close()
	})
	return db, tenant
}

func setupSessionRepo(t *testing.T) (*SessionRepository, *sql.DB, uint64) {
	t.Helper()
	db, tenant := setupDB(t)
	return NewSessionRepository(db, nil), db, tenant
}

func mustSession(t *testing.T, r *SessionRepository, tenant, user uint64, title string) uint64 {
	t.Helper()
	id, err := r.CreateSession(context.Background(), tenant, user, title)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return id
}

func TestSessionRepoPagination(t *testing.T) {
	r, _, tenant := setupSessionRepo(t)
	const user = uint64(7)
	ctx := context.Background()
	sessionID := mustSession(t, r, tenant, user, "pagination")

	for i := 0; i < 25; i++ {
		in := AppendInput{
			RequestID:        fmt.Sprintf("req-%d-%d", tenant, i),
			UserContent:      fmt.Sprintf("q%d", i),
			AssistantContent: fmt.Sprintf("a%d", i),
		}
		if _, _, _, err := r.AppendConversation(ctx, tenant, user, sessionID, in); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	var all []Message
	before := uint64(0)
	pages := 0
	for {
		page, err := r.ListMessages(ctx, tenant, user, sessionID, before, 10)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		for _, m := range page {
			if before != 0 && m.ID >= before {
				t.Fatalf("cursor violated: id=%d before=%d", m.ID, before)
			}
		}
		all = append(all, page...)
		before = page[len(page)-1].ID
		if pages > 10 {
			t.Fatal("pagination did not converge")
		}
	}
	if len(all) != 50 {
		t.Fatalf("messages = %d, want 50", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatalf("not descending at %d: %d >= %d", i, all[i].ID, all[i-1].ID)
		}
	}

	// 会话列表游标
	for i := 0; i < 4; i++ {
		mustSession(t, r, tenant, user, fmt.Sprintf("s%d", i))
	}
	first, err := r.ListSessions(ctx, tenant, user, 0, 3)
	if err != nil || len(first) != 3 {
		t.Fatalf("ListSessions first = %v, %v", first, err)
	}
	next, err := r.ListSessions(ctx, tenant, user, first[len(first)-1].ID, 3)
	if err != nil || len(next) == 0 {
		t.Fatalf("ListSessions next = %v, %v", next, err)
	}
	for _, s := range next {
		if s.ID >= first[len(first)-1].ID {
			t.Fatalf("session cursor violated: %d >= %d", s.ID, first[len(first)-1].ID)
		}
	}
}

func TestAppendConversationIdempotent(t *testing.T) {
	r, db, tenant := setupSessionRepo(t)
	const user = uint64(8)
	ctx := context.Background()
	sessionID := mustSession(t, r, tenant, user, "idem")
	req := fmt.Sprintf("req-idem-%d", tenant)

	in := AppendInput{RequestID: req, UserContent: "original-q", AssistantContent: "original-a"}
	uid1, aid1, created1, err := r.AppendConversation(ctx, tenant, user, sessionID, in)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	if !created1 {
		t.Fatal("first append should report created")
	}
	uid2, aid2, created2, err := r.AppendConversation(ctx, tenant, user, sessionID, in)
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	if created2 {
		t.Fatal("duplicate request should be idempotent")
	}
	if uid1 != uid2 || aid1 != aid2 {
		t.Fatalf("ids changed: (%d,%d) vs (%d,%d)", uid1, aid1, uid2, aid2)
	}
	// 同一 request_id 携带不同内容必须拒绝，不能静默返回旧记录。
	in.UserContent = "changed-q"
	if _, _, _, err := r.AppendConversation(ctx, tenant, user, sessionID, in); !errors.Is(err, ErrRequestPayloadMismatch) {
		t.Fatalf("same request_id different content err = %v, want ErrRequestPayloadMismatch", err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM chat_messages WHERE session_id=?", sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("message count = %d, want 2", n)
	}
	var content string
	if err := db.QueryRow("SELECT content FROM chat_messages WHERE id=?", uid1).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "original-q" {
		t.Fatalf("duplicate request overwrote final message: %q", content)
	}
}

func TestSessionRepoTenantIsolation(t *testing.T) {
	r, _, tenant := setupSessionRepo(t)
	const userA, userB = uint64(9), uint64(10)
	otherTenant := tenant + 1
	ctx := context.Background()
	sessionID := mustSession(t, r, tenant, userA, "private")
	if _, _, _, err := r.AppendConversation(ctx, tenant, userA, sessionID, AppendInput{
		RequestID: fmt.Sprintf("req-iso-%d", tenant), UserContent: "q", AssistantContent: "a",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if msgs, err := r.ListMessages(ctx, otherTenant, userA, sessionID, 0, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("cross-tenant ListMessages = %v, %v", msgs, err)
	}
	if msgs, err := r.ListMessages(ctx, tenant, userB, sessionID, 0, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("cross-user ListMessages = %v, %v", msgs, err)
	}
	if _, _, _, err := r.AppendConversation(ctx, otherTenant, userA, sessionID, AppendInput{
		RequestID: fmt.Sprintf("req-iso2-%d", tenant), UserContent: "q2", AssistantContent: "a2",
	}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant append err = %v, want ErrSessionNotFound", err)
	}
	if ok, err := r.SoftDeleteSession(ctx, otherTenant, userA, sessionID); err != nil || ok {
		t.Fatalf("cross-tenant delete = %v, %v", ok, err)
	}
	if ok, err := r.SoftDeleteSession(ctx, tenant, userA, sessionID); err != nil || !ok {
		t.Fatalf("owner delete = %v, %v", ok, err)
	}
	if _, _, _, err := r.AppendConversation(ctx, tenant, userA, sessionID, AppendInput{
		RequestID: fmt.Sprintf("req-iso3-%d", tenant), UserContent: "q3", AssistantContent: "a3",
	}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("append to deleted session err = %v, want ErrSessionNotFound", err)
	}
}

func TestExplainUsesCursorIndexes(t *testing.T) {
	r, db, tenant := setupSessionRepo(t)
	const user = uint64(11)
	ctx := context.Background()
	sessionID := mustSession(t, r, tenant, user, "explain")

	// 批量插入 500 条消息，让优化器有足够统计选择索引。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := 0; chunk < 25; chunk++ {
		args := []any{}
		vals := []string{}
		for i := 0; i < 20; i++ {
			n := chunk*20 + i
			vals = append(vals, "(?,?,?,?)")
			args = append(args, sessionID, fmt.Sprintf("bulk-%d-%d", tenant, n), "user", "c")
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO chat_messages(session_id,request_id,role,content) VALUES "+strings.Join(vals, ","),
			args...); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ANALYZE TABLE chat_messages, chat_sessions"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	explain := func(query string, args ...any) string {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN FORMAT=JSON "+query, args...).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		return plan
	}
	msgPlan := explain(`
SELECT m.id FROM chat_messages m JOIN chat_sessions s ON s.id=m.session_id
WHERE m.session_id=? AND s.tenant_id=? AND s.user_id=? AND m.id<?
ORDER BY m.id DESC LIMIT 20`, sessionID, tenant, user, uint64(1)<<62)
	if !strings.Contains(msgPlan, "idx_session_messages") {
		t.Fatalf("message pagination does not use idx_session_messages:\n%s", msgPlan)
	}
	sessPlan := explain(`
SELECT id FROM chat_sessions
WHERE tenant_id=? AND user_id=? AND deleted_at IS NULL AND (?=0 OR id<?)
ORDER BY id DESC LIMIT 20`, tenant, user, uint64(0), uint64(0))
	if !strings.Contains(sessPlan, "idx_user_cursor") {
		t.Fatalf("session pagination does not use idx_user_cursor:\n%s", sessPlan)
	}
}

// P1-01：同一 request_id 在不同会话（甚至不同租户）必须各自落库，不能静默复用。
func TestAppendConversationSameRequestDifferentSessions(t *testing.T) {
	r, db, tenant := setupSessionRepo(t)
	const user = uint64(12)
	ctx := context.Background()
	sessionA := mustSession(t, r, tenant, user, "same-req-a")
	sessionB := mustSession(t, r, tenant, user, "same-req-b")
	req := fmt.Sprintf("same-req-%d", tenant)

	uidA, aidA, createdA, err := r.AppendConversation(ctx, tenant, user, sessionA, AppendInput{
		RequestID: req, UserContent: "qa", AssistantContent: "aa",
	})
	if err != nil || !createdA {
		t.Fatalf("append A = %v %v %v", uidA, createdA, err)
	}
	uidB, aidB, createdB, err := r.AppendConversation(ctx, tenant, user, sessionB, AppendInput{
		RequestID: req, UserContent: "qb", AssistantContent: "ab",
	})
	if err != nil || !createdB {
		t.Fatalf("append B = %v %v %v", uidB, createdB, err)
	}
	if uidA == uidB || aidA == aidB {
		t.Fatalf("different sessions reused message ids: A(%d,%d) B(%d,%d)", uidA, aidA, uidB, aidB)
	}
	for _, sid := range []uint64{sessionA, sessionB} {
		msgs, err := r.ListMessages(ctx, tenant, user, sid, 0, 10)
		if err != nil || len(msgs) != 2 {
			t.Fatalf("session %d messages = %d, %v", sid, len(msgs), err)
		}
	}

	// 跨租户同一 request_id 同样独立。
	otherTenant := tenant + 1
	sessionC, err := r.CreateSession(ctx, otherTenant, user, "other-tenant")
	if err != nil {
		t.Fatalf("create other tenant session: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM chat_sessions WHERE id=?", sessionC) })
	if _, _, created, err := r.AppendConversation(ctx, otherTenant, user, sessionC, AppendInput{
		RequestID: req, UserContent: "qc", AssistantContent: "ac",
	}); err != nil || !created {
		t.Fatalf("append other tenant = %v, %v", created, err)
	}
}
