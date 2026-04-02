package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

const readyzCheckTimeout = 5 * time.Second

// isAuthenticated performs a lightweight auth check without invoking
// the full auth middleware. Used to gate detailed readyz output.
// Accepts PAT bearer tokens and session cookies.
func isAuthenticated(r *http.Request, srv *server.Server) bool {
	// Try PAT.
	token := extractBearerToken(r)
	if token != "" && strings.HasPrefix(token, "by_") {
		hash := auth.HashPAT(token)
		result, err := srv.DB.LookupPATByHash(hash)
		if err == nil && result != nil && !result.PAT.Revoked && result.User.Active {
			return true
		}
	}

	// Try session cookie.
	if srv.SigningKey != nil {
		cookieValue := extractSessionCookie(r)
		if cookieValue != "" {
			caller := authenticateFromCookie(srv, cookieValue)
			if caller != nil {
				return true
			}
		}
	}

	return false
}

// readyzHandler returns an HTTP handler that checks runtime dependencies.
// When trusted is true (management listener), per-component check details
// are always included in the response. When false (main listener), details
// are only shown to authenticated callers.
func readyzHandler(srv *server.Server, trusted bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if srv.Draining.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{
				"status": "draining",
			})
			return
		}

		checks := make(map[string]string)

		// Database
		func() {
			ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
			defer cancel()
			if err := srv.DB.Ping(ctx); err != nil {
				checks["database"] = "fail"
			} else {
				checks["database"] = "pass"
			}
		}()

		// Docker socket
		func() {
			ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
			defer cancel()
			if _, err := srv.Backend.ListManaged(ctx); err != nil {
				checks["docker"] = "fail"
			} else {
				checks["docker"] = "pass"
			}
		}()

		// IdP (OIDC discovery endpoint)
		if srv.Config.OIDC != nil {
			func() {
				ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
				defer cancel()
				if err := CheckIDP(ctx, srv); err != nil {
					checks["idp"] = "fail"
				} else {
					checks["idp"] = "pass"
				}
			}()
		}

		// Redis
		if srv.RedisClient != nil {
			func() {
				ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
				defer cancel()
				if err := srv.RedisClient.Ping(ctx); err != nil {
					checks["redis"] = "fail"
				} else {
					checks["redis"] = "pass"
				}
			}()
		}

		// OpenBao
		if srv.VaultClient != nil {
			func() {
				ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
				defer cancel()
				if err := srv.VaultClient.Health(ctx); err != nil {
					checks["openbao"] = "fail"
				} else {
					checks["openbao"] = "pass"
				}
			}()
		}

		// Vault token (AppRole renewal)
		if srv.VaultTokenHealthy != nil {
			if srv.VaultTokenHealthy() {
				checks["vault_token"] = "pass"
			} else {
				checks["vault_token"] = "fail"
			}
		}

		allOK := true
		for _, v := range checks {
			if v == "fail" {
				allOK = false
				break
			}
		}

		status := "ready"
		httpStatus := http.StatusOK
		if !allOK {
			status = "not_ready"
			httpStatus = http.StatusServiceUnavailable
			failed := make([]string, 0)
			for k, v := range checks {
				if v == "fail" {
					failed = append(failed, k)
				}
			}
			slog.Warn("readiness check failed", "failed_checks", failed)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)

		// On the management listener (trusted), always expose details.
		// On the main listener, only expose to authenticated callers.
		result := map[string]any{"status": status}
		if srv.Passive.Load() {
			result["mode"] = "passive"
		}
		if trusted || isAuthenticated(r, srv) {
			result["checks"] = checks
			if v := srv.UpdateAvailable.Load(); v != nil {
				result["update_available"] = *v
			}
		}
		json.NewEncoder(w).Encode(result)
	}
}

// CheckIDP verifies the IdP's discovery endpoint is reachable.
func CheckIDP(ctx context.Context, srv *server.Server) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.Config.OIDC.IssuerURL+"/.well-known/openid-configuration", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("idp returned %d", resp.StatusCode)
	}
	return nil
}
