//! Docker/Podman backend implementation.

#[cfg(feature = "docker")]
use std::collections::HashMap;
#[cfg(feature = "docker")]
use std::net::SocketAddr;
#[cfg(feature = "docker")]
use std::sync::Arc;

#[cfg(feature = "docker")]
use bollard::Docker;
#[cfg(feature = "docker")]
use bollard::container::{
    Config, CreateContainerOptions, ListContainersOptions, LogsOptions, RemoveContainerOptions,
    StopContainerOptions,
};
#[cfg(feature = "docker")]
use bollard::models::{EndpointSettings, HostConfig};
#[cfg(feature = "docker")]
use bollard::network::{
    ConnectNetworkOptions, CreateNetworkOptions, DisconnectNetworkOptions, ListNetworksOptions,
};
#[cfg(feature = "docker")]
use futures_util::StreamExt;

#[cfg(feature = "docker")]
use crate::backend::*;
#[cfg(feature = "docker")]
use crate::config::DockerConfig;

#[cfg(feature = "docker")]
#[derive(Clone)]
pub struct DockerBackend {
    client: Docker,
    server_id: Option<String>,
    config: DockerConfig,
    metadata_block_mode: Arc<tokio::sync::OnceCell<MetadataBlockMode>>,
}

#[cfg(feature = "docker")]
#[derive(Debug, Clone)]
enum MetadataBlockMode {
    /// Server inserts/removes per-network iptables rules
    ServerManaged,
    /// Operator-installed blanket rule detected; skip per-network rules
    HostManaged,
}

#[cfg(feature = "docker")]
#[derive(Debug, Clone)]
pub struct DockerHandle {
    pub container_id: String,
    pub network_id: String,
    pub network_name: String,
}

#[cfg(feature = "docker")]
impl WorkerHandle for DockerHandle {
    fn id(&self) -> &str {
        &self.container_id
    }
}

#[cfg(feature = "docker")]
impl DockerBackend {
    pub async fn new(config: DockerConfig) -> Result<Self, BackendError> {
        let client = Docker::connect_with_unix(&config.socket, 120, bollard::API_DEFAULT_VERSION)
            .map_err(|e| BackendError::Spawn(format!("Docker connect failed: {e}")))?;

        // Verify connectivity
        client
            .ping()
            .await
            .map_err(|e| BackendError::Spawn(format!("Docker ping failed: {e}")))?;

        let server_id = detect_server_id().await;

        Ok(Self {
            client,
            server_id,
            config,
            metadata_block_mode: Arc::new(tokio::sync::OnceCell::new()),
        })
    }

    async fn create_network(
        &self,
        name: &str,
        app_id: &str,
        worker_id: &str,
    ) -> Result<String, BackendError> {
        let options = CreateNetworkOptions {
            name: name.to_string(),
            driver: "bridge".to_string(),
            labels: network_labels(app_id, worker_id),
            ..Default::default()
        };

        let response = self
            .client
            .create_network(options)
            .await
            .map_err(|e| BackendError::Spawn(format!("create network: {e}")))?;

        Ok(response.id)
    }

    async fn create_worker_container(
        &self,
        spec: &WorkerSpec,
        network_name: &str,
    ) -> Result<String, BackendError> {
        let container_name = format!("blockyard-worker-{}", spec.worker_id);

        let host_config = HostConfig {
            network_mode: Some(network_name.to_string()),
            binds: Some(vec![
                format!(
                    "{}:{}:ro",
                    spec.bundle_path.display(),
                    spec.worker_mount.display()
                ),
                format!("{}:/blockyard-lib:ro", spec.library_path.display()),
            ]),
            tmpfs: Some(HashMap::from([("/tmp".to_string(), "".to_string())])),
            cap_drop: Some(vec!["ALL".to_string()]),
            security_opt: Some(vec!["no-new-privileges".to_string()]),
            readonly_rootfs: Some(true),
            memory: spec
                .memory_limit
                .as_ref()
                .and_then(|m| parse_memory_limit(m)),
            nano_cpus: spec.cpu_limit.map(|c| (c * 1e9) as i64),
            ..Default::default()
        };

        let config = Config {
            image: Some(spec.image.clone()),
            env: Some(vec![
                format!("SHINY_PORT={}", spec.shiny_port),
                "R_LIBS=/blockyard-lib".to_string(),
            ]),
            labels: Some(worker_labels(spec)),
            host_config: Some(host_config),
            ..Default::default()
        };

        let options = CreateContainerOptions {
            name: container_name.as_str(),
            platform: None,
        };

        let response = self
            .client
            .create_container(Some(options), config)
            .await
            .map_err(|e| BackendError::Spawn(format!("create container: {e}")))?;

        Ok(response.id)
    }

