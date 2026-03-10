use std::sync::Arc;

use dashmap::DashMap;
use sqlx::SqlitePool;

use crate::backend::Backend;
use crate::config::Config;
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
}

/// A running worker tracked by the server.
#[derive(Debug, Clone)]
pub struct ActiveWorker<H: Clone> {
    pub app_id: String,
    pub handle: H,
    pub session_id: String,
}

impl<B: Backend> AppState<B> {
    pub fn new(config: Config, backend: B, db: SqlitePool) -> Self {
        Self {
            config: Arc::new(config),
            backend: Arc::new(backend),
            db,
            workers: Arc::new(DashMap::new()),
            task_store: Arc::new(InMemoryTaskStore::new()),
        }
    }
}
