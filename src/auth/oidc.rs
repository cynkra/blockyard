use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use axum::extract::{Query, Request, State};
use axum::http::{self, HeaderMap, StatusCode};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use openidconnect::core::{
    CoreAuthDisplay, CoreAuthPrompt, CoreErrorResponseType, CoreGenderClaim, CoreJsonWebKey,
    CoreJweContentEncryptionAlgorithm, CoreProviderMetadata, CoreRevocableToken,
    CoreRevocationErrorResponse, CoreTokenIntrospectionResponse, CoreTokenType,
};
use openidconnect::{
    AdditionalClaims, AuthorizationCode, ClientId, ClientSecret, CsrfToken, EmptyExtraTokenFields,
    EndpointMaybeSet, EndpointNotSet, EndpointSet, IdTokenFields, IssuerUrl, Nonce,
    OAuth2TokenResponse as _, RedirectUrl, RefreshToken, Scope, StandardErrorResponse,
    StandardTokenResponse, TokenResponse as OidcTokenResponse,
};
use serde::{Deserialize, Serialize};

use crate::app::AppState;
use crate::auth::AuthenticatedUser;
use crate::auth::session::{CookiePayload, SessionError, SigningKey, UserSession};
use crate::backend::Backend;
use crate::config::Config;

// --- Custom claims and client type ---

/// Custom additional claims type that captures arbitrary extra fields
/// from the ID token (e.g. "groups", "roles", "cognito:groups").
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GroupsClaims {
    #[serde(flatten)]
    pub extra: HashMap<String, serde_json::Value>,
}

impl AdditionalClaims for GroupsClaims {}

/// ID token fields with our custom claims.
type BlockyardIdTokenFields = IdTokenFields<
    GroupsClaims,
    EmptyExtraTokenFields,
    CoreGenderClaim,
    CoreJweContentEncryptionAlgorithm,
    CoreJwsSigningAlgorithm,
>;

/// Token response with our custom claims.
type BlockyardTokenResponse = StandardTokenResponse<BlockyardIdTokenFields, CoreTokenType>;

/// Client type alias using our custom claims.
/// Endpoint states match the return of `from_provider_metadata`:
///   HasAuthUrl = EndpointSet, HasTokenUrl = EndpointMaybeSet,
///   HasUserInfoUrl = EndpointMaybeSet, rest = EndpointNotSet.
pub type BlockyardClient = openidconnect::Client<
    GroupsClaims,
    CoreAuthDisplay,
    CoreGenderClaim,
    CoreJweContentEncryptionAlgorithm,
    CoreJsonWebKey,
    CoreAuthPrompt,
    StandardErrorResponse<CoreErrorResponseType>,
    BlockyardTokenResponse,
    CoreTokenIntrospectionResponse,
    CoreRevocableToken,
    CoreRevocationErrorResponse,
    EndpointSet,
    EndpointNotSet,
    EndpointNotSet,
    EndpointNotSet,
    EndpointMaybeSet,
    EndpointMaybeSet,
>;

use openidconnect::core::CoreJwsSigningAlgorithm;

/// OIDC client initialized from provider discovery.
pub struct OidcClient {
    pub client: BlockyardClient,
    pub provider_metadata: CoreProviderMetadata,
    pub groups_claim: String,
}

impl OidcClient {
    /// Discover the OIDC provider and build the client.
    /// Called once at server startup.
    pub async fn discover(
        issuer_url: &str,
        client_id: &str,
        client_secret: &str,
        redirect_url: &str,
        groups_claim: &str,
    ) -> Result<Self, OidcError> {
        let issuer = IssuerUrl::new(issuer_url.to_string())?;
        let http_client = openidconnect::reqwest::Client::builder()
            .timeout(Duration::from_secs(10))
            .build()
            .map_err(|e| OidcError::HttpClient(e.to_string()))?;

        let metadata = CoreProviderMetadata::discover_async(issuer, &http_client).await?;

        let client = BlockyardClient::from_provider_metadata(
            metadata.clone(),
            ClientId::new(client_id.to_string()),
            Some(ClientSecret::new(client_secret.to_string())),
        )
        .set_redirect_uri(RedirectUrl::new(redirect_url.to_string())?);

        Ok(Self {
            client,
            provider_metadata: metadata,
            groups_claim: groups_claim.to_string(),
        })
    }
}