    async fn join_network(
        &self,
        container_id: &str,
        network_name: &str,
    ) -> Result<(), BackendError> {
        let options = ConnectNetworkOptions {
            container: container_id.to_string(),
            endpoint_config: EndpointSettings::default(),
        };

        self.client
            .connect_network(network_name, options)
            .await
            .map_err(|e| BackendError::Spawn(format!("join network: {e}")))?;

        Ok(())
    }

    /// Pull the image if it's not already present locally.
    pub async fn ensure_image(&self, image: &str) -> Result<(), BackendError> {
        use bollard::image::CreateImageOptions;

        // Check if image exists locally
        match self.client.inspect_image(image).await {
            Ok(_) => {
                tracing::debug!(image, "image already present");
                return Ok(());
            }
            Err(_) => {
                tracing::info!(image, "pulling image");
            }
        }

        let options = CreateImageOptions {
            from_image: image,
            ..Default::default()
        };

        let mut stream = self.client.create_image(Some(options), None, None);
        while let Some(result) = stream.next().await {
            match result {
                Ok(info) => {
                    if let Some(status) = info.status {
                        tracing::debug!(image, status, "pull progress");
                    }
                }
                Err(e) => {
                    return Err(BackendError::Build(format!(
                        "image pull failed for '{image}': {e}"
                    )));
                }
            }
        }

        tracing::info!(image, "image pulled successfully");
        Ok(())
    }

    /// Best-effort force-remove a container. Used for cleanup on spawn failure.
    async fn force_remove_container(&self, container_id: &str) {
        let _ = self
            .client
            .remove_container(
                container_id,
                Some(RemoveContainerOptions {
                    force: true,
                    ..Default::default()
                }),
            )
            .await;
    }

    async fn disconnect_network(
        &self,
        container_id: &str,
        network_name: &str,
    ) -> Result<(), BackendError> {
        let options = DisconnectNetworkOptions {
            container: container_id.to_string(),
            force: true,
        };

        self.client
            .disconnect_network(network_name, options)
            .await
            .map_err(|e| BackendError::Stop(format!("disconnect network: {e}")))?;

        Ok(())
    }

    /// Block metadata endpoint access for a specific bridge network.
    async fn block_metadata_for_network(
        &self,
        network_name: &str,
        worker_id: &str,
    ) -> Result<(), BackendError> {
        // Check cached mode first
        if let Some(mode) = self.metadata_block_mode.get() {
            match mode {
                MetadataBlockMode::ServerManaged => {}
                MetadataBlockMode::HostManaged => return Ok(()),
            }
        }

        // Inspect network to get subnet CIDR
        let network = self
            .client
            .inspect_network::<String>(network_name, None)
            .await
            .map_err(|e| BackendError::Spawn(format!("inspect network: {e}")))?;

        let subnet = network
            .ipam
            .and_then(|ipam| ipam.config)
            .and_then(|configs| configs.into_iter().next())
            .and_then(|config| config.subnet)
            .ok_or_else(|| {
                BackendError::Spawn(format!("no subnet found for network {network_name}"))
            })?;

        // Try inserting iptables rule
        let comment = format!("blockyard-{worker_id}");
        let status = tokio::process::Command::new("iptables")
            .args([
                "-I",
                "DOCKER-USER",
                "-s",
                &subnet,
                "-d",
                "169.254.169.254/32",
                "-j",
                "DROP",
                "-m",
                "comment",
                "--comment",
                &comment,
            ])
            .status()
            .await;

        match status {
            Ok(s) if s.success() => {
                tracing::debug!(worker_id, subnet, "metadata endpoint blocked");
                let _ = self
                    .metadata_block_mode
                    .set(MetadataBlockMode::ServerManaged);
                Ok(())
            }
            Ok(_) | Err(_) => {
                // iptables failed — check if a host-level rule already exists
                if self.host_blocks_metadata_endpoint().await {
                    tracing::info!("metadata endpoint blocked by host-level rule");
                    let _ = self.metadata_block_mode.set(MetadataBlockMode::HostManaged);
                    Ok(())
                } else {
                    Err(BackendError::Spawn(
                        "cannot block metadata endpoint: grant CAP_NET_ADMIN to the \
                         server container, or add a host-level iptables rule: \
                         iptables -I DOCKER-USER -d 169.254.169.254/32 -j DROP"
                            .into(),
                    ))
                }
            }
        }
    }

