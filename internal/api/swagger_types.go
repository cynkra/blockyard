package api

import "github.com/cynkra/blockyard/internal/db"

// Swagger documentation types — these provide clean OpenAPI schemas for
// handlers that return map literals.

// bundleListResponse wraps a list of bundles.
type bundleListResponse struct {
	Bundles []db.BundleRow `json:"bundles"`
}

// appTagListResponse wraps a list of tags for an app.
type appTagListResponse struct {
	Tags []tagResponse `json:"tags"`
}

// sessionListResponse wraps a list of sessions.
type sessionListResponse struct {
	Sessions []db.SessionRow `json:"sessions"`
}

// currentUserResponse is the shape returned by GET /users/me.
type currentUserResponse struct {
	Sub   string `json:"sub"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// stopAppResponse is the shape returned by POST /apps/{id}/stop.
type stopAppResponse struct {
	TaskID         string `json:"task_id,omitempty"`
	WorkerCount    int    `json:"worker_count,omitempty"`
	StoppedWorkers int    `json:"stopped_workers,omitempty"`
}

// taskStatusResponse is the shape returned by GET /tasks/{taskID}.
type taskStatusResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// uploadBundleResponse is returned by POST /apps/{id}/bundles.
type uploadBundleResponse struct {
	BundleID string `json:"bundle_id"`
	TaskID   string `json:"task_id"`
}

// asyncTaskResponse is returned by endpoints that start background tasks.
type asyncTaskResponse struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

// vaultExchangeResponse is returned by POST /credentials/vault.
type vaultExchangeResponse struct {
	Token string `json:"token"`
	TTL   int    `json:"ttl"`
}

// catalogResponse is the shape returned by GET /catalog.
type catalogResponse struct {
	Items   []catalogItem `json:"items"`
	Total   int           `json:"total"`
	Page    int           `json:"page"`
	PerPage int           `json:"per_page" example:"20"`
}

// appResponseV2JSON documents the v2 app response shape for OpenAPI.
type appResponseV2JSON struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Owner                string   `json:"owner"`
	AccessType           string   `json:"access_type"`
	ActiveBundle         *string  `json:"active_bundle"`
	MaxWorkersPerApp     *int     `json:"max_workers_per_app"`
	MaxSessionsPerWorker int      `json:"max_sessions_per_worker"`
	MemoryLimit          *string  `json:"memory_limit"`
	CPULimit             *float64 `json:"cpu_limit"`
	Title                *string  `json:"title"`
	Description          *string  `json:"description"`
	PreWarmedSeats       int      `json:"pre_warmed_seats"`
	Enabled              bool     `json:"enabled"`
	RefreshSchedule      string   `json:"refresh_schedule"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
	Status               string   `json:"status"`
	Tags                 []string `json:"tags"`
	Relation             string   `json:"relation,omitempty"`
}
