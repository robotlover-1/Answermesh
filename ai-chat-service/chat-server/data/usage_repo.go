package data

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// M-04：LLM 用量流水。request_id 唯一保证幂等；只有真正插入成功（非重复）时才累加日汇总，
// 从而重放同一事件不会重复计数。真实 tokens 与缓存节省估算分开存储。

const (
	UsageSourceLLM           = "llm"
	UsageSourceCacheExact    = "cache_exact"
	UsageSourceCacheSemantic = "cache_semantic"
	UsageSourceBlocked       = "blocked"
	UsageSourceKnowledge     = "knowledge"
	UsageSourceUnknown       = "unknown"

	UsageStatusSuccess   = "success"
	UsageStatusFailed    = "failed"
	UsageStatusCancelled = "cancelled"
	UsageStatusUnknown   = "unknown"

	UsageTokenSourceActual    = "actual"
	UsageTokenSourceTokenizer = "tokenizer"
	UsageTokenSourceNone      = "none"
)

type UsageEvent struct {
	RequestID            string
	TenantID             uint64
	UserID               uint64
	SessionID            uint64
	Source               string
	ModelFamily          string
	Status               string
	TokenSource          string
	InputTokens          int
	OutputTokens         int
	EstimatedSavedTokens int
	CostMicrounits       int64
	Latency              time.Duration
	CreatedAt            time.Time
}

func (e *UsageEvent) normalize() error {
	if e.RequestID == "" || len(e.RequestID) > 64 {
		return fmt.Errorf("%w: request_id must be 1..64 chars", ErrInvalidArgument)
	}
	if e.TenantID == 0 || e.UserID == 0 {
		return fmt.Errorf("%w: tenant/user required", ErrInvalidArgument)
	}
	switch e.Source {
	case UsageSourceLLM, UsageSourceCacheExact, UsageSourceCacheSemantic, UsageSourceBlocked, UsageSourceKnowledge, UsageSourceUnknown:
	default:
		e.Source = UsageSourceUnknown
	}
	switch e.Status {
	case UsageStatusSuccess, UsageStatusFailed, UsageStatusCancelled, UsageStatusUnknown:
	default:
		e.Status = UsageStatusUnknown
	}
	switch e.TokenSource {
	case UsageTokenSourceActual, UsageTokenSourceTokenizer, UsageTokenSourceNone:
	default:
		e.TokenSource = UsageTokenSourceNone
	}
	if e.ModelFamily == "" {
		e.ModelFamily = "unknown"
	}
	if len(e.ModelFamily) > 64 {
		e.ModelFamily = e.ModelFamily[:64]
	}
	if e.InputTokens < 0 {
		e.InputTokens = 0
	}
	if e.OutputTokens < 0 {
		e.OutputTokens = 0
	}
	if e.EstimatedSavedTokens < 0 {
		e.EstimatedSavedTokens = 0
	}
	if e.Latency < 0 {
		e.Latency = 0
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	return nil
}

type UsageRepository struct {
	db *sql.DB
	tx *txRunner
}

func NewUsageRepository(db *sql.DB, obs DBObserver) *UsageRepository {
	return &UsageRepository{db: db, tx: newTxRunner(db, obs)}
}

// Record 幂等写入一条用量事件；created=false 表示 request_id 已存在（重放）。
func (r *UsageRepository) Record(ctx context.Context, ev UsageEvent) (created bool, err error) {
	if err := ev.normalize(); err != nil {
		return false, err
	}
	createdAt := ev.CreatedAt.UTC()
	day := createdAt.Format("2006-01-02")
	actualTokens := 0
	if ev.Source == UsageSourceLLM && ev.TokenSource == UsageTokenSourceActual {
		actualTokens = ev.InputTokens + ev.OutputTokens
	}
	tokenizerTokens := 0
	if ev.TokenSource == UsageTokenSourceTokenizer {
		tokenizerTokens = ev.InputTokens + ev.OutputTokens
	}
	var sessionID any
	if ev.SessionID != 0 {
		sessionID = ev.SessionID
	}
	err = r.tx.run(ctx, "record_usage", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
INSERT INTO llm_usage_events(
  request_id, tenant_id, user_id, session_id, source, model_family, status, token_source,
  input_tokens, output_tokens, estimated_saved_tokens, cost_microunits, latency_ms, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`,
			ev.RequestID, ev.TenantID, ev.UserID, sessionID, ev.Source, ev.ModelFamily,
			ev.Status, ev.TokenSource, ev.InputTokens, ev.OutputTokens,
			ev.EstimatedSavedTokens, ev.CostMicrounits, ev.Latency.Milliseconds(), createdAt)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = affected == 1
		if !created {
			return nil // 重复请求：不重复累加日汇总
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO usage_daily(tenant_id, day, source, model_family, requests,
  input_tokens, output_tokens, estimated_saved_tokens, cost_microunits,
  actual_tokens, tokenizer_tokens)
VALUES(?,?,?,?,1,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE
  requests=requests+1,
  input_tokens=input_tokens+VALUES(input_tokens),
  output_tokens=output_tokens+VALUES(output_tokens),
  estimated_saved_tokens=estimated_saved_tokens+VALUES(estimated_saved_tokens),
  cost_microunits=cost_microunits+VALUES(cost_microunits),
  actual_tokens=actual_tokens+VALUES(actual_tokens),
  tokenizer_tokens=tokenizer_tokens+VALUES(tokenizer_tokens)`,
			ev.TenantID, day, ev.Source, ev.ModelFamily,
			ev.InputTokens, ev.OutputTokens, ev.EstimatedSavedTokens, ev.CostMicrounits,
			actualTokens, tokenizerTokens)
		return err
	})
	return created, err
}

// DailyReport 供测试/Grafana 之外的轻量核对使用（按租户+日期区间）。
type UsageDailyRow struct {
	Day                  string
	Source               string
	Requests             int64
	InputTokens          int64
	OutputTokens         int64
	EstimatedSavedTokens int64
}

func (r *UsageRepository) DailyReport(ctx context.Context, tenantID uint64, from, to time.Time) ([]UsageDailyRow, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT DATE_FORMAT(day,'%Y-%m-%d'), source, requests, input_tokens, output_tokens, estimated_saved_tokens
FROM usage_daily
WHERE tenant_id=? AND day>=? AND day<=?
ORDER BY day, source`, tenantID, from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageDailyRow
	for rows.Next() {
		var row UsageDailyRow
		if err := rows.Scan(&row.Day, &row.Source, &row.Requests, &row.InputTokens,
			&row.OutputTokens, &row.EstimatedSavedTokens); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
