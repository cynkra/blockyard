package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server    ServerConfig     `toml:"server"`
	Docker    DockerConfig     `toml:"docker"`
	Storage   StorageConfig    `toml:"storage"`
	Database  DatabaseConfig   `toml:"database"`
	Proxy     ProxyConfig      `toml:"proxy"`
	Process      *ProcessConfig      `toml:"process"`       // nil when backend != "process"
	Redis        *RedisConfig        `toml:"redis"`         // nil when not configured
	OIDC         *OidcConfig         `toml:"oidc"`          // nil when not configured
	Openbao      *OpenbaoConfig      `toml:"openbao"`       // nil when not configured
	Audit        *AuditConfig        `toml:"audit"`         // nil when not configured
	Telemetry    *TelemetryConfig    `toml:"telemetry"`     // nil when not configured
	Update       *UpdateConfig       `toml:"update"`        // nil when not configured

	// ConfigPath is the filesystem path to the config file this
	// Config was loaded from. Populated by main.go after Load so the
	// process orchestrator can re-exec the new blockyard with the
	// same --config flag. Not a TOML field — no struct tag.
	ConfigPath string `toml:"-"`
}

type RedisConfig struct {
	URL       string `toml:"url"`        // redis://[:password@]host:port[/db]
	KeyPrefix string `toml:"key_prefix"` // default: "blockyard:"
}

type AuditConfig struct {
	Path string `toml:"path"` // e.g. /data/audit/blockyard.jsonl
}

type TelemetryConfig struct {
	MetricsEnabled bool   `toml:"metrics_enabled"` // default: false
	OTLPEndpoint   string `toml:"otlp_endpoint"`   // e.g. http://otel-collector:4317
}

type UpdateConfig struct {
	Schedule    string   `toml:"schedule"`      // cron expression; empty = disabled
	Channel     string   `toml:"channel"`       // "stable" (default) or "main"
	WatchPeriod Duration `toml:"watch_period"`  // health monitoring after update completes

	// Repo is the GitHub owner/repo to query for releases and the
	// origin/main HEAD comparison (e.g. "cynkra/blockyard"). Empty
	// keeps the upstream default. Operators of forks override this
	// to point the update check at their own repo.
	Repo string `toml:"repo"`

	// AltBindRange is the port range the process orchestrator picks
	// an alternate bind from when spawning the new server during a
	// rolling update. Operator-configured, separate from
	// [process] port_range (worker pool). Default: "8090-8099".
	// Ignored by the Docker variant.
	AltBindRange string `toml:"alt_bind_range"`

	// DrainIdleWait is the maximum time Finish will wait for the
	// local server's session count to reach zero before tearing
	// down. Used by the process orchestrator to let active sessions
	// finish naturally during a rolling-update drain. Default: 5m.
	// Ignored by the Docker variant, which cuts over hard and
	// relies on the reverse proxy to drain in-flight requests.
	DrainIdleWait Duration `toml:"drain_idle_wait"`
}

type ServerConfig struct {
	Bind            string   `toml:"bind"`
	ManagementBind  string   `toml:"management_bind"`  // optional: separate listener for /healthz, /readyz, /metrics
	SessionSecret   *Secret  `toml:"session_secret"`   // required when [oidc] is set
	ExternalURL     string   `toml:"external_url"`
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
	DrainTimeout    Duration `toml:"drain_timeout"`
	LogLevel             string   `toml:"log_level"`              // debug, info, warn, error (default: info)
	TrustedProxies       []string `toml:"trusted_proxies"`        // CIDRs whose X-Forwarded-For to trust (e.g. ["10.0.0.0/8"])
	Backend              string   `toml:"backend"`                // "docker" (default) or "process"
	SkipPreflight        bool     `toml:"skip_preflight"`         // skip backend-specific preflight checks at startup
	DefaultMemoryLimit   string   `toml:"default_memory_limit"`   // fallback memory limit for workers (e.g. "2g")
	DefaultCPULimit      float64  `toml:"default_cpu_limit"`      // fallback CPU limit for workers (fractional vCPUs)
	BootstrapToken       string   `toml:"bootstrap_token"`        // dev only: one-time token exchanged for a real PAT via POST /api/v1/bootstrap
	WorkerEnv            map[string]string `toml:"worker_env"`  // extra env vars injected into every worker (e.g. OTEL_EXPORTER_OTLP_ENDPOINT)

	// Deprecated; copied into SkipPreflight by migrateDeprecatedFields and
	// removed in the next release.
	DeprecatedSkipDockerPreflight bool `toml:"skip_docker_preflight"`
}

