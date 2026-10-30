package data

import (
	"context"
	"sync"
	"time"

	"ai-chat-service/pkg/log"
)

const (
	sessionWorkers   = 2
	sessionQueueSize = 1024
	sessionAppendTO  = 5 * time.Second
)

// SessionAppendStore 由 SessionRepository 实现；测试可注入 fake。
type SessionAppendStore interface {
	AppendConversation(ctx context.Context, tenantID, userID, sessionID uint64,
		in AppendInput) (uint64, uint64, bool, error)
}

type SessionAppendJob struct {
	TenantID  uint64
	UserID    uint64
	SessionID uint64
	Input     AppendInput
}

type SessionObserver interface {
	SessionEnqueueFailure(reason string)
	SessionWriteFailure(reason string)
	SessionAppended(created bool)
	SessionQueueLength(n int)
}

type nopSessionObserver struct{}

func (nopSessionObserver) SessionEnqueueFailure(string) {}
func (nopSessionObserver) SessionWriteFailure(string)   {}
func (nopSessionObserver) SessionAppended(bool)         {}
func (nopSessionObserver) SessionQueueLength(int)       {}

// SessionRecorder 把“写最终 user+assistant 消息”移出聊天响应路径；
// 队列有界、Close 有界排空。SIGKILL/断电仍可能丢最后一小段内存队列（与 chat_records 同口径）。
type SessionRecorder struct {
	store SessionAppendStore
	obs   SessionObserver
	queue chan SessionAppendJob
	wg    sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	closing   chan struct{}
	senders   sync.WaitGroup
	closeOnce sync.Once
}

func NewSessionRecorder(store SessionAppendStore, obs SessionObserver) *SessionRecorder {
	if obs == nil {
		obs = nopSessionObserver{}
	}
	r := &SessionRecorder{
		store:   store,
		obs:     obs,
		queue:   make(chan SessionAppendJob, sessionQueueSize),
		closing: make(chan struct{}),
	}
	r.wg.Add(sessionWorkers)
	for i := 0; i < sessionWorkers; i++ {
		go r.worker()
	}
	return r
}

func (r *SessionRecorder) worker() {
	defer r.wg.Done()
	for job := range r.queue {
		r.obs.SessionQueueLength(len(r.queue))
		ctx, cancel := context.WithTimeout(context.Background(), sessionAppendTO)
		_, _, created, err := r.store.AppendConversation(ctx,
			job.TenantID, job.UserID, job.SessionID, job.Input)
		cancel()
		if err != nil {
			r.obs.SessionWriteFailure(insertFailureReason(err))
			log.ErrorF("session append failed: %v", err)
			continue
		}
		r.obs.SessionAppended(created)
	}
}

func (r *SessionRecorder) Add(ctx context.Context, job SessionAppendJob) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.obs.SessionEnqueueFailure("closed")
		return ErrRecordsClosed
	}
	r.senders.Add(1)
	r.mu.Unlock()
	defer r.senders.Done()

	select {
	case r.queue <- job:
		r.obs.SessionQueueLength(len(r.queue))
		return nil
	case <-ctx.Done():
		r.obs.SessionEnqueueFailure(enqueueFailureReason(ctx.Err()))
		return ctx.Err()
	case <-r.closing:
		r.obs.SessionEnqueueFailure("closed")
		return ErrRecordsClosed
	}
}

func (r *SessionRecorder) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.closing)
		r.mu.Unlock()
		r.senders.Wait()
		close(r.queue)
	})
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
