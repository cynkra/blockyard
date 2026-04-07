package api

import "github.com/cynkra/blockyard/internal/db"

// Swagger documentation types — these provide clean OpenAPI schemas for
// handlers that return map literals. They are referenced only from swag
// annotations in comments, so Go's unused checker cannot see the usage.

type bundleListResponse struct { //nolint:unused
	Bundles []db.BundleRow `json:"bundles"`
}

type appTagListResponse struct { //nolint:unused
	Tags []tagResponse `json:"tags"`
}

type sessionListResponse struct { //nolint:unused
	Sessions []db.SessionRow `json:"sessions"`
}

type currentUserResponse struct { //nolint:unused
	Sub   string `json:"sub"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

type stopAppResponse struct { //nolint:unused
	TaskID         string `json:"task_id,omitempty"`
	WorkerCount    int    `json:"worker_count,omitempty"`
	StoppedWorkers int    `json:"stopped_workers,omitempty"`
}

type taskStatusResponse struct { //nolint:unused
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type uploadBundleResponse struct { //nolint:unused
	BundleID string `json:"bundle_id"`
	TaskID   string `json:"task_id"`
}

type asyncTaskResponse struct { //nolint:unused
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

type vaultExchangeResponse struct { //nolint:unused
	Token string `json:"token"`
	TTL   int    `json:"ttl"`
}

type catalogResponse struct { //nolint:unused
	Items   []catalogItem `json:"items"`
	Total   int           `json:"total"`
	Page    int           `json:"page"`
	PerPage int           `json:"per_page" example:"20"`
}

type appResponseV2JSON struct { //nolint:unused
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Owner                string            `json:"owner"`
	AccessType           string            `json:"access_type"`
	ActiveBundle         *string           `json:"active_bundle"`
	MaxWorkersPerApp     *int              `json:"max_workers_per_app"`
	MaxSessionsPerWorker int               `json:"max_sessions_per_worker"`
	MemoryLimit          *string           `json:"memory_limit"`
	CPULimit             *float64          `json:"cpu_limit"`
	Title                *string           `json:"title"`
	Description          *string           `json:"description"`
	PreWarmedSessions    int               `json:"pre_warmed_sessions"`
	Enabled              bool              `json:"enabled"`
	RefreshSchedule      string            `json:"refresh_schedule"`
	Image                string            `json:"image"`
	Runtime              string            `json:"runtime"`
	DataMounts           []db.DataMountRow `json:"data_mounts"`
	CreatedAt            string            `json:"created_at"`
	UpdatedAt            string            `json:"updated_at"`
	Status               string            `json:"status"`
	Tags                 []string          `json:"tags"`
	Relation             string            `json:"relation,omitempty"`
}
