package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/auth"
)

// fakeVerifier lets this package's tests drive authenticate's branches
// directly, without a real Keycloak - the seam 3a suite in test/integration
// already covers the real OIDC path end to end.
type fakeVerifier struct {
	identity auth.Identity
	err      error
}

func (f fakeVerifier) Verify(ctx context.Context, rawToken string) (auth.Identity, error) {
	return f.identity, f.err
}

func alwaysPublic(*http.Request) bool { return true }
func neverPublic(*http.Request) bool  { return false }

func TestAuthenticate_MissingAuthorizationHeader_Returns401(t *testing.T) {
	handler := authenticate(fakeVerifier{}, neverPublic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not run when the Authorization header is missing")
	}))

	req := httptest.NewRequest(http.MethodGet, "/wallets/x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	assertErrorCode(t, rec.Body.Bytes(), codeUnauthorized)
}

func TestAuthenticate_MalformedAuthorizationHeader_Returns401(t *testing.T) {
	handler := authenticate(fakeVerifier{}, neverPublic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not run for a malformed Authorization header")
	}))

	req := httptest.NewRequest(http.MethodGet, "/wallets/x", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthenticate_VerifyFails_Returns401(t *testing.T) {
	verifier := fakeVerifier{err: errors.New("invalid signature")}
	handler := authenticate(verifier, neverPublic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not run when Verify fails")
	}))

	req := httptest.NewRequest(http.MethodGet, "/wallets/x", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthenticate_Success_CallsNextWithIdentityInContext(t *testing.T) {
	verifier := fakeVerifier{identity: auth.Identity{Subject: "wallet-service-sub", Roles: []string{auth.RoleWalletAdmin}}}
	var gotIdentity auth.Identity
	var gotOK bool
	handler := authenticate(verifier, neverPublic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity, gotOK = auth.IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/wallets/x", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !gotOK || gotIdentity.Subject != "wallet-service-sub" {
		t.Errorf("identity in context = (%+v, %v), want the verified identity", gotIdentity, gotOK)
	}
}

// TestAuthenticate_PublicPath_SkipsVerificationEvenWithoutToken proves
// isPublic is checked before bearerToken/Verify ever run - the mechanism
// isPublicRoute (server.go) relies on to exempt /health/* and /metrics
// without requiring a token at all.
func TestAuthenticate_PublicPath_SkipsVerificationEvenWithoutToken(t *testing.T) {
	var nextRan bool
	handler := authenticate(fakeVerifier{err: errors.New("must not be called")}, alwaysPublic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextRan = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !nextRan {
		t.Fatalf("status = %d, nextRan = %v, want 200 and next to run for a public path with no token", rec.Code, nextRan)
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
		wantOK bool
	}{
		{"canonical casing", "Bearer abc123", "abc123", true},
		{"lowercase scheme", "bearer abc123", "abc123", true},
		{"uppercase scheme", "BEARER abc123", "abc123", true},
		{"mixed casing", "BeArEr abc123", "abc123", true},
		{"multiple spaces between scheme and token", "Bearer    abc123", "abc123", true},
		{"tab between scheme and token", "Bearer\tabc123", "abc123", true},
		{"leading and trailing whitespace", "  Bearer abc123  ", "abc123", true},
		{"empty header", "", "", false},
		{"scheme only, no token", "Bearer", "", false},
		{"scheme with trailing space, no token", "Bearer ", "", false},
		{"different scheme", "Basic abc123", "", false},
		{"more than one token", "Bearer abc123 def456", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/wallets", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}

			got, ok := bearerToken(req)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// contextWithIdentity mirrors what authenticate attaches to a request's
// context once Verify has already succeeded - requireRole's tests start
// from there directly, since authenticate's own tests (above) already cover
// everything upstream of it.
func requestWithIdentity(identity auth.Identity) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/wallets/x", nil)
	return req.WithContext(auth.WithIdentity(req.Context(), identity))
}

func TestRequireRole_NoIdentityInContext_Returns401(t *testing.T) {
	handler := requireRole(auth.RoleWalletAdmin, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not run without an authenticated identity in context")
	})

	req := httptest.NewRequest(http.MethodGet, "/wallets/x", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestRequireRole_ProviderWithoutProviderID_Returns403 proves ticket 07's
// own requirement directly: "Provedor sem provider_id devolve 403" - a
// token that verifies fine but carries the provider role with no
// provider_id claim is forbidden, not unauthorized. No fixture in the real
// Keycloak realm produces this shape (every provider client there does
// carry provider_id), so this scenario is only reachable through a fake
// identity.
func TestRequireRole_ProviderWithoutProviderID_Returns403(t *testing.T) {
	handler := requireRole(auth.RoleWalletAdmin, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not run for a provider identity missing provider_id")
	})

	req := requestWithIdentity(auth.Identity{Subject: "sub", Roles: []string{auth.RoleProvider}})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	assertErrorCode(t, rec.Body.Bytes(), codeForbidden)
}

func TestRequireRole_MissingRequiredRole_Returns403(t *testing.T) {
	handler := requireRole(auth.RoleWalletAdmin, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not run when the caller lacks the required role")
	})

	req := requestWithIdentity(auth.Identity{Subject: "sub", Roles: []string{auth.RoleProvider}, ProviderID: "provider-a"})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRequireRole_Success_CallsNext(t *testing.T) {
	var nextRan bool
	handler := requireRole(auth.RoleWalletAdmin, func(w http.ResponseWriter, r *http.Request) {
		nextRan = true
		w.WriteHeader(http.StatusOK)
	})

	req := requestWithIdentity(auth.Identity{Subject: "wallet-service-sub", Roles: []string{auth.RoleWalletAdmin}})
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK || !nextRan {
		t.Fatalf("status = %d, nextRan = %v, want 200 and next to run for a caller with the required role", rec.Code, nextRan)
	}
}

func assertErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var decoded errorBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if decoded.Error.Code != want {
		t.Errorf("error code = %q, want %q", decoded.Error.Code, want)
	}
	if decoded.Error.Correctable {
		t.Error("Correctable = true, want false for an auth rejection")
	}
}
