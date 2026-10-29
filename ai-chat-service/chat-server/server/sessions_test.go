package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ai-chat-service/chat-server/data"
	"ai-chat-service/pkg/identity"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
)

const sessionTestSecret = "0123456789abcdef0123456789abcdef"

func newSessionServiceWithDB(t *testing.T) (*sessionService, uint64) {
	t.Helper()
	db, tenant := setupServerDB(t)
	return &sessionService{
		repo:     data.NewSessionRepository(db, nil),
		verifier: identity.NewVerifier(sessionTestSecret, "answermesh-backend"),
	}, tenant
}

func signedToken(t *testing.T, tenant, user uint64) string {
	t.Helper()
	token, err := contract.SignIdentity(sessionTestSecret, contract.IdentityClaims{
		TenantID: tenant, UserID: user, Issuer: "answermesh-backend",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func callSession(t *testing.T, svc *sessionService, h zrpc.UnaryHandler, req any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return h(context.Background(), raw)
}

func TestSessionAPIIdentityAndTenantIsolation(t *testing.T) {
	svc, tenant := newSessionServiceWithDB(t)

	// 无 token / 伪造 token 必须是 Unauthenticated。
	if _, err := callSession(t, svc, svc.create, contract.CreateSessionRequest{Title: "x"}); err == nil {
		t.Fatal("missing identity token should fail")
	} else if se, ok := err.(*zrpc.StatusError); !ok || se.Code != zrpc.CodeUnauthenticated {
		t.Fatalf("missing token err = %v", err)
	}
	bad, _ := contract.SignIdentity("ffffffffffffffffffffffffffffffff", contract.IdentityClaims{TenantID: tenant, UserID: 1})
	if _, err := callSession(t, svc, svc.create, contract.CreateSessionRequest{IdentityToken: bad, Title: "x"}); err == nil {
		t.Fatal("bad signature should fail")
	}

	token := signedToken(t, tenant, 7)
	out, err := callSession(t, svc, svc.create, contract.CreateSessionRequest{IdentityToken: token, Title: "hello"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created := out.(*contract.CreateSessionResponse)
	if created.SessionID == 0 {
		t.Fatal("session id is zero")
	}

	// 同一租户不同用户看不到该会话。
	otherUser := signedToken(t, tenant, 8)
	out, err = callSession(t, svc, svc.list, contract.ListSessionsRequest{IdentityToken: otherUser})
	if err != nil {
		t.Fatalf("other user list: %v", err)
	}
	if got := len(out.(*contract.ListSessionsResponse).Sessions); got != 0 {
		t.Fatalf("other user sees %d sessions", got)
	}
	_, err = callSession(t, svc, svc.listMessages, contract.ListMessagesRequest{
		IdentityToken: otherUser, SessionID: created.SessionID,
	})
	var statusErr *zrpc.StatusError
	if err == nil || !errors.As(err, &statusErr) || statusErr.Code != zrpc.CodeNotFound {
		t.Fatalf("other user list messages err = %v, want NotFound", err)
	}

	// 追加消息（幂等）并分页读取。
	appendReq := contract.AppendConversationRequest{
		IdentityToken: token, SessionID: created.SessionID, RequestID: "req-1",
		UserContent: "q", AssistantContent: "a", Source: "llm",
	}
	out, err = callSession(t, svc, svc.appendConversation, appendReq)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !out.(*contract.AppendConversationResponse).Created {
		t.Fatal("first append should create")
	}
	out, err = callSession(t, svc, svc.appendConversation, appendReq)
	if err != nil || out.(*contract.AppendConversationResponse).Created {
		t.Fatalf("second append = %+v, %v; want idempotent", out, err)
	}
	out, err = callSession(t, svc, svc.listMessages, contract.ListMessagesRequest{
		IdentityToken: token, SessionID: created.SessionID, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if got := len(out.(*contract.ListMessagesResponse).Messages); got != 2 {
		t.Fatalf("messages = %d, want 2", got)
	}

	// 删除后不可再读取。
	out, err = callSession(t, svc, svc.delete, contract.DeleteSessionRequest{
		IdentityToken: token, SessionID: created.SessionID,
	})
	if err != nil || !out.(*contract.DeleteSessionResponse).Deleted {
		t.Fatalf("delete = %+v, %v", out, err)
	}
	if _, err := callSession(t, svc, svc.listMessages, contract.ListMessagesRequest{
		IdentityToken: token, SessionID: created.SessionID,
	}); err == nil {
		t.Fatal("deleted session should not be readable")
	}
}

func TestIdentityVerifierIssuerAndConfig(t *testing.T) {
	v := identity.NewVerifier("", "answermesh-backend")
	if _, err := v.Verify("x"); !errors.Is(err, identity.ErrNotConfigured) {
		t.Fatalf("empty secret err = %v", err)
	}
	token, _ := contract.SignIdentity(sessionTestSecret, contract.IdentityClaims{
		TenantID: 1, UserID: 2, Issuer: "someone-else",
	})
	if _, err := identity.NewVerifier(sessionTestSecret, "answermesh-backend").Verify(token); !errors.Is(err, identity.ErrIssuer) {
		t.Fatalf("issuer mismatch err = %v", err)
	}
	if _, err := identity.NewVerifier(sessionTestSecret, "").Verify(token); err != nil {
		t.Fatalf("empty issuer should skip check: %v", err)
	}
}
