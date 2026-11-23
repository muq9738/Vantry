use crate::worker::Journal;
use anyhow::{Result, anyhow, ensure};
use async_trait::async_trait;
use pi_coding_agent::extension_ui::{DialogOptions, ExtensionUi, NotifyType, WidgetPlacement};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
};
use tokio::sync::oneshot;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;
use vantry_protocol::ControlResponse;

struct Dialog {
    expires: Option<tokio::time::Instant>,
    cancel: CancellationToken,
    options: Option<Vec<String>>,
    reply: oneshot::Sender<ControlResponse>,
}
pub(crate) struct WorkerUi {
    journal: Arc<Journal>,
    dialogs: Mutex<HashMap<String, Dialog>>,
    stop: CancellationToken,
    ready: AtomicBool,
}
impl WorkerUi {
    pub(crate) fn new(journal: Arc<Journal>, stop: CancellationToken) -> Self {
        Self {
            journal,
            stop,
            dialogs: Mutex::default(),
            ready: AtomicBool::default(),
        }
    }
    pub(crate) fn approval_ready(&self) -> bool {
        self.ready.load(Ordering::Acquire)
    }
    pub(crate) async fn ask(
        &self,
        id: String,
        mut data: Value,
        opts: DialogOptions,
        options: Option<Vec<String>>,
    ) -> Option<ControlResponse> {
        data["type"] = json!("extension_ui_request");
        data["id"] = json!(id);
        if let Some(d) = opts.timeout {
            data["timeout"] = json!(d.as_millis());
        }
        let (reply, rx) = oneshot::channel();
        let cancel = opts.cancel.unwrap_or_default();
        let expires = opts
            .timeout
            .and_then(|duration| tokio::time::Instant::now().checked_add(duration));
        self.dialogs.lock().unwrap().insert(
            id.clone(),
            Dialog {
                reply,
                options,
                expires,
                cancel: cancel.clone(),
            },
        );
        self.journal.record(data);
        let timeout = async {
            match expires {
                Some(deadline) => tokio::time::sleep_until(deadline).await,
                None => std::future::pending().await,
            }
        };
        let outcome = tokio::select! {
            biased;
            r = rx => r.ok(),
            _ = self.stop.cancelled() => None,
            _ = cancel.cancelled() => None,
            _ = timeout => None,
        };
        self.dialogs.lock().unwrap().remove(&id);
        if outcome.is_none() {
            self.journal
                .record(json!({"type":"extension_ui_expired","id":id}));
        }
        outcome.filter(|c| !c.cancelled)
    }
    pub(crate) fn respond(&self, control: ControlResponse) -> Result<()> {
        let mut dialogs = self.dialogs.lock().unwrap();
        let dialog = dialogs
            .get(&control.request_id)
            .ok_or_else(|| anyhow!("dialog expired or unknown"))?;
        ensure!(
            !self.stop.is_cancelled()
                && !dialog.cancel.is_cancelled()
                && dialog
                    .expires
                    .is_none_or(|d| tokio::time::Instant::now() < d),
            "dialog expired or cancelled"
        );
        if let Some(options) = &dialog.options {
            ensure!(
                control.cancelled || options.contains(&control.value),
                "invalid selection"
            );
        }
        let dialog = dialogs.remove(&control.request_id).unwrap();
        dialog
            .reply
            .send(control)
            .map_err(|_| anyhow!("dialog closed"))
    }
}
#[async_trait]
impl ExtensionUi for WorkerUi {
    async fn confirm(&self, title: &str, message: &str, opts: DialogOptions) -> bool {
        self.ask(
            Uuid::new_v4().to_string(),
            json!({"method":"confirm","title":title,"message":message}),
            opts,
            None,
        )
        .await
        .is_some_and(|c| c.confirmed)
    }
    async fn select(&self, title: &str, options: &[String], opts: DialogOptions) -> Option<String> {
        self.ask(
            Uuid::new_v4().to_string(),
            json!({"method":"select","title":title,"options":options}),
            opts,
            Some(options.to_vec()),
        )
        .await
        .map(|c| c.value)
    }
    async fn input(
        &self,
        title: &str,
        placeholder: Option<&str>,
        opts: DialogOptions,
    ) -> Option<String> {
        self.ask(Uuid::new_v4().to_string(),json!({"method":"input","title":title,"placeholder":placeholder,"prefill":opts.prefill}), opts, None).await.map(|c| c.value)
    }
    async fn editor(
        &self,
        title: &str,
        prefill: Option<&str>,
        opts: DialogOptions,
    ) -> Option<String> {
        self.ask(
            Uuid::new_v4().to_string(),
            json!({"method":"editor","title":title,"prefill":prefill}),
            opts,
            None,
        )
        .await
        .map(|c| c.value)
    }
    fn notify(&self, message: &str, kind: Option<NotifyType>) {
        if message == "vantry_approval_ready" {
            self.ready.store(true, Ordering::Release);
        }
        self.journal.record(json!({"type":"extension_ui_request","method":"notify","message":message,"notifyType":kind.unwrap_or_default().as_str()}));
    }
    fn set_status(&self, _: &str, _: Option<&str>) {}
    fn set_widget(&self, _: &str, _: Option<&[String]>, _: Option<WidgetPlacement>) {}
    fn set_title(&self, _: &str) {}
    fn set_editor_text(&self, _: &str) {}
}
