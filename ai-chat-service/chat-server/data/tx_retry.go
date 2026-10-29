package data

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
)

const (
	maxTxAttempts    = 4
	txRetryBaseDelay = 20 * time.Millisecond
)

// retryReason 把 MySQL 死锁/锁等待超时归为可重试类别；重试必须覆盖整个事务。
func retryReason(err error) (reason string, ok bool) {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return "", false
	}
	switch me.Number {
	case 1213:
		return "deadlock", true
	case 1205:
		return "lock_timeout", true
	default:
		return "", false
	}
}

type txRunner struct {
	db  *sql.DB
	obs DBObserver
}

func newTxRunner(db *sql.DB, obs DBObserver) *txRunner {
	return &txRunner{db: db, obs: DBOrNop(obs)}
}

func txResult(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func txBackoff(attempt int) time.Duration {
	d := txRetryBaseDelay << attempt
	// 简单 jitter：避免多个重试在同一时刻再次相撞。
	jitter := time.Duration(time.Now().UnixNano()%int64(d/2+1)) * time.Nanosecond
	return d + jitter
}

// run 在死锁/锁等待超时时重试完整事务（而不是最后一条 SQL）。
func (r *txRunner) run(ctx context.Context, operation string, fn func(*sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < maxTxAttempts; attempt++ {
		start := time.Now()
		err := r.runOnce(ctx, fn)
		r.obs.DBQuery(operation, txResult(err), time.Since(start).Seconds())
		if err == nil {
			return nil
		}
		reason, retryable := retryReason(err)
		if !retryable {
			return err
		}
		lastErr = err
		r.obs.TxRetry(reason)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(txBackoff(attempt)):
		}
	}
	return lastErr
}

func (r *txRunner) runOnce(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
