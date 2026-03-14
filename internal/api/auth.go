package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

// APIAuth returns a chi middleware that authenticates control-plane requests.
//
// When OIDC is configured:
//  1. Try session cookie first
//  2. Try JWT bearer token against the IdP's JWKS
//  3. Reject
//
// When OIDC is not configured (v0 compat / dev mode):
//  1. Extract Bearer token
//  2. Compare against static config token
//  3. Store CallerIdentity with Sub="admin", Role=RoleAdmin
func APIAuth(srv *server.Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Try session cookie first (when OIDC is configured).
			if srv.Config.OIDC != nil && srv.SigningKey != nil {
				cookieValue := extractSessionCookie(r)
				if cookieValue != "" {
					caller := authenticateFromCookie(srv, cookieValue)
					if caller != nil {
						ctx := auth.ContextWithCaller(r.Context(), caller)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
			}

			token := extractBearerToken(r)
			if token == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
				return
			}

			var identity *auth.CallerIdentity

			if srv.Config.OIDC != nil && srv.JWKSCache != nil {
				// JWT validation path
				claims, err := srv.JWKSCache.Validate(
					token,
					srv.Config.OIDC.IssuerURL,
					srv.Config.OIDC.ClientID,
				)
				if err != nil {
					slog.Debug("JWT validation failed", "error", err)
					writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
					return
				}

				// Look up user role from database.
				role := auth.RoleViewer
				if srv.DB != nil {
					dbUser, err := srv.DB.GetUser(claims.Subject)
					if err != nil {
						slog.Warn("failed to look up user role", "sub", claims.Subject, "error", err)
					} else if dbUser != nil && !dbUser.Active {
						writeError(w, http.StatusForbidden, "forbidden", "account deactivated")
						return
					} else if dbUser != nil {
						role = auth.ParseRole(dbUser.Role)
					}
				}

				identity = &auth.CallerIdentity{
					Sub:    claims.Subject,
					Role:   role,
					Source: auth.AuthSourceJWT,
				}
			} else {
				// Static token fallback
				if subtle.ConstantTimeCompare([]byte(token), []byte(srv.Config.Server.Token.Expose())) != 1 {
					writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
					return
				}

				identity = &auth.CallerIdentity{
					Sub:    "admin",
					Role:   auth.RoleAdmin,
					Source: auth.AuthSourceStaticToken,
				}
			}

			ctx := auth.ContextWithCaller(r.Context(), identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}
