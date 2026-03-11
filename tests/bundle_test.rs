#![cfg(feature = "test-support")]

use std::net::SocketAddr;
use std::path::PathBuf;
use std::time::Duration;

use blockyard::app::AppState;
use blockyard::backend::mock::MockBackend;
use blockyard::backend::{Backend, ManagedResource, ResourceKind};
use blockyard::config::{Config, DatabaseConfig, ProxyConfig, ServerConfig, StorageConfig};
use blockyard::db;
use blockyard::ops;
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
    let app = blockyard::proxy::full_router(state.clone());
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
            log_retention: Duration::from_secs(3600),
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

fn client() -> reqwest::Client {
    reqwest::Client::new()
}

// --- Bundle tests (from phase 0-3) ---

#[tokio::test]
async fn upload_bundle_returns_202() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let resp = client()
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

    let resp = client()
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

    let resp = client()
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

    let resp = client()
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

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    let body: serde_json::Value = resp.json().await.unwrap();
    let task_id = body["task_id"].as_str().unwrap();

    tokio::time::sleep(Duration::from_millis(100)).await;

    let resp = client()
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
async fn task_status_returns_json() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    let body: serde_json::Value = resp.json().await.unwrap();
    let task_id = body["task_id"].as_str().unwrap();

    tokio::time::sleep(Duration::from_millis(100)).await;

    let resp = client()
        .get(format!("http://{addr}/api/v1/tasks/{task_id}"))
        .header("authorization", "Bearer test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    let task: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(task["id"].as_str().unwrap(), task_id);
    assert!(
        task["status"].as_str().unwrap() == "completed"
            || task["status"].as_str().unwrap() == "running",
        "unexpected status: {}",
        task["status"]
    );
}

#[tokio::test]
async fn task_status_nonexistent_returns_404() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .get(format!("http://{addr}/api/v1/tasks/nonexistent"))
        .header("authorization", "Bearer test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn task_logs_nonexistent_returns_404() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
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

    let resp = client()
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

    client()
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    let resp = client()
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

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::PAYLOAD_TOO_LARGE);
}

#[tokio::test]
async fn bundle_restore_activates_bundle() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    let body: serde_json::Value = resp.json().await.unwrap();
    let bundle_id = body["bundle_id"].as_str().unwrap().to_string();

    tokio::time::sleep(Duration::from_millis(200)).await;

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

// --- App CRUD tests (phase 0-4) ---

#[tokio::test]
async fn create_app_returns_201() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), 201);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["name"], "my-app");
    assert_eq!(body["status"], "stopped");
    assert!(body["id"].is_string());
}

#[tokio::test]
async fn create_app_rejects_invalid_name() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    // Uppercase
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "My-App" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 400);

    // Leading hyphen
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "-app" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 400);

    // Trailing hyphen
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "app-" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 400);
}

#[tokio::test]
async fn create_duplicate_name_returns_409() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), 409);
}

#[tokio::test]
async fn list_apps_returns_all() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    for name in ["app-a", "app-b"] {
        client()
            .post(format!("http://{addr}/api/v1/apps"))
            .bearer_auth("test-token")
            .json(&serde_json::json!({ "name": name }))
            .send()
            .await
            .unwrap();
    }

    let resp = client()
        .get(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), 200);
    let body: Vec<serde_json::Value> = resp.json().await.unwrap();
    assert_eq!(body.len(), 2);
}

#[tokio::test]
async fn get_app_returns_details() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client()
        .get(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["name"], "my-app");
    assert_eq!(body["status"], "stopped");
}

#[tokio::test]
async fn get_nonexistent_app_returns_404() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .get(format!("http://{addr}/api/v1/apps/nonexistent"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 404);
}

#[tokio::test]
async fn update_app_modifies_fields() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client()
        .patch(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "memory_limit": "512m" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["memory_limit"], "512m");
}

#[tokio::test]
async fn delete_app_returns_204() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client()
        .delete(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 204);

    let resp = client()
        .get(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 404);
}

