package config

import (
	"fmt"
	"log/slog"
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
}

type ServerConfig struct {
	Bind            string   `toml:"bind"`
	Token           string   `toml:"token"`
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
	BundleHostPath   string `toml:"bundle_host_path"`
	BundleWorkerPath string `toml:"bundle_worker_path"`
	BundleVolumeName string `toml:"bundle_volume_name"`
	BundleRetention  int    `toml:"bundle_retention"`
	MaxBundleSize    int64  `toml:"max_bundle_size"`
}

// UseVolume reports whether named Docker volume mode is configured.
func (s StorageConfig) UseVolume() bool {
	return s.BundleVolumeName != ""
}

// DockerBasePath returns the base path to use when constructing paths for
// Docker bind mounts. In volume mode it returns BundleServerPath (the host
// path concept does not apply); in bind mode it returns BundleHostPath.
func (s StorageConfig) DockerBasePath() string {
	if s.UseVolume() {
		return s.BundleServerPath
	}
	return s.BundleHostPath
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
	if cfg.Storage.BundleHostPath == "" {
		cfg.Storage.BundleHostPath = cfg.Storage.BundleServerPath
	}
	if cfg.Storage.BundleVolumeName != "" && cfg.Storage.BundleHostPath != cfg.Storage.BundleServerPath {
		slog.Warn("storage.bundle_volume_name is set; bundle_host_path will be ignored")
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
}

// applyEnvOverrides walks cfg via reflection, deriving the env var name
// from toml struct tags (BLOCKYARD_ + section + _ + field, uppercased).
// Supported field types: string, int, int64, float64, Duration.
func applyEnvOverrides(cfg *Config) {
	applyEnvToStruct(reflect.ValueOf(cfg).Elem(), "BLOCKYARD")
}

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

		// Recurse into nested config sections (but not Duration,
		// which is a struct wrapper around time.Duration).
		if field.Type.Kind() == reflect.Struct && field.Type != reflect.TypeOf(Duration{}) {
			applyEnvToStruct(fv, envName)
			continue
		}

		val, ok := os.LookupEnv(envName)
		if !ok {
			continue
		}

		switch fv.Type() {
		case reflect.TypeOf(Duration{}):
			if d, err := time.ParseDuration(val); err == nil {
				fv.Set(reflect.ValueOf(Duration{d}))
			}
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
	if cfg.Server.Token == "" {
		return fmt.Errorf("config: server.token must not be empty")
	}
	if cfg.Docker.Image == "" {
		return fmt.Errorf("config: docker.image must not be empty")
	}
	if cfg.Storage.BundleServerPath == "" {
		return fmt.Errorf("config: storage.bundle_server_path must not be empty")
	}
	if cfg.Database.Path == "" {
		return fmt.Errorf("config: database.path must not be empty")
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
