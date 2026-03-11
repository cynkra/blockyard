#![cfg(feature = "keycloak-integration-tests")]

//! Integration tests against a real Keycloak instance.
//!
//! Prerequisites:
//!   docker compose -f tests/keycloak-docker-compose.yml up -d
//!   # wait for healthy, then:
//!   cargo test --features keycloak-integration-tests --test keycloak_test
//!
//! Override the Keycloak URL with KEYCLOAK_URL (default: http://localhost:9090).

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use blockyard::app::AppState;
use blockyard::auth::oidc::OidcClient;
use blockyard::auth::session::{SigningKey, UserSessionStore};
use blockyard::backend::mock::MockBackend;
use blockyard::config::{
    Config, DatabaseConfig, OidcConfig, ProxyConfig, ServerConfig, StorageConfig,
};
use blockyard::db;
use reqwest::StatusCode;
use serde::Deserialize;

// ============================================================================
// Keycloak admin helpers
// ============================================================================

const REALM: &str = "blockyard-test";
const CLIENT_ID: &str = "blockyard";
const CLIENT_SECRET: &str = "blockyard-test-secret";
const TEST_USER: &str = "testuser";
const TEST_PASSWORD: &str = "testpassword";
const TEST_GROUP: &str = "editors";

fn keycloak_url() -> String {
    std::env::var("KEYCLOAK_URL").unwrap_or_else(|_| "http://localhost:9090".to_string())
}

/// Get an admin access token from the master realm.
async fn admin_token(client: &reqwest::Client) -> String {
    let url = format!("{}/realms/master/protocol/openid-connect/token", keycloak_url());
    let resp = client
        .post(&url)
        .form(&[
            ("grant_type", "password"),
            ("client_id", "admin-cli"),
            ("username", "admin"),
            ("password", "admin"),
        ])
        .send()
        .await
        .expect("failed to get admin token");

    assert!(
        resp.status().is_success(),
        "admin token request failed: {}",
        resp.status()
    );

    #[derive(Deserialize)]
    struct TokenResp {
        access_token: String,
    }
    resp.json::<TokenResp>().await.unwrap().access_token
}

