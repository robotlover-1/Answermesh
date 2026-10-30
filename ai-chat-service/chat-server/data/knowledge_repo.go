package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 知识条目/版本与发布（M-02）。
// 权限边界：tenant_id 必须由服务端鉴权后传入；普通用户不得调用 Publish/Review。
var (
	ErrEntryNotFound   = errors.New("knowledge entry not found")
	ErrVersionNotFound = errors.New("knowledge version not found")
	ErrNotApproved     = errors.New("knowledge version is not approved")
	ErrVersionConflict = errors.New("knowledge version conflict")
	// ErrKnowledgePublished：已发布版本不允许直接改变审核状态，需显式撤稿流程。
	ErrKnowledgePublished = errors.New("published knowledge version cannot change review status")
)

type KnowledgeVersion struct {
	EntryID      uint64
	Version      uint32
	Answer       string
	ReviewStatus string
	ReviewerID   sql.NullInt64
	CreatedAt    time.Time
	ReviewedAt   sql.NullTime
}

type KnowledgeRepository struct {
	db *sql.DB
	tx *txRunner
}

func NewKnowledgeRepository(db *sql.DB, obs DBObserver) *KnowledgeRepository {
	return &KnowledgeRepository{db: db, tx: newTxRunner(db, obs)}
}

func (r *KnowledgeRepository) CreateEntry(ctx context.Context, tenantID uint64, question, subjectKey string) (uint64, error) {
	if tenantID == 0 || question == "" {
		return 0, fmt.Errorf("%w: tenant/question required", ErrInvalidArgument)
	}
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO knowledge_entries(tenant_id, question_text, subject_key) VALUES(?,?,?)",
		tenantID, question, subjectKey)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last_insert_id: %w", err)
	}
	return uint64(id), nil
}

// SubmitVersion 在锁定条目后分配下一个版本号，状态为 pending。
func (r *KnowledgeRepository) SubmitVersion(ctx context.Context, tenantID, entryID uint64, answer string) (uint32, error) {
	if tenantID == 0 || entryID == 0 || answer == "" {
		return 0, fmt.Errorf("%w: tenant/entry/answer required", ErrInvalidArgument)
	}
	var version uint32
	err := r.tx.run(ctx, "submit_knowledge", func(tx *sql.Tx) error {
		var published uint32
		if err := tx.QueryRowContext(ctx,
			"SELECT published_version FROM knowledge_entries WHERE id=? AND tenant_id=? FOR UPDATE",
			entryID, tenantID).Scan(&published); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEntryNotFound
			}
			return err
		}
		var maxVersion uint32
		if err := tx.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(version),0) FROM knowledge_versions WHERE entry_id=? FOR UPDATE",
			entryID).Scan(&maxVersion); err != nil {
			return err
		}
		version = maxVersion + 1
		_, err := tx.ExecContext(ctx,
			"INSERT INTO knowledge_versions(entry_id, version, answer_text, review_status) VALUES(?,?,?,'pending')",
			entryID, version, answer)
		return err
	})
	return version, err
}

// Review 审核一个版本；重复审核为幂等。
func (r *KnowledgeRepository) Review(ctx context.Context, tenantID, entryID uint64, version uint32,
	reviewerID uint64, approve bool) error {
	if tenantID == 0 || entryID == 0 || version == 0 || reviewerID == 0 {
		return fmt.Errorf("%w: tenant/entry/version/reviewer required", ErrInvalidArgument)
	}
	target := "rejected"
	if approve {
		target = "approved"
	}
	return r.tx.run(ctx, "review_knowledge", func(tx *sql.Tx) error {
		var cur string
		var published uint32
		if err := tx.QueryRowContext(ctx, `
SELECT v.review_status, e.published_version
FROM knowledge_versions v JOIN knowledge_entries e ON e.id=v.entry_id
WHERE v.entry_id=? AND v.version=? AND e.tenant_id=? FOR UPDATE`,
			entryID, version, tenantID).Scan(&cur, &published); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrVersionNotFound
			}
			return err
		}
		if cur == target {
			return nil
		}
		if published == version {
			return ErrKnowledgePublished
		}
		_, err := tx.ExecContext(ctx, `
UPDATE knowledge_versions v JOIN knowledge_entries e ON e.id=v.entry_id
SET v.review_status=?, v.reviewer_id=?, v.reviewed_at=CURRENT_TIMESTAMP(3)
WHERE v.entry_id=? AND v.version=? AND e.tenant_id=?`,
			target, reviewerID, entryID, version, tenantID)
		return err
	})
}

