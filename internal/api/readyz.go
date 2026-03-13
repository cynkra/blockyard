package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cynkra/blockyard/internal/server"
)

const readyzCheckTimeout = 5 * time.Second

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
		json.NewEncoder(w).Encode(map[string]any{
			"status": status,
			"checks": checks,
		})
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
