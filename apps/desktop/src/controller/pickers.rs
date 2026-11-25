//! The composer's machine and project folder pickers (the `本机 · 目录`
//! strip under the card), as MyGo's: a machine menu, and T3 Code's folder
//! palette over the directories of the machine the draft runs on.
use super::*;

/// One folder of a listing.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct Folder {
    pub name: String,
    pub path: String,
}

/// A directory and its folders, hidden ones included (the palette shows
/// them when the filter starts with `.`).
#[derive(Debug, Clone, PartialEq, Default)]
pub(crate) struct Listing {
    pub path: String,
    pub parent: String,
    pub folders: Vec<Folder>,
}

pub(crate) fn open_machine_picker() {
    ui().update(|u| {
        u.model_picker = false;
        u.folder_picker = false;
        u.machine_picker = true;
    });
}

pub(crate) fn close_machine_picker() {
    ui().update(|u| u.machine_picker = false);
}

pub(crate) fn open_folder_picker() {
    ui().update(|u| {
        u.model_picker = false;
        u.machine_picker = false;
        u.folder_picker = true;
    });
}

pub(crate) fn close_folder_picker() {
    ui().update(|u| u.folder_picker = false);
}

/// Whether the shown thread is a sent conversation (its machine and folder
/// are fixed: a choice starts a new draft).
pub(crate) fn conversation_fixed() -> bool {
    state().with(|s| s.selected_thread().is_some())
}

/// The folder the palette opens in: the parent of the current project, else home.
pub(crate) fn folder_picker_start() -> String {
    state().with_untracked(|s| {
        s.current_workspace()
            .map(|w| if w.repository.is_empty() { w.path.clone() } else { w.repository.clone() })
            .filter(|p| !p.trim().is_empty())
            .map(|p| {
                let p = p.trim_end_matches('/').to_owned();
                std::path::Path::new(&p)
                    .parent()
                    .map(|d| d.to_string_lossy().into_owned())
                    .unwrap_or(p)
            })
            .unwrap_or_default()
    })
}

/// Whether the folders are this computer's (read here) or a paired machine's
/// (read through its Core).
pub(crate) fn browsing_local() -> bool {
    current_machine().is_none()
}

/// The home directory of this computer.
pub(crate) fn local_home() -> String {
    std::env::var("HOME")
        .or_else(|_| std::env::var("USERPROFILE"))
        .map(|h| slashed(std::path::Path::new(&h)))
        .unwrap_or_default()
}

/// A path as the palette writes it: `/` between its parts on Windows too
/// (which takes either).
fn slashed(path: &std::path::Path) -> String {
    let text = path.to_string_lossy();
    if cfg!(windows) { text.replace('\\', "/") } else { text.into_owned() }
}

fn read_local(dir: &str) -> Result<Listing, String> {
    let dir = if dir.trim().is_empty() { local_home() } else { dir.to_owned() };
    let path = std::path::Path::new(&dir);
    if !path.is_absolute() {
        return Err("需要绝对路径".into());
    }
    let mut folders = Vec::new();
    for entry in std::fs::read_dir(path).map_err(|e| e.to_string())? {
        let Ok(entry) = entry else { continue };
        let entry_path = entry.path();
        // Links to folders count as folders.
        if entry_path.is_dir() {
            folders.push(Folder {
                name: entry.file_name().to_string_lossy().into_owned(),
                path: slashed(&entry_path),
            });
        }
    }
    folders.sort_by_key(|f| f.name.to_lowercase());
    Ok(Listing {
        path: slashed(path),
        parent: path.parent().map(slashed).unwrap_or_default(),
        folders,
    })
}

/// Lists `dir` (home when empty) on the machine the draft runs on.
pub(crate) async fn list_folders(dir: String) -> Result<Listing, String> {
    if browsing_local() {
        return background(async move { read_local(&dir) })
            .await
            .map_err(|e| e.to_string())?;
    }
    let Some(bridge) = workbench() else {
        return Err("未连接 Core".into());
    };
    let mut request = actions::request("environment.directories");
    request.workspace = dir;
    request.input = Some(serde_json::json!({ "hidden": true }));
    let response = call(bridge.workbench(request))
        .await
        .map_err(|e| e.message)?;
    let listing: vantry_protocol::EnvironmentDirectories =
        serde_json::from_str(&response.output).map_err(|_| "目标机器没有返回目录列表".to_owned())?;
    let mut folders: Vec<Folder> = listing
        .entries
        .unwrap_or_default()
        .into_iter()
        .map(|e| Folder { name: e.name, path: e.path })
        .collect();
    folders.sort_by_key(|f| f.name.to_lowercase());
    Ok(Listing { path: listing.path, parent: listing.parent, folders })
}

/// Makes `path` the project of the draft. A sent conversation keeps its
/// folder: a new draft starts in `path` and takes the unsent text along.
pub(crate) fn use_folder(path: String) {
    let path = path.trim().trim_end_matches('/').to_owned();
    let path = if path.is_empty() { "/".to_owned() } else { path };
    let spec = vantry_protocol::WorkspaceSpec {
        kind: "directory".into(),
        path,
        ..Default::default()
    };
    close_folder_picker();
    if state().with_untracked(|s| s.selected_thread().is_some()) {
        let mut carried = None;
        state().update(|s| {
            let key = s.draft_key();
            carried = s.drafts.remove(&key).filter(|d| !d.text.trim().is_empty() || !d.attachments.is_empty());
        });
        start_in_workspace(Some(spec));
        if let Some(draft) = carried {
            state().update(|s| {
                let key = s.draft_key();
                s.drafts.insert(key, draft);
            });
        }
    } else {
        state().update(|s| s.new_thread.workspace = Some(spec));
    }
}

/// Picks the folder with the system's dialog (this computer only).
pub(crate) fn pick_folder_natively() {
    close_folder_picker();
    spawn_for_window(async {
        match pick_paths(nsh_core::file_dialog::OpenDialog::folders().title("选择项目目录")).await {
            Ok(Some(paths)) => {
                if let Some(path) = paths.first() {
                    use_folder(path.to_string_lossy().into_owned());
                }
            }
            Ok(None) => {}
            Err(error) => warn(format!("无法打开目录选择器：{error}")),
        }
    });
}

/// Runs the draft on machine `id`: a new draft there, with the unsent text
/// of a draft (not of a sent conversation, which stays where it is).
pub(crate) fn use_machine(id: String) {
    close_machine_picker();
    if machines::navigation().with_untracked(|n| n.active == id) {
        return;
    }
    let text = (!conversation_fixed_untracked())
        .then(|| state().with_untracked(|s| s.draft()))
        .filter(|d| d.attachments.is_empty() && !d.text.trim().is_empty())
        .map(|d| d.text);
    machines::activate(&id, move || {
        new_thread();
        if let Some(text) = text {
            set_draft(&text);
        }
    });
}

fn conversation_fixed_untracked() -> bool {
    state().with_untracked(|s| s.selected_thread().is_some())
}
