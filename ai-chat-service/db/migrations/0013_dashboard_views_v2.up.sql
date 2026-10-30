-- P1-03 / P2-01：LLM 业务视图改查预聚合 usage_daily；聊天视图补时间索引。
ALTER TABLE chat_messages ADD KEY idx_created_at (created_at);

CREATE OR REPLACE VIEW vw_llm_usage_daily AS
SELECT day,
       SUM(CASE WHEN source='llm' THEN requests ELSE 0 END) AS llm_calls,
       SUM(CASE WHEN source LIKE 'cache%' THEN requests ELSE 0 END) AS cache_hits,
       SUM(actual_tokens) AS actual_llm_tokens,
       SUM(tokenizer_tokens) AS tokenizer_estimated_tokens,
       SUM(estimated_saved_tokens) AS estimated_saved_tokens
FROM usage_daily
GROUP BY day;