/// Provision a test realm, client, group, and user in Keycloak.
/// Idempotent — deletes the realm first if it exists.
async fn provision_keycloak(client: &reqwest::Client) {
    let base = keycloak_url();
    let token = admin_token(client).await;

    // Delete realm if it exists (ignore 404)
    let _ = client
        .delete(format!("{base}/admin/realms/{REALM}"))
        .bearer_auth(&token)
        .send()
        .await;

    // Create realm
    let resp = client
        .post(format!("{base}/admin/realms"))
        .bearer_auth(&token)
        .json(&serde_json::json!({
            "realm": REALM,
            "enabled": true,
        }))
        .send()
        .await
        .unwrap();
    assert!(
        resp.status().is_success(),
        "create realm failed: {} — {}",
        resp.status(),
        resp.text().await.unwrap_or_default()
    );

    // Create client with client_credentials grant (confidential)
    let resp = client
        .post(format!("{base}/admin/realms/{REALM}/clients"))
        .bearer_auth(&token)
        .json(&serde_json::json!({
            "clientId": CLIENT_ID,
            "enabled": true,
            "protocol": "openid-connect",
            "publicClient": false,
            "secret": CLIENT_SECRET,
            "redirectUris": ["*"],
            "standardFlowEnabled": true,
            "directAccessGrantsEnabled": true,
            "attributes": {
                // Include groups in ID token via protocol mapper
            }
        }))
        .send()
        .await
        .unwrap();
    assert!(
        resp.status().is_success(),
        "create client failed: {} — {}",
        resp.status(),
        resp.text().await.unwrap_or_default()
    );

    // Get the internal client UUID (needed for adding protocol mappers)
    #[derive(Deserialize)]
    struct ClientRepr {
        id: String,
    }
    let clients: Vec<ClientRepr> = client
        .get(format!(
            "{base}/admin/realms/{REALM}/clients?clientId={CLIENT_ID}"
        ))
        .bearer_auth(&token)
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let client_uuid = &clients[0].id;

    // Add a "groups" protocol mapper to include group memberships in the ID token
    let resp = client
        .post(format!(
            "{base}/admin/realms/{REALM}/clients/{client_uuid}/protocol-mappers/models"
        ))
        .bearer_auth(&token)
        .json(&serde_json::json!({
            "name": "groups",
            "protocol": "openid-connect",
            "protocolMapper": "oidc-group-membership-mapper",
            "config": {
                "full.path": "false",
                "id.token.claim": "true",
                "access.token.claim": "true",
                "claim.name": "groups",
                "userinfo.token.claim": "true"
            }
        }))
        .send()
        .await
        .unwrap();
    assert!(
        resp.status().is_success(),
        "create groups mapper failed: {} — {}",
        resp.status(),
        resp.text().await.unwrap_or_default()
    );

    // Create group
    let resp = client
        .post(format!("{base}/admin/realms/{REALM}/groups"))
        .bearer_auth(&token)
        .json(&serde_json::json!({
            "name": TEST_GROUP,
        }))
        .send()
        .await
        .unwrap();
    assert!(
        resp.status().is_success(),
        "create group failed: {} — {}",
        resp.status(),
        resp.text().await.unwrap_or_default()
    );

    // Get group ID
    #[derive(Deserialize)]
    struct GroupRepr {
        id: String,
    }
    let groups: Vec<GroupRepr> = client
        .get(format!("{base}/admin/realms/{REALM}/groups?search={TEST_GROUP}"))
        .bearer_auth(&token)
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let group_id = &groups[0].id;

    // Create user
    let resp = client
        .post(format!("{base}/admin/realms/{REALM}/users"))
        .bearer_auth(&token)
        .json(&serde_json::json!({
            "username": TEST_USER,
            "enabled": true,
            "emailVerified": true,
            "credentials": [{
                "type": "password",
                "value": TEST_PASSWORD,
                "temporary": false,
            }],
        }))
        .send()
        .await
        .unwrap();
    assert!(
        resp.status().is_success(),
        "create user failed: {} — {}",
        resp.status(),
        resp.text().await.unwrap_or_default()
    );

    // Get user ID
    #[derive(Deserialize)]
    struct UserRepr {
        id: String,
    }
    let users: Vec<UserRepr> = client
        .get(format!(
            "{base}/admin/realms/{REALM}/users?username={TEST_USER}&exact=true"
        ))
        .bearer_auth(&token)
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let user_id = &users[0].id;

    // Add user to group
    let resp = client
        .put(format!(
            "{base}/admin/realms/{REALM}/users/{user_id}/groups/{group_id}"
        ))
        .bearer_auth(&token)
        .send()
        .await
        .unwrap();
    assert!(
        resp.status().is_success(),
        "add user to group failed: {} — {}",
        resp.status(),
        resp.text().await.unwrap_or_default()
    );
}

// ============================================================================
// Test server helpers
// ============================================================================

async fn spawn_blockyard_with_keycloak() -> (SocketAddr, AppState<MockBackend>, tempfile::TempDir) {
    let tmp = tempfile::TempDir::new().unwrap();

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();

    let issuer_url = format!("{}/realms/{REALM}", keycloak_url());
    let redirect_url = format!("http://{addr}/callback");

    let config = Config {
        server: ServerConfig {
            bind: "127.0.0.1:0".parse().unwrap(),
            token: "test-token".into(),
            shutdown_timeout: Duration::from_secs(5),
            session_secret: Some("keycloak-test-secret-at-least-32-chars!!".into()),
            external_url: Some(format!("http://{addr}")),
        },
        docker: None,
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
        oidc: Some(OidcConfig {
            issuer_url: issuer_url.clone(),
            client_id: CLIENT_ID.to_string(),
            client_secret: CLIENT_SECRET.into(),
            groups_claim: "groups".to_string(),
            cookie_max_age: Duration::from_secs(86400),
        }),
    };

    let oidc_client = OidcClient::discover(
        &issuer_url,
        CLIENT_ID,
        CLIENT_SECRET,
        &redirect_url,
        "groups",
    )
    .await
    .expect("OIDC discovery against Keycloak failed");

    let signing_key = SigningKey::derive(config.server.session_secret.as_ref().unwrap().expose());

    let backend = MockBackend::new();
    let pool = sqlx::SqlitePool::connect(":memory:").await.unwrap();
    db::run_migrations(&pool).await.unwrap();

    let mut state = AppState::new(config, backend, pool);
    state.oidc_client = Some(Arc::new(oidc_client));
    state.signing_key = Some(Arc::new(signing_key));
    state.user_sessions = Some(Arc::new(UserSessionStore::new()));

    let app = blockyard::proxy::full_router(state.clone());
    tokio::spawn(axum::serve(listener, app).into_future());

    (addr, state, tmp)
}

