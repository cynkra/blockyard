package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cynkra/blockyard/internal/server"
)

const readyzCheckTimeout = 5 * time.Second

// isAuthenticated performs a lightweight bearer token check without
// invoking the full auth middleware. Used to gate detailed readyz output.
func isAuthenticated(r *http.Request, srv *server.Server) bool {
	token := extractBearerToken(r)
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(srv.Config.Server.Token.Expose())) == 1
}

func readyzHandler(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
				if err := checkIDP(ctx, srv); err != nil {
					checks["idp"] = "fail"
				} else {
					checks["idp"] = "pass"
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
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)

		// Only expose per-component check details to authenticated callers.
		// Unauthenticated requests get status only (no service topology).
		result := map[string]any{"status": status}
		if isAuthenticated(r, srv) {
			result["checks"] = checks
		}
		json.NewEncoder(w).Encode(result)
	}
}

// checkIDP verifies the IdP's discovery endpoint is reachable.
func checkIDP(ctx context.Context, srv *server.Server) error {
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
