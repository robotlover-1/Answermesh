DROP INDEX idx_created_at ON chat_messages;

CREATE OR REPLACE VIEW vw_llm_usage_daily AS
SELECT DATE(created_at) AS day,
       SUM(CASE WHEN source='llm' THEN 1 ELSE 0 END) AS llm_calls,
       SUM(CASE WHEN source LIKE 'cache%' THEN 1 ELSE 0 END) AS cache_hits,
       SUM(input_tokens + output_tokens) AS actual_tokens,
       SUM(estimated_saved_tokens) AS estimated_saved_tokens
FROM llm_usage_events
GROUP BY DATE(created_at);
