use blockyard::app::AppState;
use blockyard::config::Config;
use blockyard::ops;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "blockyard=info".parse().unwrap()),
        )
        .json()
        .init();

    let config = Config::load()?;
    tracing::info!("loaded config");

    // Initialize backend
    #[cfg(feature = "docker")]
    let backend = {
        let docker_config = config.docker.clone().expect("[docker] config required");
        blockyard::backend::docker::DockerBackend::new(docker_config).await?
    };

    // Initialize database
    let db = blockyard::db::create_pool(&config.database.path).await?;

    // Build state
    let state = AppState::new(config.clone(), backend, db);

    // Run startup cleanup before binding the listener
    ops::startup_cleanup(&state).await?;

    // CancellationToken for cooperative background task shutdown
    let token = tokio_util::sync::CancellationToken::new();

    // Spawn background tasks
    let health_handle = ops::spawn_health_poller(state.clone(), token.clone());
    let cleaner_handle = ops::spawn_log_retention_cleaner(
        state.clone(),
        config.proxy.log_retention,
        token.clone(),
    );

    let app = blockyard::proxy::full_router(state.clone());

    // Start server
    let listener = tokio::net::TcpListener::bind(&config.server.bind).await?;
    tracing::info!(bind = %config.server.bind, "server listening");

    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;

    // Cancel background tasks and wait for them to finish
    token.cancel();
    let _ = tokio::join!(health_handle, cleaner_handle);

    // Background tasks are done — clean up containers
    ops::graceful_shutdown(&state).await;

    Ok(())
}

async fn shutdown_signal() {
    tokio::signal::ctrl_c().await.ok();
    tracing::info!("shutdown signal received");
}