    /// Check if the metadata endpoint is already blocked for Docker containers.
    ///
    /// In native mode (server_id is None), a TCP connect from the host process
    /// does NOT reflect whether Docker containers are blocked — it tests host
    /// networking, not Docker-forwarded traffic. Instead, we check iptables
    /// DOCKER-USER chain for an existing DROP rule targeting 169.254.169.254.
    ///
    /// In container mode (server_id is Some), we share Docker networking, so a
    /// TCP connect is a valid proxy for container reachability.
    async fn host_blocks_metadata_endpoint(&self) -> bool {
        if self.server_id.is_none() {
            // Native mode: check iptables DOCKER-USER chain directly
            return Self::docker_user_blocks_metadata().await;
        }

        // Container mode: TCP connect test
        tokio::time::timeout(
            std::time::Duration::from_secs(2),
            tokio::net::TcpStream::connect("169.254.169.254:80"),
        )
        .await
        .map(|r| r.is_err()) // connection refused/failed = blocked
        .unwrap_or(true) // timeout = blocked
    }

    /// Check if the DOCKER-USER iptables chain contains a DROP rule for 169.254.169.254.
    /// Tries both direct `iptables` and `sudo iptables` since the process may
    /// not have CAP_NET_ADMIN but may have passwordless sudo.
    async fn docker_user_blocks_metadata() -> bool {
        for cmd in ["iptables", "sudo"] {
            let result = if cmd == "sudo" {
                tokio::process::Command::new("sudo")
                    .args(["iptables", "-S", "DOCKER-USER"])
                    .output()
                    .await
            } else {
                tokio::process::Command::new("iptables")
                    .args(["-S", "DOCKER-USER"])
                    .output()
                    .await
            };

            if let Ok(output) = result
                && output.status.success()
            {
                let stdout = String::from_utf8_lossy(&output.stdout);
                return stdout
                    .lines()
                    .any(|line| line.contains("169.254.169.254") && line.contains("DROP"));
            }
        }
        false
    }

    /// Remove the iptables rule for a worker.
    async fn unblock_metadata_for_worker(&self, worker_id: &str) {
        if let Some(MetadataBlockMode::HostManaged) = self.metadata_block_mode.get() {
            return;
        }
        let comment = format!("blockyard-{worker_id}");
        delete_iptables_rules_by_comment(&comment).await;
    }
}

/// Delete all iptables rules in DOCKER-USER whose comment contains the
/// given string.
#[cfg(feature = "docker")]
async fn delete_iptables_rules_by_comment(comment: &str) {
    let output = tokio::process::Command::new("iptables")
        .args(["-S", "DOCKER-USER"])
        .output()
        .await;

    let Ok(output) = output else { return };
    let stdout = String::from_utf8_lossy(&output.stdout);

    for line in stdout.lines() {
        if line.contains(comment)
            && let Some(rule) = line.strip_prefix("-A DOCKER-USER ")
        {
            let mut args = vec!["-D", "DOCKER-USER"];
            args.extend(rule.split_whitespace());
            let _ = tokio::process::Command::new("iptables")
                .args(&args)
                .status()
                .await;
        }
    }
}

