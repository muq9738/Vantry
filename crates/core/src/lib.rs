//! Transactional SQLite orchestration for the local Vantry service.
mod attachments;
mod commands;
pub mod engine;
pub mod lock;
pub mod runner;
pub mod scheduler;
pub mod server;
mod session;
pub mod store;
mod tasks;
pub mod telemetry;
pub use engine::{Core, CoreOptions};
pub use runner::{DurableProvider, ProviderError};
pub const SCHEMA_USER_VERSION: u32 = 11;
use vantry_protocol::ErrorCode;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("{1}")]
    Api(ErrorCode, String),
    #[error(transparent)]
    Sql(#[from] rusqlite::Error),
    #[error(transparent)]
    Io(#[from] std::io::Error),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}
impl Error {
    pub fn code(&self) -> ErrorCode {
        match self {
            Self::Api(code, _) => *code,
            Self::Sql(rusqlite::Error::QueryReturnedNoRows) => ErrorCode::NotFound,
            Self::Sql(rusqlite::Error::SqliteFailure(e, _))
                if e.code == rusqlite::ErrorCode::ConstraintViolation =>
            {
                ErrorCode::Conflict
            }
            _ => ErrorCode::InternalError,
        }
    }
}
pub type Result<T> = std::result::Result<T, Error>;
fn invalid(reason: &str) -> Error {
    Error::Api(ErrorCode::InvalidCommand, reason.into())
}
fn conflict(reason: &str) -> Error {
    Error::Api(ErrorCode::Conflict, reason.into())
}
fn now() -> i64 {
    chrono::Utc::now()
        .timestamp_nanos_opt()
        .expect("current clock outside SQLite nanosecond range")
}
fn timestamp(ns: i64) -> String {
    go_timestamp(chrono::DateTime::from_timestamp_nanos(ns).fixed_offset())
}
fn go_timestamp(time: chrono::DateTime<chrono::FixedOffset>) -> String {
    // time.RFC3339Nano trims each trailing fractional zero, independently of precision.
    let fraction = format!("{:09}", time.timestamp_subsec_nanos());
    let fraction = fraction.trim_end_matches('0');
    let mut value = time.format("%Y-%m-%dT%H:%M:%S").to_string();
    if !fraction.is_empty() {
        value.push('.');
        value.push_str(fraction);
    }
    if time.offset().local_minus_utc() == 0 {
        value.push('Z');
    } else {
        value.push_str(&time.format("%:z").to_string());
    }
    value
}
fn id(prefix: &str) -> String {
    format!("{prefix}_{}", uuid::Uuid::new_v4().simple())
}
fn first<'a>(s: &'a str, default: &'a str) -> &'a str {
    if s.is_empty() { default } else { s }
}