#[derive(Debug, thiserror::Error)]
pub enum OidcError {
    #[error("OIDC discovery failed: {0}")]
    Discovery(
        #[from]
        openidconnect::DiscoveryError<
            openidconnect::HttpClientError<openidconnect::reqwest::Error>,
        >,
    ),
    #[error("invalid URL: {0}")]
    Url(#[from] openidconnect::url::ParseError),
    #[error("HTTP client error: {0}")]
    HttpClient(String),
}

// --- Login / Callback / Logout handlers ---

#[derive(Debug, Deserialize)]
pub struct LoginParams {
    pub return_url: Option<String>,
}

#[derive(Debug, Serialize, Deserialize)]
struct OidcStatePayload {
    csrf_token: String,
    nonce: String,
    return_url: String,
}

#[derive(Debug, Deserialize)]
pub struct CallbackParams {
    pub code: String,
    pub state: String,
}

/// Unix timestamp (seconds since epoch).
fn now_unix() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64
}

/// Returns "; Secure" if external_url is HTTPS, empty string otherwise.
fn secure_flag(config: &Config) -> &'static str {
    let is_https = config
        .server
        .external_url
        .as_deref()
        .is_some_and(|u| u.starts_with("https://"));
    if is_https { "; Secure" } else { "" }
}

/// Build a signed, short-lived state cookie for the OIDC flow.
fn build_state_cookie(
    payload: &OidcStatePayload,
    key: &SigningKey,
    config: &Config,
) -> Result<String, SessionError> {
    let json = serde_json::to_string(payload)?;
    let encoded = base64::Engine::encode(
        &base64::engine::general_purpose::URL_SAFE_NO_PAD,
        json.as_bytes(),
    );
    let signature = key.sign(encoded.as_bytes());
    let secure = secure_flag(config);
    Ok(format!(
        "blockyard_oidc_state={encoded}.{signature}; Path=/; HttpOnly; SameSite=Lax{secure}; Max-Age=300"
    ))
}

/// Extract and verify the OIDC state cookie.
fn extract_state_cookie(
    headers: &HeaderMap,
    key: &SigningKey,
) -> Result<OidcStatePayload, SessionError> {
    let cookie_value = extract_named_cookie(headers, "blockyard_oidc_state")
        .ok_or(SessionError::MalformedCookie)?;

    let (payload_b64, sig_b64) = cookie_value
        .split_once('.')
        .ok_or(SessionError::MalformedCookie)?;

    key.verify(payload_b64.as_bytes(), sig_b64)?;

    let payload_bytes = base64::Engine::decode(
        &base64::engine::general_purpose::URL_SAFE_NO_PAD,
        payload_b64,
    )?;
    let payload: OidcStatePayload = serde_json::from_slice(&payload_bytes)?;
    Ok(payload)
}

/// Extract the blockyard_session cookie value from headers.
fn extract_session_cookie(headers: &HeaderMap) -> Option<&str> {
    extract_named_cookie(headers, "blockyard_session")
}

/// Generic cookie extraction by name.
fn extract_named_cookie<'a>(headers: &'a HeaderMap, name: &str) -> Option<&'a str> {
    headers
        .get_all(http::header::COOKIE)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .flat_map(|v| v.split(';'))
        .map(str::trim)
        .find_map(|c| c.strip_prefix(&format!("{name}=")))
}

