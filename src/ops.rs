use std::collections::VecDeque;
use std::sync::Arc;
use std::time::Duration;

use dashmap::DashMap;
use tokio::sync::{Mutex, broadcast};
use tokio::task::JoinHandle;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

use futures_util::future;

use crate::app::AppState;
use crate::backend::{Backend, BackendError};
use crate::db;

// ---------------------------------------------------------------------------
// LogStore
// ---------------------------------------------------------------------------

const MAX_LOG_LINES: usize = 50_000;

struct LogEntry {
    app_id: String,
    buffer: Arc<Mutex<VecDeque<String>>>,
    tx: broadcast::Sender<String>,
    ended_at: Mutex<Option<Instant>>,
}

pub struct LogStore {
    entries: DashMap<String, LogEntry>,
}

pub struct LogSubscription {
    pub lines: Vec<String>,
    pub rx: broadcast::Receiver<String>,
    pub ended: bool,
}

pub struct LogSender {
    tx: broadcast::Sender<String>,
    buffer: Arc<Mutex<VecDeque<String>>>,
}

impl LogSender {
    pub async fn send(&self, line: String) {
        let mut buf = self.buffer.lock().await;
        buf.push_back(line.clone());
        if buf.len() > MAX_LOG_LINES {
            buf.pop_front();
        }
        drop(buf);
        let _ = self.tx.send(line);
    }
}

impl Default for LogStore {
    fn default() -> Self {
        Self::new()
    }
}

impl LogStore {
    pub fn new() -> Self {
        Self {
            entries: DashMap::new(),
        }
    }

    pub fn create(&self, worker_id: &str, app_id: &str) -> LogSender {
        let (tx, _) = broadcast::channel(256);
        let buffer = Arc::new(Mutex::new(VecDeque::new()));
        self.entries.insert(
            worker_id.to_string(),
            LogEntry {
                app_id: app_id.to_string(),
                buffer: buffer.clone(),
                tx: tx.clone(),
                ended_at: Mutex::new(None),
            },
        );
        LogSender { tx, buffer }
    }

    /// Subscribe-then-snapshot: subscribe to broadcast first, then snapshot
    /// buffer, so no lines are missed. Caller skips `lines.len()` items from
    /// the receiver to deduplicate.
    pub async fn subscribe(&self, worker_id: &str) -> Option<LogSubscription> {
        let entry = self.entries.get(worker_id)?;
        let rx = entry.tx.subscribe();
        let buffer = entry.buffer.lock().await;
        let lines: Vec<String> = buffer.iter().cloned().collect();
        let ended = entry.ended_at.lock().await.is_some();
        Some(LogSubscription { lines, rx, ended })
    }

    /// Find a worker for the app; prefer a live (not ended) worker over an
    /// ended one.
    pub async fn subscribe_by_app(&self, app_id: &str) -> Option<(String, LogSubscription)> {
        // First pass: find a live worker
        for entry in self.entries.iter() {
            if entry.value().app_id == app_id && entry.value().ended_at.lock().await.is_none() {
                let worker_id = entry.key().clone();
                drop(entry);
                let sub = self.subscribe(&worker_id).await?;
                return Some((worker_id, sub));
            }
        }
        // Second pass: find any ended worker
        for entry in self.entries.iter() {
            if entry.value().app_id == app_id {
                let worker_id = entry.key().clone();
                drop(entry);
                let sub = self.subscribe(&worker_id).await?;
                return Some((worker_id, sub));
            }
        }
        None
    }

    pub async fn mark_ended(&self, worker_id: &str) {
        if let Some(entry) = self.entries.get(worker_id) {
            let mut ended = entry.ended_at.lock().await;
            ended.get_or_insert(Instant::now());
        }
    }

    pub async fn cleanup_expired(&self, retention: Duration) {
        let now = Instant::now();
        let mut to_remove = Vec::new();
        for entry in self.entries.iter() {
            if let Some(ended_at) = *entry.value().ended_at.lock().await
                && now.duration_since(ended_at) > retention
            {
                to_remove.push(entry.key().clone());
            }
        }
        for key in to_remove {
            self.entries.remove(&key);
        }
    }

    pub async fn has_active(&self, worker_id: &str) -> bool {
        if let Some(entry) = self.entries.get(worker_id) {
            entry.ended_at.lock().await.is_none()
        } else {
            false
        }
    }
}

