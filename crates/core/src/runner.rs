use async_trait::async_trait;
use std::sync::Arc;
use vantry_protocol::{ControlResponse, ProviderEvent, ProviderRequest};
pub type Emit = Arc<dyn Fn(ProviderEvent) -> crate::Result<()> + Send + Sync>;
#[derive(Debug, thiserror::Error)]
pub enum ProviderError {
    #[error("observer detached")]
    Detached,
    #[error("execution cancelled")]
    ExecutionCancelled,
    #[error("delivery state unknown: {0}")]
    Interrupted(String),
    #[error("{0}")]
    Failed(String),
}
pub type ProviderResult = std::result::Result<(), ProviderError>;
/// Attach is explicit even when the first journal record has not been committed yet.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StartMode {
    Submit,
    Attach,
}
#[async_trait]
pub trait DurableProvider: Send + Sync + 'static {
    /// Dropping this future detaches the observer. Only `stop` cancels the worker.
    async fn run_events(
        &self,
        request: ProviderRequest,
        mode: StartMode,
        emit: Emit,
    ) -> ProviderResult;
    async fn recoverable(&self, request: &ProviderRequest) -> bool;
    async fn stop(&self, request: &ProviderRequest) -> ProviderResult;
    /// Publish a control idempotently by its stable ID. A control_result journal event confirms it.
    async fn control(&self, request: &ProviderRequest, control: &ControlResponse)
    -> ProviderResult;
}
