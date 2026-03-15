package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

// APIAuth returns a chi middleware that authenticates control-plane requests.
//
// Authentication sources tried in order:
//  1. Session cookie (OIDC session)
//  2. PAT (Authorization: Bearer by_...)
//  3. Reject (401)
func APIAuth(srv *server.Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. Try session cookie (when OIDC is configured).
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

			// 2. Try bearer token (PAT).
			token := extractBearerToken(r)
			if token != "" {
				if strings.HasPrefix(token, "by_") {
					caller := authenticateFromPAT(srv, r, token)
					if caller != nil {
						ctx := auth.ContextWithCaller(r.Context(), caller)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
				return
			}

			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		})
	}
}

// authenticateFromPAT validates a PAT bearer token and returns a CallerIdentity.
func authenticateFromPAT(srv *server.Server, r *http.Request, token string) *auth.CallerIdentity {
	hash := auth.HashPAT(token)
	result, err := srv.DB.LookupPATByHash(hash)
	if err != nil {
		slog.Warn("PAT lookup error", "error", err)
		return nil
	}
	if result == nil {
		return nil
	}

	// Check revoked.
	if result.PAT.Revoked {
		slog.Debug("auth: PAT rejected (revoked)", "pat_id", result.PAT.ID)
		return nil
	}

	// Check expired.
	if result.PAT.ExpiresAt != nil {
		expiry, err := time.Parse(time.RFC3339, *result.PAT.ExpiresAt)
		if err == nil && time.Now().After(expiry) {
			slog.Debug("auth: PAT rejected (expired)", "pat_id", result.PAT.ID)
			return nil
		}
	}

	// Check user is active.
	if !result.User.Active {
		slog.Debug("auth: PAT rejected (user inactive)",
			"pat_id", result.PAT.ID, "sub", result.User.Sub)
		return nil
	}

	// Update last_used_at asynchronously. Use Background context because
	// the request context is cancelled after the handler returns.
	go srv.DB.UpdatePATLastUsed(context.Background(), result.PAT.ID)

	return &auth.CallerIdentity{
		Sub:    result.User.Sub,
		Role:   auth.ParseRole(result.User.Role),
		Source: auth.AuthSourcePAT,
	}
}

func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}
