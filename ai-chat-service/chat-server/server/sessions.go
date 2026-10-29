package server

import (
	"context"
	"encoding/json"
	"errors"

	"ai-chat-service/chat-server/data"
	metrics_bus "ai-chat-service/chat-server/metrics-bus"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
)

// IdentityVerifier 由 pkg/identity.Verifier 实现；测试可注入 fake。
type IdentityVerifier interface {
	Verify(token string) (*contract.IdentityClaims, error)
}

type sessionService struct {
	repo     *data.SessionRepository
	verifier IdentityVerifier
}

// RegisterSessionZRPC 注册会话/消息 unary 接口。tenant/user 只来自验签后的
// identity token，请求体中的任何 user_id 都不会被使用。
func RegisterSessionZRPC(zsrv *zrpc.Server, repo *data.SessionRepository,
	verifier IdentityVerifier, bus *metrics_bus.BusMetrics) error {
	s := &sessionService{repo: repo, verifier: verifier}
	reg := func(method string, h zrpc.UnaryHandler) error {
		return zsrv.RegisterUnary(method, wrapUnaryMetrics(method, h, bus))
	}
	if err := reg(contract.MethodSessionCreate, s.create); err != nil {
		return err
	}
	if err := reg(contract.MethodSessionList, s.list); err != nil {
		return err
	}
	if err := reg(contract.MethodSessionDelete, s.delete); err != nil {
		return err
	}
	if err := reg(contract.MethodMessageList, s.listMessages); err != nil {
		return err
	}
	return reg(contract.MethodConversationAppend, s.appendConversation)
}

func (s *sessionService) claims(token string) (*contract.IdentityClaims, error) {
	if s.verifier == nil {
		return nil, zrpc.Unauthenticated(errors.New("identity verifier is not configured"))
	}
	claims, err := s.verifier.Verify(token)
	if err != nil {
		return nil, zrpc.Unauthenticated(err)
	}
	return claims, nil
}

func sessionStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, data.ErrSessionNotFound), errors.Is(err, data.ErrVersionNotFound):
		return zrpc.NotFound(err)
	case errors.Is(err, data.ErrInvalidArgument):
		return zrpc.InvalidArgument(err)
	case errors.Is(err, context.DeadlineExceeded):
		return zrpc.DeadlineExceeded(err)
	case errors.Is(err, context.Canceled):
		return zrpc.Unavailable(err)
	default:
		return zrpc.Internal(err)
	}
}

func decodeRequest(raw json.RawMessage, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return zrpc.InvalidArgument(err)
	}
	return nil
}

func (s *sessionService) create(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.CreateSessionRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.claims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	id, err := s.repo.CreateSession(ctx, claims.TenantID, claims.UserID, req.Title)
	if err != nil {
		return nil, sessionStatus(err)
	}
	return &contract.CreateSessionResponse{SessionID: id}, nil
}

func (s *sessionService) list(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.ListSessionsRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.claims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	sessions, err := s.repo.ListSessions(ctx, claims.TenantID, claims.UserID, req.BeforeID, req.Limit)
	if err != nil {
		return nil, sessionStatus(err)
	}
	out := make([]contract.SessionItem, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, contract.SessionItem{
			ID:        sess.ID,
			Title:     sess.Title,
			CreatedAt: sess.CreatedAt.UnixMilli(),
			UpdatedAt: sess.UpdatedAt.UnixMilli(),
		})
	}
	return &contract.ListSessionsResponse{Sessions: out}, nil
}

func (s *sessionService) delete(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.DeleteSessionRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.claims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	deleted, err := s.repo.SoftDeleteSession(ctx, claims.TenantID, claims.UserID, req.SessionID)
	if err != nil {
		return nil, sessionStatus(err)
	}
	// 不属于该用户的会话同样返回 deleted=false，不泄露存在性。
	return &contract.DeleteSessionResponse{Deleted: deleted}, nil
}

func (s *sessionService) listMessages(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.ListMessagesRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.claims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	if _, err := s.repo.GetSession(ctx, claims.TenantID, claims.UserID, req.SessionID); err != nil {
		return nil, sessionStatus(err)
	}
	messages, err := s.repo.ListMessages(ctx, claims.TenantID, claims.UserID, req.SessionID, req.BeforeID, req.Limit)
	if err != nil {
		return nil, sessionStatus(err)
	}
	out := make([]contract.MessageItem, 0, len(messages))
	for _, m := range messages {
		out = append(out, contract.MessageItem{
			ID: m.ID, SessionID: m.SessionID, RequestID: m.RequestID, Role: m.Role,
			Content: m.Content, TokenCount: m.TokenCount, Source: m.Source,
			CreatedAt: m.CreatedAt.UnixMilli(),
		})
	}
	return &contract.ListMessagesResponse{Messages: out}, nil
}

func (s *sessionService) appendConversation(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.AppendConversationRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.claims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	uid, aid, created, err := s.repo.AppendConversation(ctx, claims.TenantID, claims.UserID, req.SessionID,
		data.AppendInput{
			RequestID: req.RequestID, UserContent: req.UserContent, UserTokens: req.UserTokens,
			AssistantContent: req.AssistantContent, AssistantTokens: req.AssistantTokens, Source: req.Source,
		})
	if err != nil {
		return nil, sessionStatus(err)
	}
	return &contract.AppendConversationResponse{UserMessageID: uid, AssistantMessageID: aid, Created: created}, nil
}
