mod package;
use clap::{Parser, Subcommand};

#[derive(Debug, Parser)]
#[command(
    name = "xtask",
    about = "Vantry Rust build, packaging, delta, and icon automation"
)]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Debug, Subcommand)]
enum Commands {
    /// Assemble a macOS arm64 app and ZIP from existing Rust binaries
    Package(package::Options),
    /// Build update delta package
    MakeDelta,
    /// Prepare application icons
    PrepareIcon,
}

fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Commands::Package(options) => package::run(options)?,
        Commands::MakeDelta => println!("xtask make-delta skeleton"),
        Commands::PrepareIcon => println!("xtask prepare-icon skeleton"),
    }
    Ok(())
}
