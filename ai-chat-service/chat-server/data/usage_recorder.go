package data

import (
	"context"
	"sync"
	"time"

	"ai-chat-service/pkg/log"
)

const (
	usageWorkers   = 2
	usageQueueSize = 2048
	usageInsertTO  = 5 * time.Second
)

// UsageStore 由 UsageRepository 实现；测试可注入 fake。
type UsageStore interface {
	Record(ctx context.Context, ev UsageEvent) (bool, error)
}

// UsageObserver 由 metrics-bus 实现。
type UsageObserver interface {
	UsageEnqueueFailure(reason string)
	UsageWriteFailure(reason string)
	UsageWritten(source string, created bool)
	UsageQueueLength(n int)
}

type nopUsageObserver struct{}

func (nopUsageObserver) UsageEnqueueFailure(string) {}
func (nopUsageObserver) UsageWriteFailure(string)   {}
func (nopUsageObserver) UsageWritten(string, bool)  {}
func (nopUsageObserver) UsageQueueLength(int)       {}

// UsageRecorder 用有界队列把用量写入移出请求路径；Close 时有界排空。
type UsageRecorder struct {
	store UsageStore
	obs   UsageObserver
	queue chan UsageEvent
	wg    sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	closing   chan struct{}
	senders   sync.WaitGroup
	closeOnce sync.Once
}

func NewUsageRecorder(store UsageStore, obs UsageObserver) *UsageRecorder {
	if obs == nil {
		obs = nopUsageObserver{}
	}
	u := &UsageRecorder{
		store:   store,
		obs:     obs,
		queue:   make(chan UsageEvent, usageQueueSize),
		closing: make(chan struct{}),
	}
	u.wg.Add(usageWorkers)
	for i := 0; i < usageWorkers; i++ {
		go u.worker()
	}
	return u
}

func (u *UsageRecorder) worker() {
	defer u.wg.Done()
	for ev := range u.queue {
		u.obs.UsageQueueLength(len(u.queue))
		ctx, cancel := context.WithTimeout(context.Background(), usageInsertTO)
		created, err := u.store.Record(ctx, ev)
		cancel()
		if err != nil {
			u.obs.UsageWriteFailure(insertFailureReason(err))
			log.ErrorF("usage record failed: %v", err)
			continue
		}
		u.obs.UsageWritten(ev.Source, created)
	}
}

// Add 仅入队；失败原因只使用固定枚举。
func (u *UsageRecorder) Add(ctx context.Context, ev UsageEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		u.obs.UsageEnqueueFailure("closed")
		return ErrRecordsClosed
	}
	u.senders.Add(1)
	u.mu.Unlock()
	defer u.senders.Done()

	select {
	case u.queue <- ev:
		u.obs.UsageQueueLength(len(u.queue))
		return nil
	case <-ctx.Done():
		u.obs.UsageEnqueueFailure(enqueueFailureReason(ctx.Err()))
		return ctx.Err()
	case <-u.closing:
		u.obs.UsageEnqueueFailure("closed")
		return ErrRecordsClosed
	}
}

func (u *UsageRecorder) Close(ctx context.Context) error {
	u.closeOnce.Do(func() {
		u.mu.Lock()
		u.closed = true
		close(u.closing)
		u.mu.Unlock()
		u.senders.Wait()
		close(u.queue)
	})
	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
