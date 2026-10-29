-- Outbox backlog 聚合（COUNT + MIN(created_at)）使用 (status, created_at) 索引。
ALTER TABLE cache_outbox ADD KEY idx_status_created (status, created_at);
