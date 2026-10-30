package data

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 会话/消息仓库（M-01）。tenant_id/user_id 必须由服务端鉴权后传入，
// 不能直接使用客户端上报的值；所有查询都带租户/用户过滤。
var (
	ErrSessionNotFound = errors.New("chat session not found")
	ErrInvalidArgument = errors.New("invalid session/message argument")
	// ErrIdempotencyConflict 表示唯一键冲突命中了不属于当前会话/请求的记录；
	// 正常索引下不应出现，作为防御性校验避免静默复用他人记录。
	ErrIdempotencyConflict = errors.New("message idempotency scope conflict")
	// ErrRequestPayloadMismatch 表示同一 (session,request,role) 重试携带了不同内容。
	ErrRequestPayloadMismatch = errors.New("request_id reused with different message content")
)

const (
	defaultPageLimit = 20
	maxPageLimit     = 100
)

type Session struct {
	ID        uint64
	TenantID  uint64
	UserID    uint64
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Message struct {
	ID         uint64
	SessionID  uint64
	RequestID  string
	Role       string
	Content    string
	TokenCount int
	Source     string
	CreatedAt  time.Time
}

// AppendInput 是一次请求的最终落库内容（一条 user + 一条最终 assistant）。
type AppendInput struct {
	RequestID        string
	UserContent      string
	UserTokens       int
	AssistantContent string
	AssistantTokens  int
	Source           string
}

type SessionRepository struct {
	db  *sql.DB
	obs DBObserver
}

func NewSessionRepository(db *sql.DB, obs DBObserver) *SessionRepository {
	return &SessionRepository{db: db, obs: DBOrNop(obs)}
}

func (r *SessionRepository) CreateSession(ctx context.Context, tenantID, userID uint64, title string) (uint64, error) {
	if tenantID == 0 || userID == 0 {
		return 0, fmt.Errorf("%w: tenant_id/user_id required", ErrInvalidArgument)
	}
	if len(title) > 255 {
		title = title[:255]
	}
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO chat_sessions(tenant_id, user_id, title) VALUES(?,?,?)",
		tenantID, userID, title)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last_insert_id: %w", err)
	}
	return uint64(id), nil
}

