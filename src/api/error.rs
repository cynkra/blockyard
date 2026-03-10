use axum::http::StatusCode;
use axum::response::Json;

#[derive(serde::Serialize)]
pub struct ErrorResponse {
    pub error: String,
    pub message: String,
}

/// Convenience type for handler return values.
pub type ApiError = (StatusCode, Json<ErrorResponse>);

pub fn bad_request(msg: String) -> ApiError {
    (
        StatusCode::BAD_REQUEST,
        Json(ErrorResponse {
            error: "bad_request".into(),
            message: msg,
        }),
    )
}

pub fn not_found(msg: String) -> ApiError {
    (
        StatusCode::NOT_FOUND,
        Json(ErrorResponse {
            error: "not_found".into(),
            message: msg,
        }),
    )
}

pub fn conflict(msg: String) -> ApiError {
    (
        StatusCode::CONFLICT,
        Json(ErrorResponse {
            error: "conflict".into(),
            message: msg,
        }),
    )
}

pub fn service_unavailable(msg: String) -> ApiError {
    (
        StatusCode::SERVICE_UNAVAILABLE,
        Json(ErrorResponse {
            error: "service_unavailable".into(),
            message: msg,
        }),
    )
}

pub fn server_error(msg: String) -> ApiError {
    (
        StatusCode::INTERNAL_SERVER_ERROR,
        Json(ErrorResponse {
            error: "internal_error".into(),
            message: msg,
        }),
    )
}