/// Extract groups from ID token claims using the configured claim name.
fn extract_groups(
    claims: &openidconnect::IdTokenClaims<GroupsClaims, CoreGenderClaim>,
    groups_claim: &str,
) -> Vec<String> {
    match claims.additional_claims().extra.get(groups_claim) {
        Some(serde_json::Value::Array(arr)) => arr
            .iter()
            .filter_map(|v| v.as_str().map(String::from))
            .collect(),
        Some(other) => {
            tracing::warn!(
                groups_claim,
                ?other,
                "groups claim is not an array — ignoring"
            );
            Vec::new()
        }
        None => {
            tracing::debug!(groups_claim, "groups claim not present in ID token");
            Vec::new()
        }
    }
}

fn redirect_to_login(req: &Request) -> Response {
    let return_url = req
        .uri()
        .path_and_query()
        .map(|pq| pq.as_str())
        .unwrap_or("/");
    let encoded = urlencoding::encode(return_url);
    axum::response::Redirect::to(&format!("/login?return_url={encoded}")).into_response()
}

// --- Handlers ---

/// Initiates the OIDC authorization code flow.
/// Query params: ?return_url=/app/my-app/ (optional, default: /)
pub async fn login_handler<B: Backend>(
    State(state): State<AppState<B>>,
    Query(params): Query<LoginParams>,
) -> Result<Response, StatusCode> {
    let oidc = state.oidc_client.as_ref().ok_or(StatusCode::NOT_FOUND)?;
    let key = state
        .signing_key
        .as_ref()
        .ok_or(StatusCode::INTERNAL_SERVER_ERROR)?;

    // Generate CSRF token and nonce, build authorization URL
    let (auth_url, csrf_token, nonce) = oidc
        .client
        .authorize_url(
            openidconnect::AuthenticationFlow::<
                openidconnect::core::CoreResponseType,
            >::AuthorizationCode,
            CsrfToken::new_random,
            Nonce::new_random,
        )
        .add_scope(Scope::new("openid".to_string()))
        .add_scope(Scope::new("profile".to_string()))
        .url();

    // Validate return_url to prevent open redirect attacks.
    let return_url = params
        .return_url
        .filter(|u| u.starts_with('/') && !u.starts_with("//"))
        .unwrap_or_else(|| "/".to_string());

    // Store CSRF token + nonce + return_url in a short-lived state cookie
    let state_payload = OidcStatePayload {
        csrf_token: csrf_token.secret().to_string(),
        nonce: nonce.secret().to_string(),
        return_url,
    };
    let state_cookie = build_state_cookie(&state_payload, key, &state.config).map_err(|e| {
        tracing::error!("failed to build state cookie: {e}");
        StatusCode::INTERNAL_SERVER_ERROR
    })?;

    Ok((
        [(http::header::SET_COOKIE, state_cookie)],
        axum::response::Redirect::to(auth_url.as_str()),
    )
        .into_response())
}

