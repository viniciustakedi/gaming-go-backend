package httpapi

import (
	"net/http"
	"strings"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/auth"
)

const (
	codeUnauthorized = "UNAUTHORIZED"
	codeForbidden    = "FORBIDDEN"
)

// authenticate wraps next so that, for every request outside isPublic,
// routing itself never happens without a valid bearer token first. It runs
// ahead of the mux, not inside a matched route's own handler chain,
// precisely so a request under the business namespace that no route maps -
// an unsupported method, a typo'd path - is rejected as unauthorized rather
// than reaching the mux's own public 404/405. Role checks depend on which
// route matched, so requireRole (below) applies them afterward, inside the
// mux, only once a route has actually been found.
func authenticate(verifier auth.Verifier, isPublic func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublic(r) {
			next.ServeHTTP(w, r)
			return
		}

		token, ok := bearerToken(r)
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, codeUnauthorized, "missing or malformed bearer token")
			return
		}

		identity, err := verifier.Verify(r.Context(), token)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, codeUnauthorized, "invalid, unsigned or expired token")
			return
		}

		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), identity)))
	})
}

// requireRole wraps a single route's handler so it only runs for a caller
// that authenticate has already verified, and that carries role. A caller
// with the provider role but no provider_id claim is rejected as forbidden
// rather than unauthorized: the token itself verified fine, only the
// identity it carries is incomplete. The missing-identity branch only
// matters if a route were ever wired up without going through authenticate
// first - every route this package registers does.
func requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFromContext(r.Context())
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, codeUnauthorized, "missing or malformed bearer token")
			return
		}
		if identity.HasRole(auth.RoleProvider) && identity.ProviderID == "" {
			writeAuthError(w, http.StatusForbidden, codeForbidden, "provider token carries no provider_id claim")
			return
		}
		if !identity.HasRole(role) {
			writeAuthError(w, http.StatusForbidden, codeForbidden, "caller does not have the required role")
			return
		}

		next(w, r)
	}
}

// requireAnyRole is requireRole's multi-role sibling, for the two read
// routes both a provider and a wallet-admin may call. Which of the two the
// caller is decides its visibility rule inside the use case, not at this
// layer - this middleware only proves the caller has at least one of the
// roles the route accepts.
//
// Precedence, decided here and nowhere else: wallet-admin wins whenever the
// route accepts it, so a token carrying both roles is never asked for
// provider_id - it already sees everything as an admin. provider_id is only
// required from a caller acting as a provider, i.e. one that does not clear
// the route on wallet-admin alone.
func requireAnyRole(roles []string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFromContext(r.Context())
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, codeUnauthorized, "missing or malformed bearer token")
			return
		}
		if roleAccepted(roles, auth.RoleWalletAdmin) && identity.HasRole(auth.RoleWalletAdmin) {
			next(w, r)
			return
		}
		if roleAccepted(roles, auth.RoleProvider) && identity.HasRole(auth.RoleProvider) {
			if identity.ProviderID == "" {
				writeAuthError(w, http.StatusForbidden, codeForbidden, "provider token carries no provider_id claim")
				return
			}
			next(w, r)
			return
		}
		writeAuthError(w, http.StatusForbidden, codeForbidden, "caller does not have the required role")
	}
}

func roleAccepted(roles []string, role string) bool {
	for _, candidate := range roles {
		if candidate == role {
			return true
		}
	}
	return false
}

// bearerToken extracts the token from a well-formed "Authorization: Bearer
// <token>" header. The scheme is matched case-insensitively - HTTP auth
// schemes are case-insensitive per RFC 7235 §2.1, and Keycloak client
// libraries in the wild send every casing. It never logs the header or the
// token it returns.
func bearerToken(r *http.Request) (string, bool) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return "", false
	}
	return fields[1], true
}

// writeAuthError answers a 401/403 with the same error envelope every other
// rejection in this package uses (see errors.go's writeErrorEnvelope).
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	writeErrorEnvelope(w, status, errorBody{Error: errorDetail{
		Code:        code,
		Message:     message,
		Correctable: false,
		Details:     []errorDetailItem{},
	}})
}