// --- App lifecycle tests (phase 0-4) ---

#[tokio::test]
async fn start_app_without_bundle_returns_400() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 400);
}

/// Helper: create app via API, upload bundle, wait for restore.
async fn create_app_with_bundle(addr: &SocketAddr) -> String {
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap().to_string();

    client()
        .post(format!("http://{addr}/api/v1/apps/{id}/bundles"))
        .bearer_auth("test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    tokio::time::sleep(Duration::from_millis(200)).await;
    id
}

#[tokio::test]
async fn start_and_stop_app() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let id = create_app_with_bundle(&addr).await;

    // Start
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["status"], "running");
    assert!(!body["worker_id"].as_str().unwrap().is_empty());

    assert_eq!(state.workers.len(), 1);

    // Start again — should be no-op
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    assert_eq!(state.workers.len(), 1);

    // Get app — should show "running"
    let resp = client()
        .get(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["status"], "running");

    // Stop
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{id}/stop"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["status"], "stopped");
    assert_eq!(body["workers_stopped"], 1);

    assert_eq!(state.workers.len(), 0);
}

#[tokio::test]
async fn start_at_max_workers_returns_503() {
    let (addr, _state, _tmp) = spawn_test_server_with_config(|cfg| {
        cfg.proxy.max_workers = 0; // no workers allowed
    })
    .await;
    let id = create_app_with_bundle(&addr).await;

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 503);
}

#[tokio::test]
async fn delete_app_stops_workers_first() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let id = create_app_with_bundle(&addr).await;

    // Start
    client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(state.workers.len(), 1);

    // Delete — should stop workers and clean up
    let resp = client()
        .delete(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 204);
    assert_eq!(state.workers.len(), 0);
}

#[tokio::test]
async fn start_nonexistent_app_returns_404() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/nonexistent/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 404);
}

#[tokio::test]
async fn stop_nonexistent_app_returns_404() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = client()
        .post(format!("http://{addr}/api/v1/apps/nonexistent/stop"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 404);
}

// --- Proxy tests (phase 0-5) ---

/// Helper: create app with active bundle directly in DB (bypasses upload/restore).
async fn setup_app_for_proxy(state: &AppState<MockBackend>, name: &str) -> String {
    let app = db::sqlite::create_app(&state.db, name).await.unwrap();
    let bundle_id = format!("bundle-{}", app.id);
    db::sqlite::create_bundle(&state.db, &bundle_id, &app.id, "/tmp/test.tar.gz")
        .await
        .unwrap();
    db::sqlite::update_bundle_status(&state.db, &bundle_id, "ready")
        .await
        .unwrap();
    db::sqlite::set_active_bundle(&state.db, &app.id, &bundle_id)
        .await
        .unwrap();
    app.id
}

fn no_redirect_client() -> reqwest::Client {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap()
}

#[tokio::test]
async fn proxy_returns_404_for_unknown_app() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = no_redirect_client()
        .get(format!("http://{addr}/app/nonexistent/"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 404);
}

#[tokio::test]
async fn proxy_redirects_missing_trailing_slash() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    let resp = no_redirect_client()
        .get(format!("http://{addr}/app/my-app"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 301);
    let location = resp.headers().get("location").unwrap().to_str().unwrap();
    assert!(
        location.ends_with("/app/my-app/"),
        "expected redirect to /app/my-app/, got: {location}"
    );
}

#[tokio::test]
async fn proxy_returns_503_when_no_active_bundle() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    // Create app without uploading a bundle
    client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();

    let resp = no_redirect_client()
        .get(format!("http://{addr}/app/my-app/"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 503);
}

