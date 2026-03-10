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
        self.sessions
            .iter()
            .filter(|e| e.value() == worker_id)
            .count()
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
    format!("{SESSION_COOKIE_NAME}={session_id}; Path=/app/{app_name}/; HttpOnly; SameSite=Lax")
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderMap;
    use axum::http::header::COOKIE;

    // ---- SessionStore tests ----

    #[test]
    fn store_insert_and_get() {
        let store = SessionStore::new();
        store.insert("s1".into(), "w1".into());
        assert_eq!(store.get("s1"), Some("w1".into()));
    }

    #[test]
    fn store_get_missing_returns_none() {
        let store = SessionStore::new();
        assert_eq!(store.get("nonexistent"), None);
    }

    #[test]
    fn store_remove_returns_worker() {
        let store = SessionStore::new();
        store.insert("s1".into(), "w1".into());
        assert_eq!(store.remove("s1"), Some("w1".into()));
        assert_eq!(store.get("s1"), None);
    }

    #[test]
    fn store_remove_missing_returns_none() {
        let store = SessionStore::new();
        assert_eq!(store.remove("nonexistent"), None);
    }

    #[test]
    fn store_remove_by_worker() {
        let store = SessionStore::new();
        store.insert("s1".into(), "w1".into());
        store.insert("s2".into(), "w1".into());
        store.insert("s3".into(), "w2".into());

        store.remove_by_worker("w1");
        assert_eq!(store.get("s1"), None);
        assert_eq!(store.get("s2"), None);
        assert_eq!(store.get("s3"), Some("w2".into()));
    }

    #[test]
    fn store_count_for_worker() {
        let store = SessionStore::new();
        store.insert("s1".into(), "w1".into());
        store.insert("s2".into(), "w1".into());
        store.insert("s3".into(), "w2".into());

        assert_eq!(store.count_for_worker("w1"), 2);
        assert_eq!(store.count_for_worker("w2"), 1);
        assert_eq!(store.count_for_worker("w3"), 0);
    }

    // ---- extract_session_id tests ----

    #[test]
    fn extract_from_single_cookie() {
        let mut headers = HeaderMap::new();
        headers.insert(COOKIE, "blockyard_session=abc123".parse().unwrap());
        assert_eq!(extract_session_id(&headers), Some("abc123".into()));
    }

    #[test]
    fn extract_from_multiple_cookies_in_one_header() {
        let mut headers = HeaderMap::new();
        headers.insert(
            COOKIE,
            "other=val; blockyard_session=abc123; another=x"
                .parse()
                .unwrap(),
        );
        assert_eq!(extract_session_id(&headers), Some("abc123".into()));
    }

    #[test]
    fn extract_with_spaces_around_value() {
        let mut headers = HeaderMap::new();
        headers.insert(COOKIE, "blockyard_session = abc123".parse().unwrap());
        assert_eq!(extract_session_id(&headers), Some("abc123".into()));
    }

    #[test]
    fn extract_missing_cookie_returns_none() {
        let mut headers = HeaderMap::new();
        headers.insert(COOKIE, "other=val; something=else".parse().unwrap());
        assert_eq!(extract_session_id(&headers), None);
    }

    #[test]
    fn extract_no_cookie_header_returns_none() {
        let headers = HeaderMap::new();
        assert_eq!(extract_session_id(&headers), None);
    }

    #[test]
    fn extract_malformed_cookie_no_equals() {
        let mut headers = HeaderMap::new();
        headers.insert(COOKIE, "blockyard_session".parse().unwrap());
        assert_eq!(extract_session_id(&headers), None);
    }

    #[test]
    fn extract_from_multiple_cookie_headers() {
        let mut headers = HeaderMap::new();
        headers.append(COOKIE, "other=val".parse().unwrap());
        headers.append(COOKIE, "blockyard_session=abc123".parse().unwrap());
        assert_eq!(extract_session_id(&headers), Some("abc123".into()));
    }

    // ---- session_cookie tests ----

    #[test]
    fn session_cookie_format() {
        let cookie = session_cookie("sess-1", "my-app");
        assert_eq!(
            cookie,
            "blockyard_session=sess-1; Path=/app/my-app/; HttpOnly; SameSite=Lax"
        );
    }
}
