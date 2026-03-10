# Phase 0-2: Docker Backend + Network Isolation

Implement the `Backend` trait for Docker using `bollard`. This is the only
production backend for v0 and the foundation that all later phases depend on
for integration testing with real containers.

## Deliverables

1. `DockerBackend` struct with `bollard::Docker` client initialization
2. Full `Backend` trait implementation (spawn, stop, health_check, logs, addr, build)
3. Per-container bridge network creation and cleanup
4. Server multi-homing — join each worker's network to enable direct container-to-container routing
5. Container hardening (cap-drop ALL, read-only rootfs, no-new-privileges, tmpfs /tmp)
6. Label management (`dev.blockyard/*`) for resource ownership tracking
7. Orphan cleanup via `list_managed` / `remove_resource`
8. Server ID detection (self-awareness for network joining)
9. Docker integration tests behind `#[cfg(feature = "docker-tests")]`
10. Enable the `docker-tests` CI job

## Step-by-step

### Step 1: DockerBackend struct and initialization

`src/backend/docker.rs` — the struct holds the bollard client, the server's
own server ID (if running in a container), and config needed for spawning.

```rust
#[cfg(feature = "docker")]
use bollard::Docker;
use crate::config::DockerConfig;

pub struct DockerBackend {
    client: Docker,
    server_id: Option<String>,
    config: DockerConfig,
}
```

**Constructor:**

```rust
impl DockerBackend {
    pub async fn new(config: DockerConfig) -> Result<Self, BackendError> {
        let client = Docker::connect_with_unix(
            &config.socket,
            120, // timeout seconds
            bollard::API_DEFAULT_VERSION,
        ).map_err(|e| BackendError::Spawn(format!("Docker connect failed: {e}")))?;

        // Verify connectivity
        client.ping().await
            .map_err(|e| BackendError::Spawn(format!("Docker ping failed: {e}")))?;

        let server_id = detect_server_id().await;

        Ok(Self { client, server_id, config })
    }
}
```

### Step 2: Server ID detection

The server must know its own Docker container ID to join worker networks.
Detection order (first match wins):

1. `BLOCKYARD_SERVER_ID` env var — explicit override for non-standard setups
2. Parse `/proc/self/cgroup` — Docker writes the container ID in cgroup paths
3. Read hostname — Docker sets it to the short container ID by default
4. If all fail: `None` — assume the server is running as a native binary
   outside Docker. Skip network joining; workers are reachable on the bridge
   gateway IP.

```rust
async fn detect_server_id() -> Option<String> {
    // 1. Explicit env var
    if let Ok(id) = std::env::var("BLOCKYARD_SERVER_ID") {
        if !id.is_empty() {
            tracing::info!(container_id = %id, "server ID from env");
            return Some(id);
        }
    }

    // 2. Parse /proc/self/cgroup
    if let Ok(cgroup) = tokio::fs::read_to_string("/proc/self/cgroup").await {
        // Look for docker container ID in cgroup paths
        // Format: "0::/docker/<container_id>" or similar
        for line in cgroup.lines() {
            if let Some(id) = extract_container_id_from_cgroup(line) {
                tracing::info!(container_id = %id, "server ID from cgroup");
                return Some(id);
            }
        }
    }

    // 3. Hostname (Docker sets this to the short container ID)
    if let Ok(hostname) = tokio::fs::read_to_string("/etc/hostname").await {
        let hostname = hostname.trim();
        // Docker container IDs are 12+ hex chars
        if hostname.len() >= 12 && hostname.chars().all(|c| c.is_ascii_hexdigit()) {
            tracing::info!(container_id = %hostname, "server ID from hostname");
            return Some(hostname.to_string());
        }
    }

    tracing::info!("no server ID detected — running in native mode");
    None
}

fn extract_container_id_from_cgroup(line: &str) -> Option<String> {
    // Match patterns like:
    //   0::/docker/<64-char-hex-id>
    //   0::/system.slice/docker-<64-char-hex-id>.scope
    let parts: Vec<&str> = line.split('/').collect();
    for (i, part) in parts.iter().enumerate() {
        if *part == "docker" || part.starts_with("docker-") {
            let candidate = if *part == "docker" {
                parts.get(i + 1).map(|s| s.to_string())
            } else {
                // docker-<id>.scope
                part.strip_prefix("docker-")
                    .and_then(|s| s.strip_suffix(".scope"))
                    .map(|s| s.to_string())
            };
            if let Some(id) = candidate {
                if id.len() >= 12 && id.chars().all(|c| c.is_ascii_hexdigit()) {
                    return Some(id);
                }
            }
        }
    }
    None
}
```

