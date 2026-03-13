package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/server"
)

// serviceNameRe validates service names: alphanumeric, hyphens, underscores, 1-64 chars.
var serviceNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// UserAuth returns a middleware that authenticates via session cookie or
// JWT bearer token. Produces a CallerIdentity in context either way.
// Used for /api/v1/users/me/ routes where both app-plane and
// control-plane users need access.
func UserAuth(srv *server.Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Try session cookie first (app-plane users).
			if caller := auth.CallerFromContext(r.Context()); caller != nil {
				// Already authenticated by upstream middleware (shouldn't
				// happen with current routing, but defensive).
				next.ServeHTTP(w, r)
				return
			}

			deps := srv.AuthDeps()

			// Try session cookie.
			if deps.SigningKey != nil {
				cookieValue := extractSessionCookie(r)
				if cookieValue != "" {
					caller := authenticateFromCookie(deps, srv.RoleCache, cookieValue)
					if caller != nil {
						ctx := auth.ContextWithCaller(r.Context(), caller)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
			}

			// Try JWT bearer token.
			token := extractBearerToken(r)
			if token != "" {
				caller := authenticateFromBearer(srv, token)
				if caller != nil {
					ctx := auth.ContextWithCaller(r.Context(), caller)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		})
	}
}

// extractSessionCookie reads the blockyard_session cookie value.
func extractSessionCookie(r *http.Request) string {
	for _, c := range r.Cookies() {
		if c.Name == "blockyard_session" {
			return c.Value
		}
	}
	return ""
}

// authenticateFromCookie validates a session cookie and returns a CallerIdentity.
func authenticateFromCookie(deps *auth.Deps, roleCache *auth.RoleMappingCache, cookieValue string) *auth.CallerIdentity {
	cookie, err := auth.DecodeCookie(cookieValue, deps.SigningKey)
	if err != nil {
		return nil
	}

	maxAge := int64(24 * 60 * 60)
	if deps.Config.OIDC != nil {
		maxAge = int64(deps.Config.OIDC.CookieMaxAge.Duration.Seconds())
	}
	if auth.NowUnix()-cookie.IssuedAt > maxAge {
		return nil
	}

	session := deps.UserSessions.Get(cookie.Sub)
	if session == nil {
		return nil
	}

	return &auth.CallerIdentity{
		Sub:    cookie.Sub,
		Groups: session.Groups,
		Role:   auth.DeriveRole(session.Groups, roleCache),
		Source: auth.AuthSourceSession,
	}
}

// authenticateFromBearer validates a JWT bearer token and returns a CallerIdentity.
func authenticateFromBearer(srv *server.Server, token string) *auth.CallerIdentity {
	if srv.Config.OIDC != nil && srv.JWKSCache != nil {
		claims, err := srv.JWKSCache.Validate(
			token,
			srv.Config.OIDC.IssuerURL,
			srv.Config.OIDC.ClientID,
		)
		if err != nil {
			return nil
		}
		groups := claims.ExtractGroups(srv.Config.OIDC.GroupsClaim)
		return &auth.CallerIdentity{
			Sub:    claims.Subject,
			Groups: groups,
			Role:   auth.DeriveRole(groups, srv.RoleCache),
			Source: auth.AuthSourceJWT,
		}
	}

	// Static token fallback.
	if subtle.ConstantTimeCompare([]byte(token), []byte(srv.Config.Server.Token.Expose())) == 1 {
		return &auth.CallerIdentity{
			Sub:    "admin",
			Groups: nil,
			Role:   auth.RoleAdmin,
			Source: auth.AuthSourceStaticToken,
		}
	}
	return nil
}

// EnrollCredential handles POST /api/v1/users/me/credentials/{service}.
// Stores a user's credential in OpenBao's KV v2 store.
func EnrollCredential(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}

		if srv.VaultClient == nil {
			serviceUnavailable(w, "credential storage not configured")
			return
		}

		service := chi.URLParam(r, "service")
		if !serviceNameRe.MatchString(service) {
			badRequest(w, "invalid service name: must be 1-64 alphanumeric, hyphen, or underscore characters")
			return
		}

		var body struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid request body")
			return
		}
		if body.APIKey == "" {
			badRequest(w, "api_key is required")
			return
		}

		err := integration.EnrollCredential(
			r.Context(), srv.VaultClient,
			caller.Sub, service,
			map[string]any{"api_key": body.APIKey},
		)
		if err != nil {
			slog.Error("credential enrollment failed",
				"sub", caller.Sub, "service", service, "error", err)
			serverError(w, "failed to store credential")
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionCredentialEnroll, service, nil))
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