// ProcessConfig configures the process (bubblewrap) backend.
type ProcessConfig struct {
	BwrapPath      string `toml:"bwrap_path"`             // path to bubblewrap binary
	RPath          string `toml:"r_path"`                 // path to R binary
	SeccompProfile string `toml:"seccomp_profile"`        // path to compiled BPF seccomp profile; empty = no seccomp
	PortRangeStart int    `toml:"port_range_start"`       // first port for workers (inclusive)
	PortRangeEnd   int    `toml:"port_range_end"`         // last port for workers (inclusive)
	WorkerUIDStart int    `toml:"worker_uid_range_start"` // first host UID for workers (inclusive)
	WorkerUIDEnd   int    `toml:"worker_uid_range_end"`   // last host UID for workers (inclusive)
	WorkerGID      int    `toml:"worker_gid"`             // shared host GID for all workers (used by egress firewall rules)
}

type DockerConfig struct {
	Socket          string            `toml:"socket"`
	Image           string            `toml:"image"`
	ShinyPort       int               `toml:"shiny_port"`
	PakVersion      string            `toml:"pak_version"`      // "stable" (default), or pinned version
	ServiceNetwork  string            `toml:"service_network"`  // Docker network whose containers are made reachable from workers
	Runtime         string            `toml:"runtime"`          // OCI runtime; empty = Docker daemon default
	RuntimeDefaults map[string]string `toml:"runtime_defaults"` // per-access-type defaults (e.g. public=kata-runtime)

	// Deprecated; copied into the new locations by migrateDeprecatedFields
	// and removed in the next release.
	DeprecatedDefaultMemoryLimit string   `toml:"default_memory_limit"`
	DeprecatedDefaultCPULimit    float64  `toml:"default_cpu_limit"`
	DeprecatedStoreRetention     Duration `toml:"store_retention"`
}

type StorageConfig struct {
	BundleServerPath    string            `toml:"bundle_server_path"`
	BundleWorkerPath    string            `toml:"bundle_worker_path"`
	BundleRetention     int               `toml:"bundle_retention"`
	MaxBundleSize       int64             `toml:"max_bundle_size"`
	SoftDeleteRetention Duration          `toml:"soft_delete_retention"`
	StoreRetention      Duration          `toml:"store_retention"` // R library cache eviction; 0 = disabled
	DataMounts          []DataMountSource `toml:"data_mounts"`
}

// DataMountSource defines an admin-approved mount source.
// The Name is referenced by apps; the Path is the host-side location.
type DataMountSource struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
}

type DatabaseConfig struct {
	Driver string `toml:"driver"` // "sqlite" (default) or "postgres"
	Path   string `toml:"path"`   // used when driver = "sqlite"
	URL    string `toml:"url"`    // PostgreSQL connection string; used when driver = "postgres"

	// Vault DB secrets engine integration (postgres only).
	//
	// VaultMount names the secrets engine mount path. Shared between
	// admin credential issuance (#238) and per-user board-storage
	// credentials (#283, #284). Default "database".
	//
	// VaultRole, when set, routes the admin connection through vault:
	// blockyard reads `{VaultMount}/static-creds/{VaultRole}` at
	// startup instead of using a static Database.URL password.
	// Requires [openbao].
	//
	// VaultDBConnection names the vault database-engine connection
	// blockyard targets when registering per-user static roles
	// (#284) — the `{name}` from `{VaultMount}/config/{name}` the
	// operator created at deploy time. Required when BoardStorage
	// is true. Passed verbatim as the `db_name` field of
	// `POST {mount}/static-roles/{name}`.
	//
	// BoardStorage enables the board-storage feature: adds a PG16+
	// preflight at startup and (in #284) drives per-user role
	// provisioning. Requires driver = "postgres" and [openbao].
	VaultMount        string `toml:"vault_mount"`
	VaultRole         string `toml:"vault_role"`
	VaultDBConnection string `toml:"vault_db_connection"`
	BoardStorage      bool   `toml:"board_storage"`
}

type ProxyConfig struct {
	WsCacheTTL         Duration `toml:"ws_cache_ttl"`
	HealthInterval     Duration `toml:"health_interval"`
	WorkerStartTimeout Duration `toml:"worker_start_timeout"`
	MaxWorkers         int      `toml:"max_workers"`
	LogRetention       Duration `toml:"log_retention"`
	SessionIdleTTL     Duration `toml:"session_idle_ttl"`
	IdleWorkerTimeout  Duration `toml:"idle_worker_timeout"`
	HTTPForwardTimeout Duration `toml:"http_forward_timeout"`
	MaxCPULimit        *float64 `toml:"max_cpu_limit"`
	TransferTimeout    Duration `toml:"transfer_timeout"`    // default 60s when unset
	SessionMaxLifetime Duration `toml:"session_max_lifetime"` // 0 = unlimited (default); hard cap on session duration
	// SessionStore selects the sticky-session backend. Empty = "auto":
	// "layered" when both [redis] and database.driver=postgres are set,
	// "redis" when only [redis] is set, "postgres" when only postgres
	// is configured, "memory" otherwise.
	SessionStore SessionStoreMode `toml:"session_store"`
}