/// Handles the IdP callback after user authentication.
pub async fn callback_handler<B: Backend>(
    State(state): State<AppState<B>>,
    Query(params): Query<CallbackParams>,
    headers: HeaderMap,
) -> Result<Response, Response> {
    let oidc = state
        .oidc_client
        .as_ref()
        .ok_or_else(|| StatusCode::NOT_FOUND.into_response())?;
    let key = state
        .signing_key
        .as_ref()
        .ok_or_else(|| StatusCode::INTERNAL_SERVER_ERROR.into_response())?;
    let sessions = state
        .user_sessions
        .as_ref()
        .ok_or_else(|| StatusCode::INTERNAL_SERVER_ERROR.into_response())?;

    // 1. Extract and validate OIDC state cookie
    let state_payload = extract_state_cookie(&headers, key).map_err(|e| {
        tracing::warn!("invalid OIDC state cookie: {e}");
        StatusCode::BAD_REQUEST.into_response()
    })?;

    // 2. Verify CSRF token matches
    if params.state != state_payload.csrf_token {
        return Err(StatusCode::BAD_REQUEST.into_response());
    }

    // 3. Exchange authorization code for tokens
    let http_client = openidconnect::reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| {
            tracing::error!("failed to build HTTP client: {e}");
            StatusCode::INTERNAL_SERVER_ERROR.into_response()
        })?;

    let token_request = oidc
        .client
        .exchange_code(AuthorizationCode::new(params.code.clone()))
        .map_err(|e| {
            tracing::error!("token endpoint not configured: {e}");
            StatusCode::BAD_GATEWAY.into_response()
        })?;

    let token_response: BlockyardTokenResponse = token_request
        .request_async(&http_client)
        .await
        .map_err(|e| {
            tracing::error!("token exchange failed: {e}");
            StatusCode::BAD_GATEWAY.into_response()
        })?;

    // 4. Validate ID token signature and extract claims
    let id_token = token_response
        .id_token()
        .ok_or_else(|| StatusCode::BAD_GATEWAY.into_response())?;

    let nonce = Nonce::new(state_payload.nonce);
    let claims = id_token
        .claims(&oidc.client.id_token_verifier(), &nonce)
        .map_err(|e| {
            tracing::error!("ID token verification failed: {e}");
            StatusCode::BAD_GATEWAY.into_response()
        })?;

    // 5. Extract sub and groups
    let sub = claims.subject().to_string();
    let groups = extract_groups(claims, &oidc.groups_claim);

    // 6. Store session server-side
    let access_token = token_response.access_token().secret().clone();
    let refresh_token = token_response
        .refresh_token()
        .map(|t| t.secret().clone())
        .unwrap_or_default();
    let expires_at = token_response
        .expires_in()
        .map(|d| now_unix() + d.as_secs() as i64)
        .unwrap_or_else(|| now_unix() + 300);

    sessions.insert(
        sub.clone(),
        UserSession {
            groups,
            access_token,
            refresh_token,
            expires_at,
        },
    );

    // 7. Build signed session cookie
    let cookie_payload = CookiePayload {
        sub,
        issued_at: now_unix(),
    };
    let cookie_value = cookie_payload
        .encode(key)
        .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())?;

    let cookie_max_age = state
        .config
        .oidc
        .as_ref()
        .map(|c| c.cookie_max_age)
        .unwrap_or(Duration::from_secs(86400));

    let secure = secure_flag(&state.config);
    let session_cookie = format!(
        "blockyard_session={cookie_value}; Path=/; HttpOnly; SameSite=Lax{secure}; Max-Age={}",
        cookie_max_age.as_secs()
    );

    // 8. Clear the OIDC state cookie
    let clear_state = format!("blockyard_oidc_state=; Path=/; HttpOnly{secure}; Max-Age=0");

    // 9. Redirect to return_url
    Ok((
        [
            (http::header::SET_COOKIE, session_cookie),
            (http::header::SET_COOKIE, clear_state),
        ],
        axum::response::Redirect::to(&state_payload.return_url),
    )
        .into_response())
}

/// Clear the session cookie and remove the server-side session.
pub async fn logout_handler<B: Backend>(
    State(state): State<AppState<B>>,
    headers: HeaderMap,
) -> Response {
    // Remove server-side session if cookie is present
    if let (Some(key), Some(sessions)) = (&state.signing_key, &state.user_sessions)
        && let Some(cookie_value) = extract_session_cookie(&headers)
        && let Ok(payload) = CookiePayload::decode(cookie_value, key)
    {
        sessions.remove(&payload.sub);
    }

    let secure = secure_flag(&state.config);
    let clear_cookie = format!("blockyard_session=; Path=/; HttpOnly{secure}; Max-Age=0");

    (
        [(http::header::SET_COOKIE, clear_cookie)],
        axum::response::Redirect::to("/"),
    )
        .into_response()
}

// --- Auth middleware ---

