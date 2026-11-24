use super::*;
use crate::{
    bridge::workbench::history::{Revision, hunks},
    state::workbench::{GitDialog, GitPage},
};

pub(crate) fn git_dialog(dialog: Option<GitDialog>) {
    bench().update(|b| {
        b.git.dialog = dialog;
        b.git.changes_patch = None;
    });
    if dialog != Some(GitDialog::Changes) {
        return;
    }
    let Some((project, bridge)) = project_workbench() else {
        return;
    };
    spawn_for_window(async move {
        let result = async {
            let staged = call(bridge.git_diff(None, true)).await?;
            let working = call(bridge.git_diff(None, false)).await?;
            Ok::<_, BridgeError>(format!("{staged}{working}"))
        }
        .await
        .map_err(|e| e.message);
        bench().update(|b| {
            b.apply_project_result(project, |b| {
                if b.git.dialog == Some(GitDialog::Changes) {
                    b.git.changes_patch = Some(result);
                }
            })
        });
    });
}

pub(crate) fn git_page(page: GitPage, branch: Option<String>) {
    let Some((project, bridge)) = project_workbench() else {
        return;
    };
    let mut branch_name = String::new();
    let mut request = 0;
    bench().update(|b| {
        let g = &mut b.git;
        g.page = page;
        g.branch_menu = false;
        g.revision = None;
        g.revision_files.clear();
        g.revision_diff.clear();
        g.revision_diff_version += 1;
        g.commits.clear();
        g.error = None;
        g.history_request += 1;
        request = g.history_request;
        match page {
            GitPage::Compare => {
                g.compare_files = false;
                if let Some(branch) = branch {
                    g.compare = branch;
                }
                branch_name = g.compare.clone();
            }
            GitPage::Log => {
                if let Some(branch) = branch {
                    g.log_branch = branch;
                }
                branch_name = g.log_branch.clone();
            }
            GitPage::Commit => {}
        }
    });
    if page == GitPage::Commit {
        refresh_git();
        return;
    }
    let path = bench().with_untracked(|b| b.git.log_path.clone());
    spawn_for_window(async move {
        let result =
            call(bridge.git_log(branch_name.clone(), path, page == GitPage::Compare)).await;
        let mut compare = false;
        bench().update(|b| {
            b.apply_project_result(project, |b| {
                if b.git.history_request != request {
                    return;
                }
                match result {
                    Ok(rows) => {
                        b.git.commits = rows;
                        compare = page == GitPage::Compare;
                    }
                    Err(e) => b.git.error = Some(e.message),
                }
            })
        });
        if compare {
            let rev = if bench().with_untracked(|b| b.git.compare_reverse) {
                Revision::CompareReverse(branch_name)
            } else {
                Revision::Compare(branch_name)
            };
            git_revision(rev);
        }
    });
}

pub(crate) fn swap_compare_direction() {
    let mut rev = None;
    bench().update(|b| {
        b.git.compare_reverse = !b.git.compare_reverse;
        let br = b.git.compare.clone();
        rev = Some(if b.git.compare_reverse {
            Revision::CompareReverse(br)
        } else {
            Revision::Compare(br)
        });
    });
    if let Some(rev) = rev {
        git_revision(rev);
    }
}

pub(crate) fn git_sync_action(action: &'static str) {
    let Some(wb) = workbench() else { return };
    let root = bench().with_untracked(|b| b.root.clone());
    if root.is_empty() {
        return;
    }
    bench().update(|b| {
        b.git.busy = true;
        b.git.error = None;
    });
    spawn_for_window(async move {
        let mut req = actions::request(action);
        req.workspace = root;
        let res = call(wb.workbench(req)).await;
        bench().update(|b| {
            b.git.busy = false;
            match res {
                Ok(resp) => {
                    if let Some(state) = resp.git {
                        b.git.status = Some(state);
                        b.git.status_version += 1;
                    }
                    b.git.error = None;
                }
                Err(e) => b.git.error = Some(e.message),
            }
        });
        refresh_git();
    });
}

pub(crate) fn git_revision(revision: Revision) {
    let Some((project, bridge)) = project_workbench() else {
        return;
    };
    let mut request = 0;
    bench().update(|b| {
        let g = &mut b.git;
        g.history_request += 1;
        request = g.history_request;
        g.revision = Some(revision.clone());
        g.revision_files.clear();
        g.revision_diff.clear();
        g.revision_diff_version += 1;
        g.revision_path.clear();
    });
    spawn_for_window(async move {
        let result = call(bridge.revision_files(revision)).await;
        let mut first = None;
        bench().update(|b| {
            b.apply_project_result(project, |b| {
                if b.git.history_request != request {
                    return;
                }
                match result {
                    Ok(files) => {
                        first = files.first().cloned();
                        b.git.revision_files = files;
                    }
                    Err(e) => b.git.error = Some(e.message),
                }
            })
        });
        if let Some(path) = first {
            git_revision_file(path);
        }
    });
}

pub(crate) fn git_revision_file(path: String) {
    let Some((project, bridge)) = project_workbench() else {
        return;
    };
    let Some(revision) = bench().with_untracked(|b| b.git.revision.clone()) else {
        return;
    };
    let mut request = 0;
    bench().update(|b| {
        b.git.history_request += 1;
        request = b.git.history_request;
        b.git.revision_path = path.clone();
        b.git.revision_diff.clear();
        b.git.revision_diff_version += 1;
        b.git.hunk = 0;
    });
    spawn_for_window(async move {
        let result = call(bridge.revision_diff(revision, path)).await;
        bench().update(|b| {
            b.apply_project_result(project, |b| {
                if b.git.history_request == request {
                    match result {
                        Ok(text) => {
                            b.git.revision_diff = text;
                            b.git.revision_diff_version += 1;
                        }
                        Err(e) => b.git.error = Some(e.message),
                    }
                }
            })
        });
    });
}

pub(crate) fn stage_hunk() {
    let Some((project, bridge)) = project_workbench() else {
        return;
    };
    let mut operation = None;
    bench().update(|b| {
        let g = &mut b.git;
        let Some((_, staged)) = &g.selected else {
            return;
        };
        let (header, blocks) = hunks(&g.diff);
        if let Some(block) = blocks.get(g.hunk) {
            let patch = (format!("{header}{block}"), *staged);
            if g.begin() {
                operation = Some(patch);
            }
        }
    });
    let Some((patch, reverse)) = operation else {
        return;
    };
    spawn_for_window(async move {
        let result = call(bridge.git_apply_hunk(patch, reverse))
            .await
            .map(|_| None)
            .map_err(|e| e.message);
        let mut reload = None;
        bench().update(|b| {
            b.apply_project_result(project, |b| {
                b.git.hunk = 0;
                reload = Some(b.git.on_done(result));
            })
        });
        run_loads(reload.into_iter().collect());
    });
}
