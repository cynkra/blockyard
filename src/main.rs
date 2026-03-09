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

    // Server wiring comes in later phases.
    drop(config);
    Ok(())
}
