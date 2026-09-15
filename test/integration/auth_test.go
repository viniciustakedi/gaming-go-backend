//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// seam 3a - internal/httpapi's auth middleware (requireRole,
// internal/httpapi/auth_middleware.go) backed by internal/auth's real OIDC
// verifier against a real Keycloak (docker-compose.yml's keycloak service,
// deploy/keycloak/realm-wallet.json). Every scenario here proves the ticket
// 07 contract end to end: no token, an invalid signature or an expired
// token all answer 401; the right role missing (no realm role at all, or
// the provider role instead of wallet-admin) answers 403; none of the
// rejected calls ever create a wallet.

func requireNoWalletCreated(t *testing.T, ctx context.Context, h *appHarness, playerID string) {
	t.Helper()
	if got := countWalletsByPlayerAndCurrency(t, ctx, h, playerID, "BRL"); got != 0 {
		t.Errorf("wallets for player+currency after a denied request = %d, want 0", got)
	}
}

func TestAuth_MissingToken_Returns401AndCreatesNothing(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	resp, body := h.do(t, http.MethodPost, "/wallets",
		map[string]string{"Authorization": ""},
		openWalletBody(playerID, "10.00", "BRL"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", resp.StatusCode, body)
	}

	requireNoWalletCreated(t, ctx, h, playerID)
}

func TestAuth_InvalidSignature_Returns401AndCreatesNothing(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	tampered := h.adminToken[:len(h.adminToken)-4] + "abcd"

	resp, body := h.do(t, http.MethodPost, "/wallets",
		map[string]string{"Authorization": "Bearer " + tampered},
		openWalletBody(playerID, "10.00", "BRL"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", resp.StatusCode, body)
	}

	requireNoWalletCreated(t, ctx, h, playerID)
}

// TestAuth_ExpiredToken_Returns401AndCreatesNothing uses
// provider-a-short-lived, the realm client provisioned with a 2-second
// access token lifespan specifically for this scenario (spec, decision 7:
// "um client de provedor com token de vida curta"). It sleeps past both
// that lifespan and the verifier's own clock-skew tolerance
// (AUTH_CLOCK_SKEW, 5s by default - internal/config) before calling the
// API, so the token is expired well outside the tolerance the app is
// deliberately lenient about, not merely past its raw exp.
func TestAuth_ExpiredToken_Returns401AndCreatesNothing(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	token := fetchToken(t, shortLivedProviderClient())
	time.Sleep(9 * time.Second)

	resp, body := h.do(t, http.MethodPost, "/wallets",
		map[string]string{"Authorization": "Bearer " + token},
		openWalletBody(playerID, "10.00", "BRL"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", resp.StatusCode, body)
	}

	requireNoWalletCreated(t, ctx, h, playerID)
}

func TestAuth_ClientWithNoRole_Returns403AndCreatesNothing(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	token := fetchToken(t, noRolesClient())

	resp, body := h.do(t, http.MethodPost, "/wallets",
		map[string]string{"Authorization": "Bearer " + token},
		openWalletBody(playerID, "10.00", "BRL"))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
	}

	requireNoWalletCreated(t, ctx, h, playerID)
}

func TestAuth_ProviderRoleOnWallets_Returns403AndCreatesNothing(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	token := fetchToken(t, providerAClient())

	resp, body := h.do(t, http.MethodPost, "/wallets",
		map[string]string{"Authorization": "Bearer " + token},
		openWalletBody(playerID, "10.00", "BRL"))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
	}

	requireNoWalletCreated(t, ctx, h, playerID)
}

// TestAuth_ProviderRoleOnGetWallet_Returns403 proves the same wallet-admin
// restriction on the read side, against a wallet that genuinely exists -
// distinguishing "forbidden" from "not found" matters here, since a wrong
// implementation could accidentally leak existence through a 404 instead of
// a uniform 403 (spec: "sem ... vazamento de dados").
func TestAuth_ProviderRoleOnGetWallet_Returns403(t *testing.T) {
	h := newAppHarness(t)
	playerID := newUUID(t)

	created, createdBody := h.do(t, http.MethodPost, "/wallets", nil, openWalletBody(playerID, "10.00", "BRL"))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("setup: POST /wallets status = %d, want 201, body = %s", created.StatusCode, createdBody)
	}
	wallet := decodeWalletResponse(t, createdBody)

	token := fetchToken(t, providerAClient())
	resp, body := h.do(t, http.MethodGet, "/wallets/"+wallet.ID,
		map[string]string{"Authorization": "Bearer " + token}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
	}
}

// TestAuth_WalletAdmin_Passes is the positive control every negative
// scenario above is paired against: the same route, the same real Keycloak,
// a genuine wallet-service token, succeeding end to end.
func TestAuth_WalletAdmin_Passes(t *testing.T) {
	h := newAppHarness(t)
	playerID := newUUID(t)

	token := fetchToken(t, walletServiceClient())
	resp, body := h.do(t, http.MethodPost, "/wallets",
		map[string]string{"Authorization": "Bearer " + token},
		openWalletBody(playerID, "10.00", "BRL"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", resp.StatusCode, body)
	}
}

// TestAuth_ProviderBWithProviderID_StillForbiddenOnWallets proves the
// wallet-admin restriction holds regardless of which provider client is
// used, and that a well-formed provider_id claim does not somehow substitute
// for the missing role.
func TestAuth_ProviderBWithProviderID_StillForbiddenOnWallets(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	playerID := newUUID(t)

	token := fetchToken(t, providerBClient())

	resp, body := h.do(t, http.MethodPost, "/wallets",
		map[string]string{"Authorization": "Bearer " + token},
		openWalletBody(playerID, "10.00", "BRL"))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
	}

	requireNoWalletCreated(t, ctx, h, playerID)
}
