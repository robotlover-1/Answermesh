-- 标记 input/output tokens 的来源：actual（模型 usage）/ tokenizer（本地 tokenizer 计数）/
-- none（没有实际调用或未返回 usage）。estimated_saved_tokens 天然是估算，不受此列影响。
ALTER TABLE llm_usage_events ADD COLUMN token_source VARCHAR(16) NOT NULL DEFAULT 'actual' AFTER status;
