//! Transport and device authorization shared by the headless relay and Core.
pub mod acme;
pub mod bootstrap;
pub mod desktop;
pub mod desktop_client;
pub mod desktop_media;
pub mod identity;
pub mod manager;
pub mod pairing;
pub mod peer;
pub mod relay;
pub mod ssh;
pub mod t3compat;
pub mod transport;
mod turn;
mod web;
pub use web::{TlsFiles, serve, serve_acme};
