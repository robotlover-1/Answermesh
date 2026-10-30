DROP TABLE IF EXISTS knowledge_audit_events;
ALTER TABLE knowledge_entries DROP COLUMN revoke_reason;
ALTER TABLE knowledge_entries DROP COLUMN revoked_by;
ALTER TABLE knowledge_entries DROP COLUMN revoked_at;
ALTER TABLE knowledge_entries DROP COLUMN revoked_version;
