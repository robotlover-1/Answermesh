package server

import (
	"context"
	"errors"
	"time"

	"ai-chat-service/chat-server/data"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
	"github.com/google/uuid"
)

// chatMeta 是 zRPC 传输层解析出的可选请求元数据；通过 context 传给业务层，
// 不修改 proto DTO，也不出现在日志里。
type chatMeta struct {
	IdentityToken    string
	SessionID        uint64
	KnowledgeEntryID uint64
}

type chatMetaKey struct{}

func withChatMeta(ctx context.Context, req *contract.ChatCompletionRequest) context.Context {
	return context.WithValue(ctx, chatMetaKey{}, chatMeta{
		IdentityToken:    req.IdentityToken,
		SessionID:        req.SessionID,
		KnowledgeEntryID: req.KnowledgeEntryID,
	})
}

func chatMetaFrom(ctx context.Context) chatMeta {
	m, _ := ctx.Value(chatMetaKey{}).(chatMeta)
	return m
}

// verifyIdentity：无 token 时返回 (nil,nil) 以兼容旧客户端；有 token 但无效则拒绝请求。
func (s *chatService) verifyIdentity(token string) (*contract.IdentityClaims, error) {
	if token == "" {
		return nil, nil
	}
	if s.identity == nil || !s.identity.Configured() {
		return nil, zrpc.Unauthenticated(errors.New("identity secret is not configured"))
	}
	claims, err := s.identity.Verify(token)
	if err != nil {
		// 只记录失败原因，不记录 token 本身。
		s.log.ErrorF("chat identity verify failed: %v", err)
		return nil, zrpc.Unauthenticated(err)
	}
	return claims, nil
}

func requestIDOf(id string) string {
	if id == "" {
		id = uuid.NewString()
	}
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

func usageStatusOf(err error) string {
	switch {
	case err == nil:
		return data.UsageStatusSuccess
	case errors.Is(err, context.Canceled):
		return data.UsageStatusCancelled
	default:
		return data.UsageStatusFailed
	}
}

// recordUsage 只入有界队列；claims 为空（旧客户端/未登录）时跳过。
func (s *chatService) recordUsage(claims *contract.IdentityClaims, meta chatMeta, requestID,
	source, status, tokenSource string, input, output, saved int, start time.Time) {
	if s.usage == nil || claims == nil {
		return
	}
	ev := data.UsageEvent{
		RequestID: requestID, TenantID: claims.TenantID, UserID: claims.UserID,
		SessionID: meta.SessionID, Source: source, ModelFamily: s.config.Chat.Model,
		Status: status, TokenSource: tokenSource,
		InputTokens: input, OutputTokens: output, EstimatedSavedTokens: saved,
		Latency: time.Since(start), CreatedAt: time.Now(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.usage.Add(ctx, ev); err != nil {
		s.log.ErrorF("usage enqueue failed: %v", err)
	}
}

// appendConversation 写最终 user+assistant 消息（幂等）；失败只日志，不影响已返回的响应。
func (s *chatService) appendConversation(claims *contract.IdentityClaims, meta chatMeta,
	requestID, source, userMsg string, userTokens int, assistantMsg string, assistantTokens int) {
	if s.sessions == nil || claims == nil || meta.SessionID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := s.sessions.Add(ctx, data.SessionAppendJob{
		TenantID: claims.TenantID, UserID: claims.UserID, SessionID: meta.SessionID,
		Input: data.AppendInput{
			RequestID: requestID, UserContent: userMsg, UserTokens: userTokens,
			AssistantContent: assistantMsg, AssistantTokens: assistantTokens, Source: source,
		},
	})
	if err != nil {
		s.log.ErrorF("session append enqueue failed: %v", err)
	}
}

// knowledgeAnswer 尝试命中“已审核发布的知识”。版本与租户校验在 MySQL/缓存层完成；
// 未发布/不存在时返回 ok=false 并继续走语义缓存/LLM。
func (s *chatService) knowledgeAnswer(ctx context.Context, claims *contract.IdentityClaims, meta chatMeta) (string, bool) {
	if s.knowledge == nil || claims == nil || meta.KnowledgeEntryID == 0 {
		return "", false
	}
	ans, _, err := s.knowledge.PublishedAnswer(ctx, claims.TenantID, meta.KnowledgeEntryID)
	if err != nil {
		if !errors.Is(err, data.ErrNotPublished) && !errors.Is(err, data.ErrEntryNotFound) &&
			!errors.Is(err, data.ErrVersionNotFound) {
			s.log.ErrorF("knowledge lookup failed: %v", err)
		}
		return "", false
	}
	return ans, ans != ""
}

// unaryIdentity 是 ChatCompletion 入口的便捷封装（保持旧调用点简洁）。
func (s *chatService) unaryIdentity(ctx context.Context) (*contract.IdentityClaims, chatMeta, error) {
	meta := chatMetaFrom(ctx)
	claims, err := s.verifyIdentity(meta.IdentityToken)
	return claims, meta, err
}