#[tokio::test]
async fn proxy_sets_session_cookie_on_first_request() {
    let (addr, state, _tmp) = spawn_test_server().await;
    setup_app_for_proxy(&state, "my-app").await;

    let resp = no_redirect_client()
        .get(format!("http://{addr}/app/my-app/"))
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), 200);

    let cookie = resp
        .headers()
        .get("set-cookie")
        .expect("should have set-cookie header")
        .to_str()
        .unwrap();
    assert!(cookie.contains("blockyard_session="));
    assert!(cookie.contains("Path=/app/my-app/"));
    assert!(cookie.contains("HttpOnly"));
}

#[tokio::test]
async fn proxy_reuses_session_on_subsequent_requests() {
    let (addr, state, _tmp) = spawn_test_server().await;
    setup_app_for_proxy(&state, "my-app").await;

    let jar_client = reqwest::Client::builder()
        .cookie_store(true)
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap();

    // First request — spawns a worker
    jar_client
        .get(format!("http://{addr}/app/my-app/"))
        .send()
        .await
        .unwrap();
    assert_eq!(state.workers.len(), 1);

    // Second request — reuses the same worker
    jar_client
        .get(format!("http://{addr}/app/my-app/"))
        .send()
        .await
        .unwrap();
    assert_eq!(state.workers.len(), 1); // still 1
}

#[tokio::test]
async fn proxy_strips_prefix_before_forwarding() {
    let (addr, state, _tmp) = spawn_test_server().await;
    setup_app_for_proxy(&state, "my-app").await;

    let jar_client = reqwest::Client::builder()
        .cookie_store(true)
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap();

    let resp = jar_client
        .get(format!("http://{addr}/app/my-app/shared/shiny.js"))
        .send()
        .await
        .unwrap();

    // The mock echo server returns the received path in the body
    let body = resp.text().await.unwrap();
    assert_eq!(body, "/shared/shiny.js");
}

#[tokio::test]
async fn proxy_returns_503_at_max_workers() {
    let (addr, state, _tmp) = spawn_test_server_with_config(|cfg| {
        cfg.proxy.max_workers = 1;
    })
    .await;

    setup_app_for_proxy(&state, "app-a").await;
    setup_app_for_proxy(&state, "app-b").await;

    // First request — fills the one worker slot
    let resp = no_redirect_client()
        .get(format!("http://{addr}/app/app-a/"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);

    // Second request for different app — should 503
    let resp = no_redirect_client()
        .get(format!("http://{addr}/app/app-b/"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 503);
}

#[tokio::test]
async fn proxy_root_path_returns_200() {
    let (addr, state, _tmp) = spawn_test_server().await;
    setup_app_for_proxy(&state, "my-app").await;

    let resp = no_redirect_client()
        .get(format!("http://{addr}/app/my-app/"))
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), 200);
    let body = resp.text().await.unwrap();
    assert_eq!(body, "/");
}

