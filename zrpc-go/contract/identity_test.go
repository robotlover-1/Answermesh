package contract

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const testIdentitySecret = "0123456789abcdef0123456789abcdef"

func TestIdentityRoundTrip(t *testing.T) {
	token, err := SignIdentity(testIdentitySecret, IdentityClaims{
		TenantID: 7, UserID: 42, Issuer: "backend", ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	claims, err := VerifyIdentity(testIdentitySecret, token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.TenantID != 7 || claims.UserID != 42 || claims.Issuer != "backend" {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestIdentityRejectsTamperAndWrongSecret(t *testing.T) {
	token, _ := SignIdentity(testIdentitySecret, IdentityClaims{TenantID: 1, UserID: 1})
	parts := strings.Split(token, ".")
	parts[1] = parts[1][:len(parts[1])-1] + "A"
	if _, err := VerifyIdentity(testIdentitySecret, strings.Join(parts, ".")); !errors.Is(err, ErrIdentityToken) {
		t.Fatalf("tampered token err = %v", err)
	}
	if _, err := VerifyIdentity("ffffffffffffffffffffffffffffffff", token); !errors.Is(err, ErrIdentityToken) {
		t.Fatalf("wrong secret err = %v", err)
	}
}

func TestIdentityExpiredAndSkew(t *testing.T) {
	expired, _ := SignIdentity(testIdentitySecret, IdentityClaims{
		TenantID: 1, UserID: 1,
		IssuedAt:  time.Now().Add(-10 * time.Minute).Unix(),
		ExpiresAt: time.Now().Add(-5 * time.Minute).Unix(),
	})
	if _, err := VerifyIdentity(testIdentitySecret, expired); !errors.Is(err, ErrIdentityExpired) {
		t.Fatalf("expired err = %v", err)
	}
}

func TestIdentitySecretRotation(t *testing.T) {
	old := &IdentityClaims{TenantID: 1, UserID: 2}
	token, _ := SignIdentity(testIdentitySecret, *old)
	if _, err := VerifyIdentity("newsecret-0123456789abcdef,"+testIdentitySecret, token); err != nil {
		t.Fatalf("rotation verify: %v", err)
	}
}

func TestIdentityValidation(t *testing.T) {
	if _, err := SignIdentity("short", IdentityClaims{TenantID: 1, UserID: 1}); err == nil {
		t.Fatal("short secret should be rejected")
	}
	if _, err := SignIdentity(testIdentitySecret, IdentityClaims{TenantID: 0, UserID: 1}); err == nil {
		t.Fatal("missing tenant should be rejected")
	}
	for _, bad := range []string{"", "v2.a.b", "v1.@@@.@@@", "v1", "a.b.c.d"} {
		if _, err := VerifyIdentity(testIdentitySecret, bad); err == nil {
			t.Fatalf("token %q should be rejected", bad)
		}
	}
}
