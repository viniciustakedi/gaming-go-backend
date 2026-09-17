package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
)

// Verifier turns a raw bearer token into the Identity it carries, or an
// error for anything that fails signature, issuer, audience or expiry
// checks.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Identity, error)
}

// keycloakClaims mirrors the token shapes this service reads out of an
// otherwise-opaque access token: the realm roles under realm_access.roles,
// the provider_id claim the realm's provider clients carry, and the
// exp/nbf/iat claims validateTimes checks. None of these fields is required
// to be present.
type keycloakClaims struct {
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	ProviderID string `json:"provider_id"`
	Expiry     *int64 `json:"exp"`
	NotBefore  *int64 `json:"nbf"`
	IssuedAt   *int64 `json:"iat"`
}

// OIDCVerifier is the production Verifier, backed by a real Keycloak
// discovered over OIDC. It is safe to use as soon as RegisterLifecycle's
// OnStart hook has completed discovery; before that, the zero value's nil
// idVerifier makes Verify fail closed rather than panic.
type OIDCVerifier struct {
	mu         sync.RWMutex
	idVerifier *oidc.IDTokenVerifier
	clockSkew  time.Duration
	// clock is the injectable "now" validateTimes checks exp/nbf/iat
	// against - time.Now in production, overridden by this package's own
	// tests.
	clock func() time.Time
}

// NewOIDCVerifier builds an OIDCVerifier with no I/O - discovery happens
// later, in RegisterLifecycle's OnStart hook.
func NewOIDCVerifier() *OIDCVerifier {
	return &OIDCVerifier{clock: time.Now}
}

// asVerifier exposes *OIDCVerifier through the Verifier interface, so
// internal/httpapi.New can depend on the interface rather than this
// package's concrete implementation.
func asVerifier(v *OIDCVerifier) Verifier {
	return v
}

var errVerifierNotReady = errors.New("auth: oidc verifier not ready (discovery has not completed)")

// Verify checks rawToken's signature, issuer and audience via go-oidc, then
// its exp/nbf/iat itself via validateTimes, and decodes its realm roles and
// provider_id claim into an Identity. It enforces no role or provider_id
// policy - that is internal/httpapi's concern, applied after Verify succeeds.
func (v *OIDCVerifier) Verify(ctx context.Context, rawToken string) (Identity, error) {
	v.mu.RLock()
	idVerifier := v.idVerifier
	clockSkew := v.clockSkew
	clock := v.clock
	v.mu.RUnlock()
	if idVerifier == nil {
		return Identity{}, errVerifierNotReady
	}

	token, err := idVerifier.Verify(ctx, rawToken)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: verify token: %w", err)
	}

	var claims keycloakClaims
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("auth: decode token claims: %w", err)
	}

	if err := validateTimes(claims, clock(), clockSkew); err != nil {
		return Identity{}, err
	}

	return Identity{
		Subject:    token.Subject,
		Roles:      claims.RealmAccess.Roles,
		ProviderID: claims.ProviderID,
	}, nil
}

// validateTimes enforces exp, nbf and iat itself rather than relying on
// go-oidc's built-in expiry check (RegisterLifecycle sets SkipExpiryCheck:
// true). go-oidc's own check hardcodes a 5-minute leeway on nbf regardless of
// the configured clock skew, which would accept a token up to 5 minutes
// before it is actually valid - far looser than this service's configured
// tolerance. exp is required; nbf and iat are checked only when present.
func validateTimes(claims keycloakClaims, now time.Time, clockSkew time.Duration) error {
	if claims.Expiry == nil {
		return errors.New("auth: token carries no exp claim")
	}
	if exp := time.Unix(*claims.Expiry, 0); now.After(exp.Add(clockSkew)) {
		return fmt.Errorf("auth: token expired at %s (now %s, tolerance %s)", exp, now, clockSkew)
	}
	if claims.NotBefore != nil {
		if nbf := time.Unix(*claims.NotBefore, 0); now.Add(clockSkew).Before(nbf) {
			return fmt.Errorf("auth: token not valid until %s (now %s, tolerance %s)", nbf, now, clockSkew)
		}
	}
	if claims.IssuedAt != nil {
		if iat := time.Unix(*claims.IssuedAt, 0); now.Add(clockSkew).Before(iat) {
			return fmt.Errorf("auth: token issued in the future at %s (now %s, tolerance %s)", iat, now, clockSkew)
		}
	}
	return nil
}

