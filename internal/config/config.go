package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server   ServerConfig   `toml:"server"`
	Docker   DockerConfig   `toml:"docker"`
	Storage  StorageConfig  `toml:"storage"`
	Database DatabaseConfig `toml:"database"`
	Proxy    ProxyConfig    `toml:"proxy"`
	OIDC     *OidcConfig    `toml:"oidc"`     // nil when not configured
	Openbao  *OpenbaoConfig `toml:"openbao"`  // nil when not configured
}

type ServerConfig struct {
	Bind            string   `toml:"bind"`
	Token           Secret   `toml:"token"`
	SessionSecret   *Secret  `toml:"session_secret"` // required when [oidc] is set
	ExternalURL     string   `toml:"external_url"`
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
}

type DockerConfig struct {
	Socket       string `toml:"socket"`
	Image        string `toml:"image"`
	ShinyPort    int    `toml:"shiny_port"`
	RvVersion    string `toml:"rv_version"`
	RvBinaryPath string `toml:"-"` // set at runtime; skips download if non-empty
}

type StorageConfig struct {
	BundleServerPath string `toml:"bundle_server_path"`
	BundleWorkerPath string `toml:"bundle_worker_path"`
	BundleRetention  int    `toml:"bundle_retention"`
	MaxBundleSize    int64  `toml:"max_bundle_size"`
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

type ProxyConfig struct {
	WsCacheTTL         Duration `toml:"ws_cache_ttl"`
	HealthInterval     Duration `toml:"health_interval"`
	WorkerStartTimeout Duration `toml:"worker_start_timeout"`
	MaxWorkers         int      `toml:"max_workers"`
	LogRetention       Duration `toml:"log_retention"`
	SessionIdleTTL     Duration `toml:"session_idle_ttl"`
	IdleWorkerTimeout  Duration `toml:"idle_worker_timeout"`
}

type OidcConfig struct {
	IssuerURL    string   `toml:"issuer_url"`
	ClientID     string   `toml:"client_id"`
	ClientSecret Secret   `toml:"client_secret"`
	GroupsClaim  string   `toml:"groups_claim"`
	CookieMaxAge Duration `toml:"cookie_max_age"`
}

type OpenbaoConfig struct {
	Address     string   `toml:"address"`
	AdminToken  Secret   `toml:"admin_token"`
	TokenTTL    Duration `toml:"token_ttl"`      // default: 1h
	JWTAuthPath string   `toml:"jwt_auth_path"`  // default: "jwt"
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(&cfg)
	applyEnvOverrides(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = "0.0.0.0:8080"
	}
	if cfg.Server.ShutdownTimeout.Duration == 0 {
		cfg.Server.ShutdownTimeout.Duration = 30 * time.Second
	}
	if cfg.Docker.Socket == "" {
		cfg.Docker.Socket = "/var/run/docker.sock"
	}
	if cfg.Docker.ShinyPort == 0 {
		cfg.Docker.ShinyPort = 3838
	}
	if cfg.Docker.RvVersion == "" {
		cfg.Docker.RvVersion = "v0.19.0"
	}
	if cfg.Storage.BundleServerPath == "" {
		cfg.Storage.BundleServerPath = "/data/bundles"
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "/data/db/blockyard.db"
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
	if cfg.Proxy.SessionIdleTTL.Duration == 0 {
		cfg.Proxy.SessionIdleTTL.Duration = 1 * time.Hour
	}
	if cfg.Proxy.IdleWorkerTimeout.Duration == 0 {
		cfg.Proxy.IdleWorkerTimeout.Duration = 5 * time.Minute
	}
	if cfg.OIDC != nil {
		oidcDefaults(cfg.OIDC)
	}
	if cfg.Openbao != nil {
		openbaoDefaults(cfg.Openbao)
	}
}

func oidcDefaults(c *OidcConfig) {
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if c.CookieMaxAge.Duration == 0 {
		c.CookieMaxAge.Duration = 24 * time.Hour
	}
}

func openbaoDefaults(c *OpenbaoConfig) {
	if c.TokenTTL.Duration == 0 {
		c.TokenTTL.Duration = 1 * time.Hour
	}
	if c.JWTAuthPath == "" {
		c.JWTAuthPath = "jwt"
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
	durationType      = reflect.TypeOf(Duration{})
	secretType        = reflect.TypeOf(Secret{})
	secretPtrType     = reflect.TypeOf((*Secret)(nil))
	oidcCfgPtrType    = reflect.TypeOf((*OidcConfig)(nil))
	openbaoCfgPtrType = reflect.TypeOf((*OpenbaoConfig)(nil))
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

		// Recurse into nested config sections (but not Duration/Secret,
		// which are struct wrappers).
		if field.Type.Kind() == reflect.Struct && field.Type != durationType && field.Type != secretType {
			applyEnvToStruct(fv, envName)
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
	if cfg.Server.Token.IsEmpty() {
		return fmt.Errorf("config: server.token must not be empty")
	}
	if cfg.Docker.Image == "" {
		return fmt.Errorf("config: docker.image must not be empty")
	}

	if cfg.OIDC != nil {
		if cfg.OIDC.IssuerURL == "" {
			return fmt.Errorf("config: oidc.issuer_url must not be empty")
		}
		if cfg.OIDC.ClientID == "" {
			return fmt.Errorf("config: oidc.client_id must not be empty")
		}
		if cfg.OIDC.ClientSecret.IsEmpty() {
			return fmt.Errorf("config: oidc.client_secret must not be empty")
		}
		if cfg.Server.SessionSecret == nil || cfg.Server.SessionSecret.IsEmpty() {
			return fmt.Errorf("config: server.session_secret is required when [oidc] is configured")
		}
	}

	if cfg.Openbao != nil {
		if cfg.Openbao.Address == "" {
			return fmt.Errorf("config: openbao.address must not be empty")
		}
		if cfg.Openbao.AdminToken.IsEmpty() {
			return fmt.Errorf("config: openbao.admin_token must not be empty")
		}
		if cfg.OIDC == nil {
			return fmt.Errorf("config: [oidc] is required when [openbao] is configured")
		}
	}

	if err := ensureDirWritable(cfg.Storage.BundleServerPath, "storage.bundle_server_path"); err != nil {
		return err
	}
	dbDir := filepath.Dir(cfg.Database.Path)
	if err := ensureDirWritable(dbDir, "database.path parent directory"); err != nil {
		return err
	}

	return nil
}

func ensureDirWritable(path, label string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("config: %s: cannot create directory %q: %w", label, path, err)
	}
	testFile := filepath.Join(path, ".blockyard-write-test")
	if err := os.WriteFile(testFile, nil, 0o644); err != nil {
		return fmt.Errorf("config: %s: directory %q is not writable: %w", label, path, err)
	}
	os.Remove(testFile)
	return nil
}
