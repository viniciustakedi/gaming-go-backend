package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
)

// fakeOIDCProvider is a minimal, in-process stand-in for Keycloak's own OIDC
// discovery and JWKS endpoints - real Keycloak is exercised by the seam 3a
// integration suite (test/integration); this package's own tests only need
// to prove OIDCVerifier's signature, issuer, audience, algorithm and
// clock-skew handling against a JWT it can construct and sign by hand, with
// no test-only JWT dependency beyond the stdlib (decision 8's allowed
// dependency list has none).
type fakeOIDCProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	p := &fakeOIDCProvider{key: key, kid: "test-key"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discoveryHandler)
	mux.HandleFunc("/jwks", p.jwksHandler)
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func (p *fakeOIDCProvider) issuer() string { return p.server.URL }

func (p *fakeOIDCProvider) discoveryHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                p.issuer(),
		"jwks_uri":                              p.issuer() + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (p *fakeOIDCProvider) jwksHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": p.kid,
			"n":   base64.RawURLEncoding.EncodeToString(p.key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(bigEndianExponent(p.key.PublicKey.E)),
		}},
	})
}

func bigEndianExponent(e int) []byte {
	// The standard exponent 65537 is 3 bytes big-endian (0x010001); this
	// helper only needs to handle that one value, since Go's rsa.GenerateKey
	// always uses it.
	b := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return b[i:]
}

