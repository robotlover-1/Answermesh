-- 注意：若已存在跨用户复用同一 request_id 的数据，回滚会因旧唯一键冲突而失败。
ALTER TABLE llm_usage_events DROP INDEX uk_tenant_user_request;
ALTER TABLE llm_usage_events ADD UNIQUE KEY uk_request (request_id);
