-- P2-03：Grafana 只读账号只授权聚合视图，不暴露原始聊天内容。
CREATE OR REPLACE VIEW vw_chat_message_daily AS
SELECT DATE(created_at) AS day, COUNT(*) AS messages
FROM chat_messages
GROUP BY DATE(created_at);

CREATE OR REPLACE VIEW vw_knowledge_review_queue AS
SELECT review_status, COUNT(*) AS versions, MIN(created_at) AS oldest
FROM knowledge_versions
GROUP BY review_status;

CREATE OR REPLACE VIEW vw_outbox_backlog AS
SELECT status, COUNT(*) AS jobs, MIN(created_at) AS oldest_created,
       SUM(attempts) AS total_attempts
FROM cache_outbox
GROUP BY status;

CREATE OR REPLACE VIEW vw_llm_usage_daily AS
SELECT DATE(created_at) AS day,
       SUM(CASE WHEN source='llm' THEN 1 ELSE 0 END) AS llm_calls,
       SUM(CASE WHEN source LIKE 'cache%' THEN 1 ELSE 0 END) AS cache_hits,
       SUM(input_tokens + output_tokens) AS actual_tokens,
       SUM(estimated_saved_tokens) AS estimated_saved_tokens
FROM llm_usage_events
GROUP BY DATE(created_at);