### Step 3: Label conventions

All blockyard-managed resources (containers and networks) are labeled for
discovery and cleanup. Labels use the `dev.blockyard/` prefix:

| Label | Value | Applied to |
|---|---|---|
| `dev.blockyard/managed` | `true` | All containers and networks |
| `dev.blockyard/app-id` | `{app_id}` | Worker and build containers, networks |
| `dev.blockyard/worker-id` | `{worker_id}` | Worker containers, networks |
| `dev.blockyard/bundle-id` | `{bundle_id}` | Build containers |
| `dev.blockyard/role` | `worker` or `build` | All containers |

These labels serve two purposes:
- **Orphan cleanup:** `list_managed()` queries for `dev.blockyard/managed=true`
- **Debugging:** `docker ps --filter label=dev.blockyard/app-id=...`

Helper to construct label maps from spec:

```rust
fn worker_labels(spec: &WorkerSpec) -> HashMap<String, String> {
    let mut labels = HashMap::new();
    labels.insert("dev.blockyard/managed".into(), "true".into());
    labels.insert("dev.blockyard/app-id".into(), spec.app_id.clone());
    labels.insert("dev.blockyard/worker-id".into(), spec.worker_id.clone());
    labels.insert("dev.blockyard/role".into(), "worker".into());
    labels.extend(spec.labels.clone());
    labels
}

fn build_labels(spec: &BuildSpec) -> HashMap<String, String> {
    let mut labels = HashMap::new();
    labels.insert("dev.blockyard/managed".into(), "true".into());
    labels.insert("dev.blockyard/app-id".into(), spec.app_id.clone());
    labels.insert("dev.blockyard/bundle-id".into(), spec.bundle_id.clone());
    labels.insert("dev.blockyard/role".into(), "build".into());
    labels.extend(spec.labels.clone());
    labels
}

fn network_labels(app_id: &str, worker_id: &str) -> HashMap<String, String> {
    let mut labels = HashMap::new();
    labels.insert("dev.blockyard/managed".into(), "true".into());
    labels.insert("dev.blockyard/app-id".into(), app_id.into());
    labels.insert("dev.blockyard/worker-id".into(), worker_id.into());
    labels
}
```

### Step 4: DockerHandle

The handle returned from `spawn()`. Contains both the container ID and
network ID so that `stop()` can clean up both.

```rust
#[derive(Debug, Clone)]
pub struct DockerHandle {
    pub container_id: String,
    pub network_id: String,
    pub network_name: String,
}

impl WorkerHandle for DockerHandle {
    fn id(&self) -> &str {
        &self.container_id
    }
}
```

### Step 5: spawn() — create network, create container, join, start

The spawn flow creates an isolated network per worker, creates a hardened
container attached to that network, optionally joins the server to the
network, and starts the container.

