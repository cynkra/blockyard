use std::net::SocketAddr;

use axum::extract::ws::{Message as AxumMessage, WebSocket};
use axum::extract::Request;
use axum::response::Response;
use futures_util::{SinkExt, StreamExt};
use hyper_util::rt::TokioIo;
use tokio_tungstenite::tungstenite::protocol::Message;

use crate::app::AppState;
use crate::backend::Backend;

/// Forward an HTTP request to the worker, stripping the /app/{name}
/// prefix.
pub async fn forward_http(
    req: Request,
    addr: SocketAddr,
    app_name: &str,
) -> Result<Response, ForwardError> {
    let (mut parts, body) = req.into_parts();

    // Strip /app/{name} prefix from the URI
    let original_path = parts.uri.path();
    let prefix = format!("/app/{app_name}");
    let new_path = original_path
        .strip_prefix(&prefix)
        .unwrap_or(original_path);
    let new_path = if new_path.is_empty() { "/" } else { new_path };

    // Reconstruct URI with query string
    let new_uri = if let Some(query) = parts.uri.query() {
        format!("{new_path}?{query}")
    } else {
        new_path.to_string()
    };
    parts.uri = new_uri.parse().map_err(|_| ForwardError::InvalidUri)?;

    // Set forwarding headers
    parts
        .headers
        .insert("x-forwarded-for", "127.0.0.1".parse().unwrap());
    parts
        .headers
        .insert("x-forwarded-proto", "http".parse().unwrap());

    let req = Request::from_parts(parts, body);

    // Connect to the worker and send the request
    let stream = tokio::net::TcpStream::connect(addr)
        .await
        .map_err(ForwardError::Connect)?;

    let (mut sender, conn) =
        hyper::client::conn::http1::handshake(TokioIo::new(stream))
            .await
            .map_err(ForwardError::Handshake)?;

    // Spawn the connection driver
    tokio::spawn(async move {
        if let Err(e) = conn.await {
            tracing::debug!(error = %e, "connection driver error");
        }
    });

    let resp = sender
        .send_request(req)
        .await
        .map_err(ForwardError::Send)?;

    Ok(resp.map(axum::body::Body::new))
}

/// Bidirectional WebSocket frame shuttling between client and backend.
/// Called from the proxy handler's `ws.on_upgrade()` callback after
/// axum has completed the HTTP upgrade handshake with the client.
pub async fn shuttle_ws<B: Backend + Clone>(
    client_ws: WebSocket,
    addr: SocketAddr,
    path: &str,
    session_id: &str,
    state: &AppState<B>,
) -> Result<(), ForwardError> {
    // Check WS cache for an existing backend connection (reconnect case)
    let backend_ws = if let Some(cached) = state.ws_cache.take(session_id).await {
        cached
    } else {
        let backend_url = format!("ws://{addr}{path}");
        let (ws, _) = tokio_tungstenite::connect_async(&backend_url)
            .await
            .map_err(|e| ForwardError::WebSocket(format!("backend connect: {e}")))?;
        ws
    };

    let (mut client_tx, mut client_rx) = client_ws.split();
    let (mut backend_tx, mut backend_rx) = backend_ws.split();

    // Track whether the client disconnected (vs backend).
    // Only cache the backend WS if the client is the one who left.
    let client_gone = loop {
        tokio::select! {
            msg = client_rx.next() => {
                match msg {
                    Some(Ok(msg)) => {
                        // Don't forward Close to backend — preserve for caching
                        if matches!(msg, AxumMessage::Close(_)) {
                            break true;
                        }
                        if backend_tx.send(axum_to_tungstenite(msg)).await.is_err() {
                            break false; // backend broke
                        }
                    }
                    _ => break true, // client gone
                }
            }
            msg = backend_rx.next() => {
                match msg {
                    Some(Ok(msg)) => {
                        if client_tx.send(tungstenite_to_axum(msg)).await.is_err() {
                            break true; // client gone
                        }
                    }
                    _ => break false, // backend gone
                }
            }
        }
    };

    // Cache the backend WS if the client disconnected
    if client_gone
        && let Ok(backend_ws) = backend_tx.reunite(backend_rx)
    {
        let state_for_expire: AppState<B> = (*state).clone();
        let sid_for_cache = session_id.to_string();
        let sid_for_expire = sid_for_cache.clone();
        state.ws_cache.cache(&sid_for_cache, backend_ws, move || {
            // Runs if client doesn't reconnect within ws_cache_ttl
            tokio::spawn(async move {
                if let Some(worker_id) = state_for_expire.sessions.remove(&sid_for_expire)
                    && state_for_expire.sessions.count_for_worker(&worker_id) == 0
                    && let Some((_, worker)) = state_for_expire.workers.remove(&worker_id)
                {
                    state_for_expire.registry.remove(&worker_id);
                    if let Err(e) = state_for_expire.backend.stop(&worker.handle).await {
                        tracing::warn!(
                            worker_id = %worker_id,
                            error = %e,
                            "failed to stop worker on session expire"
                        );
                    }
                }
            });
        });
    }

    Ok(())
}

fn axum_to_tungstenite(msg: AxumMessage) -> Message {
    match msg {
        AxumMessage::Text(t) => Message::Text(t.to_string().into()),
        AxumMessage::Binary(b) => Message::Binary(bytes::Bytes::from(b.to_vec())),
        AxumMessage::Ping(p) => Message::Ping(bytes::Bytes::from(p.to_vec())),
        AxumMessage::Pong(p) => Message::Pong(bytes::Bytes::from(p.to_vec())),
        AxumMessage::Close(c) => Message::Close(c.map(|cf| {
            tokio_tungstenite::tungstenite::protocol::CloseFrame {
                code: cf.code.into(),
                reason: cf.reason.to_string().into(),
            }
        })),
    }
}

fn tungstenite_to_axum(msg: Message) -> AxumMessage {
    match msg {
        Message::Text(t) => AxumMessage::Text(t.to_string().into()),
        Message::Binary(b) => AxumMessage::Binary(b.to_vec().into()),
        Message::Ping(p) => AxumMessage::Ping(p.to_vec().into()),
        Message::Pong(p) => AxumMessage::Pong(p.to_vec().into()),
        Message::Close(c) => AxumMessage::Close(c.map(|cf| {
            axum::extract::ws::CloseFrame {
                code: cf.code.into(),
                reason: cf.reason.to_string().into(),
            }
        })),
        Message::Frame(_) => AxumMessage::Binary(vec![].into()),
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ForwardError {
    #[error("invalid URI")]
    InvalidUri,
    #[error("connect: {0}")]
    Connect(std::io::Error),
    #[error("handshake: {0}")]
    Handshake(hyper::Error),
    #[error("send: {0}")]
    Send(hyper::Error),
    #[error("websocket: {0}")]
    WebSocket(String),
}
