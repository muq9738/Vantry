use super::{actions, call, local_workbench, spawn_for_window, ui};
use crate::state::ui::{AlmaPairingForm, AlmaShareStatus, Secret};
use nsh_core::prelude::timer;
use std::sync::atomic::{AtomicU64, Ordering};

pub(crate) fn open(thread_id: String, title: String) {
    static NEXT: AtomicU64 = AtomicU64::new(1);
    let dialog_id = NEXT.fetch_add(1, Ordering::Relaxed);
    ui().update(|u| {
        u.forms.alma = Some(AlmaPairingForm {
            dialog_id,
            thread_id,
            title,
            ..Default::default()
        });
    });
    request("remote.pair.alma.show");
    spawn_for_window(async move {
        loop {
            timer(std::time::Duration::from_secs(3)).await;
            let visible = ui().with_untracked(|u| {
                u.forms
                    .alma
                    .as_ref()
                    .filter(|f| f.dialog_id == dialog_id)
                    .map(|f| {
                        !f.busy && !f.confirm_stop && f.status.as_ref().is_some_and(|s| s.enabled)
                    })
            });
            match visible {
                None => break,
                Some(true) => dispatch("remote.pair.alma.show", true),
                Some(false) => (),
            }
        }
    });
}

pub(crate) fn close() {
    ui().update(|u| u.forms.alma = None);
}

pub(crate) fn edit(f: impl FnOnce(&mut AlmaPairingForm)) {
    ui().update(|u| {
        if let Some(form) = &mut u.forms.alma {
            f(form);
        }
    });
}

pub(crate) fn request(action: &'static str) {
    dispatch(action, false);
}

fn dispatch(action: &'static str, background: bool) {
    let Some(form) = ui().with_untracked(|u| u.forms.alma.clone()) else {
        return;
    };
    if form.busy || (background && form.refreshing) {
        return;
    }
    let Some((workbench, false)) = local_workbench() else {
        edit(|f| f.error = "请连接本机 Vantry Core 后再配对".into());
        return;
    };
    let mut req = actions::request(action);
    req.thread_id = form.thread_id;
    if action == "remote.pair.alma.create" && form.advanced {
        req.input = Some(
            serde_json::json!({"relay":form.relay.trim(),"registrationKey":form.key.expose()}),
        );
    } else if action == "remote.pair.alma.revoke" {
        let Some(status) = &form.status else { return };
        req.name = status.share_id.clone();
    }
    edit(|f| {
        if background {
            f.refreshing = true;
        } else {
            f.busy = true;
            f.error.clear();
        }
    });
    spawn_for_window(async move {
        let result = async {
            let response = call(workbench.workbench(req))
                .await
                .map_err(|e| e.message)?;
            let value = serde_json::from_str::<serde_json::Value>(&response.output)
                .map_err(|_| "Core 返回了无效的配对状态".to_owned())?;
            let link = Secret::new(value["link"].as_str().unwrap_or_default());
            let status = serde_json::from_value::<AlmaShareStatus>(value)
                .map_err(|_| "Core 返回了无效的配对状态".to_owned())?;
            Ok::<_, String>((status, link))
        }
        .await;
        edit(|f| {
            if f.dialog_id != form.dialog_id {
                return;
            }
            if background {
                f.refreshing = false;
                if f.busy || f.confirm_stop || f.status != form.status {
                    return;
                }
            } else {
                f.busy = false;
                f.confirm_stop = false;
            }
            match result {
                Ok((status, link)) => {
                    f.link = link;
                    f.status = Some(status);
                    f.error.clear();
                    if action == "remote.pair.alma.create" {
                        f.key.clear();
                    }
                }
                Err(error) => f.error = error,
            }
        });
    });
}
