package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ai-chat-service/chat-server/data"
	"ai-chat-service/pkg/identity"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
)

type memKBCache struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemKBCache() *memKBCache { return &memKBCache{data: map[string]string{}} }

func (c *memKBCache) Get(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok, nil
}

func (c *memKBCache) SetImmutable(_ context.Context, key, value string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = value
	return nil
}

func knowledgeToken(t *testing.T, tenant, user uint64, role string) string {
	t.Helper()
	token, err := contract.SignIdentity(sessionTestSecret, contract.IdentityClaims{
		TenantID: tenant, UserID: user, Issuer: "answermesh-backend", Role: role,
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func newKnowledgeServiceWithDB(t *testing.T) (*knowledgeService, uint64) {
	t.Helper()
	db, tenant := setupServerDB(t)
	repo := data.NewKnowledgeRepository(db, nil)
	return &knowledgeService{
		repo:     repo,
		read:     data.NewKnowledgeService(repo, newMemKBCache(), time.Hour, nil),
		verifier: identity.NewVerifier(sessionTestSecret, "answermesh-backend"),
	}, tenant
}

// 聊天主链路：已发布知识优先命中，且缓存未命中时回源写不可变版本键。
func TestChatKnowledgePriority(t *testing.T) {
	db, tenant := setupServerDB(t)
	repo := data.NewKnowledgeRepository(db, nil)
	cache := newMemKBCache()
	read := data.NewKnowledgeService(repo, cache, time.Hour, nil)
	svc := &chatService{knowledge: read}
	ctx := context.Background()

	entryID, err := repo.CreateEntry(ctx, tenant, "what is mvcc", "db")
	if err != nil {
		t.Fatal(err)
	}
	// 未发布：不命中。
	claims := &contract.IdentityClaims{TenantID: tenant, UserID: 1}
	if _, ok := svc.knowledgeAnswer(ctx, claims, chatMeta{KnowledgeEntryID: entryID}); ok {
		t.Fatal("unpublished knowledge should not hit")
	}
	version, err := repo.SubmitVersion(ctx, tenant, entryID, "mvcc answer")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Review(ctx, tenant, entryID, version, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Publish(ctx, tenant, entryID, version, 1); err != nil {
		t.Fatal(err)
	}
	ans, ok := svc.knowledgeAnswer(ctx, claims, chatMeta{KnowledgeEntryID: entryID})
	if !ok || ans != "mvcc answer" {
		t.Fatalf("knowledge answer = %q, %v", ans, ok)
	}
	if _, hit, _ := cache.Get(ctx, data.PublishedAnswerKey(entryID, version)); !hit {
		t.Fatal("knowledge answer was not cached")
	}
	// 无身份（旧客户端）不读知识。
	if _, ok := svc.knowledgeAnswer(ctx, nil, chatMeta{KnowledgeEntryID: entryID}); ok {
		t.Fatal("nil claims should not read knowledge")
	}
}

func TestKnowledgeAdminFlow(t *testing.T) {
	svc, tenant := newKnowledgeServiceWithDB(t)
	admin := knowledgeToken(t, tenant, 1, "admin")
	user := knowledgeToken(t, tenant, 2, "user")

	// 普通用户不能创建/审核/发布。
	if _, err := callSession(t, nil, svc.create, contract.KnowledgeCreateRequest{
		IdentityToken: user, Question: "q",
	}); err == nil {
		t.Fatal("user create should be denied")
	} else if se, ok := err.(*zrpc.StatusError); !ok || se.Code != zrpc.CodeUnauthenticated {
		t.Fatalf("user create err = %v, want Unauthenticated", err)
	}

	out, err := callSession(t, nil, svc.create, contract.KnowledgeCreateRequest{
		IdentityToken: admin, Question: "什么是 MVCC？", SubjectKey: "db",
	})
	if err != nil {
		t.Fatalf("admin create: %v", err)
	}
	entryID := out.(*contract.KnowledgeCreateResponse).EntryID

	out, err = callSession(t, nil, svc.submit, contract.KnowledgeSubmitRequest{
		IdentityToken: admin, EntryID: entryID, Answer: "v1 answer",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	version := out.(*contract.KnowledgeSubmitResponse).Version
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}

	out, err = callSession(t, nil, svc.submit, contract.KnowledgeSubmitRequest{
		IdentityToken: admin, EntryID: entryID, Answer: "v2 answer",
	})
	if err != nil {
		t.Fatalf("submit v2: %v", err)
	}
	v2 := out.(*contract.KnowledgeSubmitResponse).Version

	// 未审核版本不可发布。
	if _, err := callSession(t, nil, svc.publish, contract.KnowledgePublishRequest{
		IdentityToken: admin, EntryID: entryID, Version: version,
	}); err == nil {
		t.Fatal("publish before review should fail")
	}

	for _, v := range []uint32{version, v2} {
		if _, err := callSession(t, nil, svc.review, contract.KnowledgeReviewRequest{
			IdentityToken: admin, EntryID: entryID, Version: v, Approve: true,
		}); err != nil {
			t.Fatalf("review v%d: %v", v, err)
		}
	}
	out, err = callSession(t, nil, svc.publish, contract.KnowledgePublishRequest{
		IdentityToken: admin, EntryID: entryID, Version: version,
	})
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	if got := out.(*contract.KnowledgePublishResponse).PublishedVersion; got != version {
		t.Fatalf("published = %d, want %d", got, version)
	}
	// 旧版本不能再覆盖新版本（先发 v2，再发 v1）。
	if _, err := callSession(t, nil, svc.publish, contract.KnowledgePublishRequest{
		IdentityToken: admin, EntryID: entryID, Version: v2,
	}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if _, err := callSession(t, nil, svc.publish, contract.KnowledgePublishRequest{
		IdentityToken: admin, EntryID: entryID, Version: version,
	}); err == nil {
		t.Fatal("stale publish should fail")
	}

	// 任意登录用户可读最新已发布版本。
	out, err = callSession(t, nil, svc.getPublished, contract.KnowledgeGetPublishedRequest{
		IdentityToken: user, EntryID: entryID,
	})
	if err != nil {
		t.Fatalf("get published: %v", err)
	}
	got := out.(*contract.KnowledgeGetPublishedResponse)
	if got.Version != v2 || got.Answer != "v2 answer" {
		t.Fatalf("published answer = %+v", got)
	}

	// 撤稿：普通用户无权限；管理员撤稿后读取变为 NotFound；旧版本禁止重发。
	if _, err := callSession(t, nil, svc.revoke, contract.KnowledgeRevokeRequest{
		IdentityToken: user, EntryID: entryID, ExpectedVersion: v2, Reason: "x",
	}); err == nil {
		t.Fatal("user revoke should be denied")
	}
	out, err = callSession(t, nil, svc.revoke, contract.KnowledgeRevokeRequest{
		IdentityToken: admin, EntryID: entryID, ExpectedVersion: v2, Reason: "policy",
	})
	if err != nil {
		t.Fatalf("admin revoke: %v", err)
	}
	if got := out.(*contract.KnowledgeRevokeResponse).RevokedVersion; got != v2 {
		t.Fatalf("revoked version = %d, want %d", got, v2)
	}
	if _, err := callSession(t, nil, svc.getPublished, contract.KnowledgeGetPublishedRequest{
		IdentityToken: user, EntryID: entryID,
	}); err == nil {
		t.Fatal("revoked knowledge should not be readable")
	}
	if _, err := callSession(t, nil, svc.publish, contract.KnowledgePublishRequest{
		IdentityToken: admin, EntryID: entryID, Version: version,
	}); err == nil {
		t.Fatal("revoked v1 must not be republished")
	}

	// 跨租户不可见。
	other := knowledgeToken(t, tenant+1, 2, "user")
	_, err = callSession(t, nil, svc.getPublished, contract.KnowledgeGetPublishedRequest{
		IdentityToken: other, EntryID: entryID,
	})
	var se *zrpc.StatusError
	if err == nil || !errors.As(err, &se) || se.Code != zrpc.CodeNotFound {
		t.Fatalf("cross-tenant read err = %v, want NotFound", err)
	}
}
