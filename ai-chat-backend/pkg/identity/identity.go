// Package identity 为 backend 已登录用户签发短时身份令牌。
package identity

import (
	"errors"
	"time"

	"ai-chat-backend/pkg/config"
	"echo-zrpc-go/contract"
)

const tokenTTL = 60 * time.Second

// TokenForUser 使用共享 HMAC 密钥签发 tenant/user token；有效期 60s，
// 仅用于一次下游调用，避免长期凭证进入 payload/日志。
func TokenForUser(userID uint64) (string, error) {
	cnf := config.GetConfig()
	if cnf == nil || cnf.Identity.Secret == "" {
		return "", errors.New("identity secret is not configured (set AI_CHAT_IDENTITY_SECRET)")
	}
	tenantID := cnf.Identity.TenantID
	if tenantID == 0 {
		tenantID = 1
	}
	now := time.Now()
	return contract.SignIdentity(cnf.Identity.Secret, contract.IdentityClaims{
		TenantID:  tenantID,
		UserID:    userID,
		Issuer:    cnf.Identity.Issuer,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(tokenTTL).Unix(),
	})
}
