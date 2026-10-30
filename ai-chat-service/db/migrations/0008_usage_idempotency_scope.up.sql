-- P1-02：用量幂等作用域改为 (tenant_id, user_id, request_id)，避免跨用户少记。
ALTER TABLE llm_usage_events DROP INDEX uk_request;
ALTER TABLE llm_usage_events ADD UNIQUE KEY uk_tenant_user_request (tenant_id, user_id, request_id);