// SessionStoreMode is the selector for proxy.session_store. Despite
// the "Session" name, the same value also drives the worker registry,
// worker map, and process-backend port/UID allocators (see #286, #287,
// #288, parent #262) — operators rarely want asymmetric durability
// across these stores, so the resolver below picks one mode for all of
// them.
type SessionStoreMode string

const (
	SessionStoreAuto     SessionStoreMode = ""
	SessionStoreMemory   SessionStoreMode = "memory"
	SessionStoreRedis    SessionStoreMode = "redis"
	SessionStorePostgres SessionStoreMode = "postgres"
	SessionStoreLayered  SessionStoreMode = "layered"
)

// ResolveSessionStoreMode picks the shared-state backend. Honours an
// explicit cfg.Proxy.SessionStore value; otherwise defaults to the
// "best" available mode given which backends are configured.
//
//   - [redis] + postgres  → layered (PG primary, Redis cache)
//   - [redis] only        → redis
//   - postgres only       → postgres
//   - neither             → memory (single-process only)
//
// Lives in the config package so both cmd/blockyard/main.go and the
// process backend's allocator selection can reach it without importing
// each other.
func ResolveSessionStoreMode(cfg *Config) SessionStoreMode {
	if cfg.Proxy.SessionStore != SessionStoreAuto {
		return cfg.Proxy.SessionStore
	}
	hasRedis := cfg.Redis != nil
	hasPG := cfg.Database.Driver == "postgres"
	switch {
	case hasRedis && hasPG:
		return SessionStoreLayered
	case hasRedis:
		return SessionStoreRedis
	case hasPG:
		return SessionStorePostgres
	default:
		return SessionStoreMemory
	}
}

type OidcConfig struct {
	IssuerURL         string   `toml:"issuer_url"`
	IssuerDiscoveryURL string  `toml:"issuer_discovery_url"` // optional: use a different URL for OIDC discovery (e.g. Docker-internal DNS)
	ClientID          string   `toml:"client_id"`
	ClientSecret      Secret   `toml:"client_secret"`
	CookieMaxAge      Duration `toml:"cookie_max_age"`
	InitialAdmin      string   `toml:"initial_admin"`
	DefaultRole       string   `toml:"default_role"` // role assigned on first login: "viewer" (default) or "publisher"
}

type OpenbaoConfig struct {
	Address              string          `toml:"address"`
	AdminToken           Secret          `toml:"admin_token"`              // deprecated: use role_id with AppRole auth
	RoleID               string          `toml:"role_id"`                  // AppRole role identifier
	TokenTTL             Duration        `toml:"token_ttl"`                // default: 1h
	JWTAuthPath          string          `toml:"jwt_auth_path"`            // default: "jwt"
	TokenFile            string          `toml:"token_file"`               // persisted vault token path; default: "/data/.vault-token"
	SkipPolicyScopeCheck bool            `toml:"skip_policy_scope_check"`
	Services             []ServiceConfig `toml:"services"`
}

// ServiceConfig describes a third-party service whose API key users
// can enroll via OpenBao (e.g. OpenAI, Posit Connect).
//
// Credentials are stored at: secret/data/users/{sub}/apikeys/{id}
type ServiceConfig struct {
	ID    string `toml:"id"`
	Label string `toml:"label"`
}

