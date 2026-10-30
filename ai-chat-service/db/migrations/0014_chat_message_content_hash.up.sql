-- P2-04：同 request_id 重试必须内容一致；记录逐条消息内容 SHA-256 用于校验。
ALTER TABLE chat_messages ADD COLUMN content_hash CHAR(64) NOT NULL DEFAULT '';
UPDATE chat_messages SET content_hash=SHA2(content,256) WHERE content_hash='';