fn no_redirect_client() -> reqwest::Client {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap()
}

/// Extract the login form action URL from Keycloak's HTML login page.
fn extract_form_action(html: &str) -> String {
    // Keycloak renders: <form id="kc-form-login" ... action="https://...">
    let marker = "id=\"kc-form-login\"";
    let form_pos = html
        .find(marker)
        .unwrap_or_else(|| panic!("could not find kc-form-login in HTML:\n{html}"));
    let after_marker = &html[form_pos..];
    let action_pos = after_marker
        .find("action=\"")
        .expect("could not find action attribute");
    let action_start = action_pos + "action=\"".len();
    let action_end = after_marker[action_start..]
        .find('"')
        .expect("could not find closing quote for action");
    let action = &after_marker[action_start..action_start + action_end];
    // Keycloak HTML-encodes `&` as `&amp;` in the action URL
    action.replace("&amp;", "&")
}

// ============================================================================
// Tests
// ============================================================================

#[tokio::test]
async fn full_auth_flow_with_keycloak() {
    let http = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .cookie_store(true)
        .build()
        .unwrap();

    // Provision Keycloak with our test realm/client/user
    provision_keycloak(&http).await;

    // Start blockyard pointing at Keycloak
    let (addr, state, _tmp) = spawn_blockyard_with_keycloak().await;

    // 1. GET /login → 303 redirect to Keycloak authorize endpoint
    let resp = http
        .get(format!("http://{addr}/login?return_url=/app/myapp/"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SEE_OTHER, "login should redirect");

    let keycloak_authorize_url = resp
        .headers()
        .get("location")
        .unwrap()
        .to_str()
        .unwrap()
        .to_string();
    assert!(
        keycloak_authorize_url.contains("/realms/blockyard-test/protocol/openid-connect/auth"),
        "should redirect to Keycloak authorize: {keycloak_authorize_url}"
    );

    // 2. Follow redirect to Keycloak → get login form
    let resp = http.get(&keycloak_authorize_url).send().await.unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::OK,
        "Keycloak should serve login form"
    );
    let login_html = resp.text().await.unwrap();
    let form_action = extract_form_action(&login_html);

    // 3. Submit credentials to Keycloak's login form
    let resp = http
        .post(&form_action)
        .form(&[("username", TEST_USER), ("password", TEST_PASSWORD)])
        .send()
        .await
        .unwrap();

    // Keycloak redirects back to our /callback with code + state
    assert_eq!(
        resp.status(),
        StatusCode::FOUND,
        "Keycloak should redirect after login, got: {}",
        resp.status()
    );
    let callback_redirect = resp
        .headers()
        .get("location")
        .unwrap()
        .to_str()
        .unwrap()
        .to_string();
    assert!(
        callback_redirect.contains("/callback"),
        "should redirect to callback: {callback_redirect}"
    );
    assert!(
        callback_redirect.contains("code="),
        "should include authorization code: {callback_redirect}"
    );

    // 4. Follow redirect to /callback → exchanges code, creates session
    let resp = http.get(&callback_redirect).send().await.unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::SEE_OTHER,
        "callback should redirect to return_url, got: {}",
        resp.status()
    );
    let final_redirect = resp
        .headers()
        .get("location")
        .unwrap()
        .to_str()
        .unwrap()
        .to_string();
    assert_eq!(
        final_redirect, "/app/myapp/",
        "should redirect to original return_url"
    );

    // 5. Verify server-side session was created
    let sessions = state.user_sessions.as_ref().unwrap();
    let session = sessions
        .get(TEST_USER)
        .expect("session should exist for test user");
    assert!(
        session.groups.contains(&TEST_GROUP.to_string()),
        "session should contain group '{}', got: {:?}",
        TEST_GROUP,
        session.groups
    );

    // 6. Verify the session cookie works for authenticated requests
    let resp = http
        .get(format!("http://{addr}/app/myapp/"))
        .send()
        .await
        .unwrap();
    // Should NOT redirect to login (will get 404/502 since app doesn't exist, but not 303)
    assert_ne!(
        resp.status(),
        StatusCode::SEE_OTHER,
        "authenticated request should not redirect to login"
    );
}

