// Package worker 运行 cache_outbox 的可靠投递循环（M-03）。
package worker

import (
	"context"
	"errors"
	"time"

	"ai-chat-service/chat-server/data"
	"ai-chat-service/pkg/log"
)

// OutboxMetrics 由 metrics-bus 实现；nil 时为空操作。
type OutboxMetrics interface {
	OutboxStats(pending int, oldestSeconds float64)
	OutboxEvent(result string)
}

type nopMetrics struct{}

func (nopMetrics) OutboxStats(int, float64) {}
func (nopMetrics) OutboxEvent(string)       {}

type OutboxWorker struct {
	repo      *data.OutboxRepository
	knowledge *data.KnowledgeRepository
	cache     data.KnowledgeCache
	metrics   OutboxMetrics

	interval    time.Duration
	lease       time.Duration
	batchSize   int
	maxAttempts int
	cacheTTL    time.Duration
	backoff     time.Duration
}

type OutboxConfig struct {
	Interval    time.Duration
	Lease       time.Duration
	BatchSize   int
	MaxAttempts int
	CacheTTL    time.Duration
	BaseBackoff time.Duration
}

func NewOutboxWorker(repo *data.OutboxRepository, knowledge *data.KnowledgeRepository,
	cache data.KnowledgeCache, metrics OutboxMetrics, cfg OutboxConfig) *OutboxWorker {
	if metrics == nil {
		metrics = nopMetrics{}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 60 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 24 * time.Hour
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 10 * time.Second
	}
	return &OutboxWorker{
		repo: repo, knowledge: knowledge, cache: cache, metrics: metrics,
		interval: cfg.Interval, lease: cfg.Lease, batchSize: cfg.BatchSize,
		maxAttempts: cfg.MaxAttempts, cacheTTL: cfg.CacheTTL, backoff: cfg.BaseBackoff,
	}
}

// Run 阻塞直到 ctx 取消；每个周期顺序处理一个有界批次。
func (w *OutboxWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *OutboxWorker) runOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	jobs, err := w.repo.Claim(ctx, w.batchSize, w.lease)
	if err != nil {
		if ctx.Err() == nil {
			log.ErrorF("outbox claim failed: %v", err)
		}
	} else {
		for _, job := range jobs {
			w.process(ctx, job)
		}
	}
	if pending, oldest, err := w.repo.Stats(ctx); err == nil {
		w.metrics.OutboxStats(int(pending), oldest.Seconds())
	} else if ctx.Err() == nil {
		log.ErrorF("outbox stats failed: %v", err)
	}
}

func (w *OutboxWorker) process(ctx context.Context, job data.OutboxJob) {
	if job.EventType == "KNOWLEDGE_REVOKED" {
		// 撤稿的正确性由 MySQL published_version=0 保证；缓存只是可丢弃副本。
		if err := w.repo.MarkDone(ctx, job.ID, job.ClaimToken); err != nil {
			if errors.Is(err, data.ErrLeaseLost) {
				w.noteLeaseError(job, err)
				return
			}
			log.ErrorF("outbox mark revoked done failed id=%d: %v", job.ID, err)
			w.metrics.OutboxEvent("error")
			return
		}
		w.metrics.OutboxEvent("revoked")
		return
	}
	if job.EventType != "KNOWLEDGE_PUBLISHED" {
		// 未知事件类型不可重试，直接进入 dead 以便告警/人工处理。
		if _, err := w.repo.MarkFailed(ctx, job.ID, job.ClaimToken, w.maxAttempts, w.maxAttempts, w.backoff); err != nil {
			w.noteLeaseError(job, err)
			return
		}
		w.metrics.OutboxEvent("dead")
		return
	}
	kv, err := w.knowledge.GetVersionForWorker(ctx, job.EntryID, job.Version)
	if err != nil {
		if errors.Is(err, data.ErrVersionNotFound) {
			// 版本不存在属于永久错误，重试无意义。
			if _, err := w.repo.MarkFailed(ctx, job.ID, job.ClaimToken, w.maxAttempts, w.maxAttempts, w.backoff); err != nil {
				w.noteLeaseError(job, err)
				return
			}
			w.metrics.OutboxEvent("dead")
			log.ErrorF("outbox dead: knowledge version missing entry=%d version=%d", job.EntryID, job.Version)
			return
		}
		w.retry(ctx, job, err)
		return
	}
	if w.cache != nil {
		key := data.PublishedAnswerKey(job.EntryID, job.Version)
		if err := w.cache.SetImmutable(ctx, key, kv.Answer, w.cacheTTL); err != nil {
			w.retry(ctx, job, err)
			return
		}
	}
	if err := w.repo.MarkDone(ctx, job.ID, job.ClaimToken); err != nil {
		if errors.Is(err, data.ErrLeaseLost) {
			w.noteLeaseError(job, err)
			return
		}
		log.ErrorF("outbox mark done failed id=%d: %v", job.ID, err)
		w.metrics.OutboxEvent("error")
		return
	}
	w.metrics.OutboxEvent("done")
}

func (w *OutboxWorker) retry(ctx context.Context, job data.OutboxJob, cause error) {
	status, err := w.repo.MarkFailed(ctx, job.ID, job.ClaimToken, job.Attempts, w.maxAttempts, w.backoff)
	if errors.Is(err, data.ErrLeaseLost) {
		w.noteLeaseError(job, err)
		return
	}
	if err != nil {
		log.ErrorF("outbox mark failed id=%d: %v", job.ID, err)
		w.metrics.OutboxEvent("error")
		return
	}
	if status == data.OutboxStatusDead {
		w.metrics.OutboxEvent("dead")
		log.ErrorF("outbox dead id=%d attempts=%d: %v", job.ID, job.Attempts, cause)
		return
	}
	w.metrics.OutboxEvent("retry")
}

// noteLeaseError：旧租约晚到的完成/失败不得改写新持有者的状态。
func (w *OutboxWorker) noteLeaseError(job data.OutboxJob, err error) {
	w.metrics.OutboxEvent("lease_lost")
	log.InfoF("outbox lease lost id=%d attempts=%d: %v", job.ID, job.Attempts, err)
}
