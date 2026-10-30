-- Outbox 只使用 pending/processing/done/dead；历史 failed 统一回到 pending（由 Claim 重试）。
UPDATE cache_outbox SET status='pending', locked_until=NULL WHERE status='failed';
