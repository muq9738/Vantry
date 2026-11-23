//! Vantry Rust Core: state machine, SQLite persistence (user_version=11), single-transaction commands,
//! durable parent/child task scheduling, outbox, and crash recovery.

pub const SCHEMA_USER_VERSION: u32 = 11;