/// Remove all orphaned blockyard iptables rules from previous runs.
#[cfg(feature = "docker")]
pub async fn cleanup_orphan_metadata_rules() {
    delete_iptables_rules_by_comment("blockyard-").await;
}

#[cfg(feature = "docker")]
impl Backend for DockerBackend {
    type Handle = DockerHandle;

    async fn spawn(&self, spec: &WorkerSpec) -> Result<DockerHandle, BackendError> {
        // Ensure image is available before spawning
        self.ensure_image(&spec.image).await?;

        let network_name = format!("blockyard-{}", spec.worker_id);

        // 1. Create per-worker bridge network
        let network_id = self
            .create_network(&network_name, &spec.app_id, &spec.worker_id)
            .await?;

        // 2. Block metadata endpoint for this network
        if let Err(e) = self
            .block_metadata_for_network(&network_name, &spec.worker_id)
            .await
        {
            let _ = self.client.remove_network(&network_name).await;
            return Err(e);
        }

        // 3. Create container — clean up network on failure
        let container_id = match self.create_worker_container(spec, &network_name).await {
            Ok(id) => id,
            Err(e) => {
                self.unblock_metadata_for_worker(&spec.worker_id).await;
                let _ = self.client.remove_network(&network_name).await;
                return Err(e);
            }
        };

        // 4. Join server to worker network (if running in a container)
        if let Some(ref server_id) = self.server_id
            && let Err(e) = self.join_network(server_id, &network_name).await
        {
            self.force_remove_container(&container_id).await;
            let _ = self.client.remove_network(&network_name).await;
            return Err(e);
        }

        // 5. Start the container — clean up everything on failure
        if let Err(e) = self
            .client
            .start_container::<String>(&container_id, None)
            .await
        {
            self.force_remove_container(&container_id).await;
            if let Some(ref server_id) = self.server_id {
                let _ = self.disconnect_network(server_id, &network_name).await;
            }
            let _ = self.client.remove_network(&network_name).await;
            return Err(BackendError::Spawn(format!("start container: {e}")));
        }

        Ok(DockerHandle {
            container_id,
            network_id,
            network_name,
        })
    }

    async fn stop(&self, handle: &DockerHandle) -> Result<(), BackendError> {
        // 1. Stop the container (10s timeout) — ignore 304 (already stopped) and 404 (already gone)
        if let Err(e) = self
            .client
            .stop_container(&handle.container_id, Some(StopContainerOptions { t: 10 }))
            .await
            && !is_docker_status(&e, &[304, 404])
        {
            return Err(BackendError::Stop(format!("stop container: {e}")));
        }

        // 2. Remove the container — ignore 404 (already gone) and 409 (removal in progress)
        if let Err(e) = self
            .client
            .remove_container(
                &handle.container_id,
                Some(RemoveContainerOptions {
                    force: true,
                    ..Default::default()
                }),
            )
            .await
            && !is_docker_status(&e, &[404, 409])
        {
            return Err(BackendError::Stop(format!("remove container: {e}")));
        }

        // 3. Disconnect server from the worker's network
        if let Some(ref server_id) = self.server_id {
            let _ = self
                .disconnect_network(server_id, &handle.network_name)
                .await;
        }

        // 4. Remove iptables metadata block rule
        if let Some(worker_id) = handle.network_name.strip_prefix("blockyard-") {
            self.unblock_metadata_for_worker(worker_id).await;
        }

        // 5. Remove the network
        self.client
            .remove_network(&handle.network_name)
            .await
            .map_err(|e| BackendError::Stop(format!("remove network: {e}")))?;

        Ok(())
    }

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

