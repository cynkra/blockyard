package server

import (
	"encoding/json"
)

// WorkerEnv builds the environment variable map for worker containers.
// Always sets BLOCKYARD_API_URL (needed for runtime package installs).
// Includes Vault/OpenBao integration vars when configured.
func WorkerEnv(srv *Server) map[string]string {
	env := map[string]string{
		"BLOCKYARD_API_URL": srv.InternalAPIURL(),
	}

	if srv.Config.Openbao != nil {
		env["VAULT_ADDR"] = srv.Config.Openbao.Address
		if len(srv.Config.Openbao.Services) > 0 {
			svcMap := make(map[string]string, len(srv.Config.Openbao.Services))
			for _, svc := range srv.Config.Openbao.Services {
				svcMap[svc.ID] = "apikeys/" + svc.ID
			}
			svcJSON, _ := json.Marshal(svcMap)
			env["BLOCKYARD_VAULT_SERVICES"] = string(svcJSON)
		}
	}

	// Board storage: inject PostgREST URL so R apps can discover it.
	if srv.Config.BoardStorage != nil && srv.Config.BoardStorage.PostgrestURL != "" {
		env["POSTGREST_URL"] = srv.Config.BoardStorage.PostgrestURL
	}

	return env
}