```rust
async fn spawn(&self, spec: &WorkerSpec) -> Result<DockerHandle, BackendError> {
    let network_name = format!("blockyard-{}", spec.worker_id);

    // 1. Create per-worker bridge network
    let network_id = self.create_network(&network_name, &spec.app_id, &spec.worker_id).await?;

    // 2. Create container
    let container_id = self.create_worker_container(spec, &network_name).await?;

    // 3. Join server to worker network (if running in a container)
    if let Some(ref server_id) = self.server_id {
        self.join_network(server_id, &network_name).await?;
    }

    // 4. Start the container
    self.client.start_container::<String>(&container_id, None).await
        .map_err(|e| BackendError::Spawn(format!("start container: {e}")))?;

    Ok(DockerHandle { container_id, network_id, network_name })
}
```

**Network creation:**

```rust
async fn create_network(
    &self,
    name: &str,
    app_id: &str,
    worker_id: &str,
) -> Result<String, BackendError> {
    use bollard::network::CreateNetworkOptions;

    let options = CreateNetworkOptions {
        name: name.to_string(),
        driver: "bridge".to_string(),
        labels: network_labels(app_id, worker_id),
        ..Default::default()
    };

    let response = self.client.create_network(options).await
        .map_err(|e| BackendError::Spawn(format!("create network: {e}")))?;

    Ok(response.id)
}
```

**Container creation (hardened):**

```rust
async fn create_worker_container(
    &self,
    spec: &WorkerSpec,
    network_name: &str,
) -> Result<String, BackendError> {
    use bollard::container::{Config, CreateContainerOptions};
    use bollard::models::*;

    let container_name = format!("blockyard-worker-{}", spec.worker_id);

    let host_config = HostConfig {
        network_mode: Some(network_name.to_string()),
        binds: Some(vec![
            format!("{}:{}:ro", spec.bundle_path.display(), spec.worker_mount.display()),
            format!(
                "{}:{}/lib:ro",
                spec.library_path.display(),
                spec.worker_mount.display()
            ),
        ]),
        tmpfs: Some(HashMap::from([("/tmp".to_string(), "".to_string())])),
        cap_drop: Some(vec!["ALL".to_string()]),
        security_opt: Some(vec!["no-new-privileges".to_string()]),
        readonly_rootfs: Some(true),
        memory: spec.memory_limit.as_ref().and_then(|m| parse_memory_limit(m)),
        nano_cpus: spec.cpu_limit.map(|c| (c * 1e9) as i64),
        ..Default::default()
    };

    let config = Config {
        image: Some(spec.image.clone()),
        env: Some(vec![format!("SHINY_PORT={}", spec.shiny_port)]),
        labels: Some(worker_labels(spec)),
        host_config: Some(host_config),
        ..Default::default()
    };

    let options = CreateContainerOptions {
        name: &container_name,
        ..Default::default()
    };

    let response = self.client.create_container(Some(options), config).await
        .map_err(|e| BackendError::Spawn(format!("create container: {e}")))?;

    Ok(response.id)
}
```

**Memory limit parsing:**

```rust
/// Parse human-readable memory limits like "512m", "1g", "256mb" to bytes.
fn parse_memory_limit(s: &str) -> Option<i64> {
    let s = s.trim().to_lowercase();
    let (num_str, multiplier) = if s.ends_with("gb") || s.ends_with("g") {
        (s.trim_end_matches("gb").trim_end_matches('g'), 1024 * 1024 * 1024)
    } else if s.ends_with("mb") || s.ends_with("m") {
        (s.trim_end_matches("mb").trim_end_matches('m'), 1024 * 1024)
    } else if s.ends_with("kb") || s.ends_with("k") {
        (s.trim_end_matches("kb").trim_end_matches('k'), 1024)
    } else {
        (s.as_str(), 1)  // assume bytes
    };
    num_str.trim().parse::<i64>().ok().map(|n| n * multiplier)
}
```

**Network joining:**

```rust
async fn join_network(
    &self,
    container_id: &str,
    network_name: &str,
) -> Result<(), BackendError> {
    use bollard::network::ConnectNetworkOptions;
    use bollard::models::EndpointSettings;

    let options = ConnectNetworkOptions {
        container: container_id.to_string(),
        endpoint_config: EndpointSettings::default(),
    };

    self.client.connect_network(network_name, options).await
        .map_err(|e| BackendError::Spawn(format!("join network: {e}")))?;

    Ok(())
}
```

