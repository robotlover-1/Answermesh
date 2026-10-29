-- M-01：会话与消息（新表，不替换旧 chat_records）。
CREATE TABLE IF NOT EXISTS chat_sessions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  title VARCHAR(255) NOT NULL DEFAULT '',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
    ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  -- 会话列表：WHERE tenant,user ORDER BY updated_at DESC,id DESC
  KEY idx_user_list (tenant_id, user_id, updated_at, id),
  -- 游标分页：WHERE tenant,user AND id < ? ORDER BY id DESC
  KEY idx_user_cursor (tenant_id, user_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS chat_messages (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  session_id BIGINT UNSIGNED NOT NULL,
  request_id VARCHAR(64) NOT NULL,
  role VARCHAR(16) NOT NULL,
  content MEDIUMTEXT NOT NULL,
  token_count INT UNSIGNED NOT NULL DEFAULT 0,
  source VARCHAR(16) NOT NULL DEFAULT 'unknown',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  -- 幂等：同一 request 的同一角色只写一条（单请求一条 user + 一条最终 assistant）。
  UNIQUE KEY uk_request_role (request_id, role),
  -- 消息列表：WHERE session_id=? AND id<? ORDER BY id DESC LIMIT ?
  KEY idx_session_messages (session_id, id),
  CONSTRAINT fk_message_session FOREIGN KEY (session_id)
    REFERENCES chat_sessions(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
