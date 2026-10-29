package contract

// 会话/消息 API 契约（backend → ai-chat-service）。
// 每个请求都携带 IdentityToken；tenant/user 只从验签后的 claims 读取。

type CreateSessionRequest struct {
	IdentityToken string `json:"identity_token"`
	Title         string `json:"title"`
}

type CreateSessionResponse struct {
	SessionID uint64 `json:"session_id"`
}

type SessionItem struct {
	ID        uint64 `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at_ms"`
	UpdatedAt int64  `json:"updated_at_ms"`
}

type ListSessionsRequest struct {
	IdentityToken string `json:"identity_token"`
	BeforeID      uint64 `json:"before_id"`
	Limit         int    `json:"limit"`
}

type ListSessionsResponse struct {
	Sessions []SessionItem `json:"sessions"`
}

type DeleteSessionRequest struct {
	IdentityToken string `json:"identity_token"`
	SessionID     uint64 `json:"session_id"`
}

type DeleteSessionResponse struct {
	Deleted bool `json:"deleted"`
}

type MessageItem struct {
	ID         uint64 `json:"id"`
	SessionID  uint64 `json:"session_id"`
	RequestID  string `json:"request_id"`
	Role       string `json:"role"`
	Content    string `json:"content"`
	TokenCount int    `json:"token_count"`
	Source     string `json:"source"`
	CreatedAt  int64  `json:"created_at_ms"`
}

type ListMessagesRequest struct {
	IdentityToken string `json:"identity_token"`
	SessionID     uint64 `json:"session_id"`
	BeforeID      uint64 `json:"before_id"`
	Limit         int    `json:"limit"`
}

type ListMessagesResponse struct {
	Messages []MessageItem `json:"messages"`
}

// AppendConversationRequest 写入一次请求的最终 user+assistant 消息（幂等）。
type AppendConversationRequest struct {
	IdentityToken    string `json:"identity_token"`
	SessionID        uint64 `json:"session_id"`
	RequestID        string `json:"request_id"`
	UserContent      string `json:"user_content"`
	UserTokens       int    `json:"user_tokens"`
	AssistantContent string `json:"assistant_content"`
	AssistantTokens  int    `json:"assistant_tokens"`
	Source           string `json:"source"`
}

type AppendConversationResponse struct {
	UserMessageID      uint64 `json:"user_message_id"`
	AssistantMessageID uint64 `json:"assistant_message_id"`
	Created            bool   `json:"created"`
}
