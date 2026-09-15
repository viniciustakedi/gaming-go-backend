package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
)

// startVerifierWithIssuer mirrors verifier_test.go's startVerifier, except it
// lets the test pin a public-facing expected issuer distinct from the
// discovery URL used to reach the fake OIDC provider - the exact split
// discoverWithRetry makes with oidc.InsecureIssuerURLContext (see
// verifier.go). No prior test in this package exercised issuerURL and
// discoveryURL actually differing, which is what the ticket 07 re-review
// flagged as untested.
func startVerifierWithIssuer(t *testing.T, provider *fakeOIDCProvider, issuerURL, audience string, clockSkew time.Duration) *OIDCVerifier {
	t.Helper()
	v := NewOIDCVerifier()
	cfg := config.Config{Auth: config.AuthConfig{
		IssuerURL:        issuerURL,
		DiscoveryURL:     provider.issuer(),
		Audience:         audience,
		ClockSkew:        clockSkew,
		DiscoveryTimeout: 5 * time.Second,
	}}

	lc := fxtest.NewLifecycle(t)
	RegisterLifecycle(lc, v, cfg, discardLogger())
	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("start verifier lifecycle: %v", err)
	}
	t.Cleanup(func() { _ = lc.Stop(context.Background()) })
	return v
}

// signHS256WithPublicKeyAsSecret hand-builds an HS256 JWT whose "secret" is
// the RSA public key the fake provider publishes in its JWKS - the classic
// RS256-to-HS256 algorithm-confusion attack, where an attacker who only ever
// sees the public key tries to forge a token by (ab)using it as an HMAC key.
// It must fail regardless of iss, because RegisterLifecycle pins
// SupportedSigningAlgs to just oidc.RS256.
func signHS256WithPublicKeyAsSecret(t *testing.T, pub *rsa.PublicKey, claims tokenClaims) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	secret := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	signature := mac.Sum(nil)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// TestVerify_IssuerSeparation proves that separating the expected issuer
// (public, in AUTH_ISSUER_URL) from the discovery/JWKS URL (internal, in
// AUTH_DISCOVERY_URL) does not loosen "iss" validation: only a token whose
// iss matches the configured public issuer, byte for byte, is accepted -
// never one carrying the internal discovery URL or an unrelated issuer, and
// never one signed with an algorithm other than RS256.
func TestVerify_IssuerSeparation(t *testing.T) {
	const publicIssuer = "http://public.example/realms/wallet"
	const thirdPartyIssuer = "http://attacker.example/realms/other"

	provider := newFakeOIDCProvider(t)
	v := startVerifierWithIssuer(t, provider, publicIssuer, "wallet-api", 5*time.Second)

	validClaims := func(iss string) tokenClaims {
		return tokenClaims{
			Issuer:   iss,
			Audience: "wallet-api",
			Subject:  "sub",
			Expiry:   unixPtr(time.Now().Add(5 * time.Minute)),
			IssuedAt: unixPtr(time.Now()),
		}
	}

	t.Run("token claiming the configured public issuer is accepted", func(t *testing.T) {
		token := provider.signToken(t, validClaims(publicIssuer))
		if _, err := v.Verify(context.Background(), token); err != nil {
			t.Fatalf("Verify: want the configured public issuer accepted, got: %v", err)
		}
	})

	t.Run("token claiming the internal discovery URL as iss is rejected", func(t *testing.T) {
		token := provider.signToken(t, validClaims(provider.issuer()))
		if _, err := v.Verify(context.Background(), token); err == nil {
			t.Fatal("Verify: want a token claiming the internal discovery/JWKS URL as iss rejected, got nil")
		}
	})

	t.Run("token claiming an unrelated third-party issuer is rejected", func(t *testing.T) {
		token := provider.signToken(t, validClaims(thirdPartyIssuer))
		if _, err := v.Verify(context.Background(), token); err == nil {
			t.Fatal("Verify: want a token from an unrelated issuer rejected, got nil")
		}
	})

	t.Run("HS256 token signed with the RSA public key as secret is rejected", func(t *testing.T) {
		token := signHS256WithPublicKeyAsSecret(t, &provider.key.PublicKey, validClaims(publicIssuer))
		if _, err := v.Verify(context.Background(), token); err == nil {
			t.Fatal("Verify: want an HS256-signed token rejected even with a valid iss, since RS256 is the only supported algorithm")
		}
	})
}
