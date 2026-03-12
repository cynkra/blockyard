package api

import (
	"net/http"
	"strings"

	"github.com/cynkra/blockyard/internal/server"
)

// BearerAuth returns a chi middleware that validates the bearer token.
func BearerAuth(srv *server.Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			token, found := strings.CutPrefix(auth, "Bearer ")
			if !found || token != srv.Config.Server.Token {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
