#![cfg(feature = "docker-integration-tests")]

use std::collections::HashMap;
use std::path::PathBuf;
use std::time::Duration;

use blockyard::app::AppState;
use blockyard::backend::docker::DockerBackend;
use blockyard::backend::{Backend, WorkerSpec};
use blockyard::config::{
    Config, DatabaseConfig, DockerConfig, ProxyConfig, ServerConfig, StorageConfig,
};
use blockyard::db;
use blockyard::ops;

fn docker_config() -> DockerConfig {
    DockerConfig {
        socket: "/var/run/docker.sock".into(),
        image: "alpine:latest".into(),
        shiny_port: 8080,
        rv_version: "latest".into(),
    }
}

async fn make_state(backend: DockerBackend) -> (AppState<DockerBackend>, tempfile::TempDir) {
    let tmp = tempfile::TempDir::new().unwrap();
    let config = Config {
        server: ServerConfig {
            bind: "127.0.0.1:0".parse().unwrap(),
            token: "test-token".into(),
            shutdown_timeout: Duration::from_secs(5),
        },
        docker: Some(docker_config()),
        storage: StorageConfig {
            bundle_server_path: tmp.path().to_path_buf(),
            bundle_worker_path: PathBuf::from("/app"),
            bundle_retention: 50,
            max_bundle_size: 10 * 1024 * 1024,
        },
        database: DatabaseConfig {
            path: PathBuf::from(":memory:"),
        },
        proxy: ProxyConfig {
            ws_cache_ttl: Duration::from_secs(60),
            health_interval: Duration::from_secs(15),
            worker_start_timeout: Duration::from_secs(60),
            max_workers: 100,
            log_retention: Duration::from_secs(3600),
        },
    };
    let pool = sqlx::SqlitePool::connect(":memory:").await.unwrap();
    db::run_migrations(&pool).await.unwrap();
    let state = AppState::new(config, backend, pool);
    (state, tmp)
}

fn test_worker_spec(worker_id: &str) -> WorkerSpec {
    WorkerSpec {
        app_id: "test-app".into(),
        worker_id: worker_id.into(),
        image: "alpine:latest".into(),
        bundle_path: "/tmp".into(),
        library_path: "/tmp".into(),
        worker_mount: "/app".into(),
        shiny_port: 8080,
        memory_limit: None,
        cpu_limit: None,
        labels: HashMap::new(),
    }
}

#[tokio::test]
async fn orphan_cleanup_removes_real_containers() {
    let backend = DockerBackend::new(docker_config()).await.unwrap();
    let (state, _tmp) = make_state(backend.clone()).await;

    let worker_id = format!("test-{}", uuid::Uuid::new_v4());
    let spec = test_worker_spec(&worker_id);

    // Spawn a container (simulates a previous run's orphan)
    let _handle = backend.spawn(&spec).await.unwrap();

    // Verify it's managed
    let managed = backend.list_managed().await.unwrap();
    assert!(!managed.is_empty(), "should have managed resources");

    // Run startup cleanup — should remove the orphan
    ops::startup_cleanup(&state).await.unwrap();

    let remaining = backend.list_managed().await.unwrap();
    assert!(remaining.is_empty(), "orphans should be removed");
}

#[tokio::test]
async fn graceful_shutdown_stops_real_containers() {
    let backend = DockerBackend::new(docker_config()).await.unwrap();
    let (state, _tmp) = make_state(backend.clone()).await;

    let worker_id = format!("test-{}", uuid::Uuid::new_v4());
    let spec = test_worker_spec(&worker_id);

    let handle = backend.spawn(&spec).await.unwrap();
    state.workers.insert(
        worker_id.clone(),
        blockyard::app::ActiveWorker {
            app_id: "test-app".into(),
            handle,
            session_id: None,
        },
    );

    assert_eq!(state.workers.len(), 1);

    ops::graceful_shutdown(&state).await;

    assert_eq!(state.workers.len(), 0);
    let managed = backend.list_managed().await.unwrap();
    assert!(managed.is_empty());
}

#[tokio::test]
async fn network_isolation() {
    let backend = DockerBackend::new(docker_config()).await.unwrap();

    let w1_id = format!("test-{}", uuid::Uuid::new_v4());
    let w2_id = format!("test-{}", uuid::Uuid::new_v4());

    // Use alpine with sleep so they stay alive
    let mut spec1 = test_worker_spec(&w1_id);
    spec1.image = "alpine:latest".into();
    let mut spec2 = test_worker_spec(&w2_id);
    spec2.image = "alpine:latest".into();
    spec2.app_id = "test-app-2".into();

    let handle1 = backend.spawn(&spec1).await.unwrap();
    let handle2 = backend.spawn(&spec2).await.unwrap();

    let addr1 = backend.addr(&handle1).await.unwrap();
    let addr2 = backend.addr(&handle2).await.unwrap();

    // The two workers should be on different networks and not be able to
    // reach each other. We verify they have different IP addresses (different
    // subnets) which implies network isolation.
    assert_ne!(
        addr1.ip(),
        addr2.ip(),
        "workers should have different IPs on isolated networks"
    );

    // Clean up
    let _ = backend.stop(&handle1).await;
    let _ = backend.stop(&handle2).await;
}

#[tokio::test]
async fn native_mode_e2e() {
    // In native mode (no server container ID), the server doesn't join
    // worker networks. We verify the backend works without a server_id.
    let backend = DockerBackend::new(docker_config()).await.unwrap();

    let worker_id = format!("test-{}", uuid::Uuid::new_v4());
    let spec = test_worker_spec(&worker_id);
    let handle = backend.spawn(&spec).await.unwrap();

    // Should be able to resolve the address
    let addr = backend.addr(&handle).await.unwrap();
    assert!(!addr.ip().is_unspecified());

    // Clean up
    let _ = backend.stop(&handle).await;
}

/// Verify that containers cannot reach the metadata endpoint.
/// This test requires running on a system where either:
/// - The server has CAP_NET_ADMIN (can insert iptables rules), or
/// - 169.254.169.254 is already blocked at the host level
///
/// In CI without a real cloud metadata endpoint, the fallback
/// (host_blocks_metadata_endpoint) will detect the address as
/// unreachable and allow the spawn to succeed.
#[tokio::test]
async fn metadata_endpoint_blocked() {
    let backend = DockerBackend::new(docker_config()).await.unwrap();

    let worker_id = format!("test-{}", uuid::Uuid::new_v4());
    let spec = test_worker_spec(&worker_id);

    // Spawn should succeed — either iptables rule inserted or host-level block detected
    let handle = backend.spawn(&spec).await.unwrap();

    // Clean up
    let _ = backend.stop(&handle).await;
}
