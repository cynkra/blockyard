use axum::body::Body;
use axum::extract::{Path, State};
use axum::response::{IntoResponse, Response};
use futures_util::StreamExt;
use tokio_stream::wrappers::BroadcastStream;

use crate::api::error::not_found;
use crate::app::AppState;
use crate::backend::Backend;
use crate::task::TaskStatus;

pub async fn task_logs<B: Backend>(
    State(state): State<AppState<B>>,
    Path(task_id): Path<String>,
) -> Result<Response, Response> {
    let task_state = state
        .task_store
        .get(&task_id)
        .ok_or_else(|| not_found(format!("No task with ID {task_id}")).into_response())?;

    let (buffer, rx) = state
        .task_store
        .subscribe(&task_id)
        .await
        .ok_or_else(|| not_found(format!("No task with ID {task_id}")).into_response())?;

    let is_done = task_state.status != TaskStatus::Running;
    let buffer_len = buffer.len();

    // Build a stream: first the buffered lines, then live lines
    let buffer_stream = futures_util::stream::iter(
        buffer
            .into_iter()
            .map(|line| Ok::<_, std::convert::Infallible>(format!("{line}\n"))),
    );

    if is_done {
        // Task already finished — return just the buffer
        let body = Body::from_stream(buffer_stream);
        return Ok(Response::builder()
            .header("content-type", "text/plain")
            .body(body)
            .unwrap());
    }

    // Task is still running — stream buffer then live output.
    // Skip `buffer_len` items from the receiver to deduplicate lines
    // that landed between subscribe() and the buffer snapshot.
    let live_stream = BroadcastStream::new(rx)
        .skip(buffer_len)
        .filter_map(|result| {
            std::future::ready(match result {
                Ok(line) => Some(Ok(format!("{line}\n"))),
                Err(tokio_stream::wrappers::errors::BroadcastStreamRecvError::Lagged(n)) => {
                    Some(Ok(format!("[dropped {n} lines]\n")))
                }
            })
        });

    let combined = buffer_stream.chain(live_stream);
    let body = Body::from_stream(combined);

    Ok(Response::builder()
        .header("content-type", "text/plain")
        .body(body)
        .unwrap())
}
