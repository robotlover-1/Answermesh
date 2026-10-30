package data

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

type fakeKBStore struct {
	mu     sync.Mutex
	data   map[string]string
	setErr error
	getErr error
	sets   []string
}

func newFakeKBStore() *fakeKBStore { return &fakeKBStore{data: map[string]string{}} }

func (f *fakeKBStore) Get(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", false, f.getErr
	}
	v, ok := f.data[key]
	return v, ok, nil
}

func (f *fakeKBStore) SetImmutable(_ context.Context, key, value string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.data[key] = value
	f.sets = append(f.sets, key)
	return nil
}

func (f *fakeKBStore) put(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = value
}

func newKnowledgeTestData(t *testing.T) (*KnowledgeRepository, *OutboxRepository, uint64) {
	t.Helper()
	db, tenant := setupDB(t)
	return NewKnowledgeRepository(db, nil), NewOutboxRepository(db, nil), tenant
}

func mustPublished(t *testing.T, repo *KnowledgeRepository, tenant uint64, entryID uint64,
	answer string) uint32 {
	t.Helper()
	ctx := context.Background()
	version, err := repo.SubmitVersion(ctx, tenant, entryID, answer)
	if err != nil {
		t.Fatalf("SubmitVersion: %v", err)
	}
	if err := repo.Review(ctx, tenant, entryID, version, 42, true); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if err := repo.Publish(ctx, tenant, entryID, version, 42); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return version
}

func TestKnowledgePublishFlow(t *testing.T) {
	repo, outbox, tenant := newKnowledgeTestData(t)
	ctx := context.Background()
	entryID, err := repo.CreateEntry(ctx, tenant, "什么是 MVCC？", "db")
	if err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	version, err := repo.SubmitVersion(ctx, tenant, entryID, "v1 answer")
	if err != nil {
		t.Fatalf("SubmitVersion: %v", err)
	}
	if err := repo.Publish(ctx, tenant, entryID, version, 1); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("publish unapproved err = %v, want ErrNotApproved", err)
	}
	if err := repo.Review(ctx, tenant, entryID, version, 42, true); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if err := repo.Publish(ctx, tenant, entryID, version, 42); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// 已发布版本不能直接改审核结果（需显式撤稿流程）。
	if err := repo.Review(ctx, tenant, entryID, version, 42, false); !errors.Is(err, ErrKnowledgePublished) {
		t.Fatalf("review published err = %v, want ErrKnowledgePublished", err)
	}
	// 重复发布同一版本：成功且不重复写 Outbox。
	if err := repo.Publish(ctx, tenant, entryID, version, 42); err != nil {
		t.Fatalf("idempotent Publish: %v", err)
	}
	keys, err := outbox.PendingEventKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := fmt.Sprintf("knowledge:%d:%d", entryID, version)
	count := 0
	for _, k := range keys {
		if k == wantKey {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("outbox events for version %d = %d, want 1 (keys=%v)", version, count, keys)
	}

	v2 := mustPublished(t, repo, tenant, entryID, "v2 answer")
	if v2 != 2 {
		t.Fatalf("v2 = %d, want 2", v2)
	}
	if got, err := repo.GetPublishedVersion(ctx, tenant, entryID); err != nil || got != 2 {
		t.Fatalf("published = %d, %v", got, err)
	}
	// 旧版本不能再发布。
	if err := repo.Publish(ctx, tenant, entryID, version, 42); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale publish err = %v, want ErrVersionConflict", err)
	}
	// 跨租户不可见。
	if _, err := repo.GetVersion(ctx, tenant+1, entryID, 1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("cross-tenant GetVersion err = %v, want ErrVersionNotFound", err)
	}
}

