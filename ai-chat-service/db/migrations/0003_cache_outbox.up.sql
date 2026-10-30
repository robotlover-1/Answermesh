-- M-03：与知识发布同事务写入的 Outbox；event_key 保证同一版本事件只产生一次。
CREATE TABLE IF NOT EXISTS cache_outbox (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  event_key VARCHAR(128) NOT NULL,
  entry_id BIGINT UNSIGNED NOT NULL,
  version INT UNSIGNED NOT NULL,
  event_type VARCHAR(32) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  available_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  locked_until DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  processed_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_event_key (event_key),
  KEY idx_dispatch (status, available_at, id),
  KEY idx_locked (status, locked_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
