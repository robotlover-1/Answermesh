package contract

// 知识审核/发布 API（backend → ai-chat-service）。
// 所有请求携带 IdentityToken；写操作要求 claims.Role == "admin"。

type KnowledgeCreateRequest struct {
	IdentityToken string `json:"identity_token"`
	Question      string `json:"question"`
	SubjectKey    string `json:"subject_key"`
}

type KnowledgeCreateResponse struct {
	EntryID uint64 `json:"entry_id"`
}

type KnowledgeSubmitRequest struct {
	IdentityToken string `json:"identity_token"`
	EntryID       uint64 `json:"entry_id"`
	Answer        string `json:"answer"`
}

type KnowledgeSubmitResponse struct {
	Version uint32 `json:"version"`
}

type KnowledgeReviewRequest struct {
	IdentityToken string `json:"identity_token"`
	EntryID       uint64 `json:"entry_id"`
	Version       uint32 `json:"version"`
	Approve       bool   `json:"approve"`
}

type KnowledgeReviewResponse struct {
	ReviewStatus string `json:"review_status"`
}

type KnowledgePublishRequest struct {
	IdentityToken string `json:"identity_token"`
	EntryID       uint64 `json:"entry_id"`
	Version       uint32 `json:"version"`
}

type KnowledgePublishResponse struct {
	PublishedVersion uint32 `json:"published_version"`
}

// KnowledgeGetPublishedRequest 是用户侧读取；任意已登录身份可读本租户已发布知识。
type KnowledgeGetPublishedRequest struct {
	IdentityToken string `json:"identity_token"`
	EntryID       uint64 `json:"entry_id"`
}

type KnowledgeGetPublishedResponse struct {
	EntryID uint64 `json:"entry_id"`
	Version uint32 `json:"version"`
	Answer  string `json:"answer"`
}

// KnowledgeRevokeRequest 撤稿当前公开版本；ExpectedVersion>0 时做乐观并发检查。
type KnowledgeRevokeRequest struct {
	IdentityToken   string `json:"identity_token"`
	EntryID         uint64 `json:"entry_id"`
	ExpectedVersion uint32 `json:"expected_version"`
	Reason          string `json:"reason"`
}

type KnowledgeRevokeResponse struct {
	RevokedVersion uint32 `json:"revoked_version"`
}
