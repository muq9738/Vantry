use clap::{Parser, Subcommand};

#[derive(Debug, Parser)]
#[command(name = "vantry-relay", version, about = "Vantry standalone self-hosted relay")]
struct Cli {
    #[command(subcommand)]
    command: Option<Commands>,
}

#[derive(Debug, Subcommand)]
enum Commands {
    /// Serve relay endpoints using a JSON/YAML config file
    Serve {
        #[arg(long)]
        config: Option<String>,
    },
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt::init();
    let cli = Cli::parse();
    if let Some(Commands::Serve { config }) = cli.command {
        tracing::info!(?config, "vantry-relay serve skeleton");
    }
    Ok(())
}
