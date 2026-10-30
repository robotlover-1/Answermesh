package contract

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 服务间“用户身份令牌”契约（backend → ai-chat-service）。
//
// zRPC 的 AccessToken 只证明“调用方是受信服务”，不携带终端用户身份。
// 已登录的 backend 用共享密钥签发短时 token（默认 60s），服务端验签后得到
// tenant_id/user_id。属于业务 payload，不修改 zRPC 帧协议。
//
// 格式：v1.<base64url(payload JSON)>.<base64url(HMAC-SHA256)>

var (
	ErrIdentityToken   = errors.New("identity token invalid")
	ErrIdentityExpired = errors.New("identity token expired")
)

type IdentityClaims struct {
	TenantID uint64 `json:"tenant_id"`
	UserID   uint64 `json:"user_id"`
	Issuer   string `json:"iss,omitempty"`
	// Role 由 backend 依据管理员配置签发：user|admin。服务端据此做写操作授权。
	Role      string `json:"role,omitempty"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

const (
	identityVersion     = "v1"
	identityMaxTokenLen = 4096
	identityMinSecret   = 16
	identitySkewSeconds = 60
)

// SignIdentity 用 secrets 中的第一个密钥签名；逗号分隔可支持轮换（新密钥在前）。
func SignIdentity(secrets string, claims IdentityClaims) (string, error) {
	secret := firstSecret(secrets)
	if len(secret) < identityMinSecret {
		return "", fmt.Errorf("identity secret must be at least %d bytes", identityMinSecret)
	}
	if claims.TenantID == 0 || claims.UserID == 0 {
		return "", fmt.Errorf("%w: tenant_id/user_id required", ErrIdentityToken)
	}
	now := time.Now().Unix()
	if claims.IssuedAt == 0 {
		claims.IssuedAt = now
	}
	if claims.ExpiresAt == 0 {
		claims.ExpiresAt = now + 60
	}
	if claims.ExpiresAt <= claims.IssuedAt {
		return "", fmt.Errorf("%w: exp must be after iat", ErrIdentityToken)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := identityVersion + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyIdentity 用 secrets 中任意一个密钥验签（支持轮换），并检查有效期。
func VerifyIdentity(secrets, token string) (*IdentityClaims, error) {
	if token == "" || len(token) > identityMaxTokenLen {
		return nil, ErrIdentityToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != identityVersion {
		return nil, ErrIdentityToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrIdentityToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrIdentityToken
	}
	signingInput := parts[0] + "." + parts[1]

	matched := false
	for _, secret := range strings.Split(secrets, ",") {
		secret = strings.TrimSpace(secret)
		if len(secret) < identityMinSecret {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(signingInput))
		if hmac.Equal(mac.Sum(nil), sig) {
			matched = true
			break
		}
	}
	if !matched {
		return nil, ErrIdentityToken
	}

	var claims IdentityClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, ErrIdentityToken
	}
	if claims.TenantID == 0 || claims.UserID == 0 {
		return nil, fmt.Errorf("%w: missing tenant/user", ErrIdentityToken)
	}
	now := time.Now().Unix()
	if claims.ExpiresAt <= 0 || now > claims.ExpiresAt+identitySkewSeconds {
		return nil, ErrIdentityExpired
	}
	if claims.IssuedAt > now+identitySkewSeconds {
		return nil, fmt.Errorf("%w: iat in the future", ErrIdentityToken)
	}
	return &claims, nil
}

func firstSecret(secrets string) string {
	for _, s := range strings.Split(secrets, ",") {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}
