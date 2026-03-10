use blockyard::app::AppState;
use blockyard::config::Config;

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

    // Build state and router
    let state = AppState::new(config.clone(), backend, db);
    let app = blockyard::proxy::full_router(state);

    // Start server
    let listener = tokio::net::TcpListener::bind(&config.server.bind).await?;
    tracing::info!(bind = %config.server.bind, "server listening");

    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;

    Ok(())
}

async fn shutdown_signal() {
    tokio::signal::ctrl_c().await.ok();
    tracing::info!("shutdown signal received");
}