// ---------------------------------------------------------------------------
// evict_worker
// ---------------------------------------------------------------------------

/// Fully decommission a worker: stop backend, remove from workers/registry/
/// sessions, mark log ended. Idempotent — safe to call if the worker is
/// already gone.
pub async fn evict_worker<B: Backend>(state: &AppState<B>, worker_id: &str) {
    if let Some((_, worker)) = state.workers.remove(worker_id)
        && let Err(e) = state.backend.stop(&worker.handle).await
    {
        tracing::warn!(worker_id, error = %e, "failed to stop worker");
    }
    state.registry.remove(worker_id);
    state.sessions.remove_by_worker(worker_id);
    state.log_store.mark_ended(worker_id).await;
}

// ---------------------------------------------------------------------------
// startup_cleanup
// ---------------------------------------------------------------------------

pub async fn startup_cleanup<B: Backend>(state: &AppState<B>) -> Result<(), BackendError> {
    // 0. Remove orphaned iptables rules from previous runs
    #[cfg(feature = "docker")]
    crate::backend::docker::cleanup_orphan_metadata_rules().await;

    // 1. Remove orphaned containers and networks.
    //    Fail hard if we can't talk to the backend.
    let resources = state.backend.list_managed().await?;
    if !resources.is_empty() {
        tracing::info!(count = resources.len(), "removing orphaned resources");
    }
    for resource in &resources {
        if let Err(e) = state.backend.remove_resource(resource).await {
            tracing::warn!(id = %resource.id, error = %e, "failed to remove orphan");
        }
    }

    // 2. Fail stale bundles
    let count = db::sqlite::fail_stale_bundles(&state.db)
        .await
        .expect("fail_stale_bundles: db reachable at startup");
    if count > 0 {
        tracing::info!(count, "marked stale bundles as failed");
    }

    Ok(())
}

// ---------------------------------------------------------------------------
// graceful_shutdown
// ---------------------------------------------------------------------------

pub async fn graceful_shutdown<B: Backend + Clone>(state: &AppState<B>) {
    // 1. Evict all tracked workers concurrently
    let worker_ids: Vec<String> = state.workers.iter().map(|e| e.key().clone()).collect();

    if !worker_ids.is_empty() {
        tracing::info!(count = worker_ids.len(), "shutting down workers");
    }

    let evictions = worker_ids.iter().map(|worker_id| {
        let worker_id = worker_id.clone();
        let state = state.clone();
        async move {
            tokio::time::timeout(Duration::from_secs(15), evict_worker(&state, &worker_id))
                .await
                .ok();
        }
    });
    future::join_all(evictions).await;

    // 2. Remove any remaining managed resources (build containers, networks)
    if let Ok(resources) = state.backend.list_managed().await {
        for resource in &resources {
            let _ = state.backend.remove_resource(resource).await;
        }
    }

    // 3. Fail any in-progress builds
    if let Ok(count) = db::sqlite::fail_stale_bundles(&state.db).await
        && count > 0
    {
        tracing::info!(count, "shutdown: marked stale bundles as failed");
    }
}

// ---------------------------------------------------------------------------
// Health poller
// ---------------------------------------------------------------------------

pub fn spawn_health_poller<B: Backend + Clone>(
    state: AppState<B>,
    token: CancellationToken,
) -> JoinHandle<()> {
    let interval_duration = state.config.proxy.health_interval;
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(interval_duration);
        // Consume the first (immediate) tick so we don't health-check
        // workers that may still be starting up.
        interval.tick().await;

        loop {
            tokio::select! {
                _ = token.cancelled() => break,
                _ = interval.tick() => {}
            }

            // Snapshot worker IDs + handles
            let snapshot: Vec<(String, B::Handle)> = state
                .workers
                .iter()
                .map(|e| (e.key().clone(), e.value().handle.clone()))
                .collect();

            if snapshot.is_empty() {
                continue;
            }

            let mut checks = tokio::task::JoinSet::new();
            for (worker_id, handle) in snapshot {
                let backend = state.backend.clone();
                checks.spawn(async move {
                    let healthy = backend.health_check(&handle).await;
                    (worker_id, healthy)
                });
            }

            while let Some(Ok((worker_id, healthy))) = checks.join_next().await {
                if !healthy {
                    tracing::warn!(worker_id, "worker unhealthy — evicting");
                    evict_worker(&state, &worker_id).await;
                }
            }
        }
    })
}