// Duration wraps time.Duration for TOML deserialization from strings
// like "30s", "5m", "1h".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: reads config from user-specified path
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	migrateDeprecatedFields(&cfg)
	applyDefaults(&cfg)
	applyEnvOverrides(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// migrateDeprecatedFields copies values from old [docker]/[server] field
// locations into their new homes when the new field is unset and the
// old field is present. Emits a deprecation warning for each move.
// Called once from Load(), between toml.Unmarshal and applyDefaults.
func migrateDeprecatedFields(cfg *Config) {
	if cfg.Server.DefaultMemoryLimit == "" && cfg.Docker.DeprecatedDefaultMemoryLimit != "" {
		cfg.Server.DefaultMemoryLimit = cfg.Docker.DeprecatedDefaultMemoryLimit
		slog.Warn("config: docker.default_memory_limit is deprecated; use server.default_memory_limit")
	}
	if cfg.Server.DefaultCPULimit == 0 && cfg.Docker.DeprecatedDefaultCPULimit != 0 {
		cfg.Server.DefaultCPULimit = cfg.Docker.DeprecatedDefaultCPULimit
		slog.Warn("config: docker.default_cpu_limit is deprecated; use server.default_cpu_limit")
	}
	if cfg.Storage.StoreRetention.Duration == 0 && cfg.Docker.DeprecatedStoreRetention.Duration != 0 {
		cfg.Storage.StoreRetention = cfg.Docker.DeprecatedStoreRetention
		slog.Warn("config: docker.store_retention is deprecated; use storage.store_retention")
	}
	if !cfg.Server.SkipPreflight && cfg.Server.DeprecatedSkipDockerPreflight {
		cfg.Server.SkipPreflight = true
		slog.Warn("config: server.skip_docker_preflight is deprecated; use server.skip_preflight")
	}
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = "127.0.0.1:8080"
	}
	if cfg.Server.Backend == "" {
		cfg.Server.Backend = "docker"
	}
	if cfg.Server.ShutdownTimeout.Duration == 0 {
		cfg.Server.ShutdownTimeout.Duration = 30 * time.Second
	}
	if cfg.Server.DrainTimeout.Duration == 0 {
		cfg.Server.DrainTimeout.Duration = 30 * time.Second
	}
	if cfg.Process != nil {
		processDefaults(cfg.Process)
	}
	if cfg.Docker.Socket == "" {
		cfg.Docker.Socket = "/var/run/docker.sock"
	}
	if cfg.Docker.ShinyPort == 0 {
		cfg.Docker.ShinyPort = 3838
	}
	if cfg.Docker.PakVersion == "" {
		cfg.Docker.PakVersion = "stable"
	}
	if cfg.Storage.BundleServerPath == "" {
		cfg.Storage.BundleServerPath = "/data/bundles"
	}
	if cfg.Database.Driver == "" {
		cfg.Database.Driver = "sqlite"
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "/data/db/blockyard.db"
	}
	if cfg.Database.VaultMount == "" {
		cfg.Database.VaultMount = "database"
	}
	if cfg.Storage.BundleWorkerPath == "" {
		cfg.Storage.BundleWorkerPath = "/app"
	}
	if cfg.Storage.BundleRetention == 0 {
		cfg.Storage.BundleRetention = 50
	}
	if cfg.Storage.MaxBundleSize == 0 {
		cfg.Storage.MaxBundleSize = 104857600 // 100 MiB
	}
	if cfg.Proxy.WsCacheTTL.Duration == 0 {
		cfg.Proxy.WsCacheTTL.Duration = 60 * time.Second
	}
	if cfg.Proxy.HealthInterval.Duration == 0 {
		cfg.Proxy.HealthInterval.Duration = 15 * time.Second
	}
	if cfg.Proxy.WorkerStartTimeout.Duration == 0 {
		cfg.Proxy.WorkerStartTimeout.Duration = 60 * time.Second
	}
	if cfg.Proxy.MaxWorkers == 0 {
		cfg.Proxy.MaxWorkers = 100
	}
	if cfg.Proxy.LogRetention.Duration == 0 {
		cfg.Proxy.LogRetention.Duration = 1 * time.Hour
	}
	// session_idle_ttl defaults to 0 (disabled). When non-zero, idle
	// WebSocket connections are closed and stale session records are
	// swept after this duration of inactivity.
	if cfg.Proxy.IdleWorkerTimeout.Duration == 0 {
		cfg.Proxy.IdleWorkerTimeout.Duration = 5 * time.Minute
	}
	if cfg.Proxy.HTTPForwardTimeout.Duration == 0 {
		cfg.Proxy.HTTPForwardTimeout.Duration = 5 * time.Minute
	}
	if cfg.Proxy.MaxCPULimit == nil {
		v := 16.0
		cfg.Proxy.MaxCPULimit = &v
	}
	if cfg.Update != nil {
		updateDefaults(cfg.Update)
	}
	if cfg.Redis != nil {
		redisDefaults(cfg.Redis)
	}
	if cfg.OIDC != nil {
		oidcDefaults(cfg.OIDC)
	}
	if cfg.Openbao != nil {
		openbaoDefaults(cfg.Openbao)
	}
}

func updateDefaults(c *UpdateConfig) {
	if c.Channel == "" {
		c.Channel = "stable"
	}
	if c.WatchPeriod.Duration == 0 {
		c.WatchPeriod.Duration = 5 * time.Minute
	}
	if c.AltBindRange == "" {
		c.AltBindRange = "8090-8099"
	}
	if c.DrainIdleWait.Duration == 0 {
		c.DrainIdleWait.Duration = 5 * time.Minute
	}
}

