use clap::{Parser, Subcommand};

#[derive(Debug, Parser)]
#[command(
    name = "vantry-service",
    version,
    about = "Vantry Core, Provider Worker, and CLI service"
)]
struct Cli {
    #[command(subcommand)]
    command: Option<Commands>,
}

#[derive(Debug, Subcommand)]
enum Commands {
    /// Run Vantry Core daemon
    Core,
    /// Run isolated durable provider worker for a single attempt
    ProviderWorker {
        #[arg(long)]
        job_dir: std::path::PathBuf,
    },
    /// Deliver a durable response or steer control from stdin
    WorkerControl {
        #[arg(long)]
        job_dir: std::path::PathBuf,
    },
    /// Request durable cancellation of an attempt
    WorkerStop {
        #[arg(long)]
        job_dir: std::path::PathBuf,
    },
    /// Prepare a P0 job from JSON on stdin, then run the worker
    SmokeWorker,
    /// Merge a PiSetupRequest from stdin into the active Pi config directory
    PiSetup,
    /// Control plane CLI commands
    Ctl,
}

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt::init();
    let cli = Cli::parse();
    match cli.command {
        Some(Commands::Core) => tracing::info!("vantry-service core skeleton"),
        Some(Commands::ProviderWorker { job_dir }) => {
            vantry_pi_runtime::run_worker(&job_dir).await?
        }
        Some(Commands::WorkerControl { job_dir }) => {
            let control = serde_json::from_reader(std::io::stdin().lock())?;
            vantry_pi_runtime::deliver_control(&job_dir, &control)?;
        }
        Some(Commands::WorkerStop { job_dir }) => vantry_pi_runtime::request_stop(&job_dir)?,
        Some(Commands::SmokeWorker) => {
            let job = serde_json::from_reader(std::io::stdin().lock())?;
            let dir = vantry_pi_runtime::prepare_job(&job)?;
            vantry_pi_runtime::run_worker(&dir).await?;
        }
        Some(Commands::PiSetup) => {
            let request = serde_json::from_reader(std::io::stdin().lock())?;
            vantry_pi_runtime::save_setup(&vantry_pi_runtime::agent_dir(), &request)?;
        }
        Some(Commands::Ctl) => tracing::info!("vantry-service ctl skeleton"),
        None => tracing::info!("vantry-service ready"),
    }
    Ok(())
}
