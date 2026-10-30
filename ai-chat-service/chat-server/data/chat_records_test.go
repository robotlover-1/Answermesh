package data

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingObserver struct {
	enqueue   int64
	insert    int64
	dur       int64
	queue     int64
	persisted int64
	query     int64
	retry     int64
}

func (o *countingObserver) EnqueueFailure(string)          { atomic.AddInt64(&o.enqueue, 1) }
func (o *countingObserver) InsertFailure(string)           { atomic.AddInt64(&o.insert, 1) }
func (o *countingObserver) InsertDuration(float64)         { atomic.AddInt64(&o.dur, 1) }
func (o *countingObserver) QueueLength(n int)              { atomic.StoreInt64(&o.queue, int64(n)) }
func (o *countingObserver) RecordPersisted()               { atomic.AddInt64(&o.persisted, 1) }
func (o *countingObserver) DBQuery(_, _ string, _ float64) { atomic.AddInt64(&o.query, 1) }
func (o *countingObserver) TxRetry(string)                 { atomic.AddInt64(&o.retry, 1) }

func newTestRecords(t *testing.T, insert func(context.Context, *ChatRecord) (int64, error)) (*chatRecordsData, *countingObserver) {
	t.Helper()
	obs := &countingObserver{}
	d := NewChatRecordsData(nil, obs).(*chatRecordsData)
	if insert != nil {
		d.insert = insert
	} else {
		d.insert = func(context.Context, *ChatRecord) (int64, error) { return 1, nil }
	}
	return d, obs
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// 并发 Add 与 Close 不得触发 send on closed channel（配合 go test -race）。
func TestAddCloseConcurrentNoPanic(t *testing.T) {
	d, _ := newTestRecords(t, nil)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
				err := d.Add(ctx, &ChatRecord{UserMsg: "q"})
				cancel()
				if err != nil && !errors.Is(err, ErrRecordsClosed) && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("unexpected Add error: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	time.Sleep(2 * time.Millisecond) // 让部分 Add 与 Close 真正重叠
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	if err := d.Add(context.Background(), &ChatRecord{UserMsg: "late"}); !errors.Is(err, ErrRecordsClosed) {
		t.Fatalf("Add after Close = %v, want ErrRecordsClosed", err)
	}
}

// 队列满时阻塞的 Add（ctx 无 deadline）必须被 Close 立即唤醒，不能永久挂住。
// 不启动 worker，直接构造满队列，保证阻塞状态确定发生。
func TestCloseUnblocksBlockedAdd(t *testing.T) {
	d := &chatRecordsData{
		queue:   make(chan *ChatRecord, 1),
		obs:     nopRecordsObserver{},
		closing: make(chan struct{}),
	}
	d.queue <- &ChatRecord{UserMsg: "full"}

	addErr := make(chan error, 1)
	go func() {
		addErr <- d.Add(context.Background(), &ChatRecord{UserMsg: "blocked"})
	}()
	time.Sleep(20 * time.Millisecond) // 等生产者真正进入阻塞 select

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- d.Close(closeCtx) }()

	select {
	case err := <-addErr:
		if !errors.Is(err, ErrRecordsClosed) {
			t.Fatalf("blocked Add returned %v, want ErrRecordsClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock pending Add")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not finish")
	}
}

func TestEnqueueFailureRecorded(t *testing.T) {
	d, obs := newTestRecords(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Add(context.Background(), &ChatRecord{}); !errors.Is(err, ErrRecordsClosed) {
		t.Fatalf("Add after Close = %v", err)
	}
	if got := atomic.LoadInt64(&obs.enqueue); got != 1 {
		t.Fatalf("enqueue failures = %d, want 1", got)
	}
}

func TestWorkerInsertFailureRecorded(t *testing.T) {
	wantErr := errors.New("db down")
	d, obs := newTestRecords(t, func(context.Context, *ChatRecord) (int64, error) {
		return 0, wantErr
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Add(ctx, &ChatRecord{UserMsg: "q"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := atomic.LoadInt64(&obs.insert); got != 1 {
		t.Fatalf("db insert failures = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&obs.dur); got != 1 {
		t.Fatalf("insert duration observations = %d, want 1", got)
	}
}

func TestCloseDrainsQueuedRecords(t *testing.T) {
	const n = 64
	var inserted int64
	d, _ := newTestRecords(t, func(context.Context, *ChatRecord) (int64, error) {
		atomic.AddInt64(&inserted, 1)
		return 1, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < n; i++ {
		if err := d.Add(ctx, &ChatRecord{UserMsg: "q"}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := atomic.LoadInt64(&inserted); got != n {
		t.Fatalf("inserted = %d, want %d", got, n)
	}
}
