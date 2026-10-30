-- M-02：知识条目、版本审核与发布。
CREATE TABLE IF NOT EXISTS knowledge_entries (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id BIGINT UNSIGNED NOT NULL,
  question_text TEXT NOT NULL,
  subject_key VARCHAR(128) NOT NULL DEFAULT '',
  published_version INT UNSIGNED NOT NULL DEFAULT 0,
  status VARCHAR(20) NOT NULL DEFAULT 'draft',
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
    ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_tenant_status (tenant_id, status, updated_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS knowledge_versions (
  entry_id BIGINT UNSIGNED NOT NULL,
  version INT UNSIGNED NOT NULL,
  answer_text MEDIUMTEXT NOT NULL,
  review_status VARCHAR(16) NOT NULL,
  reviewer_id BIGINT UNSIGNED NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  reviewed_at DATETIME(3) NULL,
  PRIMARY KEY (entry_id, version),
  KEY idx_review_queue (review_status, created_at, entry_id),
  CONSTRAINT fk_version_entry FOREIGN KEY (entry_id)
    REFERENCES knowledge_entries(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
