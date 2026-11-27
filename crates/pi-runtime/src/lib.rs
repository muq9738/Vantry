//! Embedded Pi worker and its on-disk handoff contract.
mod attachments;
mod catalog;
mod controlled;
mod instructions;
mod secret_tool;
mod todo_tool;
pub mod mcp;
mod pdf;
mod sandbox;
mod session;
mod title;
pub use title::summarize_title;
pub use attachments::attachment_payload;
pub use catalog::{
    MagpieSelection, ResolvedModelCatalog, SkillInfo, discover_skills, model_catalog,
    resolve_model_catalog, resolve_with_plugins, split_model_thinking,
};
pub use controlled::{ControlledStep, ControlledTestProvider};
pub use sandbox::WorkspaceSandbox;
pub use session::{clone_session_file, truncate_session_file};
mod config;
mod control;
pub use control::{deliver_control, request_stop};
mod disk;
mod events;
mod gemini_web;
pub use events::{IncrementalJournalReader, JobEvents, JournalCursor};
mod ui;
mod worker;
mod workspace;
pub use config::{PiConfig, agent_dir, read_config, save_setup, set_managed_provider};
pub use worker::{JobRequest, prepare_job, run_worker};

pub fn embedded_pi_version() -> &'static str {
    pi_coding_agent::platform::http::VERSION
}
