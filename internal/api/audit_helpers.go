package api

import (
	"net/http"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
)

// auditEntry builds an audit.Entry from the request context.
func auditEntry(r *http.Request, action audit.Action, target string, detail map[string]any) audit.Entry {
	actor := "anonymous"
	if caller := auth.CallerFromContext(r.Context()); caller != nil {
		actor = caller.Sub
	}
	return audit.Entry{
		Action:   action,
		Actor:    actor,
		Target:   target,
		Detail:   detail,
		SourceIP: r.RemoteAddr,
	}
}