/// Middleware for app-plane routes. Redirects unauthenticated users
/// to /login with the current URL as return_url.
pub async fn app_auth_middleware<B: Backend>(
    State(state): State<AppState<B>>,
    mut req: Request,
    next: Next,
) -> Result<Response, Response> {
    // If OIDC is not configured, pass through (v0 compat)
    let Some(key) = &state.signing_key else {
        return Ok(next.run(req).await);
    };
    let sessions = state.user_sessions.as_ref().unwrap();

    // Extract session cookie
    let cookie_value =
        extract_session_cookie(req.headers()).ok_or_else(|| redirect_to_login(&req))?;

    // Decode and verify signature
    let cookie = CookiePayload::decode(cookie_value, key).map_err(|_| redirect_to_login(&req))?;

    // Check cookie max-age
    let max_age = state
        .config
        .oidc
        .as_ref()
        .map(|c| c.cookie_max_age.as_secs() as i64)
        .unwrap_or(86400);
    if now_unix() - cookie.issued_at > max_age {
        return Err(redirect_to_login(&req));
    }

    // Look up server-side session
    let session = sessions
        .get(&cookie.sub)
        .ok_or_else(|| redirect_to_login(&req))?;

    // Refresh access token if near expiry (within 60 seconds)
    let needs_refresh = session.expires_at - now_unix() < 60;
    drop(session); // release DashMap read lock before async refresh

    if needs_refresh {
        let lock = sessions.refresh_lock(&cookie.sub);
        let _guard = lock.lock().await;

        // Re-check after acquiring the lock
        let still_needs_refresh = sessions
            .get(&cookie.sub)
            .is_none_or(|s| s.expires_at - now_unix() < 60);

        if still_needs_refresh {
            match refresh_access_token(&state, &cookie.sub).await {
                Ok(()) => {}
                Err(_) => {
                    sessions.remove(&cookie.sub);
                    return Err(redirect_to_login(&req));
                }
            }
        }
    }

    // Re-read session (may have been updated by refresh)
    let session = sessions
        .get(&cookie.sub)
        .ok_or_else(|| redirect_to_login(&req))?;

    // Insert authenticated user into request extensions
    let user = AuthenticatedUser {
        sub: cookie.sub.clone(),
        groups: session.groups.clone(),
        access_token: session.access_token.clone(),
    };
    drop(session);

    req.extensions_mut().insert(user);
    Ok(next.run(req).await)
}

/// Exchange the refresh token for a new access token via the IdP's
/// token endpoint. Updates the server-side session in place.
async fn refresh_access_token<B: Backend>(
    state: &AppState<B>,
    sub: &str,
) -> Result<(), SessionError> {
    let oidc = state
        .oidc_client
        .as_ref()
        .ok_or(SessionError::OidcNotConfigured)?;
    let sessions = state
        .user_sessions
        .as_ref()
        .ok_or(SessionError::OidcNotConfigured)?;

    let refresh_token = sessions
        .get(sub)
        .map(|s| s.refresh_token.clone())
        .ok_or(SessionError::SessionNotFound)?;

    let http_client = openidconnect::reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| SessionError::RefreshFailed(e.to_string()))?;

    let refresh_token_obj = RefreshToken::new(refresh_token);
    let token_request = oidc
        .client
        .exchange_refresh_token(&refresh_token_obj)
        .map_err(|e| SessionError::RefreshFailed(e.to_string()))?;

    let token_response: BlockyardTokenResponse = token_request
        .request_async(&http_client)
        .await
        .map_err(|e| SessionError::RefreshFailed(e.to_string()))?;

    let new_access_token = token_response.access_token().secret().clone();
    let new_refresh_token = token_response.refresh_token().map(|t| t.secret().clone());
    let new_expires_at = token_response
        .expires_in()
        .map(|d| now_unix() + d.as_secs() as i64)
        .unwrap_or_else(|| now_unix() + 300);

    sessions.update_tokens(sub, new_access_token, new_refresh_token, new_expires_at);

    Ok(())
}

pub type SharedOidcClient = Arc<OidcClient>;
