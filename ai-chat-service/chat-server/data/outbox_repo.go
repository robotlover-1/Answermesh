package data

import (
	"context"
	"database/sql"
	"time"
)

// cache_outbox 可靠任务（M-03）。claim 使用 FOR UPDATE SKIP LOCKED（MySQL 8.0+），
// 租约到期由下一次 claim 前的回收 UPDATE 重新接管。

const (
	OutboxStatusPending    = "pending"
	OutboxStatusProcessing = "processing"
	OutboxStatusDone       = "done"
	OutboxStatusDead       = "dead"
)

type OutboxJob struct {
	ID        uint64
	EntryID   uint64
	Version   uint32
	EventType string
	Attempts  int
}

type OutboxRepository struct {
	db  *sql.DB
	obs DBObserver
}

func NewOutboxRepository(db *sql.DB, obs DBObserver) *OutboxRepository {
	return &OutboxRepository{db: db, obs: DBOrNop(obs)}
}

// Claim 在一个事务内回收过期租约并领取最多 limit 个任务，标记为 processing。
func (r *OutboxRepository) Claim(ctx context.Context, limit int, lease time.Duration) ([]OutboxJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	start := time.Now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.obs.DBQuery("claim_outbox", "error", time.Since(start).Seconds())
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 租约过期：processing 但 locked_until 已过 → 回到 pending 重新排队。
	if _, err := tx.ExecContext(ctx, `
UPDATE cache_outbox SET status='pending', locked_until=NULL
WHERE status='processing' AND (locked_until IS NULL OR locked_until < CURRENT_TIMESTAMP(3))`); err != nil {
		r.obs.DBQuery("claim_outbox", "error", time.Since(start).Seconds())
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, entry_id, version, event_type, attempts
FROM cache_outbox
WHERE status IN ('pending','failed')
  AND available_at <= CURRENT_TIMESTAMP(3)
  AND (locked_until IS NULL OR locked_until < CURRENT_TIMESTAMP(3))
ORDER BY id
LIMIT ?
FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		r.obs.DBQuery("claim_outbox", "error", time.Since(start).Seconds())
		return nil, err
	}
	var jobs []OutboxJob
	for rows.Next() {
		var j OutboxJob
		if err := rows.Scan(&j.ID, &j.EntryID, &j.Version, &j.EventType, &j.Attempts); err != nil {
			rows.Close()
			r.obs.DBQuery("claim_outbox", "error", time.Since(start).Seconds())
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(jobs) > 0 {
		args := make([]any, 0, len(jobs)+1)
		args = append(args, int(lease.Seconds())) // 对应 SQL 中第一个 ?
		placeholders := ""
		for i, j := range jobs {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, j.ID)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE cache_outbox
SET status='processing', attempts=attempts+1,
    locked_until=DATE_ADD(CURRENT_TIMESTAMP(3), INTERVAL ? SECOND)
WHERE id IN (`+placeholders+`)`, args...); err != nil {
			r.obs.DBQuery("claim_outbox", "error", time.Since(start).Seconds())
			return nil, err
		}
		for i := range jobs {
			jobs[i].Attempts++ // 与数据库落库后的 attempts 对齐
		}
	}
	if err := tx.Commit(); err != nil {
		r.obs.DBQuery("claim_outbox", "error", time.Since(start).Seconds())
		return nil, err
	}
	r.obs.DBQuery("claim_outbox", "ok", time.Since(start).Seconds())
	return jobs, nil
}

func (r *OutboxRepository) MarkDone(ctx context.Context, id uint64) error {
	start := time.Now()
	_, err := r.db.ExecContext(ctx, `
UPDATE cache_outbox SET status='done', locked_until=NULL, processed_at=CURRENT_TIMESTAMP(3)
WHERE id=?`, id)
	r.obs.DBQuery("ack_outbox", txResult(err), time.Since(start).Seconds())
	return err
}

// MarkFailed：未超过 maxAttempts 时回到 pending 并按指数退避设置 available_at；
// 超过则进入 dead，等待人工/告警处理。
func (r *OutboxRepository) MarkFailed(ctx context.Context, id uint64, attempts, maxAttempts int,
	baseBackoff time.Duration) (string, error) {
	status := OutboxStatusPending
	if attempts >= maxAttempts {
		status = OutboxStatusDead
	}
	backoffSec := int(baseBackoff.Seconds())
	if backoffSec < 1 {
		backoffSec = 1
	}
	if shifts := attempts - 1; shifts > 0 && shifts < 8 {
		backoffSec <<= shifts
	}
	if backoffSec > 3600 {
		backoffSec = 3600
	}
	start := time.Now()
	_, err := r.db.ExecContext(ctx, `
UPDATE cache_outbox
SET status=?, locked_until=NULL,
    available_at=DATE_ADD(CURRENT_TIMESTAMP(3), INTERVAL ? SECOND)
WHERE id=?`, status, backoffSec, id)
	r.obs.DBQuery("retry_outbox", txResult(err), time.Since(start).Seconds())
	return status, err
}

// Stats 使用 idx_dispatch/idx_status_created 的轻量聚合，供 30–60s 周期更新 Gauge。
func (r *OutboxRepository) Stats(ctx context.Context) (pending int64, oldest time.Duration, err error) {
	var oldestSec sql.NullFloat64
	err = r.db.QueryRowContext(ctx, `
SELECT COUNT(*), TIMESTAMPDIFF(SECOND, MIN(created_at), CURRENT_TIMESTAMP(3))
FROM cache_outbox
WHERE status IN ('pending','processing','failed')`).Scan(&pending, &oldestSec)
	if err != nil {
		return 0, 0, err
	}
	if oldestSec.Valid && oldestSec.Float64 > 0 {
		oldest = time.Duration(oldestSec.Float64) * time.Second
	}
	return pending, oldest, nil
}

// PendingEventKeys 供测试/排障查看事件去重键。
func (r *OutboxRepository) PendingEventKeys(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT event_key FROM cache_outbox WHERE status <> 'done' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
