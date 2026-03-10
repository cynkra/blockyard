use dashmap::DashMap;

pub type SessionId = String;
pub type WorkerId = String;

/// Maps session IDs to the worker handling that session.
/// In v0, this is a 1:1 mapping (one session per worker).
pub struct SessionStore {
    sessions: DashMap<SessionId, WorkerId>,
}

impl Default for SessionStore {
    fn default() -> Self {
        Self::new()
    }
}

impl SessionStore {
    pub fn new() -> Self {
        Self {
            sessions: DashMap::new(),
        }
    }

    pub fn get(&self, session_id: &str) -> Option<WorkerId> {
        self.sessions.get(session_id).map(|v| v.clone())
    }

    pub fn insert(&self, session_id: SessionId, worker_id: WorkerId) {
        self.sessions.insert(session_id, worker_id);
    }

    pub fn remove(&self, session_id: &str) -> Option<WorkerId> {
        self.sessions.remove(session_id).map(|(_, v)| v)
    }

    /// Remove all sessions pointing to the given worker.
    pub fn remove_by_worker(&self, worker_id: &str) {
        self.sessions.retain(|_, v| v != worker_id);
    }

    /// Count sessions assigned to a specific worker.
    pub fn count_for_worker(&self, worker_id: &str) -> usize {
        self.sessions.iter().filter(|e| e.value() == worker_id).count()
    }
}

const SESSION_COOKIE_NAME: &str = "blockyard_session";

/// Extract the session ID from the blockyard_session cookie.
pub fn extract_session_id(headers: &axum::http::HeaderMap) -> Option<String> {
    headers
        .get_all(axum::http::header::COOKIE)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .flat_map(|s| s.split(';'))
        .map(|s| s.trim())
        .find_map(|cookie| {
            let (name, value) = cookie.split_once('=')?;
            if name.trim() == SESSION_COOKIE_NAME {
                Some(value.trim().to_string())
            } else {
                None
            }
        })
}

/// Build the Set-Cookie header value for a new session.
pub fn session_cookie(session_id: &str, app_name: &str) -> String {
    format!(
        "{SESSION_COOKIE_NAME}={session_id}; Path=/app/{app_name}/; HttpOnly; SameSite=Lax"
    )
}
