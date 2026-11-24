//! Vantry release discovery, v1 binary deltas and offline installation.
mod archive;
mod delta;
mod inflate;
mod install;
mod manager;
mod offline;
mod patch;
mod release;
mod token;
mod tree;
pub use delta::{apply_delta, write_delta};
pub use install::{InstallPlan, UpdateLock};
pub use manager::*;
pub use offline::{ApplyOptions, apply_staged, launch_apply, run_apply};
pub use release::*;
pub use token::{
    bundled_token_file, read_update_credentials, resolve_current_app_bundle, update_token_file,
};
const MAX_ASSET: u64 = 1 << 30;
const MAX_FILE: u64 = 1 << 30;
const MAX_TREE: u64 = 2 << 30;