func (v *OIDCVerifier) setVerifier(idVerifier *oidc.IDTokenVerifier, clockSkew time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.idVerifier = idVerifier
	v.clockSkew = clockSkew
}

// RegisterLifecycle discovers the Keycloak realm's OIDC configuration
// (issuer, JWKS endpoint) on start, retrying with a short fixed backoff until
// discovery succeeds or cfg.Auth.DiscoveryTimeout elapses - Keycloak's own
// boot routinely takes longer than the app's other dependencies, so a single
// attempt would make every cold start a race. SupportedSigningAlgs is set
// explicitly rather than left to the provider's advertised default, so a
// misconfigured realm can never widen this to an insecure algorithm.
//
// SkipExpiryCheck is set because go-oidc's own exp/nbf check cannot be
// trusted with this service's clock-skew tolerance (see validateTimes) -
// Verify enforces exp/nbf/iat itself, with ClockSkew, once go-oidc's
// signature/issuer/audience checks have already passed.
func RegisterLifecycle(lc fx.Lifecycle, v *OIDCVerifier, cfg config.Config, logger *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, cfg.Auth.DiscoveryTimeout)
			defer cancel()

			provider, err := discoverWithRetry(ctx, cfg.Auth.DiscoveryURL, cfg.Auth.IssuerURL)
			if err != nil {
				return fmt.Errorf("auth: discover oidc provider (discovery %s, issuer %s): %w", cfg.Auth.DiscoveryURL, cfg.Auth.IssuerURL, err)
			}

			idVerifier := provider.Verifier(&oidc.Config{
				ClientID:             cfg.Auth.Audience,
				SupportedSigningAlgs: []string{oidc.RS256},
				SkipExpiryCheck:      true,
			})
			v.setVerifier(idVerifier, cfg.Auth.ClockSkew)

			logger.Info("oidc discovery complete", "issuer", cfg.Auth.IssuerURL, "discoveryUrl", cfg.Auth.DiscoveryURL, "audience", cfg.Auth.Audience)
			return nil
		},
	})
}

// discoverWithRetry keeps calling oidc.NewProvider against discoveryURL until
// it succeeds or ctx is done; a connection refused (Keycloak still booting)
// is retried, and ctx's deadline bounds how long start can take.
//
// discoveryURL and issuerURL are deliberately allowed to differ: the app
// container reaches Keycloak for discovery/JWKS over the Compose network
// (http://keycloak:8080/...), while the token's own "iss" claim is the
// publicly reachable issuer (http://localhost:<port>/...).
// oidc.InsecureIssuerURLContext is go-oidc's documented mechanism for that
// split and does not weaken the token's issuer check - the provider stays
// pinned to issuerURL and Verify still rejects any token whose "iss" does not
// match byte for byte; it only skips comparing issuerURL against the
// discovery document's self-reported issuer field.
func discoverWithRetry(ctx context.Context, discoveryURL, issuerURL string) (*oidc.Provider, error) {
	const backoff = 250 * time.Millisecond

	ctx = oidc.InsecureIssuerURLContext(ctx, issuerURL)

	var lastErr error
	for {
		provider, err := oidc.NewProvider(ctx, discoveryURL)
		if err == nil {
			return provider, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last attempt: %v)", ctx.Err(), lastErr)
		case <-time.After(backoff):
		}
	}
}
