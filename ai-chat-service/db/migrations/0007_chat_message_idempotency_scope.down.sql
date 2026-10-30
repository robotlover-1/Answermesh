-- 注意：若已存在跨会话复用同一 request_id 的数据，回滚会因旧唯一键冲突而失败。
ALTER TABLE chat_messages DROP INDEX uk_session_request_role;
ALTER TABLE chat_messages ADD UNIQUE KEY uk_request_role (request_id, role);