#[tokio::test]
async fn logout_flow_with_keycloak() {
    let http = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .cookie_store(true)
        .build()
        .unwrap();

    provision_keycloak(&http).await;
    let (addr, state, _tmp) = spawn_blockyard_with_keycloak().await;

    // Log in first (same as above, abbreviated)
    let resp = http
        .get(format!("http://{addr}/login"))
        .send()
        .await
        .unwrap();
    let kc_url = resp.headers().get("location").unwrap().to_str().unwrap().to_string();
    let resp = http.get(&kc_url).send().await.unwrap();
    let html = resp.text().await.unwrap();
    let action = extract_form_action(&html);
    let resp = http
        .post(&action)
        .form(&[("username", TEST_USER), ("password", TEST_PASSWORD)])
        .send()
        .await
        .unwrap();
    let cb_url = resp.headers().get("location").unwrap().to_str().unwrap().to_string();
    let resp = http.get(&cb_url).send().await.unwrap();
    assert_eq!(resp.status(), StatusCode::SEE_OTHER);

    // Verify session exists
    assert!(
        state.user_sessions.as_ref().unwrap().get(TEST_USER).is_some(),
        "session should exist after login"
    );

    // POST /logout
    let resp = http
        .post(format!("http://{addr}/logout"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SEE_OTHER);
    assert_eq!(
        resp.headers().get("location").unwrap().to_str().unwrap(),
        "/"
    );

    // Verify session is removed
    assert!(
        state.user_sessions.as_ref().unwrap().get(TEST_USER).is_none(),
        "session should be removed after logout"
    );

    // Verify subsequent requests are unauthenticated
    let resp = http
        .get(format!("http://{addr}/app/myapp/"))
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::SEE_OTHER,
        "should redirect to login after logout"
    );
}

#[tokio::test]
async fn unauthenticated_app_redirects_to_login() {
    let http = no_redirect_client();

    provision_keycloak(&http).await;
    let (addr, _state, _tmp) = spawn_blockyard_with_keycloak().await;

    let resp = http
        .get(format!("http://{addr}/app/myapp/"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SEE_OTHER);

    let location = resp.headers().get("location").unwrap().to_str().unwrap();
    assert!(
        location.starts_with("/login"),
        "should redirect to /login, got: {location}"
    );
}

#[tokio::test]
async fn healthz_bypasses_keycloak_auth() {
    let http = no_redirect_client();

    provision_keycloak(&http).await;
    let (addr, _state, _tmp) = spawn_blockyard_with_keycloak().await;

    let resp = http
        .get(format!("http://{addr}/healthz"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
}

#[tokio::test]
async fn api_uses_bearer_token_not_keycloak() {
    let http = no_redirect_client();

    provision_keycloak(&http).await;
    let (addr, _state, _tmp) = spawn_blockyard_with_keycloak().await;

    let resp = http
        .get(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
}
