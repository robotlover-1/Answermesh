package ai_chat_service

import (
	"context"
	"fmt"

	"ai-chat-backend/pkg/config"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
)

func callUnary(ctx context.Context, method string, in, out any) error {
	dep := config.GetConfig().DependOn.AiChatService
	cli, err := zrpc.NewClient(clientOptionsFromAddress(dep.Address, dep.AccessToken))
	if err != nil {
		return fmt.Errorf("zrpc client: %w", err)
	}
	defer cli.Close()
	return cli.Unary(ctx, method, in, out)
}

func CreateSession(ctx context.Context, identityToken, title string) (*contract.CreateSessionResponse, error) {
	out := &contract.CreateSessionResponse{}
	err := callUnary(ctx, contract.MethodSessionCreate,
		&contract.CreateSessionRequest{IdentityToken: identityToken, Title: title}, out)
	return out, err
}

func ListSessions(ctx context.Context, identityToken string, beforeID uint64, limit int) (*contract.ListSessionsResponse, error) {
	out := &contract.ListSessionsResponse{}
	err := callUnary(ctx, contract.MethodSessionList,
		&contract.ListSessionsRequest{IdentityToken: identityToken, BeforeID: beforeID, Limit: limit}, out)
	return out, err
}

func DeleteSession(ctx context.Context, identityToken string, sessionID uint64) (*contract.DeleteSessionResponse, error) {
	out := &contract.DeleteSessionResponse{}
	err := callUnary(ctx, contract.MethodSessionDelete,
		&contract.DeleteSessionRequest{IdentityToken: identityToken, SessionID: sessionID}, out)
	return out, err
}

func ListMessages(ctx context.Context, identityToken string, sessionID, beforeID uint64, limit int) (*contract.ListMessagesResponse, error) {
	out := &contract.ListMessagesResponse{}
	err := callUnary(ctx, contract.MethodMessageList,
		&contract.ListMessagesRequest{IdentityToken: identityToken, SessionID: sessionID, BeforeID: beforeID, Limit: limit}, out)
	return out, err
}

func AppendConversation(ctx context.Context, in *contract.AppendConversationRequest) (*contract.AppendConversationResponse, error) {
	out := &contract.AppendConversationResponse{}
	err := callUnary(ctx, contract.MethodConversationAppend, in, out)
	return out, err
}
