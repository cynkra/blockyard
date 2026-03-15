package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

// ExchangeVaultCredential handles POST /api/v1/credentials/vault.
// Accepts a session reference token (as Bearer auth), validates it,
// and returns a scoped OpenBao token.
//
// This endpoint does NOT use the standard API bearer token auth.
// The session reference token is its own authentication — it proves
// the caller was routed through the proxy to a specific worker.
func ExchangeVaultCredential(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Extract Bearer token
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"Missing Bearer token")
			return
		}
		rawToken := authHeader[7:]

		// 2. Decode and validate session token
		claims, err := auth.DecodeSessionToken(rawToken, srv.SessionTokenKey)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_token",
				"Invalid or expired session token")
			return
		}

		// 3. Verify worker exists and belongs to the claimed app
		worker, ok := srv.Workers.Get(claims.Wid)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid_token",
				"Worker not found")
			return
		}
		if worker.AppID != claims.App {
			writeError(w, http.StatusUnauthorized, "invalid_token",
				"Token app does not match worker")
			return
		}

		// 4. Exchange user identity for a scoped OpenBao token.
		if srv.VaultClient == nil {
			serviceUnavailable(w, "Credential service not configured")
			return
		}

		// Ensure the user's access token is fresh before using it
		// for vault login.
		userSession, freshErr := auth.EnsureFreshToken(r.Context(), srv.AuthDeps(), claims.Sub)
		if freshErr != nil || userSession == nil || userSession.AccessToken == "" {
			writeError(w, http.StatusUnauthorized, "session_expired",
				"User session not found or expired")
			return
		}

		vaultToken, ok := srv.VaultTokenCache.Get(claims.Sub)
		if !ok {
			ttl := srv.Config.Openbao.TokenTTL.Duration
			var loginErr error
			var loginTTL time.Duration
			vaultToken, loginTTL, loginErr = srv.VaultClient.JWTLogin(
				r.Context(),
				srv.Config.Openbao.JWTAuthPath,
				userSession.AccessToken,
			)
			if loginErr != nil {
				slog.Warn("credential exchange: vault login failed",
					"sub", claims.Sub, "error", loginErr)
				writeError(w, http.StatusBadGateway, "vault_error",
					"Failed to obtain vault token")
				return
			}
			if loginTTL != 0 {
				ttl = loginTTL
			}
			srv.VaultTokenCache.Set(claims.Sub, vaultToken, ttl)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"token": vaultToken,
			"ttl":   int(srv.Config.Openbao.TokenTTL.Duration.Seconds()),
		})
	}
}