### Step 6: addr() — resolve worker IP on its named network

The worker's IP address must be looked up on the specific `blockyard-*`
network, not just any network the container is attached to.

```rust
async fn addr(&self, handle: &DockerHandle) -> Result<SocketAddr, BackendError> {
    let info = self.client.inspect_container(&handle.container_id, None).await
        .map_err(|e| BackendError::Addr(format!("inspect container: {e}")))?;

    let networks = info.network_settings
        .and_then(|ns| ns.networks)
        .ok_or_else(|| BackendError::Addr("no networks on container".into()))?;

    let endpoint = networks.get(&handle.network_name)
        .ok_or_else(|| BackendError::Addr(
            format!("container not on network {}", handle.network_name)
        ))?;

    let ip = endpoint.ip_address.as_ref()
        .ok_or_else(|| BackendError::Addr("no IP on network".into()))?;

    let addr: std::net::IpAddr = ip.parse()
        .map_err(|e| BackendError::Addr(format!("invalid IP '{ip}': {e}")))?;

    Ok(SocketAddr::new(addr, self.config.shiny_port))
}
```

### Step 7: stop() — stop container, disconnect server, remove network

```rust
async fn stop(&self, handle: &DockerHandle) -> Result<(), BackendError> {
    use bollard::container::{StopContainerOptions, RemoveContainerOptions};

    // 1. Stop the container (10s timeout)
    self.client.stop_container(
        &handle.container_id,
        Some(StopContainerOptions { t: 10 }),
    ).await.map_err(|e| BackendError::Stop(format!("stop container: {e}")))?;

    // 2. Remove the container
    self.client.remove_container(
        &handle.container_id,
        Some(RemoveContainerOptions { force: true, ..Default::default() }),
    ).await.map_err(|e| BackendError::Stop(format!("remove container: {e}")))?;

    // 3. Disconnect server from the worker's network
    if let Some(ref server_id) = self.server_id {
        let _ = self.disconnect_network(server_id, &handle.network_name).await;
    }

    // 4. Remove the network
    self.client.remove_network(&handle.network_name).await
        .map_err(|e| BackendError::Stop(format!("remove network: {e}")))?;

    Ok(())
}

async fn disconnect_network(
    &self,
    container_id: &str,
    network_name: &str,
) -> Result<(), BackendError> {
    use bollard::network::DisconnectNetworkOptions;

    let options = DisconnectNetworkOptions {
        container: container_id.to_string(),
        force: true,
    };

    self.client.disconnect_network(network_name, options).await
        .map_err(|e| BackendError::Stop(format!("disconnect network: {e}")))?;

    Ok(())
}
```

### Step 8: health_check() — TCP probe

A simple TCP connection attempt to the worker's Shiny port. If the
connection succeeds, the worker is healthy. No HTTP-level check — Shiny
doesn't expose a health endpoint.

```rust
async fn health_check(&self, handle: &DockerHandle) -> bool {
    match self.addr(handle).await {
        Ok(addr) => tokio::time::timeout(
            std::time::Duration::from_secs(10),
            tokio::net::TcpStream::connect(addr),
        )
        .await
        .is_ok_and(|r| r.is_ok()),
        Err(_) => false,
    }
}
```

The 10-second timeout ensures a hanging connection doesn't stall the
health polling loop (which runs every 15s by default).

### Step 9: logs() — stream container stdout/stderr

