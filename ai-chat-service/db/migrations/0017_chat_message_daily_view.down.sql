CREATE OR REPLACE VIEW vw_chat_message_daily AS
SELECT DATE(created_at) AS day, COUNT(*) AS messages
FROM chat_messages
GROUP BY DATE(created_at);