// Publish 原子地更新权威版本并写入 Outbox。DB 事务内不做任何外部调用。
// 幂等：同一版本重复发布成功返回；旧版本发布返回 ErrVersionConflict。
func (r *KnowledgeRepository) Publish(ctx context.Context, tenantID, entryID uint64,
	version uint32, actorID uint64) error {
	if tenantID == 0 || entryID == 0 || version == 0 {
		return fmt.Errorf("%w: tenant/entry/version required", ErrInvalidArgument)
	}
	return r.tx.run(ctx, "publish_knowledge", func(tx *sql.Tx) error {
		var published uint32
		if err := tx.QueryRowContext(ctx,
			"SELECT published_version FROM knowledge_entries WHERE id=? AND tenant_id=? FOR UPDATE",
			entryID, tenantID).Scan(&published); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEntryNotFound
			}
			return err
		}
		var reviewStatus string
		if err := tx.QueryRowContext(ctx,
			"SELECT review_status FROM knowledge_versions WHERE entry_id=? AND version=? FOR UPDATE",
			entryID, version).Scan(&reviewStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrVersionNotFound
			}
			return err
		}
		if reviewStatus != "approved" {
			return ErrNotApproved
		}
		if version == published {
			return nil // 幂等重试
		}
		if version < published {
			return ErrVersionConflict
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE knowledge_entries SET published_version=?, status='published' WHERE id=? AND tenant_id=?",
			version, entryID, tenantID); err != nil {
			return err
		}
		eventKey := fmt.Sprintf("knowledge:%d:%d", entryID, version)
		_, err := tx.ExecContext(ctx, `
INSERT INTO cache_outbox(event_key, entry_id, version, event_type)
VALUES(?,?,?,'KNOWLEDGE_PUBLISHED')
ON DUPLICATE KEY UPDATE id=id`, eventKey, entryID, version)
		return err
	})
}

// GetPublishedVersion 返回权威版本；published_version=0 表示尚未发布。
func (r *KnowledgeRepository) GetPublishedVersion(ctx context.Context, tenantID, entryID uint64) (uint32, error) {
	var published uint32
	if err := r.db.QueryRowContext(ctx,
		"SELECT published_version FROM knowledge_entries WHERE id=? AND tenant_id=?",
		entryID, tenantID).Scan(&published); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrEntryNotFound
		}
		return 0, err
	}
	return published, nil
}

func (r *KnowledgeRepository) GetVersion(ctx context.Context, tenantID, entryID uint64,
	version uint32) (*KnowledgeVersion, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT v.entry_id, v.version, v.answer_text, v.review_status, v.reviewer_id, v.created_at, v.reviewed_at
FROM knowledge_versions v JOIN knowledge_entries e ON e.id=v.entry_id
WHERE v.entry_id=? AND v.version=? AND e.tenant_id=?`,
		entryID, version, tenantID)
	return scanKnowledgeVersion(row)
}

// GetVersionForWorker 供内部 Outbox Worker 使用，不做租户过滤（Worker 是受信内部组件）。
func (r *KnowledgeRepository) GetVersionForWorker(ctx context.Context, entryID uint64,
	version uint32) (*KnowledgeVersion, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT entry_id, version, answer_text, review_status, reviewer_id, created_at, reviewed_at
FROM knowledge_versions WHERE entry_id=? AND version=?`, entryID, version)
	return scanKnowledgeVersion(row)
}

func scanKnowledgeVersion(row *sql.Row) (*KnowledgeVersion, error) {
	v := &KnowledgeVersion{}
	if err := row.Scan(&v.EntryID, &v.Version, &v.Answer, &v.ReviewStatus,
		&v.ReviewerID, &v.CreatedAt, &v.ReviewedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotFound
		}
		return nil, err
	}
	return v, nil
}
