package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
)

// AuthenticatedUser represents a validated user identity extracted
// from a session. Stored in the request context by the auth middleware.
type AuthenticatedUser struct {
	Sub         string
	AccessToken string
}

// contextKey is an unexported type for context keys in this package.
type contextKey struct{}

// userKey is the context key for AuthenticatedUser.
var userKey = contextKey{}

// UserFromContext retrieves the AuthenticatedUser from the request
// context, or nil if not present.
func UserFromContext(ctx context.Context) *AuthenticatedUser {
	u, _ := ctx.Value(userKey).(*AuthenticatedUser)
	return u
}

// ContextWithUser returns a new context carrying the given AuthenticatedUser.
func ContextWithUser(ctx context.Context, u *AuthenticatedUser) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// AppAuthMiddleware authenticates if possible, but does not require it.
// Public apps allow unauthenticated access; the proxy handler decides
// whether to allow or deny based on the app's access_type.
//
// When OIDC is not configured (v0 compat), the middleware passes
// all requests through unchanged (no identity in context).
func AppAuthMiddleware(deps *Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// If OIDC is not configured, pass through (v0 compat).
			if deps.SigningKey == nil {
				next.ServeHTTP(w, r)
				return
			}

			cookieValue := extractSessionCookie(r)
			if cookieValue == "" {
				// No session — proceed without identity.
				slog.Debug("auth: no session cookie")
				next.ServeHTTP(w, r)
				return
			}

			cookie, err := DecodeCookie(cookieValue, deps.SigningKey)
			if err != nil {
				slog.Debug("auth: invalid session cookie", "error", err)
				next.ServeHTTP(w, r)
				return
			}

			// Check cookie max-age.
			maxAge := int64(24 * 60 * 60)
			if deps.Config.OIDC != nil {
				maxAge = int64(deps.Config.OIDC.CookieMaxAge.Duration.Seconds())
			}
			if nowUnix()-cookie.IssuedAt > maxAge {
				slog.Debug("auth: session cookie expired", "sub", cookie.Sub)
				next.ServeHTTP(w, r)
				return
			}

			session := deps.UserSessions.Get(cookie.Sub)
			if session == nil {
				slog.Debug("auth: no server-side session", "sub", cookie.Sub)
				next.ServeHTTP(w, r)
				return
			}

			// Refresh access token if near expiry (within 60 seconds).
			session, err = EnsureFreshToken(r.Context(), deps, cookie.Sub)
			if err != nil {
				slog.Error("token refresh failed, removing session",
					"sub", cookie.Sub, "error", err)
				deps.UserSessions.Delete(cookie.Sub)
				next.ServeHTTP(w, r)
				return
			}

			user := &AuthenticatedUser{
				Sub:         cookie.Sub,
				AccessToken: session.AccessToken,
			}
			ctx := context.WithValue(r.Context(), userKey, user)

			// Look up user role from database.
			if deps.DB != nil {
				dbUser, err := deps.DB.GetUser(cookie.Sub)
				if err != nil {
					slog.Warn("failed to look up user for role",
						"sub", cookie.Sub, "error", err)
					// Fail closed: do not attach identity when DB is unreachable.
					next.ServeHTTP(w, r)
					return
				}
				role := RoleViewer // default
				if dbUser != nil && dbUser.Active {
					role = ParseRole(dbUser.Role)
				} else if dbUser != nil && !dbUser.Active {
					// Deactivated user — deny access.
					next.ServeHTTP(w, r)
					return
				}
				caller := &CallerIdentity{
					Sub:    cookie.Sub,
					Role:   role,
					Source: AuthSourceSession,
				}
				ctx = ContextWithCaller(ctx, caller)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractSessionCookie reads the blockyard_session cookie value from
// the request. Returns empty string if not found.
func extractSessionCookie(r *http.Request) string {
	for _, c := range r.Cookies() {
		if c.Name == "blockyard_session" {
			return c.Value
		}
	}
	return ""
}

// EnsureFreshToken checks if the user's access token is near expiry
// (within 60 seconds) and refreshes it if needed. Returns the current
// session, or an error if refresh fails. Thread-safe via per-user locks.
func EnsureFreshToken(ctx context.Context, deps *Deps, sub string) (*UserSession, error) {
	session := deps.UserSessions.Get(sub)
	if session == nil {
		return nil, fmt.Errorf("session not found for sub %q", sub)
	}

	if session.ExpiresAt-nowUnix() < 60 {
		lock := deps.UserSessions.RefreshLock(sub)
		lock.Lock()

		// Re-check after acquiring the lock.
		session = deps.UserSessions.Get(sub)
		needsRefresh := session == nil || session.ExpiresAt-nowUnix() < 60

		if needsRefresh {
			if err := refreshAccessToken(ctx, deps, sub); err != nil {
				lock.Unlock()
				return nil, err
			}
		}
		lock.Unlock()

		session = deps.UserSessions.Get(sub)
		if session == nil {
			return nil, fmt.Errorf("session lost after refresh for sub %q", sub)
		}
	}

	return session, nil
}

// refreshAccessToken exchanges the refresh token for a new access
// token and updates the server-side session.
func refreshAccessToken(ctx context.Context, deps *Deps, sub string) error {
	if deps.OIDCClient == nil {
		return fmt.Errorf("OIDC not configured")
	}

	session := deps.UserSessions.Get(sub)
	if session == nil {
		return fmt.Errorf("session not found for sub %q", sub)
	}

	newToken, err := deps.OIDCClient.RefreshToken(ctx, session.RefreshToken)
	if err != nil {
		return err
	}

	newExpiresAt := nowUnix() + 300
	if !newToken.Expiry.IsZero() {
		newExpiresAt = newToken.Expiry.Unix()
	}

	var newRefresh *string
	if newToken.RefreshToken != "" {
		newRefresh = &newToken.RefreshToken
	}

	deps.UserSessions.UpdateTokens(sub, newToken.AccessToken, newRefresh, newExpiresAt)
	return nil
}
