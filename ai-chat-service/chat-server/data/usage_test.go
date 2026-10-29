package data

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageRecordIdempotentAndDailyAggregate(t *testing.T) {
	db, tenant := setupDB(t)
	repo := NewUsageRepository(db, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	ev := UsageEvent{
		RequestID: fmt.Sprintf("usage-%d-1", tenant), TenantID: tenant, UserID: 7, SessionID: 0,
		Source: UsageSourceLLM, ModelFamily: "deepseek-v4-flash", Status: UsageStatusSuccess,
		TokenSource: UsageTokenSourceActual, InputTokens: 100, OutputTokens: 50,
		Latency: 900 * time.Millisecond, CreatedAt: now,
	}
	created, err := repo.Record(ctx, ev)
	if err != nil || !created {
		t.Fatalf("first Record = %v, %v; want created", created, err)
	}
	created, err = repo.Record(ctx, ev)
	if err != nil || created {
		t.Fatalf("duplicate Record = %v, %v; want idempotent", created, err)
	}

	// 缓存命中：只记 estimated_saved，token_source=none。
	cacheEv := ev
	cacheEv.RequestID = fmt.Sprintf("usage-%d-2", tenant)
	cacheEv.Source = UsageSourceCacheSemantic
	cacheEv.TokenSource = UsageTokenSourceNone
	cacheEv.InputTokens, cacheEv.OutputTokens = 0, 0
	cacheEv.EstimatedSavedTokens = 300
	if created, err = repo.Record(ctx, cacheEv); err != nil || !created {
		t.Fatalf("cache Record = %v, %v", created, err)
	}

	rows, err := repo.DailyReport(ctx, tenant, now.Add(-24*time.Hour), now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("daily rows = %+v, want 2", rows)
	}
	var llm, cache *UsageDailyRow
	for i := range rows {
		switch rows[i].Source {
		case UsageSourceLLM:
			llm = &rows[i]
		case UsageSourceCacheSemantic:
			cache = &rows[i]
		}
	}
	if llm == nil || llm.Requests != 1 || llm.InputTokens != 100 || llm.OutputTokens != 50 {
		t.Fatalf("llm daily = %+v", llm)
	}
	if cache == nil || cache.Requests != 1 || cache.EstimatedSavedTokens != 300 || cache.InputTokens != 0 {
		t.Fatalf("cache daily = %+v", cache)
	}

	var tokenSource string
	if err := db.QueryRow("SELECT token_source FROM llm_usage_events WHERE request_id=?", fmt.Sprintf("usage-%d-1", tenant)).Scan(&tokenSource); err != nil {
		t.Fatal(err)
	}
	if tokenSource != UsageTokenSourceActual {
		t.Fatalf("token_source = %q", tokenSource)
	}
}

func TestUsageRecordConcurrentSameRequest(t *testing.T) {
	db, tenant := setupDB(t)
	repo := NewUsageRepository(db, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	ev := UsageEvent{
		RequestID: fmt.Sprintf("usage-dup-%d", tenant), TenantID: tenant, UserID: 1,
		Source: UsageSourceLLM, ModelFamily: "m", Status: UsageStatusSuccess,
		TokenSource: UsageTokenSourceActual, InputTokens: 10, OutputTokens: 20, CreatedAt: now,
	}
	var createdCount int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repo.Record(ctx, ev)
			if err != nil {
				t.Errorf("Record: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&createdCount, 1)
			}
		}()
	}
	wg.Wait()
	if createdCount != 1 {
		t.Fatalf("created = %d, want exactly 1", createdCount)
	}
	rows, err := repo.DailyReport(ctx, tenant, now.Add(-24*time.Hour), now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("daily after duplicate = %+v", rows)
	}
}

type fakeUsageStore struct {
	mu     sync.Mutex
	seen   map[string]bool
	events []UsageEvent
	fail   bool
}

func (f *fakeUsageStore) Record(_ context.Context, ev UsageEvent) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return false, errors.New("db down")
	}
	created := !f.seen[ev.RequestID]
	f.seen[ev.RequestID] = true
	if created {
		f.events = append(f.events, ev)
	}
	return created, nil
}

func (f *fakeUsageStore) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

type countingUsageObserver struct {
	enqueue int64
	write   int64
	written int64
	queue   int64
}

func (o *countingUsageObserver) UsageEnqueueFailure(string) { atomic.AddInt64(&o.enqueue, 1) }
func (o *countingUsageObserver) UsageWriteFailure(string)   { atomic.AddInt64(&o.write, 1) }
func (o *countingUsageObserver) UsageWritten(string, bool)  { atomic.AddInt64(&o.written, 1) }
func (o *countingUsageObserver) UsageQueueLength(n int)     { atomic.StoreInt64(&o.queue, int64(n)) }

func TestUsageRecorderDrainsAndCloses(t *testing.T) {
	store := &fakeUsageStore{seen: map[string]bool{}}
	obs := &countingUsageObserver{}
	rec := NewUsageRecorder(store, obs)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if err := rec.Add(ctx, UsageEvent{RequestID: string(rune('a' + i%26))}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rec.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := store.len(); got != 26 { // 26 个不同 request_id
		t.Fatalf("stored = %d, want 26", got)
	}
	if atomic.LoadInt64(&obs.written) != 100 {
		t.Fatalf("written metric = %d, want 100", atomic.LoadInt64(&obs.written))
	}
	if err := rec.Add(ctx, UsageEvent{RequestID: "late"}); !errors.Is(err, ErrRecordsClosed) {
		t.Fatalf("Add after Close = %v", err)
	}
	if atomic.LoadInt64(&obs.enqueue) != 1 {
		t.Fatalf("enqueue failures = %d", atomic.LoadInt64(&obs.enqueue))
	}
}