func redisDefaults(c *RedisConfig) {
	if c.KeyPrefix == "" {
		c.KeyPrefix = "blockyard:"
	}
	if !strings.HasSuffix(c.KeyPrefix, ":") {
		c.KeyPrefix += ":"
	}
}

func oidcDefaults(c *OidcConfig) {
	if c.CookieMaxAge.Duration == 0 {
		c.CookieMaxAge.Duration = 24 * time.Hour
	}
	if c.DefaultRole == "" {
		c.DefaultRole = "viewer"
	}
}

func openbaoDefaults(c *OpenbaoConfig) {
	if c.TokenTTL.Duration == 0 {
		c.TokenTTL.Duration = 1 * time.Hour
	}
	if c.JWTAuthPath == "" {
		c.JWTAuthPath = "jwt"
	}
	if c.TokenFile == "" {
		// Lives at /data/.vault-token so it survives restarts regardless
		// of database.driver. Previously derived from database.path,
		// which broke Postgres deployments that don't mount /data/db/.
		c.TokenFile = "/data/.vault-token"
	}
}

func processDefaults(c *ProcessConfig) {
	if c.BwrapPath == "" {
		c.BwrapPath = "/usr/bin/bwrap"
	}
	if c.RPath == "" {
		c.RPath = "/usr/bin/R"
	}
	if c.PortRangeStart == 0 {
		c.PortRangeStart = 10000
	}
	if c.PortRangeEnd == 0 {
		c.PortRangeEnd = 10999
	}
	if c.WorkerUIDStart == 0 {
		c.WorkerUIDStart = 60000
	}
	if c.WorkerUIDEnd == 0 {
		c.WorkerUIDEnd = 60999
	}
	if c.WorkerGID == 0 {
		c.WorkerGID = 65534 // nogroup
	}
}

// applyEnvOverrides walks cfg via reflection, deriving the env var name
// from toml struct tags (BLOCKYARD_ + section + _ + field, uppercased).
// Supported field types: string, int, int64, float64, Duration, Secret, *Secret.
func applyEnvOverrides(cfg *Config) {
	// Auto-construct [oidc] section if any BLOCKYARD_OIDC_* env var is set.
	if cfg.OIDC == nil && envPrefixExists("BLOCKYARD_OIDC_") {
		cfg.OIDC = &OidcConfig{}
		oidcDefaults(cfg.OIDC)
	}

	// Auto-construct [openbao] section if any BLOCKYARD_OPENBAO_* env var is set.
	if cfg.Openbao == nil && envPrefixExists("BLOCKYARD_OPENBAO_") {
		cfg.Openbao = &OpenbaoConfig{}
		openbaoDefaults(cfg.Openbao)
	}

	// Auto-construct [redis] section if any BLOCKYARD_REDIS_* env var is set.
	if cfg.Redis == nil && envPrefixExists("BLOCKYARD_REDIS_") {
		cfg.Redis = &RedisConfig{}
		redisDefaults(cfg.Redis)
	}

	// Auto-construct [audit] section if any BLOCKYARD_AUDIT_* env var is set.
	if cfg.Audit == nil && envPrefixExists("BLOCKYARD_AUDIT_") {
		cfg.Audit = &AuditConfig{}
	}

	// Auto-construct [telemetry] section if any BLOCKYARD_TELEMETRY_* env var is set.
	if cfg.Telemetry == nil && envPrefixExists("BLOCKYARD_TELEMETRY_") {
		cfg.Telemetry = &TelemetryConfig{}
	}

	// Auto-construct [update] section if any BLOCKYARD_UPDATE_* env var is set.
	if cfg.Update == nil && envPrefixExists("BLOCKYARD_UPDATE_") {
		cfg.Update = &UpdateConfig{}
		updateDefaults(cfg.Update)
	}

	// Auto-construct [process] section if any BLOCKYARD_PROCESS_* env var is set.
	if cfg.Process == nil && envPrefixExists("BLOCKYARD_PROCESS_") {
		cfg.Process = &ProcessConfig{}
		processDefaults(cfg.Process)
	}

	applyEnvToStruct(reflect.ValueOf(cfg).Elem(), "BLOCKYARD")
}

// envPrefixExists returns true if any environment variable starts with
// the given prefix.
func envPrefixExists(prefix string) bool {
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, prefix) {
			return true
		}
	}
	return false
}

var (
	durationType         = reflect.TypeOf(Duration{})
	secretType           = reflect.TypeOf(Secret{})
	secretPtrType        = reflect.TypeOf((*Secret)(nil))
	stringSliceType      = reflect.TypeOf([]string{})
	dataMountSliceType   = reflect.TypeOf([]DataMountSource{})
	mapStringStringType  = reflect.TypeOf(map[string]string{})
)