```rust
async fn logs(&self, handle: &DockerHandle) -> Result<LogStream, BackendError> {
    use bollard::container::LogsOptions;
    use futures_util::StreamExt;

    let options = LogsOptions::<String> {
        follow: true,
        stdout: true,
        stderr: true,
        ..Default::default()
    };

    let stream = self.client.logs(&handle.container_id, Some(options));
    let (tx, rx) = tokio::sync::mpsc::channel(256);

    tokio::spawn(async move {
        let mut stream = std::pin::pin!(stream);
        while let Some(result) = stream.next().await {
            match result {
                Ok(output) => {
                    let line = output.to_string();
                    if tx.send(line).await.is_err() {
                        break; // receiver dropped
                    }
                }
                Err(_) => break,
            }
        }
    });

    Ok(rx)
}
```

**Note:** Add `futures-util` to dependencies:

```toml
futures-util = "0.3"
```

### Step 10: build() — run a build container to completion

The build flow creates a short-lived container that runs dependency
restoration (`R -e "renv::restore()"`). The container mounts the bundle
read-only and a library output directory read-write.

```rust
async fn build(&self, spec: &BuildSpec) -> Result<BuildResult, BackendError> {
    use bollard::container::*;
    use bollard::models::*;

    let container_name = format!("blockyard-build-{}", spec.bundle_id);

    let host_config = HostConfig {
        binds: Some(vec![
            format!("{}:/app:ro", spec.bundle_path.display()),
            format!("{}:/app/rv/library:rw", spec.library_path.display()),
        ]),
        tmpfs: Some(HashMap::from([
            ("/tmp".to_string(), "".to_string()),
            ("/root/.cache/rv".to_string(), "".to_string()),
        ])),
        cap_drop: Some(vec!["ALL".to_string()]),
        security_opt: Some(vec!["no-new-privileges".to_string()]),
        // rootfs not read-only — need to write rv binary to /usr/local/bin
        ..Default::default()
    };

    // Download rv and run sync in one shot
    let rv_url = format!(
        "https://github.com/a2-ai/rv/releases/download/{}/rv-x86_64-unknown-linux-gnu",
        self.config.rv_version,
    );
    let install_and_sync = vec![
        "sh".to_string(), "-c".to_string(),
        format!("curl -sSL {rv_url} -o /usr/local/bin/rv && chmod +x /usr/local/bin/rv && rv sync"),
    ];

    let config = Config {
        image: Some(spec.image.clone()),
        cmd: Some(install_and_sync),
        working_dir: Some("/app".to_string()),
        labels: Some(build_labels(spec)),
        host_config: Some(host_config),
        ..Default::default()
    };

    let options = CreateContainerOptions {
        name: &container_name,
        ..Default::default()
    };

    let response = self.client.create_container(Some(options), config).await
        .map_err(|e| BackendError::Build(format!("create build container: {e}")))?;

    let container_id = response.id;

    // Start the build container
    self.client.start_container::<String>(&container_id, None).await
        .map_err(|e| BackendError::Build(format!("start build container: {e}")))?;

    // Wait for completion
    let wait_result = self.client
        .wait_container::<String>(&container_id, None)
        .next()
        .await;

    let exit_code = match wait_result {
        Some(Ok(response)) => Some(response.status_code),
        Some(Err(e)) => {
            tracing::warn!(error = %e, "build container wait error");
            None
        }
        None => None,
    };

    let success = exit_code == Some(0);

    // Clean up the build container
    let _ = self.client.remove_container(
        &container_id,
        Some(RemoveContainerOptions { force: true, ..Default::default() }),
    ).await;

    Ok(BuildResult { success, exit_code })
}
```

The build command is `rv sync` — rv is a declarative R package manager
(written in Rust) that restores packages from an `rv.lock` lockfile. It
replaces renv for dependency management.

**Build and worker containers use the same image.** The configured image
(`[docker] image`) is a standard rocker image (e.g. `rocker/r-ver`) — it
ships R but not rv. The build container downloads rv from GitHub releases
as the first step of its command, then runs `rv sync`. This is a one-line
shell command (`curl | chmod | rv sync`). Worker containers don't need
rv — they just run the Shiny app with the pre-built library.

