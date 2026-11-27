//! Approval surfaces laid out headless (nsh's approximate text engine) at a
//! narrow, a medium and a wide width: nothing may run past the column, the
//! answers wrap instead. With `APPROVAL_PREVIEW_DIR` set, each layout is
//! also written as `<name>-<width>.json` (frames, fills, texts) for
//! `scripts`-side wireframe rendering.

use std::{cell::RefCell, collections::HashMap};

use nsh_core::{
    ApproxTextEngine, Core, MountItem, NodeId,
    element::{Environment, Root},
    platform::{Idiom, Os},
    prelude::*,
};
use serde_json::json;
use vantry_protocol::{PendingRequest, Run, RunStatus, Snapshot, Thread, ToolCall};

use crate::{
    controller,
    state::{AppState, TimelineItem, approval_view},
};

thread_local! {
    static CASE: RefCell<Option<(f32, Case)>> = const { RefCell::new(None) };
}

#[derive(Clone, Copy, Debug)]
enum Case {
    Pending,
    Submitting,
    Settled,
    Form,
    Background,
}

fn request(id: &str, title: &str, method: &str) -> PendingRequest {
    PendingRequest {
        id: id.into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        method: method.into(),
        title: title.into(),
        options: if method == "select" {
            vec![
                "允许本次".into(),
                "本会话允许 rg ls find grep sed".into(),
                "拒绝".into(),
            ]
        } else {
            vec![]
        },
        status: "pending".into(),
        created_at: "2026-11-23T00:00:01Z".into(),
        ..PendingRequest::default()
    }
}

fn state_for(case: Case) -> (AppState, PendingRequest) {
    let mut state = AppState::default();
    let long = format!(
        "{}\nfind . -name '*.py' | xargs grep -n qmt | head -200 && echo done",
        "ls -la; rg -i \"qmt\" --glob '!node_modules' --glob '!target' ."
    );
    let bash = request(
        "q1",
        &format!(
            "允许执行 bash？\n{}",
            json!({"command": long, "timeout": 120, "description": "查找 qmt 相关文件"})
        ),
        "select",
    );
    let req = match case {
        Case::Form => request("q2", "请输入数据库密码", "input"),
        _ => bash,
    };
    let mut threads = vec![Thread {
        id: "t1".into(),
        title: "查找 qmt 相关文件".into(),
        ..Thread::default()
    }];
    if matches!(case, Case::Background) {
        threads.push(Thread {
            id: "t2".into(),
            title: "后台任务：清理 2026 年全部构建产物与缓存目录并重新生成索引".into(),
            ..Thread::default()
        });
        threads.push(Thread {
            id: "t3".into(),
            title: "另一个会话".into(),
            ..Thread::default()
        });
    }
    state.apply_snapshot(Snapshot {
        sequence: 1,
        threads: Some(threads),
        runs: Some(vec![Run {
            id: "r1".into(),
            thread_id: "t1".into(),
            status: RunStatus::Running,
            ..Run::default()
        }]),
        tools: vec![ToolCall::default()]
            .into_iter()
            .filter(|_| false)
            .collect(),
        requests: match case {
            Case::Background => vec![
                PendingRequest {
                    thread_id: "t2".into(),
                    ..req.clone()
                },
                PendingRequest {
                    id: "q9".into(),
                    thread_id: "t2".into(),
                    ..req.clone()
                },
                PendingRequest {
                    id: "q8".into(),
                    thread_id: "t3".into(),
                    ..req.clone()
                },
            ],
            _ => vec![req.clone()],
        },
        ..Snapshot::default()
    });
    state.select_thread(Some("t1".into()));
    match case {
        Case::Submitting => {
            let _ = state.respond("q1", &crate::state::ApprovalDecision::Allow);
        }
        Case::Settled => {
            let command = state
                .respond("q1", &crate::state::ApprovalDecision::Allow)
                .unwrap();
            state.on_command_result(
                &command.command_id,
                Err(crate::bridge::BridgeError::new(
                    crate::bridge::ErrorCode::Conflict,
                    "request is no longer pending",
                )),
            );
        }
        _ => {}
    }
    let shown = state
        .requests
        .iter()
        .find(|r| r.id == req.id)
        .cloned()
        .unwrap_or(req);
    (state, shown)
}

