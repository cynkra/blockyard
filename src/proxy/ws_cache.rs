use std::sync::Arc;
use std::time::Duration;

use dashmap::DashMap;
use tokio::sync::Mutex;
use tokio_tungstenite::MaybeTlsStream;
use tokio_tungstenite::WebSocketStream;

pub type BackendWs = WebSocketStream<MaybeTlsStream<tokio::net::TcpStream>>;

struct CachedConnection {
    ws: Mutex<Option<BackendWs>>,
    expires_at: tokio::time::Instant,
}

/// Hold backend WebSocket connections open after client disconnect,
/// allowing reconnection to the same session within `ttl`.
pub struct WsCache {
    entries: DashMap<String, Arc<CachedConnection>>,
    ttl: Duration,
}

impl WsCache {
    pub fn new(ttl: Duration) -> Self {
        Self {
            entries: DashMap::new(),
            ttl,
        }
    }

    /// Store a backend WS connection when the client disconnects.
    /// `on_expire` is called if the TTL fires without a reconnect.
    pub fn cache<F>(&self, session_id: &str, ws: BackendWs, on_expire: F)
    where
        F: FnOnce() + Send + 'static,
    {
        let entry = Arc::new(CachedConnection {
            ws: Mutex::new(Some(ws)),
            expires_at: tokio::time::Instant::now() + self.ttl,
        });

        let entry_clone = entry.clone();
        let session_id = session_id.to_string();
        let entries = self.entries.clone();

        self.entries.insert(session_id.clone(), entry);

        tokio::spawn(async move {
            tokio::time::sleep_until(entry_clone.expires_at).await;
            if entries
                .remove_if(&session_id, |_, v| Arc::ptr_eq(v, &entry_clone))
                .is_some()
            {
                on_expire();
            }
        });
    }

    /// Attempt to reclaim a cached backend WS for this session.
    /// Returns None if no cached connection exists or it has expired.
    pub async fn take(&self, session_id: &str) -> Option<BackendWs> {
        let entry = self.entries.remove(session_id)?;
        let (_, entry) = entry;
        if tokio::time::Instant::now() >= entry.expires_at {
            return None;
        }
        entry.ws.lock().await.take()
    }
}
