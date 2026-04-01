package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

// ExchangeBootstrapToken handles POST /api/v1/bootstrap.
//
// Exchanges a one-time bootstrap token for a real PAT. The bootstrap
// token is burned after the first successful exchange.
func ExchangeBootstrapToken(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Is bootstrapping configured?
		if len(srv.BootstrapTokenHash) == 0 {
			writeError(w, http.StatusNotFound, "not_found", "bootstrap not available")
			return
		}

		// Already redeemed?
		if srv.BootstrapRedeemed.Load() {
			writeError(w, http.StatusGone, "gone", "bootstrap token already redeemed")
			return
		}

		// Extract bearer token.
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthorized", "bearer token required")
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")

		// Constant-time comparison of hashes.
		hash := auth.HashPAT(token)
		if subtle.ConstantTimeCompare(hash, srv.BootstrapTokenHash) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bootstrap token")
			return
		}

		// Parse request body for PAT parameters.
		var body struct {
			Name      string `json:"name"`
			ExpiresIn string `json:"expires_in"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid request body")
			return
		}
		if body.Name == "" {
			body.Name = "bootstrap"
		}

		var expiresAt *string
		if body.ExpiresIn != "" {
			dur, ok := parseDuration(body.ExpiresIn)
			if !ok {
				badRequest(w, "invalid expires_in format, use e.g. '90d', '24h'")
				return
			}
			exp := time.Now().Add(dur).UTC().Format(time.RFC3339)
			expiresAt = &exp
		}

		// Ensure the initial admin user exists.
		sub := srv.Config.OIDC.InitialAdmin
		if _, err := srv.DB.UpsertUserWithRole(sub, "bootstrap@blockyard.local", "Bootstrap User", "admin"); err != nil {
			serverError(w, "failed to create bootstrap user")
			return
		}

		// Generate a real PAT.
		plaintext, patHash, err := auth.GeneratePAT()
		if err != nil {
			serverError(w, "failed to generate token")
			return
		}

		id := uuid.New().String()
		pat, err := srv.DB.CreatePAT(id, patHash, sub, body.Name, expiresAt)
		if err != nil {
			serverError(w, "failed to create token")
			return
		}

		// Burn the bootstrap token (atomic — only one caller wins).
		if !srv.BootstrapRedeemed.CompareAndSwap(false, true) {
			writeError(w, http.StatusGone, "gone", "bootstrap token already redeemed")
			return
		}

		// Persist the redeemed state: store the bootstrap hash as a
		// revoked PAT sentinel so it survives server restarts.
		revoked := time.Now().UTC().Format(time.RFC3339)
		srv.DB.CreatePAT("bootstrap-redeemed", hash, sub, "bootstrap-redeemed", &revoked) //nolint:errcheck

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id":         pat.ID,
			"name":       pat.Name,
			"token":      plaintext,
			"created_at": pat.CreatedAt,
			"expires_at": pat.ExpiresAt,
		})
	}
}
