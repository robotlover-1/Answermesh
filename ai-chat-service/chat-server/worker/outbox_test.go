package worker

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"ai-chat-service/chat-server/data"
	"ai-chat-service/db/migrations"
	"ai-chat-service/pkg/db/migrate"

	_ "github.com/go-sql-driver/mysql"
)

type recordingCache struct {
	mu   sync.Mutex
	data map[string]string
	err  error
}

func (c *recordingCache) Get(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok, nil
}

func (c *recordingCache) SetImmutable(_ context.Context, key, value string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.data[key] = value
	return nil
}

func (c *recordingCache) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.data[key]
	return ok
}

func TestOutboxWorkerPublishesToCache(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	})

	knowledge := data.NewKnowledgeRepository(db, nil)
	outbox := data.NewOutboxRepository(db, nil)
	entryID, err := knowledge.CreateEntry(ctx, tenant, "outbox worker", "db")
	if err != nil {
		t.Fatal(err)
	}
	version, err := knowledge.SubmitVersion(ctx, tenant, entryID, "worker answer")
	if err != nil {
		t.Fatal(err)
	}
	if err := knowledge.Review(ctx, tenant, entryID, version, 7, true); err != nil {
		t.Fatal(err)
	}
	if err := knowledge.Publish(ctx, tenant, entryID, version, 7); err != nil {
		t.Fatal(err)
	}

	cache := &recordingCache{data: map[string]string{}}
	w := NewOutboxWorker(outbox, knowledge, cache, nil, OutboxConfig{
		Interval: 5 * time.Millisecond, Lease: time.Minute, BatchSize: 10,
		MaxAttempts: 3, CacheTTL: time.Hour, BaseBackoff: time.Second,
	})
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(runCtx) }()

	key := data.PublishedAnswerKey(entryID, version)
	deadline := time.Now().Add(5 * time.Second)
	for !cache.has(key) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop on context cancel")
	}
	if !cache.has(key) {
		t.Fatalf("worker did not write %s", key)
	}
	if pending, _, err := outbox.Stats(ctx); err != nil || pending != 0 {
		t.Fatalf("outbox pending = %d, %v; want 0", pending, err)
	}
}
