//! Embedded Pi worker and its on-disk handoff contract.
mod config;
mod control;
pub use control::{deliver_control, request_stop};
mod disk;
mod events;
pub use events::JobEvents;
mod ui;
mod worker;
pub use config::{PiConfig, agent_dir, read_config, save_setup};
pub use worker::{JobRequest, prepare_job, run_worker};

pub fn embedded_pi_version() -> &'static str {
    pi_coding_agent::platform::http::VERSION
}
