//! Asynchronous HTTP/1.1 client for the Vantry Core Unix socket.
pub use vantry_protocol as protocol;
#[cfg(unix)]
mod local;
#[cfg(unix)]
pub use local::*;
