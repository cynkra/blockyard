use std::sync::Arc;

use dashmap::DashMap;
use sqlx::SqlitePool;

use crate::auth::oidc::OidcClient;
use crate::auth::session::{SigningKey, UserSessionStore};
use crate::backend::Backend;
use crate::config::Config;
use crate::ops::LogStore;
use crate::proxy::registry::WorkerRegistry;
use crate::proxy::session::SessionStore;
use crate::proxy::ws_cache::WsCache;
use crate::task::InMemoryTaskStore;

/// Shared server state. Cloneable (all fields behind Arc).
/// Generic over the backend so tests can use MockBackend.
#[derive(Clone)]
pub struct AppState<B: Backend> {
    pub config: Arc<Config>,
    pub backend: Arc<B>,
    pub db: SqlitePool,
    /// Currently running workers, keyed by worker_id.
    pub workers: Arc<DashMap<String, ActiveWorker<B::Handle>>>,
    pub task_store: Arc<InMemoryTaskStore>,
    pub sessions: Arc<SessionStore>,
    pub registry: Arc<WorkerRegistry>,
    pub ws_cache: Arc<WsCache>,
    pub log_store: Arc<LogStore>,
    /// OIDC client — None when OIDC is not configured (v0 compat).
    pub oidc_client: Option<Arc<OidcClient>>,
    /// HMAC signing key for session cookies — None when OIDC is not configured.
    pub signing_key: Option<Arc<SigningKey>>,
    /// Server-side user session store — None when OIDC is not configured.
    pub user_sessions: Option<Arc<UserSessionStore>>,
}

/// A running worker tracked by the server.
#[derive(Debug, Clone)]
pub struct ActiveWorker<H: Clone> {
    pub app_id: String,
    pub handle: H,
    pub session_id: Option<String>,
}

impl<B: Backend> AppState<B> {
    pub fn new(config: Config, backend: B, db: SqlitePool) -> Self {
        let ws_cache_ttl = config.proxy.ws_cache_ttl;
        Self {
            config: Arc::new(config),
            backend: Arc::new(backend),
            db,
            workers: Arc::new(DashMap::new()),
            task_store: Arc::new(InMemoryTaskStore::new()),
            sessions: Arc::new(SessionStore::new()),
            registry: Arc::new(WorkerRegistry::new()),
            ws_cache: Arc::new(WsCache::new(ws_cache_ttl)),
            log_store: Arc::new(LogStore::new()),
            oidc_client: None,
            signing_key: None,
            user_sessions: None,
        }
    }
}
