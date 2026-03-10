use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::PathBuf;

#[cfg(feature = "docker")]
pub mod docker;
#[cfg(feature = "test-support")]
pub mod mock;

/// Pluggable container runtime. Docker/Podman for v0, Kubernetes for v2.
pub trait Backend: Send + Sync + 'static {
    type Handle: WorkerHandle;

    /// Spawn a long-lived worker (Shiny app container).
    fn spawn(
        &self,
        spec: &WorkerSpec,
    ) -> impl std::future::Future<Output = Result<Self::Handle, BackendError>> + Send;

    /// Stop and remove a worker.
    fn stop(
        &self,
        handle: &Self::Handle,
    ) -> impl std::future::Future<Output = Result<(), BackendError>> + Send;

    /// TCP or HTTP health check against the worker.
    fn health_check(&self, handle: &Self::Handle)
    -> impl std::future::Future<Output = bool> + Send;

    /// Stream stdout/stderr logs from the worker.
    fn logs(
        &self,
        handle: &Self::Handle,
    ) -> impl std::future::Future<Output = Result<LogStream, BackendError>> + Send;

    /// Resolve the worker's address (IP + Shiny port).
    fn addr(
        &self,
        handle: &Self::Handle,
    ) -> impl std::future::Future<Output = Result<SocketAddr, BackendError>> + Send;

    /// Run a build task to completion (dependency restore).
    fn build(
        &self,
        spec: &BuildSpec,
    ) -> impl std::future::Future<Output = Result<BuildResult, BackendError>> + Send;

    /// List all managed resources (containers + networks) for orphan cleanup.
    fn list_managed(
        &self,
    ) -> impl std::future::Future<Output = Result<Vec<ManagedResource>, BackendError>> + Send;

    /// Remove an orphaned resource.
    fn remove_resource(
        &self,
        resource: &ManagedResource,
    ) -> impl std::future::Future<Output = Result<(), BackendError>> + Send;
}

pub trait WorkerHandle: Send + Sync + Clone + std::fmt::Debug {
    fn id(&self) -> &str;
}

/// Everything a backend needs to launch a worker.
#[derive(Debug, Clone)]
pub struct WorkerSpec {
    pub app_id: String,
    pub worker_id: String,
    pub image: String,
    pub bundle_path: PathBuf,
    pub library_path: PathBuf,
    pub worker_mount: PathBuf,
    pub shiny_port: u16,
    pub memory_limit: Option<String>,
    pub cpu_limit: Option<f64>,
    pub labels: HashMap<String, String>,
}

/// Everything a backend needs to run a build task (dependency restore).
#[derive(Debug, Clone)]
pub struct BuildSpec {
    pub app_id: String,
    pub bundle_id: String,
    pub image: String,
    pub bundle_path: PathBuf,
    pub library_path: PathBuf,
    pub labels: HashMap<String, String>,
}

#[derive(Debug)]
pub struct BuildResult {
    pub success: bool,
    pub exit_code: Option<i64>,
}

/// A managed resource discovered during orphan cleanup.
#[derive(Debug)]
pub struct ManagedResource {
    pub id: String,
    pub kind: ResourceKind,
}

/// Resource kinds ordered by removal priority: containers must be removed
/// before networks (networks fail to remove while containers are connected).
#[derive(Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum ResourceKind {
    Container,
    Network,
}

/// A stream of log lines.
pub type LogStream = tokio::sync::mpsc::Receiver<String>;

#[derive(Debug, thiserror::Error)]
pub enum BackendError {
    #[error("spawn failed: {0}")]
    Spawn(String),
    #[error("stop failed: {0}")]
    Stop(String),
    #[error("build failed: {0}")]
    Build(String),
    #[error("address resolution failed: {0}")]
    Addr(String),
    #[error("log streaming failed: {0}")]
    Logs(String),
    #[error("cleanup failed: {0}")]
    Cleanup(String),
}
