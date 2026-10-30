CREATE OR REPLACE VIEW vw_chat_message_daily AS
SELECT day, SUM(messages) AS messages
FROM chat_message_daily
GROUP BY day;