func TestOutboxClaimRetryDoneAndLease(t *testing.T) {
	repo, outbox, tenant := newKnowledgeTestData(t)
	ctx := context.Background()
	entryID, err := repo.CreateEntry(ctx, tenant, "outbox", "db")
	if err != nil {
		t.Fatal(err)
	}
	mustPublished(t, repo, tenant, entryID, "answer")

	jobs, err := outbox.Claim(ctx, 10, time.Second)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim = %v, %v", jobs, err)
	}
	if jobs[0].Attempts != 1 || jobs[0].EventType != "KNOWLEDGE_PUBLISHED" {
		t.Fatalf("job = %+v", jobs[0])
	}
	if jobs2, err := outbox.Claim(ctx, 10, time.Second); err != nil || len(jobs2) != 0 {
		t.Fatalf("second Claim before lease expiry = %v, %v", jobs2, err)
	}
	if pending, _, err := outbox.Stats(ctx); err != nil || pending != 1 {
		t.Fatalf("Stats = %d, %v", pending, err)
	}

	// 租约过期后必须能被重新领取。
	db := repo.db
	if _, err := db.ExecContext(ctx,
		"UPDATE cache_outbox SET locked_until=DATE_SUB(CURRENT_TIMESTAMP(3), INTERVAL 1 SECOND) WHERE id=?",
		jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	jobs, err = outbox.Claim(ctx, 10, time.Second)
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 2 {
		t.Fatalf("reclaim = %+v, %v", jobs, err)
	}

	// 失败退避：立即重领应为空。
	if status, err := outbox.MarkFailed(ctx, jobs[0].ID, jobs[0].ClaimToken, jobs[0].Attempts, 8, time.Second); err != nil || status != OutboxStatusPending {
		t.Fatalf("MarkFailed = %s, %v", status, err)
	}
	if jobs, err := outbox.Claim(ctx, 10, time.Second); err != nil || len(jobs) != 0 {
		t.Fatalf("claim during backoff = %v, %v", jobs, err)
	}
	// 到期后可重领并完成。
	if _, err := db.ExecContext(ctx,
		"UPDATE cache_outbox SET available_at=DATE_SUB(CURRENT_TIMESTAMP(3), INTERVAL 1 SECOND) WHERE id=?",
		jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	jobs, err = outbox.Claim(ctx, 10, time.Second)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim after backoff = %v, %v", jobs, err)
	}
	if err := outbox.MarkDone(ctx, jobs[0].ID, jobs[0].ClaimToken); err != nil {
		t.Fatal(err)
	}
	if pending, _, err := outbox.Stats(ctx); err != nil || pending != 0 {
		t.Fatalf("Stats after done = %d, %v", pending, err)
	}
}

func TestOutboxDeadAfterMaxAttempts(t *testing.T) {
	repo, outbox, tenant := newKnowledgeTestData(t)
	ctx := context.Background()
	entryID, err := repo.CreateEntry(ctx, tenant, "dead", "db")
	if err != nil {
		t.Fatal(err)
	}
	mustPublished(t, repo, tenant, entryID, "answer")
	jobs, err := outbox.Claim(ctx, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim = %v, %v", jobs, err)
	}
	status, err := outbox.MarkFailed(ctx, jobs[0].ID, jobs[0].ClaimToken, 8, 8, time.Second)
	if err != nil || status != OutboxStatusDead {
		t.Fatalf("MarkFailed max = %s, %v", status, err)
	}
}

func TestKnowledgeServiceCacheAside(t *testing.T) {
	repo, _, tenant := newKnowledgeTestData(t)
	ctx := context.Background()
	entryID, err := repo.CreateEntry(ctx, tenant, "cache", "db")
	if err != nil {
		t.Fatal(err)
	}
	v1 := mustPublished(t, repo, tenant, entryID, "answer-v1")
	v2 := mustPublished(t, repo, tenant, entryID, "answer-v2")

	store := newFakeKBStore()
	// 模拟迟到 Worker 只写了旧版本缓存：读取仍必须以 MySQL 最新版本为准。
	store.put(PublishedAnswerKey(entryID, v1), "answer-v1")
	svc := NewKnowledgeService(repo, store, time.Hour, nil)
	ans, ver, err := svc.PublishedAnswer(ctx, tenant, entryID)
	if err != nil || ans != "answer-v2" || ver != v2 {
		t.Fatalf("PublishedAnswer = %q v%d, %v; want answer-v2 v%d", ans, ver, err, v2)
	}
	if _, ok, _ := store.Get(ctx, PublishedAnswerKey(entryID, v2)); !ok {
		t.Fatal("v2 answer was not written to cache on miss")
	}
	// 第二次命中缓存。
	ans, ver, err = svc.PublishedAnswer(ctx, tenant, entryID)
	if err != nil || ans != "answer-v2" || ver != v2 {
		t.Fatalf("second PublishedAnswer = %q v%d, %v", ans, ver, err)
	}
	// 缓存故障不影响正确性。
	store.setErr = errors.New("pocketkv down")
	store.getErr = errors.New("pocketkv down")
	ans, _, err = NewKnowledgeService(repo, store, time.Hour, nil).PublishedAnswer(ctx, tenant, entryID)
	if err != nil || ans != "answer-v2" {
		t.Fatalf("cache-down PublishedAnswer = %q, %v", ans, err)
	}
}

