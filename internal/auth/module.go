package auth

import "go.uber.org/fx"

// Module provides the OIDC-backed Verifier and runs its discovery on start.
// It must be composed ahead of httpapi.Module in internal/app.Modules, so
// this package's OnStart hook - and thus discovery - runs before the HTTP
// listener ever accepts a connection.
var Module = fx.Module("auth",
	fx.Provide(NewOIDCVerifier),
	fx.Provide(asVerifier),
	fx.Invoke(RegisterLifecycle),
)