Using the same base image for builds and workers guarantees that the R
version, architecture, and system libraries are identical — which means
rv's namespaced library path (`rv/library/<R version>/<arch>/<codename>`)
resolves to the same directory in both containers.

### Step 11: list_managed() and remove_resource() — orphan cleanup

Queries Docker for all containers and networks labeled with
`dev.blockyard/managed=true`. Used at startup to clean up resources left
behind by a previous server crash.

```rust
async fn list_managed(&self) -> Result<Vec<ManagedResource>, BackendError> {
    use bollard::container::ListContainersOptions;
    use bollard::network::ListNetworksOptions;

    let mut resources = Vec::new();

    // Find managed containers (including stopped)
    let container_filters = HashMap::from([
        ("label".to_string(), vec!["dev.blockyard/managed=true".to_string()]),
    ]);
    let containers = self.client.list_containers(Some(ListContainersOptions {
        all: true,
        filters: container_filters,
        ..Default::default()
    })).await.map_err(|e| BackendError::Cleanup(format!("list containers: {e}")))?;

    for c in containers {
        if let Some(id) = c.id {
            resources.push(ManagedResource {
                id,
                kind: ResourceKind::Container,
            });
        }
    }

    // Find managed networks
    let network_filters = HashMap::from([
        ("label".to_string(), vec!["dev.blockyard/managed=true".to_string()]),
    ]);
    let networks = self.client.list_networks(Some(ListNetworksOptions {
        filters: network_filters,
    })).await.map_err(|e| BackendError::Cleanup(format!("list networks: {e}")))?;

    for n in networks {
        if let Some(id) = n.id {
            resources.push(ManagedResource {
                id,
                kind: ResourceKind::Network,
            });
        }
    }

    resources.sort_by(|a, b| a.kind.cmp(&b.kind));
    Ok(resources)
}

async fn remove_resource(&self, resource: &ManagedResource) -> Result<(), BackendError> {
    match resource.kind {
        ResourceKind::Container => {
            self.client.remove_container(
                &resource.id,
                Some(bollard::container::RemoveContainerOptions {
                    force: true,
                    ..Default::default()
                }),
            ).await.map_err(|e| BackendError::Cleanup(format!("remove container: {e}")))?;
        }
        ResourceKind::Network => {
            self.client.remove_network(&resource.id).await
                .map_err(|e| BackendError::Cleanup(format!("remove network: {e}")))?;
        }
    }
    Ok(())
}
```

### Step 12: Docker integration tests

Add a `docker-tests` feature flag and write integration tests that exercise
the real Docker backend. These require a running Docker daemon.

**Cargo.toml addition:**

```toml
[features]
docker-tests = ["docker"]  # implies docker feature
```

**Tests** (`src/backend/docker.rs` or `tests/docker_test.rs`):