func applyEnvToStruct(v reflect.Value, prefix string) {
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		envName := prefix + "_" + strings.ToUpper(tag)
		fv := v.Field(i)

		// Pointer-to-struct: dereference if non-nil and recurse.
		if field.Type.Kind() == reflect.Ptr && field.Type.Elem().Kind() == reflect.Struct {
			// Skip *Secret — handled below as a leaf.
			if field.Type == secretPtrType {
				val, ok := os.LookupEnv(envName)
				if !ok {
					continue
				}
				s := NewSecret(val)
				fv.Set(reflect.ValueOf(&s))
				continue
			}
			if fv.IsNil() {
				continue
			}
			applyEnvToStruct(fv.Elem(), envName)
			continue
		}

		// Pointer-to-primitive (e.g. *float64 for MaxCPULimit):
		// parse the env var and set the pointer.
		if field.Type.Kind() == reflect.Ptr {
			val, ok := os.LookupEnv(envName)
			if !ok {
				continue
			}
			switch field.Type.Elem().Kind() {
			case reflect.Float64:
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					fv.Set(reflect.ValueOf(&f))
				}
			case reflect.Int:
				if n, err := strconv.Atoi(val); err == nil {
					fv.Set(reflect.ValueOf(&n))
				}
			case reflect.Int64:
				if n, err := strconv.ParseInt(val, 10, 64); err == nil {
					fv.Set(reflect.ValueOf(&n))
				}
			case reflect.Bool:
				if b, err := strconv.ParseBool(val); err == nil {
					fv.Set(reflect.ValueOf(&b))
				}
			case reflect.String:
				fv.Set(reflect.ValueOf(&val))
			}
			continue
		}

		// Recurse into nested config sections (but not Duration/Secret,
		// which are struct wrappers).
		if field.Type.Kind() == reflect.Struct && field.Type != durationType && field.Type != secretType {
			applyEnvToStruct(fv, envName)
			continue
		}

		// []DataMountSource: semicolon-separated name:path pairs.
		if fv.Type() == dataMountSliceType {
			val, ok := os.LookupEnv(envName)
			if !ok || val == "" {
				continue
			}
			var mounts []DataMountSource
			for _, entry := range strings.Split(val, ";") {
				entry = strings.TrimSpace(entry)
				if entry == "" {
					continue
				}
				name, path, ok := strings.Cut(entry, ":")
				if !ok {
					continue
				}
				mounts = append(mounts, DataMountSource{
					Name: strings.TrimSpace(name),
					Path: strings.TrimSpace(path),
				})
			}
			fv.Set(reflect.ValueOf(mounts))
			continue
		}

		// map[string]string: semicolon-separated key:value pairs.
		if fv.Type() == mapStringStringType {
			val, ok := os.LookupEnv(envName)
			if !ok || val == "" {
				continue
			}
			m := make(map[string]string)
			for _, entry := range strings.Split(val, ";") {
				entry = strings.TrimSpace(entry)
				if entry == "" {
					continue
				}
				k, v, ok := strings.Cut(entry, ":")
				if !ok {
					continue
				}
				m[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
			fv.Set(reflect.ValueOf(m))
			continue
		}

		val, ok := os.LookupEnv(envName)
		if !ok {
			continue
		}

		switch fv.Type() {
		case durationType:
			if d, err := time.ParseDuration(val); err == nil {
				fv.Set(reflect.ValueOf(Duration{d}))
			}
		case secretType:
			fv.Set(reflect.ValueOf(NewSecret(val)))
		case stringSliceType:
			parts := strings.Split(val, ",")
			trimmed := make([]string, 0, len(parts))
			for _, p := range parts {
				if s := strings.TrimSpace(p); s != "" {
					trimmed = append(trimmed, s)
				}
			}
			fv.Set(reflect.ValueOf(trimmed))
		default:
			switch fv.Kind() {
			case reflect.String:
				fv.SetString(val)
			case reflect.Bool:
				if b, err := strconv.ParseBool(val); err == nil {
					fv.SetBool(b)
				}
			case reflect.Int, reflect.Int64:
				if n, err := strconv.ParseInt(val, 10, 64); err == nil {
					fv.SetInt(n)
				}
			case reflect.Float64:
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					fv.SetFloat(f)
				}
			}
		}
	}
}