// GetSession 校验租户/用户归属；不存在或已软删返回 ErrSessionNotFound。
func (r *SessionRepository) GetSession(ctx context.Context, tenantID, userID, sessionID uint64) (*Session, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, tenant_id, user_id, title, created_at, updated_at
FROM chat_sessions
WHERE id=? AND tenant_id=? AND user_id=? AND deleted_at IS NULL`,
		sessionID, tenantID, userID)
	s := &Session{}
	if err := row.Scan(&s.ID, &s.TenantID, &s.UserID, &s.Title, &s.CreatedAt, &s.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	return s, nil
}

// ListSessions 按 id 降序做游标分页；beforeID=0 表示第一页。
// 使用 idx_user_cursor(tenant_id,user_id,id)，不依赖 OFFSET。
func (r *SessionRepository) ListSessions(ctx context.Context, tenantID, userID, beforeID uint64, limit int) ([]Session, error) {
	limit = clampLimit(limit)
	start := time.Now()
	rows, err := r.db.QueryContext(ctx, `
SELECT id, tenant_id, user_id, title, created_at, updated_at
FROM chat_sessions FORCE INDEX (idx_user_cursor)
WHERE tenant_id=? AND user_id=? AND deleted_at IS NULL
  AND (?=0 OR id<?)
ORDER BY id DESC
LIMIT ?`,
		tenantID, userID, beforeID, beforeID, limit)
	r.obs.DBQuery("list_sessions", txResult(err), time.Since(start).Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Session, 0, limit)
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.TenantID, &s.UserID, &s.Title, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SoftDeleteSession 只允许操作本租户/用户自己的会话。
func (r *SessionRepository) SoftDeleteSession(ctx context.Context, tenantID, userID, sessionID uint64) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
UPDATE chat_sessions SET deleted_at=CURRENT_TIMESTAMP(3)
WHERE id=? AND tenant_id=? AND user_id=? AND deleted_at IS NULL`,
		sessionID, tenantID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListMessages 使用 idx_session_messages(session_id,id) 做游标分页；
// beforeID=0 表示第一页。返回按 id 降序（最新在前）。
func (r *SessionRepository) ListMessages(ctx context.Context, tenantID, userID, sessionID, beforeID uint64, limit int) ([]Message, error) {
	limit = clampLimit(limit)
	start := time.Now()
	rows, err := r.db.QueryContext(ctx, `
SELECT m.id, m.session_id, m.request_id, m.role, m.content, m.token_count, m.source, m.created_at
FROM chat_messages m FORCE INDEX (idx_session_messages)
JOIN chat_sessions s ON s.id = m.session_id
WHERE m.session_id=? AND s.tenant_id=? AND s.user_id=? AND s.deleted_at IS NULL
  AND (?=0 OR m.id<?)
ORDER BY m.id DESC
LIMIT ?`,
		sessionID, tenantID, userID, beforeID, beforeID, limit)
	r.obs.DBQuery("list_messages", txResult(err), time.Since(start).Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.RequestID, &m.Role, &m.Content,
			&m.TokenCount, &m.Source, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AppendConversation 在一个事务里写 user + assistant 两条最终消息，并刷新会话 updated_at。
// 幂等：uk_session_request_role(session_id, request_id, role) + ON DUPLICATE KEY 保留原 id；
// 重复请求返回同一组 id，不同会话复用同一 request_id 互不影响。
// 注意：不要在持有该事务时调用 LLM / PocketKV / HTTP。
func (r *SessionRepository) AppendConversation(ctx context.Context, tenantID, userID, sessionID uint64, in AppendInput) (userMsgID, assistantMsgID uint64, created bool, err error) {
	if tenantID == 0 || userID == 0 || sessionID == 0 {
		return 0, 0, false, fmt.Errorf("%w: tenant/user/session required", ErrInvalidArgument)
	}
	if in.RequestID == "" || len(in.RequestID) > 64 {
		return 0, 0, false, fmt.Errorf("%w: request_id must be 1..64 chars", ErrInvalidArgument)
	}
	if strings.TrimSpace(in.UserContent) == "" || strings.TrimSpace(in.AssistantContent) == "" {
		return 0, 0, false, fmt.Errorf("%w: empty user/assistant content", ErrInvalidArgument)
	}
	source := in.Source
	if source == "" {
		source = "unknown"
	}

	start := time.Now()
	defer func() {
		r.obs.DBQuery("append_conversation", txResult(err), time.Since(start).Seconds())
	}()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var exists int
	if err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM chat_sessions WHERE id=? AND tenant_id=? AND user_id=? AND deleted_at IS NULL",
		sessionID, tenantID, userID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, false, ErrSessionNotFound
		}
		return 0, 0, false, err
	}

	userMsgID, userCreated, err := upsertFinalMessage(ctx, tx, sessionID, in.RequestID, "user",
		in.UserContent, in.UserTokens, source)
	if err != nil {
		return 0, 0, false, err
	}
	assistantMsgID, assistantCreated, err := upsertFinalMessage(ctx, tx, sessionID, in.RequestID, "assistant",
		in.AssistantContent, in.AssistantTokens, source)
	if err != nil {
		return 0, 0, false, err
	}
	// 同一事务内维护日汇总：只有真正新增的消息才 +1，重试不双计。
	if userCreated {
		if err = bumpMessageDaily(ctx, tx, tenantID, "user"); err != nil {
			return 0, 0, false, err
		}
	}
	if assistantCreated {
		if err = bumpMessageDaily(ctx, tx, tenantID, "assistant"); err != nil {
			return 0, 0, false, err
		}
	}
	if _, err = tx.ExecContext(ctx,
		"UPDATE chat_sessions SET updated_at=CURRENT_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND user_id=?",
		sessionID, tenantID, userID); err != nil {
		return 0, 0, false, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, false, err
	}
	return userMsgID, assistantMsgID, userCreated && assistantCreated, nil
}

func upsertFinalMessage(ctx context.Context, tx *sql.Tx, sessionID uint64, requestID, role, content string,
	tokens int, source string) (uint64, bool, error) {
	// 幂等作用域是 (session_id, request_id, role)（migration 0007）：
	// 重复请求保留原记录与 id（LAST_INSERT_ID(id) 让 LastInsertId 返回已有 id），
	// 不用新内容覆盖已完成的最终消息；冲突行必须属于同一会话。
	sum := sha256.Sum256([]byte(content))
	contentHash := hex.EncodeToString(sum[:])
	res, err := tx.ExecContext(ctx, `
INSERT INTO chat_messages(session_id, request_id, role, content, token_count, source, content_hash)
VALUES(?,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`,
		sessionID, requestID, role, content, tokens, source, contentHash)
	if err != nil {
		return 0, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("last_insert_id: %w", err)
	}
	if affected != 1 {
		var dbSession uint64
		var dbRequest, dbRole, dbHash string
		if err := tx.QueryRowContext(ctx,
			"SELECT session_id, request_id, role, content_hash FROM chat_messages WHERE id=?", id).
			Scan(&dbSession, &dbRequest, &dbRole, &dbHash); err != nil {
			return 0, false, fmt.Errorf("verify idempotent row: %w", err)
		}
		if dbSession != sessionID || dbRequest != requestID || dbRole != role {
			return 0, false, fmt.Errorf("%w: existing row session=%d request=%q role=%q",
				ErrIdempotencyConflict, dbSession, dbRequest, dbRole)
		}
		if dbHash != "" && dbHash != contentHash {
			return 0, false, fmt.Errorf("%w: session=%d request=%q role=%q",
				ErrRequestPayloadMismatch, sessionID, requestID, role)
		}
	}
	return uint64(id), affected == 1, nil
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	if limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}

// bumpMessageDaily 与 chat_messages 写入同事务提交；day 使用数据库 CURRENT_DATE()，
// 与 created_at 的 DB 默认时区保持一致。
func bumpMessageDaily(ctx context.Context, tx *sql.Tx, tenantID uint64, role string) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO chat_message_daily(tenant_id, day, role, messages)
VALUES(?, CURRENT_DATE(), ?, 1)
ON DUPLICATE KEY UPDATE messages=messages+1`, tenantID, role)
	return err
}
