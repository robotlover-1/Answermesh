-- M-04：LLM 用量流水（request_id 唯一，真实用量与缓存节省估算分离）与日汇总表。
CREATE TABLE IF NOT EXISTS llm_usage_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  request_id VARCHAR(64) NOT NULL,
  tenant_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  session_id BIGINT UNSIGNED NULL,
  source VARCHAR(16) NOT NULL,       -- llm/cache_exact/cache_semantic/blocked
  model_family VARCHAR(64) NOT NULL DEFAULT '',
  status VARCHAR(16) NOT NULL,       -- success/failed/cancelled/unknown
  input_tokens INT UNSIGNED NOT NULL DEFAULT 0,
  output_tokens INT UNSIGNED NOT NULL DEFAULT 0,
  estimated_saved_tokens INT UNSIGNED NOT NULL DEFAULT 0,
  cost_microunits BIGINT UNSIGNED NOT NULL DEFAULT 0,
  latency_ms INT UNSIGNED NOT NULL DEFAULT 0,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_request (request_id),
  KEY idx_tenant_time (tenant_id, created_at, id),
  KEY idx_source_time (source, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 汇总表使用确定性 upsert（tenant,day,source,model_family 作为主键），重跑不重复计数。
CREATE TABLE IF NOT EXISTS usage_daily (
  tenant_id BIGINT UNSIGNED NOT NULL,
  day DATE NOT NULL,
  source VARCHAR(16) NOT NULL,
  model_family VARCHAR(64) NOT NULL DEFAULT '',
  requests BIGINT UNSIGNED NOT NULL DEFAULT 0,
  input_tokens BIGINT UNSIGNED NOT NULL DEFAULT 0,
  output_tokens BIGINT UNSIGNED NOT NULL DEFAULT 0,
  estimated_saved_tokens BIGINT UNSIGNED NOT NULL DEFAULT 0,
  cost_microunits BIGINT UNSIGNED NOT NULL DEFAULT 0,
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
    ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, day, source, model_family),
  KEY idx_day (day)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
