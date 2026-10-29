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

type knowledgeService struct {
	repo     *data.KnowledgeRepository
	read     *data.KnowledgeService
	verifier IdentityVerifier
}

// RegisterKnowledgeZRPC 注册知识管理/读取接口。
// create/submit/review/publish 要求 claims.Role=="admin"；get_published 任意登录用户可读。
func RegisterKnowledgeZRPC(zsrv *zrpc.Server, repo *data.KnowledgeRepository,
	read *data.KnowledgeService, verifier IdentityVerifier, bus *metrics_bus.BusMetrics) error {
	s := &knowledgeService{repo: repo, read: read, verifier: verifier}
	reg := func(method string, h zrpc.UnaryHandler) error {
		return zsrv.RegisterUnary(method, wrapUnaryMetrics(method, h, bus))
	}
	if err := reg(contract.MethodKnowledgeCreate, s.create); err != nil {
		return err
	}
	if err := reg(contract.MethodKnowledgeSubmit, s.submit); err != nil {
		return err
	}
	if err := reg(contract.MethodKnowledgeReview, s.review); err != nil {
		return err
	}
	if err := reg(contract.MethodKnowledgePublish, s.publish); err != nil {
		return err
	}
	return reg(contract.MethodKnowledgeGetPublished, s.getPublished)
}

func (s *knowledgeService) claims(token string) (*contract.IdentityClaims, error) {
	if s.verifier == nil {
		return nil, zrpc.Unauthenticated(errors.New("identity verifier is not configured"))
	}
	claims, err := s.verifier.Verify(token)
	if err != nil {
		return nil, zrpc.Unauthenticated(err)
	}
	return claims, nil
}

func (s *knowledgeService) adminClaims(token string) (*contract.IdentityClaims, error) {
	claims, err := s.claims(token)
	if err != nil {
		return nil, err
	}
	if claims.Role != "admin" {
		return nil, zrpc.Unauthenticated(errors.New("forbidden: admin role required"))
	}
	return claims, nil
}

func knowledgeStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, data.ErrEntryNotFound), errors.Is(err, data.ErrVersionNotFound),
		errors.Is(err, data.ErrNotPublished), errors.Is(err, data.ErrSessionNotFound):
		return zrpc.NotFound(err)
	case errors.Is(err, data.ErrInvalidArgument), errors.Is(err, data.ErrNotApproved),
		errors.Is(err, data.ErrVersionConflict):
		return zrpc.InvalidArgument(err)
	case errors.Is(err, context.DeadlineExceeded):
		return zrpc.DeadlineExceeded(err)
	case errors.Is(err, context.Canceled):
		return zrpc.Unavailable(err)
	default:
		return zrpc.Internal(err)
	}
}

func (s *knowledgeService) create(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.KnowledgeCreateRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.adminClaims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	id, err := s.repo.CreateEntry(ctx, claims.TenantID, req.Question, req.SubjectKey)
	if err != nil {
		return nil, knowledgeStatus(err)
	}
	return &contract.KnowledgeCreateResponse{EntryID: id}, nil
}

func (s *knowledgeService) submit(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.KnowledgeSubmitRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.adminClaims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	version, err := s.repo.SubmitVersion(ctx, claims.TenantID, req.EntryID, req.Answer)
	if err != nil {
		return nil, knowledgeStatus(err)
	}
	return &contract.KnowledgeSubmitResponse{Version: version}, nil
}

func (s *knowledgeService) review(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.KnowledgeReviewRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.adminClaims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Review(ctx, claims.TenantID, req.EntryID, req.Version, claims.UserID, req.Approve); err != nil {
		return nil, knowledgeStatus(err)
	}
	reviewStatus := "rejected"
	if req.Approve {
		reviewStatus = "approved"
	}
	return &contract.KnowledgeReviewResponse{ReviewStatus: reviewStatus}, nil
}

func (s *knowledgeService) publish(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.KnowledgePublishRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.adminClaims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Publish(ctx, claims.TenantID, req.EntryID, req.Version, claims.UserID); err != nil {
		return nil, knowledgeStatus(err)
	}
	published, err := s.repo.GetPublishedVersion(ctx, claims.TenantID, req.EntryID)
	if err != nil {
		return nil, knowledgeStatus(err)
	}
	return &contract.KnowledgePublishResponse{PublishedVersion: published}, nil
}

func (s *knowledgeService) getPublished(ctx context.Context, raw json.RawMessage) (any, error) {
	var req contract.KnowledgeGetPublishedRequest
	if err := decodeRequest(raw, &req); err != nil {
		return nil, err
	}
	claims, err := s.claims(req.IdentityToken)
	if err != nil {
		return nil, err
	}
	answer, err := s.read.PublishedAnswer(ctx, claims.TenantID, req.EntryID)
	if err != nil {
		return nil, knowledgeStatus(err)
	}
	version, err := s.repo.GetPublishedVersion(ctx, claims.TenantID, req.EntryID)
	if err != nil {
		return nil, knowledgeStatus(err)
	}
	return &contract.KnowledgeGetPublishedResponse{EntryID: req.EntryID, Version: version, Answer: answer}, nil
}
