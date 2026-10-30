// Package identity 校验 backend 签发的短时用户身份令牌。
package identity

import (
	"errors"
	"fmt"

	"echo-zrpc-go/contract"
)

var (
	ErrNotConfigured = errors.New("identity secret is not configured")
	ErrIssuer        = errors.New("identity token issuer mismatch")
)

type Verifier struct {
	secrets string
	issuer  string
}

// NewVerifier：secrets 支持逗号分隔轮换（新密钥在前）；issuer 为空则不校验。
func NewVerifier(secrets, issuer string) *Verifier {
	return &Verifier{secrets: secrets, issuer: issuer}
}

func (v *Verifier) Configured() bool { return v != nil && v.secrets != "" }

func (v *Verifier) Verify(token string) (*contract.IdentityClaims, error) {
	if !v.Configured() {
		return nil, ErrNotConfigured
	}
	claims, err := contract.VerifyIdentity(v.secrets, token)
	if err != nil {
		return nil, err
	}
	if v.issuer != "" && claims.Issuer != v.issuer {
		return nil, fmt.Errorf("%w: got %q want %q", ErrIssuer, claims.Issuer, v.issuer)
	}
	return claims, nil
}