#[component]
fn Preview() -> impl IntoElement {
    let (width, case) = CASE.with(|c| c.borrow().unwrap());
    let (state, request) = state_for(case);
    controller::state().set(state);
    let item = computed(move || {
        Some(TimelineItem::Approval {
            id: "row".into(),
            view: approval_view(&request),
        })
    });
    let attention = computed(|| controller::state().with(|s| s.background_attention()));
    view()
        .w(px(width))
        .flex_col()
        .gap_2()
        .p_3()
        .bg(crate::theme::palette().canvas)
        .child(when(
            move || attention.with(|a| !a.is_empty()),
            move || super::composer::attention_line(attention),
        ))
        .child(when(
            move || !matches!(case, Case::Background),
            move || super::timeline::approval_row(item),
        ))
}

#[derive(Default)]
struct Node {
    parent: Option<NodeId>,
    frame: (f32, f32, f32, f32),
    fill: u32,
    border: u32,
    text: Option<String>,
}

fn layout(width: f32, case: Case) -> (Vec<serde_json::Value>, f32) {
    CASE.with(|c| *c.borrow_mut() = Some((width, case)));
    let mut ui = Core::new(ApproxTextEngine, Size::new(px(width), px(1200.0)));
    ui.set_environment(Environment {
        os: Os::MacOs,
        idiom: Idiom::Desktop,
        ..Default::default()
    });
    ui.mount_root(&Root::of::<Preview>());
    let mut nodes: HashMap<NodeId, Node> = HashMap::new();
    let mut order = Vec::new();
    for _ in 0..4 {
        let Some(batch) = ui.commit() else { break };
        for item in batch.items {
            match item {
                MountItem::Create { id, .. } => {
                    order.push(id);
                    nodes.entry(id).or_default();
                }
                MountItem::Insert { parent, child, .. } => {
                    nodes.entry(child).or_default().parent = Some(parent)
                }
                MountItem::Remove { id } | MountItem::Delete { id } => {
                    if let Some(n) = nodes.get_mut(&id) {
                        n.parent = None;
                    }
                }
                MountItem::Frame { id, frame } => {
                    nodes.entry(id).or_default().frame = (
                        frame.origin.x.0,
                        frame.origin.y.0,
                        frame.size.width.0,
                        frame.size.height.0,
                    )
                }
                MountItem::ViewStyle { id, style } => {
                    let n = nodes.entry(id).or_default();
                    n.fill = style.background_color.0;
                    n.border = if style.borders.widths.top.0 > 0.0 {
                        style.borders.colors[0].0
                    } else {
                        0
                    };
                }
                MountItem::Text { id, text, .. } => {
                    nodes.entry(id).or_default().text = Some(text.text.clone())
                }
                _ => {}
            }
        }
    }
    let abs = |id: NodeId| {
        let (mut x, mut y) = (0.0, 0.0);
        let mut cur = Some(id);
        let mut attached = false;
        while let Some(c) = cur {
            let Some(n) = nodes.get(&c) else {
                attached = true; // the window's own root
                break;
            };
            x += n.frame.0;
            y += n.frame.1;
            cur = n.parent;
            if cur.is_none() {
                attached = c == *order.first().unwrap();
            }
        }
        attached.then_some((x, y))
    };
    let mut out = Vec::new();
    let mut overflow: f32 = 0.0;
    for id in &order {
        let Some((x, y)) = abs(*id) else { continue };
        let n = &nodes[id];
        overflow = overflow.max(x + n.frame.2 - width).max(-x);
        out.push(json!({
            "x": x, "y": y, "w": n.frame.2, "h": n.frame.3,
            "fill": format!("{:08x}", n.fill), "border": format!("{:08x}", n.border),
            "text": n.text,
        }));
    }
    (out, overflow)
}

#[test]
fn approval_surfaces_fit_narrow_and_wide_columns() {
    let dir = std::env::var_os("APPROVAL_PREVIEW_DIR");
    for case in [
        Case::Pending,
        Case::Submitting,
        Case::Settled,
        Case::Form,
        Case::Background,
    ] {
        for width in [280.0, 340.0, 520.0, 820.0] {
            let (nodes, overflow) = layout(width, case);
            assert!(
                nodes.iter().any(|n| n["text"].is_string()),
                "{case:?} rendered text"
            );
            assert!(
                overflow <= 0.5,
                "{case:?} at {width}: runs {overflow}pt outside the column"
            );
            if let Some(dir) = &dir {
                let path = std::path::Path::new(dir).join(format!("{case:?}-{width}.json"));
                std::fs::write(path, serde_json::to_string(&nodes).unwrap()).unwrap();
            }
        }
    }
}
