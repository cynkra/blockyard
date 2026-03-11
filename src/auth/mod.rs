pub mod oidc;
pub mod session;

/// Represents a validated user identity extracted from a session.
/// Inserted into axum request extensions by the auth middleware.
#[derive(Debug, Clone)]
pub struct AuthenticatedUser {
    pub sub: String,
    pub groups: Vec<String>,
    pub access_token: String,
}
