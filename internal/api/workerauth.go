package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/cynkra/blockyard/internal/auth"
)

type contextKey int

const (
	workerIDKey contextKey = iota
	appIDKey
)

// WorkerIDFromContext extracts the worker ID set by WorkerAuth middleware.
func WorkerIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(workerIDKey).(string)
	return v
}

// AppIDFromContext extracts the app ID set by WorkerAuth middleware.
func AppIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(appIDKey).(string)
	return v
}

// WorkerAuth validates the worker HMAC token and injects worker/app IDs
// into the request context.
func WorkerAuth(signingKey *auth.SigningKey) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get("Authorization")
			if token == "" {
				http.Error(w, "missing worker token", http.StatusUnauthorized)
				return
			}
			token = strings.TrimPrefix(token, "Bearer ")

			claims, err := auth.DecodeSessionToken(token, signingKey)
			if err != nil {
				http.Error(w, "invalid or expired worker token",
					http.StatusUnauthorized)
				return
			}
			if !strings.HasPrefix(claims.Sub, "worker:") {
				http.Error(w, "not a worker token", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), workerIDKey, claims.Wid)
			ctx = context.WithValue(ctx, appIDKey, claims.App)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