func validate(cfg *Config) error {
	switch cfg.Server.Backend {
	case "docker":
		if cfg.Docker.Image == "" {
			return fmt.Errorf("config: docker.image must not be empty")
		}
	case "process":
		if cfg.Process == nil {
			return fmt.Errorf("config: [process] section required when backend = \"process\"")
		}
		if cfg.Process.PortRangeEnd < cfg.Process.PortRangeStart {
			return fmt.Errorf("config: process.port_range_end must be >= port_range_start")
		}
		if cfg.Process.WorkerUIDEnd < cfg.Process.WorkerUIDStart {
			return fmt.Errorf("config: process.worker_uid_range_end must be >= worker_uid_range_start")
		}
		uidCount := cfg.Process.WorkerUIDEnd - cfg.Process.WorkerUIDStart + 1
		portCount := cfg.Process.PortRangeEnd - cfg.Process.PortRangeStart + 1
		if uidCount < portCount {
			return fmt.Errorf("config: process.worker_uid_range must be at least as large as port_range (got %d UIDs vs %d ports)", uidCount, portCount)
		}
	default:
		return fmt.Errorf("config: server.backend must be \"docker\" or \"process\", got %q", cfg.Server.Backend)
	}

	if cfg.OIDC != nil {
		if cfg.Server.ExternalURL == "" {
			return fmt.Errorf("config: server.external_url is required when [oidc] is configured")
		}
		if cfg.OIDC.IssuerURL == "" {
			return fmt.Errorf("config: oidc.issuer_url must not be empty")
		}
		if cfg.OIDC.ClientID == "" {
			return fmt.Errorf("config: oidc.client_id must not be empty")
		}
		if cfg.OIDC.ClientSecret.IsEmpty() {
			return fmt.Errorf("config: oidc.client_secret must not be empty")
		}
		// session_secret validation is deferred when openbao is configured
		// (it may be auto-generated or resolved from a vault reference).
		if cfg.Openbao == nil {
			if cfg.Server.SessionSecret == nil || cfg.Server.SessionSecret.IsEmpty() {
				return fmt.Errorf("config: server.session_secret is required when [oidc] is configured without [openbao]")
			}
		}
		switch cfg.OIDC.DefaultRole {
		case "viewer", "publisher":
		default:
			return fmt.Errorf(`config: oidc.default_role must be "viewer" or "publisher", got %q`, cfg.OIDC.DefaultRole)
		}
	}

	if cfg.Openbao != nil {
		if cfg.Openbao.Address == "" {
			return fmt.Errorf("config: openbao.address must not be empty")
		}
		if !strings.HasPrefix(cfg.Openbao.Address, "http://") && !strings.HasPrefix(cfg.Openbao.Address, "https://") {
			return fmt.Errorf("config: openbao.address must start with http:// or https://")
		}
		if !cfg.Openbao.AdminToken.IsEmpty() && cfg.Openbao.RoleID != "" {
			return fmt.Errorf("config: openbao.admin_token and openbao.role_id are mutually exclusive")
		}
		if cfg.Openbao.AdminToken.IsEmpty() && cfg.Openbao.RoleID == "" {
			return fmt.Errorf("config: openbao requires either admin_token or role_id")
		}
		if !cfg.Openbao.AdminToken.IsEmpty() {
			slog.Warn("openbao.admin_token is deprecated; use openbao.role_id with AppRole auth")
		}
		if cfg.OIDC == nil {
			return fmt.Errorf("config: [oidc] is required when [openbao] is configured")
		}
		seen := make(map[string]bool)
		for _, svc := range cfg.Openbao.Services {
			if svc.ID == "" || svc.Label == "" {
				return fmt.Errorf("config: openbao.services entries must have id and label")
			}
			if seen[svc.ID] {
				return fmt.Errorf("config: duplicate openbao.services id %q", svc.ID)
			}
			seen[svc.ID] = true
		}
	}

	if cfg.Redis != nil && cfg.Redis.URL == "" {
		return fmt.Errorf("config: [redis] section present but url is empty")
	}

	if cfg.Database.VaultRole != "" {
		if cfg.Openbao == nil {
			return fmt.Errorf("config: database.vault_role requires [openbao]")
		}
		if cfg.Database.Driver != "postgres" {
			return fmt.Errorf("config: database.vault_role requires database.driver = \"postgres\"")
		}
		if cfg.Database.VaultMount == "" {
			return fmt.Errorf("config: database.vault_role requires database.vault_mount")
		}
	}

	if cfg.Database.BoardStorage {
		if cfg.Openbao == nil {
			return fmt.Errorf("config: database.board_storage requires [openbao]")
		}
		if cfg.Database.Driver != "postgres" {
			return fmt.Errorf("config: database.board_storage requires database.driver = \"postgres\"")
		}
		if cfg.Database.VaultMount == "" {
			return fmt.Errorf("config: database.board_storage requires database.vault_mount")
		}
		if cfg.Database.VaultDBConnection == "" {
			return fmt.Errorf("config: database.board_storage requires database.vault_db_connection")
		}
	}

	if cfg.Audit != nil {
		if cfg.Audit.Path == "" {
			return fmt.Errorf("config: audit.path must not be empty")
		}
	}

	if err := validateDataMounts(cfg.Storage.DataMounts); err != nil {
		return fmt.Errorf("config: storage.%w", err)
	}

	if err := validateRuntimeDefaults(cfg.Docker.RuntimeDefaults); err != nil {
		return fmt.Errorf("config: docker.%w", err)
	}

	for _, cidr := range cfg.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("config: server.trusted_proxies: invalid CIDR %q: %w", cidr, err)
		}
	}

	if err := ensureDirWritable(cfg.Storage.BundleServerPath, "storage.bundle_server_path"); err != nil {
		return err
	}

	switch cfg.Database.Driver {
	case "sqlite":
		dbDir := filepath.Dir(cfg.Database.Path)
		if err := ensureDirWritable(dbDir, "database.path parent directory"); err != nil {
			return err
		}
	case "postgres":
		if cfg.Database.URL == "" {
			return fmt.Errorf("config: database.url is required when driver = \"postgres\"")
		}
	default:
		return fmt.Errorf("config: database.driver must be \"sqlite\" or \"postgres\", got %q", cfg.Database.Driver)
	}

	switch cfg.Proxy.SessionStore {
	case SessionStoreAuto, SessionStoreMemory, SessionStoreRedis, SessionStorePostgres, SessionStoreLayered:
	default:
		return fmt.Errorf("config: proxy.session_store must be one of memory|redis|postgres|layered, got %q", cfg.Proxy.SessionStore)
	}
	if cfg.Proxy.SessionStore == SessionStoreRedis && cfg.Redis == nil {
		return fmt.Errorf("config: proxy.session_store = \"redis\" requires [redis]")
	}
	if (cfg.Proxy.SessionStore == SessionStorePostgres || cfg.Proxy.SessionStore == SessionStoreLayered) && cfg.Database.Driver != "postgres" {
		return fmt.Errorf("config: proxy.session_store = %q requires database.driver = \"postgres\"", cfg.Proxy.SessionStore)
	}
	if cfg.Proxy.SessionStore == SessionStoreLayered && cfg.Redis == nil {
		return fmt.Errorf("config: proxy.session_store = \"layered\" requires [redis]")
	}

	return nil
}

var dataMountNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func validateDataMounts(mounts []DataMountSource) error {
	seen := make(map[string]bool)
	for _, m := range mounts {
		if m.Name == "" {
			return fmt.Errorf("data_mounts: name must not be empty")
		}
		if !dataMountNameRe.MatchString(m.Name) {
			return fmt.Errorf("data_mounts: name %q contains invalid characters", m.Name)
		}
		if !filepath.IsAbs(m.Path) {
			return fmt.Errorf("data_mounts: path %q must be absolute", m.Path)
		}
		if seen[m.Name] {
			return fmt.Errorf("data_mounts: duplicate name %q", m.Name)
		}
		seen[m.Name] = true
	}
	return nil
}

func validateRuntimeDefaults(defaults map[string]string) error {
	validAccessTypes := map[string]bool{
		"acl": true, "logged_in": true, "public": true,
	}
	for key := range defaults {
		if !validAccessTypes[key] {
			return fmt.Errorf("runtime_defaults: unknown access type %q"+
				" (valid: acl, logged_in, public)", key)
		}
	}
	return nil
}

func ensureDirWritable(path, label string) error {
	if err := os.MkdirAll(path, 0o755); err != nil { //nolint:gosec // G301: app config dir, not secrets
		return fmt.Errorf("config: %s: cannot create directory %q: %w", label, path, err)
	}
	testFile := filepath.Join(path, ".blockyard-write-test")
	if err := os.WriteFile(testFile, nil, 0o644); err != nil { //nolint:gosec // G306: writability test file, not secrets
		return fmt.Errorf("config: %s: directory %q is not writable: %w", label, path, err)
	}
	if err := os.Remove(testFile); err != nil {
		slog.Warn("config: failed to clean up write-test file",
			"path", testFile, "error", err)
	}
	return nil
}

// LevelTrace is a custom log level below Debug for fine-grained
// diagnostic output (protocol details, per-message flow, algorithm steps).
const LevelTrace = slog.Level(-8)

// ParseLogLevel converts a log level name (trace, debug, info, warn, error)
// to an slog.Level. Returns slog.LevelInfo for empty or unrecognized values.
func ParseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
