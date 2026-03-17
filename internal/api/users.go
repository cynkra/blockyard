package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/server"
)

// serviceNameRe validates service names: alphanumeric, hyphens, underscores, 1-64 chars.
var serviceNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// UserAuth returns a middleware that authenticates via session cookie or
// PAT bearer token. Produces a CallerIdentity in context either way.
// Used for /api/v1/users/me/ routes where both app-plane and
// control-plane users need access.
func UserAuth(srv *server.Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Already authenticated by upstream middleware (shouldn't
			// happen with current routing, but defensive).
			if caller := auth.CallerFromContext(r.Context()); caller != nil {
				next.ServeHTTP(w, r)
				return
			}

			// Try session cookie.
			if srv.SigningKey != nil {
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

			// Try bearer token (PAT).
			token := extractBearerToken(r)
			if token != "" && strings.HasPrefix(token, "by_") {
				caller := authenticateFromPAT(srv, r, token)
				if caller != nil {
					ctx := auth.ContextWithCaller(r.Context(), caller)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		})
	}
}

// extractSessionCookie reads the blockyard_session cookie value.
func extractSessionCookie(r *http.Request) string {
	for _, c := range r.Cookies() {
		if c.Name == "blockyard_session" {
			return c.Value
		}
	}
	return ""
}

// authenticateFromCookie validates a session cookie and returns a CallerIdentity.
func authenticateFromCookie(srv *server.Server, cookieValue string) *auth.CallerIdentity {
	deps := srv.AuthDeps()
	cookie, err := auth.DecodeCookie(cookieValue, deps.SigningKey)
	if err != nil {
		return nil
	}

	maxAge := int64(24 * 60 * 60)
	if deps.Config.OIDC != nil {
		maxAge = int64(deps.Config.OIDC.CookieMaxAge.Duration.Seconds())
	}
	if auth.NowUnix()-cookie.IssuedAt > maxAge {
		return nil
	}

	session := deps.UserSessions.Get(cookie.Sub)
	if session == nil {
		return nil
	}

	// Look up role from database.
	role := auth.RoleViewer
	name := ""
	if srv.DB != nil {
		dbUser, err := srv.DB.GetUser(cookie.Sub)
		if err != nil {
			slog.Warn("failed to look up user role", "sub", cookie.Sub, "error", err)
			return nil // fail closed: deny access when DB is unreachable
		}
		if dbUser != nil && !dbUser.Active {
			return nil // deactivated
		}
		if dbUser != nil {
			role = auth.ParseRole(dbUser.Role)
			name = dbUser.Name
		}
	}

	return &auth.CallerIdentity{
		Sub:    cookie.Sub,
		Name:   name,
		Role:   role,
		Source: auth.AuthSourceSession,
	}
}

// EnrollCredential handles POST /api/v1/users/me/credentials/{service}.
// Stores a user's credential in OpenBao's KV v2 store.
func EnrollCredential(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}

		if srv.VaultClient == nil {
			serviceUnavailable(w, "credential storage not configured")
			return
		}

		service := chi.URLParam(r, "service")
		if !serviceNameRe.MatchString(service) {
			badRequest(w, "invalid service name: must be 1-64 alphanumeric, hyphen, or underscore characters")
			return
		}

		var body struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid request body")
			return
		}
		if body.APIKey == "" {
			badRequest(w, "api_key is required")
			return
		}

		err := integration.EnrollCredential(
			r.Context(), srv.VaultClient,
			caller.Sub, service,
			map[string]any{"api_key": body.APIKey},
		)
		if err != nil {
			slog.Error("credential enrollment failed",
				"sub", caller.Sub, "service", service, "error", err)
			serverError(w, "failed to store credential")
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionCredentialEnroll, service, nil))
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// --- User management endpoints (admin only) ---

// ListUsers handles GET /api/v1/users — list all users.
func ListUsers(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		users, err := srv.DB.ListUsers()
		if err != nil {
			serverError(w, "failed to list users")
			return
		}

		if users == nil {
			users = []db.UserRow{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(users)
	}
}

// GetUser handles GET /api/v1/users/{sub} — get a single user.
func GetUser(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		sub := chi.URLParam(r, "sub")
		user, err := srv.DB.GetUser(sub)
		if err != nil {
			serverError(w, "failed to get user")
			return
		}
		if user == nil {
			notFound(w, "user not found")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)
	}
}