    async fn logs(&self, handle: &DockerHandle) -> Result<LogStream, BackendError> {
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

    async fn addr(&self, handle: &DockerHandle) -> Result<SocketAddr, BackendError> {
        let info = self
            .client
            .inspect_container(&handle.container_id, None)
            .await
            .map_err(|e| BackendError::Addr(format!("inspect container: {e}")))?;

        let networks = info
            .network_settings
            .and_then(|ns| ns.networks)
            .ok_or_else(|| BackendError::Addr("no networks on container".into()))?;

        let endpoint = networks.get(&handle.network_name).ok_or_else(|| {
            BackendError::Addr(format!("container not on network {}", handle.network_name))
        })?;

        let ip = endpoint
            .ip_address
            .as_ref()
            .filter(|s| !s.is_empty())
            .ok_or_else(|| BackendError::Addr("no IP on network".into()))?;

        let addr: std::net::IpAddr = ip
            .parse()
            .map_err(|e| BackendError::Addr(format!("invalid IP '{ip}': {e}")))?;

        Ok(SocketAddr::new(addr, self.config.shiny_port))
    }

    async fn build(&self, spec: &BuildSpec) -> Result<BuildResult, BackendError> {
        // Ensure image is available before building
        self.ensure_image(&spec.image).await?;

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
            // rootfs not read-only -- need to write rv binary to /usr/local/bin
            ..Default::default()
        };

        // Download rv and run sync in one shot
        let rv_url = format!(
            "https://github.com/a2-ai/rv/releases/download/{}/rv-x86_64-unknown-linux-gnu",
            self.config.rv_version,
        );
        let install_and_sync = vec![
            "sh".to_string(),
            "-c".to_string(),
            format!(
                "curl -sSL {rv_url} -o /usr/local/bin/rv && chmod +x /usr/local/bin/rv && rv sync"
            ),
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
            name: container_name.as_str(),
            platform: None,
        };

        let response = self
            .client
            .create_container(Some(options), config)
            .await
            .map_err(|e| BackendError::Build(format!("create build container: {e}")))?;

        let container_id = response.id;

        // Start the build container
        self.client
            .start_container::<String>(&container_id, None)
            .await
            .map_err(|e| BackendError::Build(format!("start build container: {e}")))?;

        // Wait for completion
        let wait_result = self
            .client
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
        let _ = self
            .client
            .remove_container(
                &container_id,
                Some(RemoveContainerOptions {
                    force: true,
                    ..Default::default()
                }),
            )
            .await;

        Ok(BuildResult { success, exit_code })
    }

    async fn list_managed(&self) -> Result<Vec<ManagedResource>, BackendError> {
        let mut resources = Vec::new();

        // Find managed containers (including stopped)
        let container_filters = HashMap::from([(
            "label".to_string(),
            vec!["dev.blockyard/managed=true".to_string()],
        )]);
        let containers = self
            .client
            .list_containers(Some(ListContainersOptions {
                all: true,
                filters: container_filters,
                ..Default::default()
            }))
            .await
            .map_err(|e| BackendError::Cleanup(format!("list containers: {e}")))?;

        for c in containers {
            if let Some(id) = c.id {
                resources.push(ManagedResource {
                    id,
                    kind: ResourceKind::Container,
                });
            }
        }

        // Find managed networks
        let network_filters = HashMap::from([(
            "label".to_string(),
            vec!["dev.blockyard/managed=true".to_string()],
        )]);
        let networks = self
            .client
            .list_networks(Some(ListNetworksOptions {
                filters: network_filters,
            }))
            .await
            .map_err(|e| BackendError::Cleanup(format!("list networks: {e}")))?;

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
                if let Err(e) = self
                    .client
                    .remove_container(
                        &resource.id,
                        Some(RemoveContainerOptions {
                            force: true,
                            ..Default::default()
                        }),
                    )
                    .await
                    && !is_docker_status(&e, &[404, 409])
                {
                    return Err(BackendError::Cleanup(format!("remove container: {e}")));
                }
            }
            ResourceKind::Network => {
                if let Err(e) = self.client.remove_network(&resource.id).await
                    && !is_docker_status(&e, &[404])
                {
                    return Err(BackendError::Cleanup(format!("remove network: {e}")));
                }
            }
        }
        Ok(())
    }
}

// --- Helper functions ---

/// Check if a bollard error is a Docker API response with one of the given status codes.
#[cfg(feature = "docker")]
fn is_docker_status(err: &bollard::errors::Error, codes: &[u16]) -> bool {
    matches!(err, bollard::errors::Error::DockerResponseServerError { status_code, .. } if codes.contains(status_code))
}

