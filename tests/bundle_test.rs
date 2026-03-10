#![cfg(feature = "test-support")]

use std::net::SocketAddr;
use std::path::PathBuf;
use std::time::Duration;

use blockyard::app::AppState;
use blockyard::backend::mock::MockBackend;
use blockyard::config::{Config, DatabaseConfig, ProxyConfig, ServerConfig, StorageConfig};
use blockyard::db;
use reqwest::StatusCode;

async fn spawn_test_server() -> (SocketAddr, AppState<MockBackend>, tempfile::TempDir) {
    spawn_test_server_with_config(|_| {}).await
}

async fn spawn_test_server_with_config(
    customize: impl FnOnce(&mut Config),
) -> (SocketAddr, AppState<MockBackend>, tempfile::TempDir) {
    let tmp = tempfile::TempDir::new().unwrap();
    let mut config = test_config(tmp.path().to_path_buf());
    customize(&mut config);
    let backend = MockBackend::new();
    let pool = sqlx::SqlitePool::connect(":memory:").await.unwrap();
    db::run_migrations(&pool).await.unwrap();
    let state = AppState::new(config, backend, pool);
    let app = blockyard::api::api_router(state.clone()).with_state(state.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(axum::serve(listener, app).into_future());
    (addr, state, tmp)
}

fn test_config(bundle_path: PathBuf) -> Config {
    Config {
        server: ServerConfig {
            bind: "127.0.0.1:0".parse().unwrap(),
            token: "test-token".into(),
            shutdown_timeout: Duration::from_secs(5),
        },
        docker: None,
        storage: StorageConfig {
            bundle_server_path: bundle_path,
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
        },
    }
}

fn make_test_bundle() -> Vec<u8> {
    use std::io::Write;

    let app_r = b"library(shiny)";
    let mut tar_buf = Vec::new();
    {
        let mut archive = tar::Builder::new(&mut tar_buf);
        let mut header = tar::Header::new_gnu();
        header.set_size(app_r.len() as u64);
        header.set_mode(0o644);
        header.set_cksum();
        archive
            .append_data(&mut header, "app.R", &app_r[..])
            .unwrap();
        archive.finish().unwrap();
    }

    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    encoder.write_all(&tar_buf).unwrap();
    encoder.finish().unwrap()
}

#[tokio::test]
async fn upload_bundle_returns_202() {
    let (addr, state, _tmp) = spawn_test_server().await;

    // Create an app first
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::ACCEPTED);

    let body: serde_json::Value = resp.json().await.unwrap();
    assert!(body["bundle_id"].is_string());
    assert!(body["task_id"].is_string());
}

#[tokio::test]
async fn upload_without_auth_returns_401() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn upload_to_nonexistent_app_returns_404() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/nonexistent/bundles"))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn healthz_returns_200_without_auth() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let client = reqwest::Client::new();
    let resp = client
        .get(format!("http://{addr}/healthz"))
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.text().await.unwrap(), "ok");
}

#[tokio::test]
async fn task_logs_streams_output() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    let body: serde_json::Value = resp.json().await.unwrap();
    let task_id = body["task_id"].as_str().unwrap();

    // Give the background task a moment to run
    tokio::time::sleep(Duration::from_millis(100)).await;

    // Fetch task logs
    let resp = client
        .get(format!("http://{addr}/api/v1/tasks/{task_id}/logs"))
        .header("authorization", "Bearer test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    let logs = resp.text().await.unwrap();
    assert!(
        logs.contains("Starting dependency restoration"),
        "logs should contain restore message, got: {logs}"
    );
}

#[tokio::test]
async fn task_logs_nonexistent_returns_404() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let client = reqwest::Client::new();
    let resp = client
        .get(format!("http://{addr}/api/v1/tasks/nonexistent/logs"))
        .header("authorization", "Bearer test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn upload_empty_body_returns_400() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(Vec::<u8>::new())
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn list_bundles_returns_uploaded() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();

    // Upload a bundle
    client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    // List bundles
    let resp = client
        .get(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    let bundles: Vec<serde_json::Value> = resp.json().await.unwrap();
    assert_eq!(bundles.len(), 1);
}

#[tokio::test]
async fn upload_oversized_bundle_returns_413() {
    let (addr, state, _tmp) = spawn_test_server_with_config(|cfg| {
        cfg.storage.max_bundle_size = 64; // 64 bytes
    })
    .await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle()) // larger than 64 bytes
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::PAYLOAD_TOO_LARGE);
}

#[tokio::test]
async fn bundle_restore_activates_bundle() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    let body: serde_json::Value = resp.json().await.unwrap();
    let bundle_id = body["bundle_id"].as_str().unwrap().to_string();

    // Wait for the async restore to complete
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Check that the bundle is now "ready" and is the active bundle
    let app_row = db::sqlite::get_app(&state.db, &app.id)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(app_row.active_bundle.as_deref(), Some(bundle_id.as_str()));

    let bundles = db::sqlite::list_bundles_by_app(&state.db, &app.id)
        .await
        .unwrap();
    assert_eq!(bundles[0].status, "ready");
}