func TestRetryReason(t *testing.T) {
	if reason, ok := retryReason(&mysql.MySQLError{Number: 1213}); !ok || reason != "deadlock" {
		t.Fatalf("1213 => %q %v", reason, ok)
	}
	if reason, ok := retryReason(&mysql.MySQLError{Number: 1205}); !ok || reason != "lock_timeout" {
		t.Fatalf("1205 => %q %v", reason, ok)
	}
	if _, ok := retryReason(errors.New("other")); ok {
		t.Fatal("non-deadlock error should not retry")
	}
}

type sequenceReader struct {
	versions []uint32
	idx      int
	answers  map[uint32]string
}

func (r *sequenceReader) GetPublishedVersion(context.Context, uint64, uint64) (uint32, error) {
	v := r.versions[len(r.versions)-1]
	if r.idx < len(r.versions) {
		v = r.versions[r.idx]
	}
	r.idx++
	return v, nil
}

func (r *sequenceReader) GetVersion(_ context.Context, _ uint64, _ uint64, version uint32) (*KnowledgeVersion, error) {
	return &KnowledgeVersion{Version: version, Answer: r.answers[version]}, nil
}

// 模拟 T1 读到 v1 并 cache hit 后、返回前 T2 已发布 v2：
// 服务端必须重读新版本，返回 (v2, answer-v2) 而不是 (v2, answer-v1)。
func TestPublishedAnswerCacheHitRechecksVersion(t *testing.T) {
	cache := newFakeKBStore()
	cache.put(PublishedAnswerKey(10, 1), "answer-v1")
	reader := &sequenceReader{
		versions: []uint32{1, 2, 2, 2},
		answers:  map[uint32]string{1: "answer-v1", 2: "answer-v2"},
	}
	svc := NewKnowledgeService(reader, cache, time.Hour, nil)
	ans, ver, err := svc.PublishedAnswer(context.Background(), 1, 10)
	if err != nil {
		t.Fatalf("PublishedAnswer: %v", err)
	}
	if ans != "answer-v2" || ver != 2 {
		t.Fatalf("got (%q,v%d), want (answer-v2,v2)", ans, ver)
	}
	if _, ok, _ := cache.Get(context.Background(), PublishedAnswerKey(10, 2)); !ok {
		t.Fatal("v2 answer was not cached after recheck")
	}
}

// P1-02：旧租约晚到的完成/失败不得覆盖新租约持有者。
func TestOutboxLeaseFencing(t *testing.T) {
	repo, outbox, tenant := newKnowledgeTestData(t)
	ctx := context.Background()
	entryID, err := repo.CreateEntry(ctx, tenant, "fencing", "db")
	if err != nil {
		t.Fatal(err)
	}
	mustPublished(t, repo, tenant, entryID, "answer")

	a, err := outbox.Claim(ctx, 1, time.Second)
	if err != nil || len(a) != 1 {
		t.Fatalf("claim A = %v, %v", a, err)
	}
	db := repo.db
	if _, err := db.ExecContext(ctx,
		"UPDATE cache_outbox SET locked_until=DATE_SUB(CURRENT_TIMESTAMP(3), INTERVAL 1 SECOND) WHERE id=?",
		a[0].ID); err != nil {
		t.Fatal(err)
	}
	b, err := outbox.Claim(ctx, 1, time.Minute)
	if err != nil || len(b) != 1 {
		t.Fatalf("claim B = %v, %v", b, err)
	}
	if a[0].ClaimToken == "" || b[0].ClaimToken == "" || a[0].ClaimToken == b[0].ClaimToken {
		t.Fatalf("expected distinct claim tokens: %q vs %q", a[0].ClaimToken, b[0].ClaimToken)
	}
	if err := outbox.MarkDone(ctx, a[0].ID, a[0].ClaimToken); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale MarkDone err = %v, want ErrLeaseLost", err)
	}
	if _, err := outbox.MarkFailed(ctx, a[0].ID, a[0].ClaimToken, 2, 8, time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale MarkFailed err = %v, want ErrLeaseLost", err)
	}
	if err := outbox.MarkDone(ctx, b[0].ID, b[0].ClaimToken); err != nil {
		t.Fatalf("current MarkDone: %v", err)
	}
	var status string
	if err := db.QueryRow("SELECT status FROM cache_outbox WHERE id=?", b[0].ID).Scan(&status); err != nil || status != OutboxStatusDone {
		t.Fatalf("status = %q, %v; want done", status, err)
	}
}