#[tokio::test]
async fn proxy_preserves_query_string() {
    let (addr, state, _tmp) = spawn_test_server().await;
    setup_app_for_proxy(&state, "my-app").await;

    let jar_client = reqwest::Client::builder()
        .cookie_store(true)
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap();

    // The mock echo server returns the path (without query), but
    // we can at least verify the request succeeds.
    let resp = jar_client
        .get(format!("http://{addr}/app/my-app/page?foo=bar"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
}

#[tokio::test]
async fn proxy_websocket_echo() {
    let (addr, state, _tmp) = spawn_test_server().await;
    setup_app_for_proxy(&state, "my-app").await;

    // First, make an HTTP request to establish a session
    let resp = no_redirect_client()
        .get(format!("http://{addr}/app/my-app/"))
        .send()
        .await
        .unwrap();
    let cookie = resp.headers().get("set-cookie").unwrap().to_str().unwrap();
    let session_id = cookie.split('=').nth(1).unwrap().split(';').next().unwrap();

    // Connect WebSocket with the session cookie
    let ws_url = format!("ws://{addr}/app/my-app/websocket/");
    let request = tokio_tungstenite::tungstenite::http::Request::builder()
        .uri(&ws_url)
        .header("Host", addr.to_string())
        .header("Connection", "Upgrade")
        .header("Upgrade", "websocket")
        .header("Sec-WebSocket-Version", "13")
        .header("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
        .header("Cookie", format!("blockyard_session={session_id}"))
        .body(())
        .unwrap();

    let (mut ws, _) = tokio_tungstenite::connect_async(request).await.unwrap();

    // Send a text message and verify echo
    use futures_util::{SinkExt, StreamExt};
    use tokio_tungstenite::tungstenite::protocol::Message;

    ws.send(Message::Text("hello".into())).await.unwrap();

    let msg = tokio::time::timeout(Duration::from_secs(5), ws.next())
        .await
        .expect("timeout waiting for echo")
        .expect("stream ended")
        .expect("ws error");

    assert_eq!(msg, Message::Text("hello".into()));
    ws.close(None).await.ok();
}

// --- Phase 0-6 tests: orphan cleanup, stale bundles, graceful shutdown, health poller, log capture ---

#[tokio::test]
async fn startup_cleanup_removes_orphans() {
    let (_, state, _tmp) = spawn_test_server().await;

    // Configure mock to report orphaned resources
    state.backend.set_managed_resources(vec![
        ManagedResource {
            id: "orphan-container-1".into(),
            kind: ResourceKind::Container,
        },
        ManagedResource {
            id: "orphan-network-1".into(),
            kind: ResourceKind::Network,
        },
    ]);

    // Verify they are present
    let managed = state.backend.list_managed().await.unwrap();
    assert_eq!(managed.len(), 2);

    // Run startup cleanup
    ops::startup_cleanup(&state).await.unwrap();

    // Verify all orphans removed
    let remaining = state.backend.list_managed().await.unwrap();
    assert!(remaining.is_empty());
}

#[tokio::test]
async fn startup_cleanup_fails_stale_bundles() {
    let (_, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    // Create a bundle stuck in "building"
    let bundle = db::sqlite::create_bundle(&state.db, "stale-1", &app.id, "/tmp/test.tar.gz")
        .await
        .unwrap();
    db::sqlite::update_bundle_status(&state.db, &bundle.id, "building")
        .await
        .unwrap();

    // Run startup cleanup
    ops::startup_cleanup(&state).await.unwrap();

    // Verify bundle is now "failed"
    let bundles = db::sqlite::list_bundles_by_app(&state.db, &app.id)
        .await
        .unwrap();
    assert_eq!(bundles[0].status, "failed");
}

#[tokio::test]
async fn graceful_shutdown_stops_all_workers() {
    let (addr, state, _tmp) = spawn_test_server().await;

    // Create two apps with bundles and start them
    let id1 = create_app_with_bundle(&addr).await;
    // Need a second app — create_app_with_bundle uses "my-app" so we do the second inline
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "app-two" }))
        .send()
        .await
        .unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id2 = created["id"].as_str().unwrap().to_string();
    client()
        .post(format!("http://{addr}/api/v1/apps/{id2}/bundles"))
        .bearer_auth("test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Start both
    client()
        .post(format!("http://{addr}/api/v1/apps/{id1}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    client()
        .post(format!("http://{addr}/api/v1/apps/{id2}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(state.workers.len(), 2);

    // Run graceful shutdown
    ops::graceful_shutdown(&state).await;

    assert_eq!(state.workers.len(), 0);
}

#[tokio::test]
async fn graceful_shutdown_fails_in_progress_builds() {
    let (_, state, _tmp) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let bundle = db::sqlite::create_bundle(&state.db, "build-1", &app.id, "/tmp/test.tar.gz")
        .await
        .unwrap();
    db::sqlite::update_bundle_status(&state.db, &bundle.id, "building")
        .await
        .unwrap();

    ops::graceful_shutdown(&state).await;

    let bundles = db::sqlite::list_bundles_by_app(&state.db, &app.id)
        .await
        .unwrap();
    assert_eq!(bundles[0].status, "failed");
}

#[tokio::test]
async fn health_poller_removes_unhealthy_workers() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let id = create_app_with_bundle(&addr).await;

    // Start the app
    client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(state.workers.len(), 1);

    // Set health to fail
    state.backend.set_health_response(false);

    // Spawn health poller with short interval
    let token = tokio_util::sync::CancellationToken::new();
    let mut config = (*state.config).clone();
    config.proxy.health_interval = Duration::from_millis(50);
    let poller_state = AppState {
        config: std::sync::Arc::new(config),
        backend: state.backend.clone(),
        db: state.db.clone(),
        workers: state.workers.clone(),
        task_store: state.task_store.clone(),
        sessions: state.sessions.clone(),
        registry: state.registry.clone(),
        ws_cache: state.ws_cache.clone(),
        log_store: state.log_store.clone(),
    };
    let handle = ops::spawn_health_poller(poller_state, token.clone());

    // Wait for health check to fire
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Worker should be evicted
    assert_eq!(state.workers.len(), 0);

    token.cancel();
    handle.await.unwrap();
}

#[tokio::test]
async fn health_poller_keeps_healthy_workers() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let id = create_app_with_bundle(&addr).await;

    client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(state.workers.len(), 1);

    // Health stays true (default)
    let token = tokio_util::sync::CancellationToken::new();
    let mut config = (*state.config).clone();
    config.proxy.health_interval = Duration::from_millis(50);
    let poller_state = AppState {
        config: std::sync::Arc::new(config),
        backend: state.backend.clone(),
        db: state.db.clone(),
        workers: state.workers.clone(),
        task_store: state.task_store.clone(),
        sessions: state.sessions.clone(),
        registry: state.registry.clone(),
        ws_cache: state.ws_cache.clone(),
        log_store: state.log_store.clone(),
    };
    let handle = ops::spawn_health_poller(poller_state, token.clone());

    // Wait for several poll cycles
    tokio::time::sleep(Duration::from_millis(300)).await;

    // Worker should still be present
    assert_eq!(state.workers.len(), 1);

    token.cancel();
    handle.await.unwrap();
}

#[tokio::test]
async fn log_capture_stores_worker_logs() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let id = create_app_with_bundle(&addr).await;

    // Configure mock to emit log lines
    state.backend.set_log_lines(vec![
        "hello from shiny".into(),
        "listening on port 3838".into(),
    ]);

    // Start app (which triggers log capture)
    client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();

    // Wait for log capture to drain
    tokio::time::sleep(Duration::from_millis(200)).await;

    // GET logs should return captured lines
    let resp = client()
        .get(format!("http://{addr}/api/v1/apps/{id}/logs"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    let body = resp.text().await.unwrap();
    assert!(body.contains("hello from shiny"), "body: {body}");
    assert!(body.contains("listening on port 3838"), "body: {body}");
}

#[tokio::test]
async fn logs_persist_after_worker_stops() {
    let (addr, state, _tmp) = spawn_test_server().await;
    let id = create_app_with_bundle(&addr).await;

    state
        .backend
        .set_log_lines(vec!["startup log".into(), "running...".into()]);

    // Start and wait for log capture
    client()
        .post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Stop the app
    client()
        .post(format!("http://{addr}/api/v1/apps/{id}/stop"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(state.workers.len(), 0);

    // Logs should still be available (previously would have returned 404)
    let resp = client()
        .get(format!("http://{addr}/api/v1/apps/{id}/logs"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    let body = resp.text().await.unwrap();
    assert!(body.contains("startup log"), "body: {body}");
}

#[tokio::test]
async fn logs_unavailable_returns_404() {
    let (addr, _state, _tmp) = spawn_test_server().await;

    // Create app but never start it
    let resp = client()
        .post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "no-logs-app" }))
        .send()
        .await
        .unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client()
        .get(format!("http://{addr}/api/v1/apps/{id}/logs"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}