type updateUserRequest struct {
	Role   *string `json:"role,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

// UpdateUser handles PATCH /api/v1/users/{sub} — update a user's role or active status.
func UpdateUser(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		sub := chi.URLParam(r, "sub")

		// Prevent self-demotion/deactivation.
		if sub == caller.Sub {
			badRequest(w, "cannot modify your own account")
			return
		}

		var body updateUserRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid request body")
			return
		}

		if body.Role == nil && body.Active == nil {
			badRequest(w, "nothing to update")
			return
		}

		// Validate role if provided.
		if body.Role != nil {
			role := auth.ParseRole(*body.Role)
			if role == auth.RoleNone {
				badRequest(w, "invalid role '"+*body.Role+"', must be one of: admin, publisher, viewer")
				return
			}
		}

		update := db.UserUpdate{
			Role:   body.Role,
			Active: body.Active,
		}

		user, err := srv.DB.UpdateUser(sub, update)
		if err != nil {
			serverError(w, "failed to update user")
			return
		}
		if user == nil {
			notFound(w, "user not found")
			return
		}

		if srv.AuditLog != nil {
			detail := map[string]any{}
			if body.Role != nil {
				detail["role"] = *body.Role
			}
			if body.Active != nil {
				detail["active"] = *body.Active
			}
			srv.AuditLog.Emit(auditEntry(r, audit.ActionUserUpdate, sub, detail))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)
	}
}

// --- Personal Access Token endpoints ---

// durationRe parses duration strings like "90d", "24h", "30m".
var durationRe = regexp.MustCompile(`^(\d+)([dhm])$`)

func parseDuration(s string) (time.Duration, bool) {
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	switch m[2] {
	case "d":
		return time.Duration(n) * 24 * time.Hour, true
	case "h":
		return time.Duration(n) * time.Hour, true
	case "m":
		return time.Duration(n) * time.Minute, true
	}
	return 0, false
}

type createTokenRequest struct {
	Name      string `json:"name"`
	ExpiresIn string `json:"expires_in,omitempty"` // e.g. "90d"
}

type createTokenResponse struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Token     string  `json:"token"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at"`
}

// CreateToken handles POST /api/v1/users/me/tokens — create a new PAT.
// Session-only: PATs cannot create other PATs.
func CreateToken(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		if caller.Source != auth.AuthSourceSession {
			forbidden(w, "tokens can only be created via browser session")
			return
		}

		var body createTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid request body")
			return
		}
		if body.Name == "" {
			badRequest(w, "name is required")
			return
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

		plaintext, hash, err := auth.GeneratePAT()
		if err != nil {
			serverError(w, "failed to generate token")
			return
		}

		id := uuid.New().String()
		pat, err := srv.DB.CreatePAT(id, hash, caller.Sub, body.Name, expiresAt)
		if err != nil {
			serverError(w, "failed to create token")
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionTokenCreate, pat.ID,
				map[string]any{"name": body.Name}))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(createTokenResponse{
			ID:        pat.ID,
			Name:      pat.Name,
			Token:     plaintext,
			CreatedAt: pat.CreatedAt,
			ExpiresAt: pat.ExpiresAt,
		})
	}
}

// ListTokens handles GET /api/v1/users/me/tokens — list caller's PATs.
func ListTokens(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}

		pats, err := srv.DB.ListPATsByUser(caller.Sub)
		if err != nil {
			serverError(w, "failed to list tokens")
			return
		}

		if pats == nil {
			pats = []db.PATRow{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pats)
	}
}

// RevokeToken handles DELETE /api/v1/users/me/tokens/{id} — revoke a single PAT.
func RevokeToken(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}

		tokenID := chi.URLParam(r, "tokenID")
		revoked, err := srv.DB.RevokePAT(tokenID, caller.Sub)
		if err != nil {
			serverError(w, "failed to revoke token")
			return
		}
		if !revoked {
			notFound(w, "token not found")
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionTokenRevoke, tokenID, nil))
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// RevokeAllTokens handles DELETE /api/v1/users/me/tokens — revoke all PATs.
func RevokeAllTokens(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}

		n, err := srv.DB.RevokeAllPATs(caller.Sub)
		if err != nil {
			serverError(w, "failed to revoke tokens")
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionTokenRevokeAll, caller.Sub,
				map[string]any{"count": n}))
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
