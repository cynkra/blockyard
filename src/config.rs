use serde::{Deserialize, Serialize};
use std::net::SocketAddr;
use std::path::PathBuf;
use std::time::Duration;

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Config {
    pub server: ServerConfig,
    #[serde(default)]
    pub docker: Option<DockerConfig>,
    pub storage: StorageConfig,
    pub database: DatabaseConfig,
    pub proxy: ProxyConfig,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct ServerConfig {
    #[serde(default = "default_bind")]
    pub bind: SocketAddr,
    pub token: String,
    #[serde(default = "default_shutdown_timeout", with = "humantime_serde")]
    pub shutdown_timeout: Duration,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct DockerConfig {
    #[serde(default = "default_socket")]
    pub socket: String,
    pub image: String,
    #[serde(default = "default_shiny_port")]
    pub shiny_port: u16,
    #[serde(default = "default_rv_version")]
    pub rv_version: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct StorageConfig {
    pub bundle_server_path: PathBuf,
    #[serde(default = "default_worker_path")]
    pub bundle_worker_path: PathBuf,
    #[serde(default = "default_retention")]
    pub bundle_retention: u32,
    #[serde(default = "default_max_bundle_size")]
    pub max_bundle_size: usize,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct DatabaseConfig {
    pub path: PathBuf,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct ProxyConfig {
    #[serde(default = "default_ws_cache_ttl", with = "humantime_serde")]
    pub ws_cache_ttl: Duration,
    #[serde(default = "default_health_interval", with = "humantime_serde")]
    pub health_interval: Duration,
    #[serde(default = "default_start_timeout", with = "humantime_serde")]
    pub worker_start_timeout: Duration,
    #[serde(default = "default_max_workers")]
    pub max_workers: u32,
    #[serde(default = "default_log_retention", with = "humantime_serde")]
    pub log_retention: Duration,
}

// --- defaults ---

fn default_bind() -> SocketAddr {
    "0.0.0.0:8080".parse().unwrap()
}
fn default_shutdown_timeout() -> Duration {
    Duration::from_secs(30)
}
fn default_socket() -> String {
    "/var/run/docker.sock".into()
}
fn default_shiny_port() -> u16 {
    3838
}
fn default_rv_version() -> String {
    "latest".into()
}
fn default_worker_path() -> PathBuf {
    PathBuf::from("/app")
}
fn default_retention() -> u32 {
    50
}
fn default_max_bundle_size() -> usize {
    100 * 1024 * 1024
}
fn default_ws_cache_ttl() -> Duration {
    Duration::from_secs(60)
}
fn default_health_interval() -> Duration {
    Duration::from_secs(15)
}
fn default_start_timeout() -> Duration {
    Duration::from_secs(60)
}
fn default_max_workers() -> u32 {
    100
}
fn default_log_retention() -> Duration {
    Duration::from_secs(3600) // 1 hour
}

/// Returns the set of env var names that apply_env_overrides handles.
/// Used by the completeness test to verify no field is missing.
pub fn supported_env_vars() -> &'static [&'static str] {
    &[
        "BLOCKYARD_SERVER_BIND",
        "BLOCKYARD_SERVER_TOKEN",
        "BLOCKYARD_SERVER_SHUTDOWN_TIMEOUT",
        "BLOCKYARD_DOCKER_SOCKET",
        "BLOCKYARD_DOCKER_IMAGE",
        "BLOCKYARD_DOCKER_SHINY_PORT",
        "BLOCKYARD_DOCKER_RV_VERSION",
        "BLOCKYARD_STORAGE_BUNDLE_SERVER_PATH",
        "BLOCKYARD_STORAGE_BUNDLE_WORKER_PATH",
        "BLOCKYARD_STORAGE_BUNDLE_RETENTION",
        "BLOCKYARD_STORAGE_MAX_BUNDLE_SIZE",
        "BLOCKYARD_DATABASE_PATH",
        "BLOCKYARD_PROXY_WS_CACHE_TTL",
        "BLOCKYARD_PROXY_HEALTH_INTERVAL",
        "BLOCKYARD_PROXY_WORKER_START_TIMEOUT",
        "BLOCKYARD_PROXY_MAX_WORKERS",
        "BLOCKYARD_PROXY_LOG_RETENTION",
    ]
}

impl Config {
    /// Load config from file + env var overrides.
    /// File path: --config CLI arg, or ./blockyard.toml by default.
    pub fn load() -> Result<Self, ConfigError> {
        let path = std::env::args()
            .skip_while(|a| a != "--config")
            .nth(1)
            .unwrap_or_else(|| "blockyard.toml".into());

        let text =
            std::fs::read_to_string(&path).map_err(|e| ConfigError::ReadFile(path.clone(), e))?;
        let mut config: Config = toml::from_str(&text).map_err(ConfigError::Parse)?;

        config.apply_env_overrides();
        config.validate()?;
        Ok(config)
    }

    /// Override individual fields from BLOCKYARD_* env vars.
    fn apply_env_overrides(&mut self) {
        if let Ok(v) = std::env::var("BLOCKYARD_SERVER_BIND")
            && let Ok(addr) = v.parse()
        {
            self.server.bind = addr;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_SERVER_TOKEN") {
            self.server.token = v;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_SERVER_SHUTDOWN_TIMEOUT")
            && let Ok(d) = v.parse::<humantime::Duration>()
        {
            self.server.shutdown_timeout = d.into();
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DOCKER_SOCKET")
            && let Some(docker) = &mut self.docker
        {
            docker.socket = v;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DOCKER_IMAGE")
            && let Some(docker) = &mut self.docker
        {
            docker.image = v;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DOCKER_SHINY_PORT")
            && let (Some(docker), Ok(p)) = (&mut self.docker, v.parse())
        {
            docker.shiny_port = p;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DOCKER_RV_VERSION")
            && let Some(docker) = &mut self.docker
        {
            docker.rv_version = v;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_STORAGE_BUNDLE_SERVER_PATH") {
            self.storage.bundle_server_path = PathBuf::from(v);
        }
        if let Ok(v) = std::env::var("BLOCKYARD_STORAGE_BUNDLE_WORKER_PATH") {
            self.storage.bundle_worker_path = PathBuf::from(v);
        }
        if let Ok(v) = std::env::var("BLOCKYARD_STORAGE_BUNDLE_RETENTION")
            && let Ok(n) = v.parse()
        {
            self.storage.bundle_retention = n;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_STORAGE_MAX_BUNDLE_SIZE")
            && let Ok(n) = v.parse()
        {
            self.storage.max_bundle_size = n;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DATABASE_PATH") {
            self.database.path = PathBuf::from(v);
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_WS_CACHE_TTL")
            && let Ok(d) = v.parse::<humantime::Duration>()
        {
            self.proxy.ws_cache_ttl = d.into();
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_HEALTH_INTERVAL")
            && let Ok(d) = v.parse::<humantime::Duration>()
        {
            self.proxy.health_interval = d.into();
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_WORKER_START_TIMEOUT")
            && let Ok(d) = v.parse::<humantime::Duration>()
        {
            self.proxy.worker_start_timeout = d.into();
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_MAX_WORKERS")
            && let Ok(n) = v.parse()
        {
            self.proxy.max_workers = n;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_LOG_RETENTION")
            && let Ok(d) = v.parse::<humantime::Duration>()
        {
            self.proxy.log_retention = d.into();
        }
    }

    /// Validate config after all overrides are applied.
    fn validate(&self) -> Result<(), ConfigError> {
        if self.server.token.is_empty() {
            return Err(ConfigError::Validation(
                "server.token must not be empty".into(),
            ));
        }
        #[cfg(feature = "docker")]
        {
            let docker = self.docker.as_ref().ok_or_else(|| {
                ConfigError::Validation(
                    "[docker] section required when docker feature is enabled".into(),
                )
            })?;
            if docker.image.is_empty() {
                return Err(ConfigError::Validation(
                    "docker.image must not be empty".into(),
                ));
            }
        }

        // Validate storage paths are accessible
        Self::ensure_dir_writable(
            &self.storage.bundle_server_path,
            "storage.bundle_server_path",
        )?;
        let db_parent = self
            .database
            .path
            .parent()
            .unwrap_or(&self.database.path);
        Self::ensure_dir_writable(db_parent, "database.path parent directory")?;

        Ok(())
    }

    /// Verify a directory exists (or can be created) and is writable.
    fn ensure_dir_writable(path: &std::path::Path, label: &str) -> Result<(), ConfigError> {
        std::fs::create_dir_all(path).map_err(|e| {
            ConfigError::Validation(format!(
                "{label}: cannot create directory '{}': {e}",
                path.display()
            ))
        })?;
        let test_file = path.join(".blockyard-write-test");
        std::fs::write(&test_file, b"").map_err(|e| {
            ConfigError::Validation(format!(
                "{label}: directory '{}' is not writable: {e}",
                path.display()
            ))
        })?;
        let _ = std::fs::remove_file(&test_file);
        Ok(())
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("failed to read config file '{0}': {1}")]
    ReadFile(String, std::io::Error),
    #[error("failed to parse config: {0}")]
    Parse(toml::de::Error),
    #[error("config validation failed: {0}")]
    Validation(String),
}

#[cfg(test)]
mod tests {
    use super::*;

    fn minimal_toml() -> &'static str {
        r#"
        [server]
        token = "test-token"

        [docker]
        image = "ghcr.io/rocker-org/r-ver:latest"

        [storage]
        bundle_server_path = "/tmp/bundles"

        [database]
        path = "/tmp/blockyard.db"

        [proxy]
        "#
    }

    #[test]
    fn parse_minimal_config() {
        let config: Config = toml::from_str(minimal_toml()).unwrap();
        assert_eq!(config.server.bind, "0.0.0.0:8080".parse().unwrap());
        assert_eq!(config.server.token, "test-token");
        assert_eq!(config.proxy.max_workers, 100);
    }

    #[test]
    fn env_var_overrides_token() {
        let mut config: Config = toml::from_str(minimal_toml()).unwrap();
        unsafe { std::env::set_var("BLOCKYARD_SERVER_TOKEN", "override-token") };
        config.apply_env_overrides();
        unsafe { std::env::remove_var("BLOCKYARD_SERVER_TOKEN") };
        assert_eq!(config.server.token, "override-token");
    }

    #[test]
    fn validation_rejects_empty_token() {
        let mut config: Config = toml::from_str(minimal_toml()).unwrap();
        config.server.token = String::new();
        assert!(config.validate().is_err());
    }

    /// Verify every leaf field in Config has a corresponding BLOCKYARD_* env var.
    #[test]
    fn env_var_coverage_complete() {
        let config: Config = toml::from_str(minimal_toml()).unwrap();
        let value = serde_json::to_value(&config).unwrap();

        let mut field_paths = Vec::new();
        collect_leaf_paths(&value, "", &mut field_paths);

        let supported: std::collections::HashSet<&str> =
            supported_env_vars().iter().copied().collect();

        let mut missing = Vec::new();
        for path in &field_paths {
            let env_var = format!("BLOCKYARD_{}", path.to_uppercase().replace('.', "_"));
            if !supported.contains(env_var.as_str()) {
                missing.push(format!("{env_var} (for config field '{path}')"));
            }
        }

        assert!(
            missing.is_empty(),
            "Config fields without env var support:\n  {}",
            missing.join("\n  ")
        );
    }

    /// Verify no two config fields map to the same env var name.
    #[test]
    fn env_var_names_unique() {
        let vars = supported_env_vars();
        let mut seen = std::collections::HashSet::new();
        let mut dupes = Vec::new();
        for var in vars {
            if !seen.insert(var) {
                dupes.push(*var);
            }
        }
        assert!(
            dupes.is_empty(),
            "Duplicate env var names:\n  {}",
            dupes.join("\n  ")
        );
    }

    /// Recursively collect dotted paths to all leaf (non-object) fields.
    fn collect_leaf_paths(value: &serde_json::Value, prefix: &str, out: &mut Vec<String>) {
        match value {
            serde_json::Value::Object(map) => {
                for (key, val) in map {
                    let path = if prefix.is_empty() {
                        key.clone()
                    } else {
                        format!("{prefix}.{key}")
                    };
                    collect_leaf_paths(val, &path, out);
                }
            }
            _ => {
                out.push(prefix.to_string());
            }
        }
    }
}
