#![cfg(feature = "test-support")]

use std::net::SocketAddr;
use std::sync::atomic::{AtomicBool, Ordering};

use dashmap::DashMap;

use crate::backend::*;

pub struct MockBackend {
    workers: DashMap<String, MockWorker>,
    pub health_response: AtomicBool,
    pub build_success: AtomicBool,
}

#[derive(Debug, Clone)]
pub struct MockHandle {
    pub id: String,
    pub addr: SocketAddr,
}

impl WorkerHandle for MockHandle {
    fn id(&self) -> &str {
        &self.id
    }
}

struct MockWorker {
    _handle: MockHandle,
    _listener: tokio::net::TcpListener,
}

impl MockBackend {
    pub fn new() -> Self {
        Self {
            workers: DashMap::new(),
            health_response: AtomicBool::new(true),
            build_success: AtomicBool::new(true),
        }
    }

    pub fn worker_count(&self) -> usize {
        self.workers.len()
    }

    pub fn has_worker(&self, id: &str) -> bool {
        self.workers.contains_key(id)
    }
}

impl Default for MockBackend {
    fn default() -> Self {
        Self::new()
    }
}

impl Backend for MockBackend {
    type Handle = MockHandle;

    async fn spawn(&self, spec: &WorkerSpec) -> Result<MockHandle, BackendError> {
        // Bind to port 0 to let the OS assign an available port
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .map_err(|e| BackendError::Spawn(e.to_string()))?;
        let actual_addr = listener
            .local_addr()
            .map_err(|e| BackendError::Spawn(e.to_string()))?;

        let handle = MockHandle {
            id: spec.worker_id.clone(),
            addr: actual_addr,
        };

        self.workers.insert(
            spec.worker_id.clone(),
            MockWorker {
                _handle: handle.clone(),
                _listener: listener,
            },
        );

        Ok(handle)
    }

    async fn stop(&self, handle: &MockHandle) -> Result<(), BackendError> {
        self.workers
            .remove(handle.id())
            .ok_or_else(|| BackendError::Stop(format!("worker {} not found", handle.id())))?;
        Ok(())
    }

    async fn health_check(&self, _handle: &MockHandle) -> bool {
        self.health_response.load(Ordering::SeqCst)
    }

    async fn logs(&self, _handle: &MockHandle) -> Result<LogStream, BackendError> {
        let (_tx, rx) = tokio::sync::mpsc::channel(16);
        Ok(rx)
    }

    async fn addr(&self, handle: &MockHandle) -> Result<SocketAddr, BackendError> {
        Ok(handle.addr)
    }

    async fn build(&self, _spec: &BuildSpec) -> Result<BuildResult, BackendError> {
        let success = self.build_success.load(Ordering::SeqCst);
        Ok(BuildResult {
            success,
            exit_code: if success { Some(0) } else { Some(1) },
        })
    }

    async fn list_managed(&self) -> Result<Vec<ManagedResource>, BackendError> {
        Ok(Vec::new())
    }

    async fn remove_resource(&self, _resource: &ManagedResource) -> Result<(), BackendError> {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn spawn_and_stop() {
        let backend = MockBackend::new();
        let spec = test_worker_spec("app-1", "worker-1");
        let handle = backend.spawn(&spec).await.unwrap();
        assert_eq!(backend.worker_count(), 1);
        assert!(backend.has_worker("worker-1"));

        backend.stop(&handle).await.unwrap();
        assert_eq!(backend.worker_count(), 0);
    }

    #[tokio::test]
    async fn health_check_configurable() {
        let backend = MockBackend::new();
        let spec = test_worker_spec("app-1", "worker-1");
        let handle = backend.spawn(&spec).await.unwrap();

        assert!(backend.health_check(&handle).await);

        backend.health_response.store(false, Ordering::SeqCst);
        assert!(!backend.health_check(&handle).await);
    }

    #[tokio::test]
    async fn build_configurable() {
        let backend = MockBackend::new();
        let spec = BuildSpec {
            app_id: "app-1".into(),
            bundle_id: "bundle-1".into(),
            image: "test:latest".into(),
            bundle_path: "/tmp/bundle".into(),
            library_path: "/tmp/lib".into(),
            labels: Default::default(),
        };

        let result = backend.build(&spec).await.unwrap();
        assert!(result.success);

        backend.build_success.store(false, Ordering::SeqCst);
        let result = backend.build(&spec).await.unwrap();
        assert!(!result.success);
    }

    #[tokio::test]
    async fn addr_returns_bound_address() {
        let backend = MockBackend::new();
        let spec = test_worker_spec("app-1", "worker-1");
        let handle = backend.spawn(&spec).await.unwrap();

        let addr = backend.addr(&handle).await.unwrap();
        assert_eq!(addr, handle.addr);
    }

    fn test_worker_spec(app_id: &str, worker_id: &str) -> WorkerSpec {
        WorkerSpec {
            app_id: app_id.into(),
            worker_id: worker_id.into(),
            image: "test:latest".into(),
            bundle_path: "/tmp/bundle".into(),
            library_path: "/tmp/lib".into(),
            worker_mount: "/app".into(),
            shiny_port: 3838,
            memory_limit: None,
            cpu_limit: None,
            labels: Default::default(),
        }
    }
}