#[cfg(feature = "docker")]
fn worker_labels(spec: &WorkerSpec) -> HashMap<String, String> {
    let mut labels = HashMap::new();
    labels.insert("dev.blockyard/managed".into(), "true".into());
    labels.insert("dev.blockyard/app-id".into(), spec.app_id.clone());
    labels.insert("dev.blockyard/worker-id".into(), spec.worker_id.clone());
    labels.insert("dev.blockyard/role".into(), "worker".into());
    labels.extend(spec.labels.clone());
    labels
}

#[cfg(feature = "docker")]
fn build_labels(spec: &BuildSpec) -> HashMap<String, String> {
    let mut labels = HashMap::new();
    labels.insert("dev.blockyard/managed".into(), "true".into());
    labels.insert("dev.blockyard/app-id".into(), spec.app_id.clone());
    labels.insert("dev.blockyard/bundle-id".into(), spec.bundle_id.clone());
    labels.insert("dev.blockyard/role".into(), "build".into());
    labels.extend(spec.labels.clone());
    labels
}

#[cfg(feature = "docker")]
fn network_labels(app_id: &str, worker_id: &str) -> HashMap<String, String> {
    let mut labels = HashMap::new();
    labels.insert("dev.blockyard/managed".into(), "true".into());
    labels.insert("dev.blockyard/app-id".into(), app_id.into());
    labels.insert("dev.blockyard/worker-id".into(), worker_id.into());
    labels
}

/// Parse human-readable memory limits like "512m", "1g", "256mb" to bytes.
#[cfg(feature = "docker")]
fn parse_memory_limit(s: &str) -> Option<i64> {
    let s = s.trim().to_lowercase();
    let (num_str, multiplier) = if s.ends_with("gb") || s.ends_with('g') {
        (
            s.trim_end_matches("gb").trim_end_matches('g'),
            1024 * 1024 * 1024,
        )
    } else if s.ends_with("mb") || s.ends_with('m') {
        (s.trim_end_matches("mb").trim_end_matches('m'), 1024 * 1024)
    } else if s.ends_with("kb") || s.ends_with('k') {
        (s.trim_end_matches("kb").trim_end_matches('k'), 1024)
    } else {
        (s.as_str(), 1) // assume bytes
    };
    num_str.trim().parse::<i64>().ok().map(|n| n * multiplier)
}

// --- Server ID detection ---

#[cfg(feature = "docker")]
async fn detect_server_id() -> Option<String> {
    // 1. Explicit env var
    if let Ok(id) = std::env::var("BLOCKYARD_SERVER_ID")
        && !id.is_empty()
    {
        tracing::info!(container_id = %id, "server ID from env");
        return Some(id);
    }

    // 2. Parse /proc/self/cgroup
    if let Ok(cgroup) = tokio::fs::read_to_string("/proc/self/cgroup").await {
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

#[cfg(feature = "docker")]
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
            if let Some(id) = candidate
                && id.len() >= 12
                && id.chars().all(|c| c.is_ascii_hexdigit())
            {
                return Some(id);
            }
        }
    }
    None
}

// --- Tests ---

#[cfg(all(test, feature = "docker"))]
mod unit_tests {
    use super::*;

    #[test]
    fn memory_limit_parsing() {
        assert_eq!(parse_memory_limit("512m"), Some(512 * 1024 * 1024));
        assert_eq!(parse_memory_limit("1g"), Some(1024 * 1024 * 1024));
        assert_eq!(parse_memory_limit("256mb"), Some(256 * 1024 * 1024));
        assert_eq!(parse_memory_limit("100kb"), Some(100 * 1024));
        assert_eq!(parse_memory_limit("1024"), Some(1024));
        assert_eq!(parse_memory_limit("  2g  "), Some(2 * 1024 * 1024 * 1024));
        assert_eq!(parse_memory_limit("invalid"), None);
    }

