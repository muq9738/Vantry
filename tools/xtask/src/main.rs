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
    /// Create and verify cross-version v1 binary deltas from multiple base versions in parallel.
    MakeDeltas(MultiDelta),
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
#[derive(Debug, Args)]
struct MultiDelta {
    /// Target release version (e.g. 0.2.24).
    #[arg(long)]
    to: String,
    /// Path to target release archive (.zip/.tar.gz) or unpacked bundle directory.
    #[arg(long)]
    new: PathBuf,
    /// Release platform suffix (e.g. darwin-arm64, windows-amd64).
    #[arg(long)]
    platform: String,
    /// Asset slug prefix (default: vantry).
    #[arg(long, default_value = "vantry")]
    slug: String,
    /// Output directory where `<slug>-<from>-to-<to>-<platform>.delta` files are written.
    #[arg(long)]
    out_dir: PathBuf,
    /// Base version and archive/bundle path in `<version>=<path>` form (repeatable).
    #[arg(long = "base", required = true)]
    bases: Vec<String>,
}
fn parent(path: &Path) -> &Path {
    path.parent()
        .filter(|p| !p.as_os_str().is_empty())
        .unwrap_or(Path::new("."))
}
fn generate_verified_delta(
    from: &str,
    to: &str,
    old_dir: &Path,
    new_dir: &Path,
    out_path: &Path,
    full_archive_bytes: Option<u64>,
) -> Result<Option<(u64, u64)>> {
    let dir = parent(out_path);
    fs::create_dir_all(dir)?;
    let dir = dir.canonicalize()?;
    let generate = |out: &mut fs::File| {
        vantry_updater::write_delta(out, from, to, old_dir, new_dir)
    };
    let (file, total) = if dir.starts_with(old_dir.canonicalize()?)
        || dir.starts_with(new_dir.canonicalize()?)
    {
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
    let bytes = file.as_file().metadata()?.len();
    if let Some(full) = full_archive_bytes
        && !vantry_updater::delta_worth_using(bytes, full, 1)
    {
        return Ok(None);
    }
    let verify_dir = tempfile::tempdir()?;
    let rebuilt = verify_dir.path().join("verified");
    vantry_updater::apply_delta(fs::File::open(file.path())?, from, to, old_dir, &rebuilt)?;
    file.persist(out_path).context("publish delta")?;
    Ok(Some((bytes, total)))
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
    let Some((bytes, total)) =
        generate_verified_delta(&opts.from, &opts.to, &opts.old, &opts.new, &opts.out, None)?
    else {
        anyhow::bail!("delta skipped unexpectedly");
    };
    println!(
        "{}: {bytes} bytes (vs {total} bytes uncompressed target)",
        opts.out.display()
    );
    Ok(())
}

fn deltas(opts: MultiDelta) -> Result<()> {
    let to = vantry_updater::normalize_version(&opts.to).to_owned();
    anyhow::ensure!(!to.is_empty(), "missing target version");
    let full_archive_bytes = opts
        .new
        .is_file()
        .then(|| opts.new.metadata().map(|m| m.len()))
        .transpose()?;
    let unpacked_new = tempfile::tempdir()?;
    let new_dir = if opts.new.is_file() {
        let root = unpacked_new.path().join("new");
        fs::create_dir(&root)?;
        vantry_updater::unpack_release(&opts.new, &root)?
    } else {
        opts.new.clone()
    };
    fs::create_dir_all(&opts.out_dir)?;

    let mut parsed_bases = Vec::with_capacity(opts.bases.len());
    for raw in &opts.bases {
        let (from, path) = raw
            .split_once('=')
            .with_context(|| format!("--base must be <version>=<path>, got {raw:?}"))?;
        let from = vantry_updater::normalize_version(from).to_owned();
        anyhow::ensure!(!from.is_empty(), "empty base version in {raw:?}");
        parsed_bases.push((from, PathBuf::from(path)));
    }

    let results: Vec<Result<String>> = std::thread::scope(|scope| {
        let handles: Vec<_> = parsed_bases
            .into_iter()
            .map(|(from, old_path)| {
                let to = &to;
                let new_dir = &new_dir;
                let platform = &opts.platform;
                let slug = &opts.slug;
                let out_dir = &opts.out_dir;
                scope.spawn(move || -> Result<String> {
                    let unpacked_old = tempfile::tempdir()?;
                    let old_dir = if old_path.is_file() {
                        let root = unpacked_old.path().join("old");
                        fs::create_dir(&root)?;
                        vantry_updater::unpack_release(&old_path, &root)?
                    } else {
                        old_path
                    };
                    let out_path = out_dir.join(format!("{slug}-{from}-to-{to}-{platform}.delta"));
                    match generate_verified_delta(
                        &from,
                        to,
                        &old_dir,
                        new_dir,
                        &out_path,
                        full_archive_bytes,
                    )? {
                        Some((bytes, total)) => {
                            let ratio = full_archive_bytes
                                .filter(|&f| f > 0)
                                .map(|f| format!(" ({:.1}% of full archive)", (bytes as f64) * 100.0 / (f as f64)))
                                .unwrap_or_default();
                            Ok(format!(
                                "{}: {bytes} bytes{ratio} (vs {total} bytes uncompressed)",
                                out_path.display()
                            ))
                        }
                        None => Ok(format!(
                            "skipped {from} -> {to}: delta exceeded worthwhile savings threshold"
                        )),
                    }
                })
            })
            .collect();
        handles
            .into_iter()
            .map(|h| h.join().map_err(|_| anyhow::anyhow!("delta worker panicked"))?)
            .collect()
    });

    for line in results {
        println!("{}", line?);
    }
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
        Commands::MakeDeltas(o) => deltas(o),
        Commands::PrepareIcon(o) => icon::run(o),
    }
}
