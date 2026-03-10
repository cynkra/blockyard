use std::sync::Arc;

use dashmap::DashMap;
use tokio::sync::{Mutex, broadcast};

pub type TaskId = String;

#[derive(Debug, Clone, serde::Serialize)]
pub struct TaskState {
    pub id: TaskId,
    pub status: TaskStatus,
    pub created_at: String,
}

#[derive(Debug, Clone, PartialEq, serde::Serialize)]
#[serde(rename_all = "lowercase")]
pub enum TaskStatus {
    Running,
    Completed,
    Failed,
}

struct TaskEntry {
    status: TaskStatus,
    created_at: String,
    buffer: Arc<Mutex<Vec<String>>>,
    tx: broadcast::Sender<String>,
}

/// Sender handle returned from `InMemoryTaskStore::create()`.
/// Used by the background task to write log lines and mark completion.
pub struct TaskSender {
    pub id: TaskId,
    tx: broadcast::Sender<String>,
    buffer: Arc<Mutex<Vec<String>>>,
}

impl TaskSender {
    /// Append a log line to the buffer and broadcast it.
    pub async fn send(&self, line: String) {
        self.buffer.lock().await.push(line.clone());
        // Ignore send errors — no receivers is fine
        let _ = self.tx.send(line);
    }

    /// Mark the task as completed or failed.
    pub async fn complete(self, store: &InMemoryTaskStore, success: bool) {
        if let Some(mut entry) = store.tasks.get_mut(&self.id) {
            entry.status = if success {
                TaskStatus::Completed
            } else {
                TaskStatus::Failed
            };
        }
    }
}

pub struct InMemoryTaskStore {
    tasks: DashMap<TaskId, TaskEntry>,
}

impl Default for InMemoryTaskStore {
    fn default() -> Self {
        Self::new()
    }
}

impl InMemoryTaskStore {
    pub fn new() -> Self {
        Self {
            tasks: DashMap::new(),
        }
    }

    /// Create a new task. Returns a sender for writing log output.
    pub fn create(&self, task_id: TaskId) -> TaskSender {
        let (tx, _) = broadcast::channel(256);
        let buffer = Arc::new(Mutex::new(Vec::new()));
        let now = chrono::Utc::now().to_rfc3339();

        self.tasks.insert(
            task_id.clone(),
            TaskEntry {
                status: TaskStatus::Running,
                created_at: now,
                buffer: buffer.clone(),
                tx: tx.clone(),
            },
        );

        TaskSender {
            id: task_id,
            tx,
            buffer,
        }
    }

    /// Get the current state of a task.
    pub fn get(&self, task_id: &str) -> Option<TaskState> {
        self.tasks.get(task_id).map(|entry| TaskState {
            id: task_id.to_string(),
            status: entry.status.clone(),
            created_at: entry.created_at.clone(),
        })
    }

    /// Subscribe-then-snapshot: returns buffered lines and a receiver that
    /// will receive lines appended *after* the subscribe. Lines between
    /// subscribe and snapshot appear in both — callers should skip
    /// `buffer.len()` items from the receiver to deduplicate.
    pub async fn subscribe(
        &self,
        task_id: &str,
    ) -> Option<(Vec<String>, broadcast::Receiver<String>)> {
        let entry = self.tasks.get(task_id)?;
        let rx = entry.tx.subscribe(); // subscribe first
        let buffer = entry.buffer.lock().await; // then snapshot
        Some((buffer.clone(), rx))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn create_and_get_task() {
        let store = InMemoryTaskStore::new();
        let sender = store.create("task-1".into());
        let state = store.get("task-1").unwrap();
        assert_eq!(state.status, TaskStatus::Running);
        sender.complete(&store, true).await;
        let state = store.get("task-1").unwrap();
        assert_eq!(state.status, TaskStatus::Completed);
    }

    #[tokio::test]
    async fn failed_task() {
        let store = InMemoryTaskStore::new();
        let sender = store.create("task-1".into());
        sender.complete(&store, false).await;
        let state = store.get("task-1").unwrap();
        assert_eq!(state.status, TaskStatus::Failed);
    }

    #[tokio::test]
    async fn log_buffering_and_subscribe() {
        let store = InMemoryTaskStore::new();
        let sender = store.create("task-1".into());

        sender.send("line 1".into()).await;
        sender.send("line 2".into()).await;

        let (buffer, mut rx) = store.subscribe("task-1").await.unwrap();
        assert_eq!(buffer.len(), 2);
        assert_eq!(buffer[0], "line 1");
        assert_eq!(buffer[1], "line 2");

        sender.send("line 3".into()).await;
        let live = rx.recv().await.unwrap();
        assert_eq!(live, "line 3");
    }

    #[tokio::test]
    async fn get_nonexistent_returns_none() {
        let store = InMemoryTaskStore::new();
        assert!(store.get("nope").is_none());
    }

    #[tokio::test]
    async fn subscribe_nonexistent_returns_none() {
        let store = InMemoryTaskStore::new();
        assert!(store.subscribe("nope").await.is_none());
    }
}
