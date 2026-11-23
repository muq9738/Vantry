use clap::{Parser, Subcommand};

#[derive(Debug, Parser)]
#[command(name = "xtask", about = "Vantry Rust build, packaging, delta, and icon automation")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Debug, Subcommand)]
enum Commands {
    /// Package desktop, service, relay, and helper artifacts
    Package {
        #[arg(long)]
        target: Option<String>,
    },
    /// Build update delta package
    MakeDelta,
    /// Prepare application icons
    PrepareIcon,
}

fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Commands::Package { target } => {
            println!("xtask package skeleton (target={target:?})");
        }
        Commands::MakeDelta => println!("xtask make-delta skeleton"),
        Commands::PrepareIcon => println!("xtask prepare-icon skeleton"),
    }
    Ok(())
}
