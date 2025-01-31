use bolt_sidecar::{Config, SidecarDriver};
use eyre::{bail, Result};
use tracing::info;

#[tokio::main]
async fn main() -> Result<()> {
    // TODO: improve telemetry setup (#116)
    tracing_subscriber::fmt::init();

    let config = match Config::parse_from_cli() {
        Ok(config) => config,
        Err(err) => bail!("Failed to parse CLI arguments: {:?}", err),
    };

    info!(chain = config.chain.name(), "Starting Bolt sidecar");
    match SidecarDriver::new(config).await {
        Ok(driver) => driver.run_forever().await,
        Err(err) => bail!("Failed to initialize the sidecar driver: {:?}", err),
    };

    Ok(())
}
