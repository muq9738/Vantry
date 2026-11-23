//! In-process Pi coding-agent runtime adapter, ExtensionUi bridge, configuration management,
//! controlled test provider, and incremental durable job journals.

pub fn embedded_pi_version() -> &'static str {
    env!("CARGO_PKG_VERSION")
}