type tokenClaims struct {
	Issuer      string `json:"iss"`
	Audience    string `json:"aud"`
	Subject     string `json:"sub"`
	Expiry      *int64 `json:"exp,omitempty"`
	IssuedAt    *int64 `json:"iat,omitempty"`
	NotBefore   *int64 `json:"nbf,omitempty"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
}

// unixPtr is the tokenClaims field helper every hand-written test claim set
// uses for exp/iat/nbf, since those fields are now pointers (see
// keycloakClaims in verifier.go) so a claim's absence - not just its value -
// is part of what a test can express.
func unixPtr(t time.Time) *int64 {
	v := t.Unix()
	return &v
}

// signToken hand-builds and RS256-signs a JWT carrying claims - the same
// shape a real Keycloak access token has (see deploy/keycloak/realm-wallet.json).
func (p *fakeOIDCProvider) signToken(t *testing.T, claims tokenClaims) string {
	t.Helper()
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": p.kid}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startVerifier runs RegisterLifecycle's OnStart hook against a fake
// provider, exactly as internal/app.New's real Fx graph would, and returns
// the OIDCVerifier once discovery has completed.
func startVerifier(t *testing.T, provider *fakeOIDCProvider, audience string, clockSkew time.Duration) *OIDCVerifier {
	t.Helper()
	v := NewOIDCVerifier()
	cfg := config.Config{Auth: config.AuthConfig{
		IssuerURL:        provider.issuer(),
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

func TestVerify_ValidToken_ExtractsRolesAndProviderID(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "wallet-api", Subject: "service-account-provider-a", Expiry: unixPtr(time.Now().Add(5 * time.Minute)), IssuedAt: unixPtr(time.Now())}
	claims.RealmAccess.Roles = []string{RoleProvider}
	claims.ProviderID = "provider-a"
	token := provider.signToken(t, claims)

	identity, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.Subject != "service-account-provider-a" || !identity.HasRole(RoleProvider) || identity.ProviderID != "provider-a" {
		t.Errorf("identity = %+v, want subject/role/providerId from the token", identity)
	}
}

func TestVerify_NoRoles_HasRoleFalse(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "wallet-api", Subject: "service-account-no-roles-client", Expiry: unixPtr(time.Now().Add(5 * time.Minute)), IssuedAt: unixPtr(time.Now())}
	token := provider.signToken(t, claims)

	identity, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.HasRole(RoleProvider) || identity.HasRole(RoleWalletAdmin) {
		t.Errorf("identity = %+v, want no roles", identity)
	}
}

func TestVerify_ExpiredToken_Rejected(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 2*time.Second)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "wallet-api", Subject: "sub", Expiry: unixPtr(time.Now().Add(-10 * time.Second)), IssuedAt: unixPtr(time.Now().Add(-1 * time.Hour))}
	token := provider.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("Verify: want an error for a token expired well beyond the clock skew, got nil")
	}
}

func TestVerify_ExpiredWithinClockSkew_Accepted(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "wallet-api", Subject: "sub", Expiry: unixPtr(time.Now().Add(-2 * time.Second)), IssuedAt: unixPtr(time.Now().Add(-1 * time.Minute))}
	token := provider.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify: want the %s clock-skew tolerance to accept a token expired 2s ago, got: %v", 5*time.Second, err)
	}
}

// fixedClockAt overrides v's injectable clock (see OIDCVerifier.clock) with
// a fixed instant, so validateTimes's boundary arithmetic (exp/nbf plus the
// configured tolerance) can be checked against hand-computed values instead
// of a real, jittery wall clock (ticket 07 review: "testes unitários com
// valores escritos à mão").
func fixedClockAt(v *OIDCVerifier, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clock = func() time.Time { return now }
}

func TestVerify_ExpiredOutsideTolerance_Rejected(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)
	now := time.Unix(1_700_000_000, 0)
	fixedClockAt(v, now)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "wallet-api", Subject: "sub", Expiry: unixPtr(now.Add(-6 * time.Second))}
	token := provider.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("Verify: want an error for exp 6s in the past against a 5s tolerance, got nil")
	}
}

func TestVerify_ExpiredWithinTolerance_Accepted(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)
	now := time.Unix(1_700_000_000, 0)
	fixedClockAt(v, now)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "wallet-api", Subject: "sub", Expiry: unixPtr(now.Add(-4 * time.Second))}
	token := provider.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify: want exp 4s in the past accepted within a 5s tolerance, got: %v", err)
	}
}

// TestVerify_NbfFutureOutsideTolerance_Rejected proves the fix for the
// review's exact scenario: go-oidc's own nbf check hardcodes a 5-minute
// leeway, so a genuine RS256 token with nbf 4 minutes in the future used to
// reach Verify with go-oidc's internal Now already shifted back by the
// configured (5s) tolerance and still pass. validateTimes replaces that
// check entirely (RegisterLifecycle sets SkipExpiryCheck: true), so this
// must now be rejected.
func TestVerify_NbfFutureOutsideTolerance_Rejected(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)
	now := time.Unix(1_700_000_000, 0)
	fixedClockAt(v, now)

	claims := tokenClaims{
		Issuer: provider.issuer(), Audience: "wallet-api", Subject: "sub",
		Expiry:    unixPtr(now.Add(10 * time.Minute)),
		NotBefore: unixPtr(now.Add(4 * time.Minute)),
	}
	token := provider.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("Verify: want an error for nbf 4 minutes in the future against a 5s tolerance, got nil")
	}
}

func TestVerify_NbfFutureWithinTolerance_Accepted(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)
	now := time.Unix(1_700_000_000, 0)
	fixedClockAt(v, now)

	claims := tokenClaims{
		Issuer: provider.issuer(), Audience: "wallet-api", Subject: "sub",
		Expiry:    unixPtr(now.Add(10 * time.Minute)),
		NotBefore: unixPtr(now.Add(3 * time.Second)),
	}
	token := provider.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify: want nbf 3s in the future accepted within a 5s tolerance, got: %v", err)
	}
}

func TestVerify_NoExpiryClaim_Rejected(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)
	now := time.Unix(1_700_000_000, 0)
	fixedClockAt(v, now)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "wallet-api", Subject: "sub"}
	token := provider.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("Verify: want an error for a token with no exp claim, got nil")
	}
}

func TestVerify_WrongAudience_Rejected(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "some-other-api", Subject: "sub", Expiry: unixPtr(time.Now().Add(5 * time.Minute)), IssuedAt: unixPtr(time.Now())}
	token := provider.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("Verify: want an error for a token with the wrong audience, got nil")
	}
}

func TestVerify_TamperedSignature_Rejected(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	v := startVerifier(t, provider, "wallet-api", 5*time.Second)

	claims := tokenClaims{Issuer: provider.issuer(), Audience: "wallet-api", Subject: "sub", Expiry: unixPtr(time.Now().Add(5 * time.Minute)), IssuedAt: unixPtr(time.Now())}
	token := provider.signToken(t, claims)
	tampered := token[:len(token)-4] + "abcd"

	if _, err := v.Verify(context.Background(), tampered); err == nil {
		t.Fatal("Verify: want an error for a tampered signature, got nil")
	}
}

func TestVerify_NotReady_FailsClosed(t *testing.T) {
	v := NewOIDCVerifier()
	if _, err := v.Verify(context.Background(), "whatever"); err == nil {
		t.Fatal("Verify: want an error before discovery has completed, got nil")
	}
}

func TestRegisterLifecycle_DiscoveryTimesOut_WhenIssuerUnreachable(t *testing.T) {
	v := NewOIDCVerifier()
	cfg := config.Config{Auth: config.AuthConfig{
		IssuerURL:        "http://127.0.0.1:1/realms/unreachable",
		DiscoveryURL:     "http://127.0.0.1:1/realms/unreachable",
		Audience:         "wallet-api",
		ClockSkew:        5 * time.Second,
		DiscoveryTimeout: 500 * time.Millisecond,
	}}

	lc := fxtest.NewLifecycle(t)
	RegisterLifecycle(lc, v, cfg, discardLogger())
	if err := lc.Start(context.Background()); err == nil {
		t.Fatal("Start: want discovery to fail against an unreachable issuer, got nil")
	}
}

func TestIdentity_HasRole(t *testing.T) {
	identity := Identity{Roles: []string{RoleProvider}}
	if !identity.HasRole(RoleProvider) {
		t.Error("HasRole(provider) = false, want true")
	}
	if identity.HasRole(RoleWalletAdmin) {
		t.Error("HasRole(wallet-admin) = true, want false")
	}
}

func TestIdentityContext_RoundTrip(t *testing.T) {
	want := Identity{Subject: "sub", Roles: []string{RoleWalletAdmin}}
	ctx := WithIdentity(context.Background(), want)

	got, ok := IdentityFromContext(ctx)
	if !ok || got.Subject != want.Subject || !got.HasRole(RoleWalletAdmin) {
		t.Errorf("IdentityFromContext() = (%+v, %v), want (%+v, true)", got, ok, want)
	}

	if _, ok := IdentityFromContext(context.Background()); ok {
		t.Error("IdentityFromContext() on a bare context = true, want false")
	}
}