```rust
#[cfg(all(test, feature = "docker-tests"))]
mod tests {
    use super::*;

    fn test_config() -> DockerConfig {
        DockerConfig {
            socket: "/var/run/docker.sock".into(),
            image: "ghcr.io/rocker-org/r-ver:latest".into(),
            shiny_port: 3838,
        }
    }

    #[tokio::test]
    async fn spawn_and_stop_container() {
        let backend = DockerBackend::new(test_config()).await.unwrap();

        let spec = WorkerSpec {
            app_id: "test-app".into(),
            worker_id: format!("test-{}", uuid::Uuid::new_v4()),
            image: "ghcr.io/rocker-org/r-ver:latest".into(),
            bundle_path: "/tmp".into(),  // just needs to exist
            library_path: "/tmp".into(),
            worker_mount: "/app".into(),
            shiny_port: 3838,
            memory_limit: Some("256m".into()),
            cpu_limit: Some(0.5),
            labels: Default::default(),
        };

        let handle = backend.spawn(&spec).await.unwrap();

        // Container should be running
        let addr = backend.addr(&handle).await.unwrap();
        assert!(!addr.ip().is_unspecified());

        // Stop and clean up
        backend.stop(&handle).await.unwrap();
    }

    #[tokio::test]
    async fn health_check_before_ready() {
        let backend = DockerBackend::new(test_config()).await.unwrap();

        let spec = WorkerSpec {
            app_id: "test-app".into(),
            worker_id: format!("test-{}", uuid::Uuid::new_v4()),
            // Use a simple image that exits immediately — won't listen on a port
            image: "alpine:latest".into(),
            bundle_path: "/tmp".into(),
            library_path: "/tmp".into(),
            worker_mount: "/app".into(),
            shiny_port: 3838,
            memory_limit: None,
            cpu_limit: None,
            labels: Default::default(),
        };

        let handle = backend.spawn(&spec).await.unwrap();

        // Health check should fail — nothing listening on port
        let healthy = backend.health_check(&handle).await;
        assert!(!healthy);

        backend.stop(&handle).await.unwrap();
    }

    #[tokio::test]
    async fn orphan_cleanup() {
        let backend = DockerBackend::new(test_config()).await.unwrap();

        let spec = WorkerSpec {
            app_id: "test-app".into(),
            worker_id: format!("test-{}", uuid::Uuid::new_v4()),
            image: "alpine:latest".into(),
            bundle_path: "/tmp".into(),
            library_path: "/tmp".into(),
            worker_mount: "/app".into(),
            shiny_port: 3838,
            memory_limit: None,
            cpu_limit: None,
            labels: Default::default(),
        };

        let handle = backend.spawn(&spec).await.unwrap();

        // Simulate crash — don't call stop(), just list and clean up
        let managed = backend.list_managed().await.unwrap();
        assert!(!managed.is_empty());

        for resource in &managed {
            backend.remove_resource(resource).await.unwrap();
        }

        // Should be clean now
        let remaining = backend.list_managed().await.unwrap();
        assert!(remaining.is_empty());
    }

    #[tokio::test]
    async fn memory_limit_parsing() {
        assert_eq!(parse_memory_limit("512m"), Some(512 * 1024 * 1024));
        assert_eq!(parse_memory_limit("1g"), Some(1024 * 1024 * 1024));
        assert_eq!(parse_memory_limit("256mb"), Some(256 * 1024 * 1024));
        assert_eq!(parse_memory_limit("100kb"), Some(100 * 1024));
    }
}
```

### Step 13: Enable Docker CI job

Flip the `docker-tests` CI job from `if: false` to active. The job needs
Docker available on the runner — GitHub Actions `ubuntu-latest` has Docker
pre-installed.

```yaml
docker-tests:
    runs-on: ubuntu-latest
    # Removed: if: false
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
      - uses: Swatinem/rust-cache@v2
      - run: docker pull ghcr.io/rocker-org/r-ver:latest
      - run: docker pull alpine:latest
      - run: cargo test --features docker-tests
```

**Note:** Pre-pulling images avoids timeout flakiness in tests.

## Container hardening summary

All blockyard-managed containers (workers and builds) apply these security
settings:

| Setting | Value | Why |
|---|---|---|
| `cap_drop` | `ALL` | Drop all Linux capabilities — Shiny needs none |
| `security_opt` | `no-new-privileges` | Prevent privilege escalation via setuid/setgid |
| `readonly_rootfs` | `true` (workers only) | Prevent filesystem writes outside mounts |
| `tmpfs` | `/tmp` (all), `/root/.cache/rv` (builds) | Writable scratch space |
| Published ports | None | Workers are only reachable via the bridge network |
| Network | Per-worker bridge | Workers cannot reach each other |

Build containers do not use `readonly_rootfs` — they need to download and
install the `rv` binary to `/usr/local/bin` before running `rv sync`. The
build container is short-lived and discarded after completion.

## Network topology

