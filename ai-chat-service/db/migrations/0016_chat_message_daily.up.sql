-- PR-M1：聊天消息按天汇总（历史成功入库消息数；软删会话不追溯扣减）。
CREATE TABLE IF NOT EXISTS chat_message_daily (
  tenant_id BIGINT UNSIGNED NOT NULL,
  day DATE NOT NULL,
  role VARCHAR(16) NOT NULL,
  messages BIGINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, day, role)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 一次性回填；messages 使用绝对值覆盖，迁移只应在应用写入新版本前执行。
INSERT INTO chat_message_daily(tenant_id, day, role, messages)
SELECT s.tenant_id, DATE(m.created_at), m.role, COUNT(*)
FROM chat_messages m JOIN chat_sessions s ON s.id = m.session_id
GROUP BY s.tenant_id, DATE(m.created_at), m.role
ON DUPLICATE KEY UPDATE messages=VALUES(messages);
