// Package auth verifies the OAuth 2.0 bearer tokens a real Keycloak issues
// and turns them into the caller identity internal/httpapi's middleware and
// handlers reason about. It never touches an HTTP request or response - that
// stays in internal/httpapi.
package auth

import "context"

// Realm roles Keycloak's wallet realm assigns: provider service accounts get
// RoleProvider, the internal wallet service gets RoleWalletAdmin.
const (
	RoleProvider    = "provider"
	RoleWalletAdmin = "wallet-admin"
)

// Identity is the caller a verified token carries: its realm roles and,
// for a provider service account, the provider_id claim the realm's
// hardcoded-claim mapper stamps on every provider token. It is role-less for
// a token that carries no realm_access.roles at all.
type Identity struct {
	Subject    string
	Roles      []string
	ProviderID string
}

// HasRole reports whether role is among the realm roles the token carried.
func (i Identity) HasRole(role string) bool {
	for _, r := range i.Roles {
		if r == role {
			return true
		}
	}
	return false
}

type identityContextKey struct{}

// WithIdentity returns a copy of ctx carrying identity, for handlers and the
// use cases they call to read back with IdentityFromContext.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns the Identity a request's auth middleware
// attached, if any.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)
	return identity, ok
}
