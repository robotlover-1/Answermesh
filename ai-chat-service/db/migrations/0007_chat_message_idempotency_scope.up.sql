-- P1-01：消息幂等作用域改为“单会话”。同一 request_id 在不同会话/租户不再误判为重复。
-- 拆成两条 ALTER：DDL 隐式提交后重试时，缺失的 DROP 可被迁移器识别并跳过，
-- 不会连带跳过后续 ADD（多动作 ALTER 失败会整条回滚）。
ALTER TABLE chat_messages DROP INDEX uk_request_role;
ALTER TABLE chat_messages ADD UNIQUE KEY uk_session_request_role (session_id, request_id, role);