// ---------------------------------------------------------------------------
// Log capture
// ---------------------------------------------------------------------------

/// Spawn log capture for a worker. Call after a successful spawn.
pub fn spawn_log_capture<B: Backend>(
    state: &AppState<B>,
    worker_id: String,
    app_id: String,
    handle: B::Handle,
) {
    let log_store = state.log_store.clone();
    let backend = state.backend.clone();
    tokio::spawn(async move {
        let sender = log_store.create(&worker_id, &app_id);
        match backend.logs(&handle).await {
            Ok(mut rx) => {
                while let Some(line) = rx.recv().await {
                    sender.send(line).await;
                }
            }
            Err(e) => {
                tracing::warn!(worker_id, error = %e, "log capture failed");
            }
        }
        log_store.mark_ended(&worker_id).await;
    });
}

// ---------------------------------------------------------------------------
// Log retention cleaner
// ---------------------------------------------------------------------------

pub fn spawn_log_retention_cleaner<B: Backend>(
    state: AppState<B>,
    retention: Duration,
    token: CancellationToken,
) -> JoinHandle<()> {
    let interval_duration = retention.min(Duration::from_secs(60));
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(interval_duration);
        interval.tick().await; // consume immediate tick

        loop {
            tokio::select! {
                _ = token.cancelled() => break,
                _ = interval.tick() => {}
            }
            state.log_store.cleanup_expired(retention).await;
        }
    })
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn logstore_create_and_subscribe() {
        let store = LogStore::new();
        let sender = store.create("w1", "app1");
        sender.send("line 1".into()).await;
        sender.send("line 2".into()).await;

        let sub = store.subscribe("w1").await.unwrap();
        assert_eq!(sub.lines.len(), 2);
        assert_eq!(sub.lines[0], "line 1");
        assert_eq!(sub.lines[1], "line 2");
        assert!(!sub.ended);
    }

    #[tokio::test]
    async fn logstore_mark_ended() {
        let store = LogStore::new();
        let _sender = store.create("w1", "app1");
        assert!(store.has_active("w1").await);

        store.mark_ended("w1").await;
        assert!(!store.has_active("w1").await);

        let sub = store.subscribe("w1").await.unwrap();
        assert!(sub.ended);
    }

    #[tokio::test]
    async fn logstore_subscribe_by_app_prefers_live() {
        let store = LogStore::new();
        let _sender1 = store.create("w1", "app1");
        store.mark_ended("w1").await;
        let _sender2 = store.create("w2", "app1");

        let (wid, sub) = store.subscribe_by_app("app1").await.unwrap();
        assert_eq!(wid, "w2");
        assert!(!sub.ended);
    }

    #[tokio::test]
    async fn logstore_cleanup_expired_removes_ended() {
        let store = LogStore::new();
        let _sender = store.create("w1", "app1");
        store.mark_ended("w1").await;

        // Zero retention — should remove immediately
        store.cleanup_expired(Duration::ZERO).await;
        assert!(store.subscribe("w1").await.is_none());
    }

    #[tokio::test]
    async fn logstore_subscribe_nonexistent_returns_none() {
        let store = LogStore::new();
        assert!(store.subscribe("nope").await.is_none());
    }

    #[tokio::test]
    async fn logstore_has_active_reflects_state() {
        let store = LogStore::new();
        assert!(!store.has_active("w1").await);

        let _sender = store.create("w1", "app1");
        assert!(store.has_active("w1").await);

        store.mark_ended("w1").await;
        assert!(!store.has_active("w1").await);
    }

    #[tokio::test]
    async fn logstore_mark_ended_idempotent() {
        let store = LogStore::new();
        let _sender = store.create("w1", "app1");
        store.mark_ended("w1").await;
        store.mark_ended("w1").await; // should not panic
        assert!(!store.has_active("w1").await);
    }

    #[tokio::test]
    async fn logstore_buffer_cap() {
        let store = LogStore::new();
        let sender = store.create("w1", "app1");
        for i in 0..MAX_LOG_LINES + 100 {
            sender.send(format!("line {i}")).await;
        }
        let sub = store.subscribe("w1").await.unwrap();
        assert_eq!(sub.lines.len(), MAX_LOG_LINES);
        // First line should be 100 (oldest 100 were dropped)
        assert_eq!(sub.lines[0], "line 100");
    }
}
