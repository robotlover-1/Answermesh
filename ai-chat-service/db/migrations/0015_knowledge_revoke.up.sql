-- PR-K1：知识撤稿状态、审计与旧版本禁止重发。
ALTER TABLE knowledge_entries ADD COLUMN revoked_version INT UNSIGNED NOT NULL DEFAULT 0;
ALTER TABLE knowledge_entries ADD COLUMN revoked_at DATETIME(3) NULL;
ALTER TABLE knowledge_entries ADD COLUMN revoked_by BIGINT UNSIGNED NULL;
ALTER TABLE knowledge_entries ADD COLUMN revoke_reason VARCHAR(255) NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS knowledge_audit_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id BIGINT UNSIGNED NOT NULL,
  entry_id BIGINT UNSIGNED NOT NULL,
  version INT UNSIGNED NOT NULL,
  action VARCHAR(16) NOT NULL,
  actor_id BIGINT UNSIGNED NOT NULL,
  reason VARCHAR(255) NOT NULL DEFAULT '',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_entry_time (entry_id, created_at),
  KEY idx_tenant_time (tenant_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