```
                  ┌─────────────────┐
                  │  blockyard      │
                  │  server          │
                  │  (container or  │
                  │   native)       │
                  └──┬──┬──┬───────┘
                     │  │  │
          ┌──────────┘  │  └──────────┐
          │             │             │
   ┌──────▼──────┐ ┌───▼──────┐ ┌────▼─────┐
   │ blockyard-  │ │ blockyard-│ │ blockyard-│
   │ {worker-1}  │ │ {worker-2}│ │ {worker-3}│
   │ (bridge)    │ │ (bridge)  │ │ (bridge)  │
   └──────┬──────┘ └───┬──────┘ └────┬─────┘
          │             │             │
   ┌──────▼──────┐ ┌───▼──────┐ ┌────▼─────┐
   │  worker-1   │ │ worker-2 │ │ worker-3 │
   │  container  │ │ container│ │ container│
   └─────────────┘ └──────────┘ └──────────┘
```

Each worker gets its own bridge network. The server is multi-homed — it
joins every worker network so it can route to any worker. Workers are
isolated from each other (no shared network).

When the server is running natively (not in a container), network joining is
skipped. Workers are reachable on the Docker bridge gateway IP instead.

## New dependency

```toml
futures-util = "0.3"   # for StreamExt on bollard log streams
```

## Exit criteria

Phase 0-2 is done when:

- `DockerBackend::new()` connects to Docker and detects server ID
- `spawn()` creates a network + hardened container, returns a handle
- `addr()` resolves the worker's IP on its named network
- `stop()` cleans up the container and network
- `health_check()` returns `true`/`false` based on TCP reachability
- `logs()` streams container stdout/stderr
- `build()` runs a build container to completion and returns exit code
- `list_managed()` discovers all labeled containers and networks
- `remove_resource()` removes containers and networks by ID
- Memory limit parsing handles common units (`m`, `g`, `mb`, `gb`)
- All Docker integration tests pass with a real Docker daemon
- `docker-tests` CI job is enabled and green
- `cargo test --features test-support` still passes (mock backend unaffected)
- `cargo clippy` clean

## Implementation notes

- **bollard version:** 0.18 is already in Cargo.toml from phase 0-1. No
  version change needed.

- **Error handling:** all Docker API calls map bollard errors to the
  appropriate `BackendError` variant. The error message includes enough
  context to diagnose failures (container ID, network name, etc.).

- **Stop ordering matters.** When stopping a worker: stop container first,
  remove container, disconnect server from network, remove network. If you
  remove the network before disconnecting the server, the network removal
  fails because it still has connected endpoints.

- **Build container cleanup.** The build container is removed after
  completion regardless of success or failure. We don't use Docker's
  `AutoRemove` because we need to inspect the exit code before the container
  disappears.

- **Native mode (no server ID).** When the server runs outside Docker,
  `spawn()` and `stop()` skip the network join/disconnect steps.
  `addr()` returns the worker's IP on its bridge network, which must be
  routable from the host for native mode to work. This is the case on
  Linux and with some macOS Docker runtimes, but not all. If container
  IPs are not routable from your host, run the server inside a container
  (e.g. the devcontainer) instead.

- **Image pulling is not handled in this phase.** `ensure_image()` is
  added in phase 0-3 and wired into `build()` and `spawn()` as their first
  step — pull on demand, not at startup. For phase 0-2, images must be
  pre-pulled.

- **rv library path namespacing.** rv's default library path is
  `<project>/rv/library/<R version>/<arch>/<codename>` (e.g.
  `/app/rv/library/4.4/x86_64-pc-linux-gnu/jammy`). The build container
  mounts the host library dir at `/app/rv/library` and rv creates the
  namespaced subdirectories inside it. Two things to verify during testing:
  1. That rv correctly creates and populates the namespaced subdirectory
     under the mount point.
  2. That the worker container's R process finds the library at runtime —
     R needs `.libPaths()` to include the full namespaced path. This may
     require setting `R_LIBS` or `R_LIBS_USER` in the worker container's
     env, or relying on rv/the image entrypoint to handle it.