    #[test]
    fn cgroup_id_extraction() {
        assert_eq!(
            extract_container_id_from_cgroup(
                "0::/docker/abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
            ),
            Some("abc123def456abc123def456abc123def456abc123def456abc123def456abcd".to_string())
        );

        assert_eq!(
            extract_container_id_from_cgroup(
                "0::/system.slice/docker-abc123def456abc123def456abc123def456abc123def456abc123def456abcd.scope"
            ),
            Some("abc123def456abc123def456abc123def456abc123def456abc123def456abcd".to_string())
        );

        // Not a docker cgroup line
        assert_eq!(
            extract_container_id_from_cgroup("0::/user.slice/user-1000.slice"),
            None
        );

        // Too short to be a container ID
        assert_eq!(extract_container_id_from_cgroup("0::/docker/abc"), None);
    }

    #[test]
    fn worker_labels_include_required_fields() {
        let spec = WorkerSpec {
            app_id: "app-1".into(),
            worker_id: "worker-1".into(),
            image: "test:latest".into(),
            bundle_path: "/tmp".into(),
            library_path: "/tmp".into(),
            worker_mount: "/app".into(),
            shiny_port: 3838,
            memory_limit: None,
            cpu_limit: None,
            labels: Default::default(),
        };

        let labels = worker_labels(&spec);
        assert_eq!(labels.get("dev.blockyard/managed"), Some(&"true".into()));
        assert_eq!(labels.get("dev.blockyard/app-id"), Some(&"app-1".into()));
        assert_eq!(
            labels.get("dev.blockyard/worker-id"),
            Some(&"worker-1".into())
        );
        assert_eq!(labels.get("dev.blockyard/role"), Some(&"worker".into()));
    }

    #[test]
    fn build_labels_include_required_fields() {
        let spec = BuildSpec {
            app_id: "app-1".into(),
            bundle_id: "bundle-1".into(),
            image: "test:latest".into(),
            bundle_path: "/tmp".into(),
            library_path: "/tmp".into(),
            labels: Default::default(),
        };

        let labels = build_labels(&spec);
        assert_eq!(labels.get("dev.blockyard/managed"), Some(&"true".into()));
        assert_eq!(labels.get("dev.blockyard/app-id"), Some(&"app-1".into()));
        assert_eq!(
            labels.get("dev.blockyard/bundle-id"),
            Some(&"bundle-1".into())
        );
        assert_eq!(labels.get("dev.blockyard/role"), Some(&"build".into()));
    }

    #[test]
    fn network_labels_include_required_fields() {
        let labels = network_labels("app-1", "worker-1");
        assert_eq!(labels.get("dev.blockyard/managed"), Some(&"true".into()));
        assert_eq!(labels.get("dev.blockyard/app-id"), Some(&"app-1".into()));
        assert_eq!(
            labels.get("dev.blockyard/worker-id"),
            Some(&"worker-1".into())
        );
    }
}

#[cfg(all(test, feature = "docker-tests"))]
mod integration_tests {
    use super::*;

    fn test_config() -> DockerConfig {
        DockerConfig {
            socket: "/var/run/docker.sock".into(),
            image: crate::config::DEFAULT_IMAGE.into(),
            shiny_port: 3838,
            rv_version: "latest".into(),
        }
    }

    #[tokio::test]
    async fn spawn_and_stop_container() {
        let backend = DockerBackend::new(test_config()).await.unwrap();

        let spec = WorkerSpec {
            app_id: "test-app".into(),
            worker_id: format!("test-{}", uuid::Uuid::new_v4()),
            image: "ghcr.io/rocker-org/r-ver:latest".into(),
            bundle_path: "/tmp".into(),
            library_path: "/tmp".into(),
            worker_mount: "/app".into(),
            shiny_port: 3838,
            memory_limit: Some("256m".into()),
            cpu_limit: Some(0.5),
            labels: Default::default(),
        };

        let handle = backend.spawn(&spec).await.unwrap();

        // Container should be running — address should resolve
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
            // Use alpine — exits quickly, won't listen on a port
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

        // Clean up — ignore stop errors since alpine may have already exited
        let _ = backend.stop(&handle).await;
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

        let _handle = backend.spawn(&spec).await.unwrap();

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
}
