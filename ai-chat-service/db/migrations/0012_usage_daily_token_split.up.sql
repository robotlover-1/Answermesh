-- P2-01：usage_daily 区分真实模型 token 与 tokenizer 估算 token。
ALTER TABLE usage_daily ADD COLUMN actual_tokens BIGINT UNSIGNED NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN tokenizer_tokens BIGINT UNSIGNED NOT NULL DEFAULT 0;
UPDATE usage_daily d
JOIN (
  SELECT tenant_id, DATE(created_at) AS day, source, model_family,
         SUM(CASE WHEN source='llm' AND token_source='actual'
                  THEN input_tokens+output_tokens ELSE 0 END) AS actual_tokens,
         SUM(CASE WHEN token_source='tokenizer'
                  THEN input_tokens+output_tokens ELSE 0 END) AS tokenizer_tokens
  FROM llm_usage_events
  GROUP BY tenant_id, DATE(created_at), source, model_family
) e
  ON d.tenant_id=e.tenant_id AND d.day=e.day AND d.source=e.source AND d.model_family=e.model_family
SET d.actual_tokens=e.actual_tokens, d.tokenizer_tokens=e.tokenizer_tokens;
