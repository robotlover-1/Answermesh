package data

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSessionStore struct {
	mu    sync.Mutex
	jobs  []SessionAppendJob
	byKey map[string]bool
	fail  bool
}

func (f *fakeSessionStore) AppendConversation(_ context.Context, tenantID, userID, sessionID uint64,
	in AppendInput) (uint64, uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, 0, false, errors.New("db down")
	}
	key := in.RequestID
	created := !f.byKey[key]
	f.byKey[key] = true
	f.jobs = append(f.jobs, SessionAppendJob{TenantID: tenantID, UserID: userID, SessionID: sessionID, Input: in})
	return 1, 2, created, nil
}

type countingSessionObserver struct {
	enqueue int64
	write   int64
	appends int64
	queue   int64
}

func (o *countingSessionObserver) SessionEnqueueFailure(string) { atomic.AddInt64(&o.enqueue, 1) }
func (o *countingSessionObserver) SessionWriteFailure(string)   { atomic.AddInt64(&o.write, 1) }
func (o *countingSessionObserver) SessionAppended(bool)         { atomic.AddInt64(&o.appends, 1) }
func (o *countingSessionObserver) SessionQueueLength(n int)     { atomic.StoreInt64(&o.queue, int64(n)) }

func TestSessionRecorderDrainsAndCloses(t *testing.T) {
	store := &fakeSessionStore{byKey: map[string]bool{}}
	obs := &countingSessionObserver{}
	rec := NewSessionRecorder(store, obs)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if err := rec.Add(ctx, SessionAppendJob{TenantID: 1, UserID: 1, SessionID: 1, Input: AppendInput{
			RequestID: "r" + string(rune('a'+i%26)), UserContent: "q", AssistantContent: "a",
		}}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rec.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if atomic.LoadInt64(&obs.appends) != 100 {
		t.Fatalf("appends metric = %d, want 100", atomic.LoadInt64(&obs.appends))
	}
	if err := rec.Add(ctx, SessionAppendJob{}); !errors.Is(err, ErrRecordsClosed) {
		t.Fatalf("Add after Close = %v", err)
	}
	if atomic.LoadInt64(&obs.enqueue) != 1 {
		t.Fatalf("enqueue failures = %d", atomic.LoadInt64(&obs.enqueue))
	}
}

func TestSessionRecorderWritesHistory(t *testing.T) {
	db, tenant := setupDB(t)
	repo := NewSessionRepository(db, nil)
	ctx := context.Background()
	sessionID, err := repo.CreateSession(ctx, tenant, 7, "async")
	if err != nil {
		t.Fatal(err)
	}
	rec := NewSessionRecorder(repo, nil)
	if err := rec.Add(ctx, SessionAppendJob{
		TenantID: tenant, UserID: 7, SessionID: sessionID,
		Input: AppendInput{RequestID: "async-req-1", UserContent: "q", AssistantContent: "a", Source: "llm"},
	}); err != nil {
		t.Fatal(err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rec.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	msgs, err := repo.ListMessages(ctx, tenant, 7, sessionID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
}
