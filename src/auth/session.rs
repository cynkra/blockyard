use std::sync::Arc;

use dashmap::DashMap;
use hmac::{Hmac, Mac};
use serde::{Deserialize, Serialize};
use sha2::Sha256;

/// HMAC signing key derived from session_secret.
pub struct SigningKey {
    key: Vec<u8>,
}

impl SigningKey {
    /// Derive the signing key from a secret using HMAC with a domain
    /// separation string.
    pub fn derive(secret: &str) -> Self {
        let mut mac =
            Hmac::<Sha256>::new_from_slice(secret.as_bytes()).expect("HMAC accepts any key length");
        mac.update(b"blockyard-cookie-signing");
        Self {
            key: mac.finalize().into_bytes().to_vec(),
        }
    }

    /// Compute HMAC-SHA256 signature and return as URL-safe base64.
    pub fn sign(&self, data: &[u8]) -> String {
        let mut mac =
            Hmac::<Sha256>::new_from_slice(&self.key).expect("HMAC accepts any key length");
        mac.update(data);
        base64::Engine::encode(
            &base64::engine::general_purpose::URL_SAFE_NO_PAD,
            mac.finalize().into_bytes(),
        )
    }

    /// Verify a base64-encoded HMAC signature against data.
    pub fn verify(&self, data: &[u8], sig_b64: &str) -> Result<(), SessionError> {
        let signature =
            base64::Engine::decode(&base64::engine::general_purpose::URL_SAFE_NO_PAD, sig_b64)?;
        let mut mac =
            Hmac::<Sha256>::new_from_slice(&self.key).expect("HMAC accepts any key length");
        mac.update(data);
        mac.verify_slice(&signature)
            .map_err(|_| SessionError::InvalidSignature)
    }
}

/// Minimal payload encoded into the session cookie.
/// Signed with HMAC-SHA256. All other session data is server-side.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CookiePayload {
    /// IdP subject identifier
    pub sub: String,
    /// Cookie issue time (unix timestamp)
    pub issued_at: i64,
}

impl CookiePayload {
    /// Encode the payload into a signed cookie value.
    /// Format: base64(json) + "." + base64(hmac)
    pub fn encode(&self, key: &SigningKey) -> Result<String, SessionError> {
        let json = serde_json::to_string(self)?;
        let payload = base64::Engine::encode(
            &base64::engine::general_purpose::URL_SAFE_NO_PAD,
            json.as_bytes(),
        );
        let signature = key.sign(payload.as_bytes());
        Ok(format!("{payload}.{signature}"))
    }

    /// Decode and verify a signed cookie value.
    pub fn decode(value: &str, key: &SigningKey) -> Result<Self, SessionError> {
        let (payload_b64, sig_b64) = value.split_once('.').ok_or(SessionError::MalformedCookie)?;

        key.verify(payload_b64.as_bytes(), sig_b64)?;

        let payload_bytes = base64::Engine::decode(
            &base64::engine::general_purpose::URL_SAFE_NO_PAD,
            payload_b64,
        )?;
        let payload: CookiePayload = serde_json::from_slice(&payload_bytes)?;
        Ok(payload)
    }
}

/// Per-user session data stored server-side. Keyed by `sub`.
#[derive(Debug, Clone)]
pub struct UserSession {
    /// Group memberships from IdP groups claim
    pub groups: Vec<String>,
    /// IdP access token (short-lived, refreshed transparently)
    pub access_token: String,
    /// IdP refresh token (long-lived)
    pub refresh_token: String,
    /// Access token expiry (unix timestamp)
    pub expires_at: i64,
}

/// In-memory session store. Wraps a DashMap keyed by user `sub`.
#[derive(Default)]
pub struct UserSessionStore {
    sessions: DashMap<String, UserSession>,
    /// Per-user refresh locks. Prevents concurrent token refreshes for
    /// the same user.
    refresh_locks: DashMap<String, Arc<tokio::sync::Mutex<()>>>,
}

impl UserSessionStore {
    pub fn new() -> Self {
        Self {
            sessions: DashMap::new(),
            refresh_locks: DashMap::new(),
        }
    }

    /// Get or create a per-user refresh lock.
    pub fn refresh_lock(&self, sub: &str) -> Arc<tokio::sync::Mutex<()>> {
        self.refresh_locks
            .entry(sub.to_string())
            .or_insert_with(|| Arc::new(tokio::sync::Mutex::new(())))
            .clone()
    }

    /// Insert or replace the session for a user.
    pub fn insert(&self, sub: String, session: UserSession) {
        self.sessions.insert(sub, session);
    }

