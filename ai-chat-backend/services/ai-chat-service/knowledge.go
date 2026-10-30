package ai_chat_service

import (
	"context"

	"echo-zrpc-go/contract"
)

func CreateKnowledge(ctx context.Context, identityToken, question, subjectKey string) (*contract.KnowledgeCreateResponse, error) {
	out := &contract.KnowledgeCreateResponse{}
	err := callUnary(ctx, contract.MethodKnowledgeCreate, &contract.KnowledgeCreateRequest{
		IdentityToken: identityToken, Question: question, SubjectKey: subjectKey,
	}, out)
	return out, err
}

func SubmitKnowledge(ctx context.Context, identityToken string, entryID uint64, answer string) (*contract.KnowledgeSubmitResponse, error) {
	out := &contract.KnowledgeSubmitResponse{}
	err := callUnary(ctx, contract.MethodKnowledgeSubmit, &contract.KnowledgeSubmitRequest{
		IdentityToken: identityToken, EntryID: entryID, Answer: answer,
	}, out)
	return out, err
}

func ReviewKnowledge(ctx context.Context, identityToken string, entryID uint64, version uint32, approve bool) (*contract.KnowledgeReviewResponse, error) {
	out := &contract.KnowledgeReviewResponse{}
	err := callUnary(ctx, contract.MethodKnowledgeReview, &contract.KnowledgeReviewRequest{
		IdentityToken: identityToken, EntryID: entryID, Version: version, Approve: approve,
	}, out)
	return out, err
}

func PublishKnowledge(ctx context.Context, identityToken string, entryID uint64, version uint32) (*contract.KnowledgePublishResponse, error) {
	out := &contract.KnowledgePublishResponse{}
	err := callUnary(ctx, contract.MethodKnowledgePublish, &contract.KnowledgePublishRequest{
		IdentityToken: identityToken, EntryID: entryID, Version: version,
	}, out)
	return out, err
}

func GetPublishedKnowledge(ctx context.Context, identityToken string, entryID uint64) (*contract.KnowledgeGetPublishedResponse, error) {
	out := &contract.KnowledgeGetPublishedResponse{}
	err := callUnary(ctx, contract.MethodKnowledgeGetPublished, &contract.KnowledgeGetPublishedRequest{
		IdentityToken: identityToken, EntryID: entryID,
	}, out)
	return out, err
}

func RevokeKnowledge(ctx context.Context, identityToken string, entryID uint64, expectedVersion uint32, reason string) (*contract.KnowledgeRevokeResponse, error) {
	out := &contract.KnowledgeRevokeResponse{}
	err := callUnary(ctx, contract.MethodKnowledgeRevoke, &contract.KnowledgeRevokeRequest{
		IdentityToken: identityToken, EntryID: entryID, ExpectedVersion: expectedVersion, Reason: reason,
	}, out)
	return out, err
}
