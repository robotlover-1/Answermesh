-- P1-02：Outbox 租约 fencing token。Claim 写入随机 token；Ack/Retry 必须匹配，
-- 防止过期旧 Worker 晚到完成覆盖新租约持有者的结果。
ALTER TABLE cache_outbox ADD COLUMN claim_token VARCHAR(64) NOT NULL DEFAULT '';