    /// Look up a user's session by sub.
    pub fn get(&self, sub: &str) -> Option<dashmap::mapref::one::Ref<'_, String, UserSession>> {
        self.sessions.get(sub)
    }

    /// Remove a user's session (on logout).
    pub fn remove(&self, sub: &str) -> Option<(String, UserSession)> {
        self.refresh_locks.remove(sub);
        self.sessions.remove(sub)
    }

    /// Update tokens after a refresh. Returns false if session not found.
    pub fn update_tokens(
        &self,
        sub: &str,
        access_token: String,
        refresh_token: Option<String>,
        expires_at: i64,
    ) -> bool {
        if let Some(mut session) = self.sessions.get_mut(sub) {
            session.access_token = access_token;
            if let Some(rt) = refresh_token {
                session.refresh_token = rt;
            }
            session.expires_at = expires_at;
            true
        } else {
            false
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum SessionError {
    #[error("malformed cookie")]
    MalformedCookie,
    #[error("invalid cookie signature")]
    InvalidSignature,
    #[error("base64 decode error: {0}")]
    Base64(#[from] base64::DecodeError),
    #[error("JSON error: {0}")]
    Json(#[from] serde_json::Error),
    #[error("OIDC not configured")]
    OidcNotConfigured,
    #[error("session not found")]
    SessionNotFound,
    #[error("token refresh failed: {0}")]
    RefreshFailed(String),
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_key() -> SigningKey {
        SigningKey::derive("test-secret")
    }

    // --- CookiePayload tests ---

    #[test]
    fn cookie_round_trip() {
        let key = test_key();
        let payload = CookiePayload {
            sub: "user-123".into(),
            issued_at: 1700000000,
        };
        let encoded = payload.encode(&key).unwrap();
        let decoded = CookiePayload::decode(&encoded, &key).unwrap();
        assert_eq!(decoded.sub, "user-123");
        assert_eq!(decoded.issued_at, 1700000000);
    }

    #[test]
    fn tampered_payload_rejected() {
        let key = test_key();
        let payload = CookiePayload {
            sub: "user-123".into(),
            issued_at: 1700000000,
        };
        let encoded = payload.encode(&key).unwrap();
        let (_, sig) = encoded.split_once('.').unwrap();

        // Tamper with payload
        let tampered_json = r#"{"sub":"evil","issued_at":1700000000}"#;
        let tampered_b64 = base64::Engine::encode(
            &base64::engine::general_purpose::URL_SAFE_NO_PAD,
            tampered_json.as_bytes(),
        );
        let tampered = format!("{tampered_b64}.{sig}");
        assert!(CookiePayload::decode(&tampered, &key).is_err());
    }

    #[test]
    fn tampered_signature_rejected() {
        let key = test_key();
        let payload = CookiePayload {
            sub: "user-123".into(),
            issued_at: 1700000000,
        };
        let encoded = payload.encode(&key).unwrap();
        let (payload_b64, _) = encoded.split_once('.').unwrap();

        let tampered = format!("{payload_b64}.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA");
        assert!(CookiePayload::decode(&tampered, &key).is_err());
    }

    #[test]
    fn malformed_cookie_no_dot() {
        let key = test_key();
        assert!(CookiePayload::decode("nodothere", &key).is_err());
    }

    #[test]
    fn malformed_cookie_empty_segments() {
        let key = test_key();
        assert!(CookiePayload::decode(".", &key).is_err());
    }

    #[test]
    fn wrong_key_rejected() {
        let key1 = SigningKey::derive("secret-1");
        let key2 = SigningKey::derive("secret-2");
        let payload = CookiePayload {
            sub: "user-123".into(),
            issued_at: 1700000000,
        };
        let encoded = payload.encode(&key1).unwrap();
        assert!(CookiePayload::decode(&encoded, &key2).is_err());
    }

    // --- UserSessionStore tests ---

    #[test]
    fn session_store_insert_and_get() {
        let store = UserSessionStore::new();
        store.insert(
            "user-1".into(),
            UserSession {
                groups: vec!["admin".into()],
                access_token: "at-1".into(),
                refresh_token: "rt-1".into(),
                expires_at: 9999999999,
            },
        );
        let session = store.get("user-1").unwrap();
        assert_eq!(session.groups, vec!["admin"]);
        assert_eq!(session.access_token, "at-1");
    }

    #[test]
    fn session_store_get_missing() {
        let store = UserSessionStore::new();
        assert!(store.get("nonexistent").is_none());
    }

    #[test]
    fn session_store_remove() {
        let store = UserSessionStore::new();
        store.insert(
            "user-1".into(),
            UserSession {
                groups: vec![],
                access_token: "at".into(),
                refresh_token: "rt".into(),
                expires_at: 0,
            },
        );
        let removed = store.remove("user-1");
        assert!(removed.is_some());
        assert!(store.get("user-1").is_none());
    }

    #[test]
    fn session_store_update_tokens() {
        let store = UserSessionStore::new();
        store.insert(
            "user-1".into(),
            UserSession {
                groups: vec![],
                access_token: "old-at".into(),
                refresh_token: "old-rt".into(),
                expires_at: 100,
            },
        );
        let updated = store.update_tokens("user-1", "new-at".into(), Some("new-rt".into()), 200);
        assert!(updated);

        let session = store.get("user-1").unwrap();
        assert_eq!(session.access_token, "new-at");
        assert_eq!(session.refresh_token, "new-rt");
        assert_eq!(session.expires_at, 200);
    }

    #[test]
    fn session_store_update_tokens_preserves_refresh_when_none() {
        let store = UserSessionStore::new();
        store.insert(
            "user-1".into(),
            UserSession {
                groups: vec![],
                access_token: "old-at".into(),
                refresh_token: "keep-rt".into(),
                expires_at: 100,
            },
        );
        store.update_tokens("user-1", "new-at".into(), None, 200);

        let session = store.get("user-1").unwrap();
        assert_eq!(session.access_token, "new-at");
        assert_eq!(session.refresh_token, "keep-rt"); // preserved
    }

    #[test]
    fn session_store_update_missing_returns_false() {
        let store = UserSessionStore::new();
        assert!(!store.update_tokens("nope", "at".into(), None, 0));
    }
}
