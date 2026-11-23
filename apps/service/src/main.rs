use clap::{Parser, Subcommand};

#[derive(Debug, Parser)]
#[command(name = "vantry-service", version, about = "Vantry Core, Provider Worker, and CLI service")]
struct Cli {
    #[command(subcommand)]
    command: Option<Commands>,
}

#[derive(Debug, Subcommand)]
enum Commands {
    /// Run Vantry Core daemon
    Core,
    /// Run isolated durable provider worker for a single attempt
    ProviderWorker,
    /// Control plane CLI commands
    Ctl,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt::init();
    let cli = Cli::parse();
    match cli.command {
        Some(Commands::Core) => tracing::info!("vantry-service core skeleton"),
        Some(Commands::ProviderWorker) => tracing::info!("vantry-service provider-worker skeleton"),
        Some(Commands::Ctl) => tracing::info!("vantry-service ctl skeleton"),
        None => tracing::info!("vantry-service ready"),
    }
    Ok(())
}
