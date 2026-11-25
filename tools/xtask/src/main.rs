mod icon;
mod package;
use anyhow::{Context, Result};
use clap::{Args, Parser, Subcommand};
use std::{
    fs,
    path::{Path, PathBuf},
};
#[derive(Debug, Parser)]
#[command(name = "xtask", about = "Vantry Rust build and release tools")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}
#[derive(Debug, Subcommand)]
enum Commands {
    /// Build and assemble a desktop, standalone service or relay release.
    Package(package::Options),
    /// Create a v1 binary delta between two release archives or bundle directories.
    MakeDelta(Delta),
    /// Remove connected edge background and generate application icons.
    PrepareIcon(icon::Options),
}
#[derive(Debug, Args)]
struct Delta {
    #[arg(long)]
    from: String,
    #[arg(long)]
    to: String,
    #[arg(long)]
    old: PathBuf,
    #[arg(long)]
    new: PathBuf,
    #[arg(long)]
    out: PathBuf,
}
fn parent(path: &Path) -> &Path {
    path.parent()
        .filter(|p| !p.as_os_str().is_empty())
        .unwrap_or(Path::new("."))
}
fn delta(mut opts: Delta) -> Result<()> {
    let unpacked = tempfile::tempdir()?;
    for (input, name) in [(&mut opts.old, "old"), (&mut opts.new, "new")] {
        if input.is_file() {
            let root = unpacked.path().join(name);
            fs::create_dir(&root)?;
            *input = vantry_updater::unpack_release(input, &root)?;
        }
    }
    let dir = parent(&opts.out);
    fs::create_dir_all(dir)?;
    let dir = dir.canonicalize()?;
    let generate = |out: &mut fs::File| {
        vantry_updater::write_delta(out, &opts.from, &opts.to, &opts.old, &opts.new)
    };
    let (file, total) =
        if dir.starts_with(opts.old.canonicalize()?) || dir.starts_with(opts.new.canonicalize()?) {
            // Delay creating the named destination until the tree walk is complete.
            use std::io::Seek;
            let mut spool = tempfile::tempfile()?;
            let total = generate(&mut spool)?;
            spool.rewind()?;
            let mut file = tempfile::NamedTempFile::new_in(dir)?;
            std::io::copy(&mut spool, &mut file)?;
            (file, total)
        } else {
            let mut file = tempfile::NamedTempFile::new_in(dir)?;
            let total = generate(file.as_file_mut())?;
            (file, total)
        };
    file.as_file().sync_all()?;
    let rebuilt = unpacked.path().join("verified");
    vantry_updater::apply_delta(
        fs::File::open(file.path())?,
        &opts.from,
        &opts.to,
        &opts.old,
        &rebuilt,
    )?;
    let bytes = file.as_file().metadata()?.len();
    file.persist(&opts.out).context("publish delta")?;
    println!(
        "{}: {bytes} bytes (vs {total} bytes uncompressed target)",
        opts.out.display()
    );
    Ok(())
}
fn main() -> Result<()> {
    // Go flags accepted both -name and --name. Normalize only known legacy flags.
    let args = std::env::args_os().map(|a| match a.to_str() {
        Some(s)
            if matches!(
                s.split('=').next(),
                Some("-source" | "-out" | "-old" | "-new" | "-from" | "-to")
            ) =>
        {
            format!("-{s}").into()
        }
        _ => a,
    });
    match Cli::parse_from(args).command {
        Commands::Package(o) => package::run(o),
        Commands::MakeDelta(o) => delta(o),
        Commands::PrepareIcon(o) => icon::run(o),
    }
}
