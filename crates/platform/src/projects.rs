//! Owner-private project registry, independent of the checkout lifetime.
use crate::{
    git::{self, Repository},
    hostfs,
};
use anyhow::{Context, Result, ensure};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
};
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Project {
    pub id: String,
    pub title: String,
    pub root_path: PathBuf,
    pub default_base_branch: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub setup_script: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub dev_command: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub custom_instructions: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub env: BTreeMap<String, String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}
#[derive(Default, Debug)]
pub struct Settings {
    pub title: Option<String>,
    pub default_base_branch: Option<String>,
    pub setup_script: Option<String>,
    pub dev_command: Option<String>,
    pub custom_instructions: Option<String>,
    pub env: Option<BTreeMap<String, String>>,
}
impl Settings {
    fn apply(self, p: &mut Project) -> Result<()> {
        for (value, field) in [
            (self.title, &mut p.title),
            (self.default_base_branch, &mut p.default_base_branch),
            (self.setup_script, &mut p.setup_script),
            (self.dev_command, &mut p.dev_command),
            (self.custom_instructions, &mut p.custom_instructions),
        ] {
            if let Some(value) = value {
                *field = value;
            }
        }
        if let Some(env) = self.env {
            for (k, v) in &env {
                ensure!(
                    !k.is_empty()
                        && k.bytes().enumerate().all(|(i, c)| c == b'_'
                            || c.is_ascii_alphabetic()
                            || (i > 0 && c.is_ascii_digit()))
                        && !v.contains('\0'),
                    "invalid project environment"
                );
            }
            p.env = env;
        }
        Ok(())
    }
}
pub struct Store {
    dir: PathBuf,
}
impl Store {
    pub fn open(data: &Path) -> Result<Self> {
        Ok(Self {
            dir: hostfs::private_root(data)?,
        })
    }
    pub fn list(&self) -> Result<Vec<Project>> {
        Ok(
            hostfs::read_json::<Option<Vec<Project>>>(&self.dir.join("projects.json"))?
                .flatten()
                .unwrap_or_default(),
        )
    }
    pub fn register(
        &self,
        path: &Path,
        id: Option<&str>,
        settings: Settings,
        init_git: bool,
    ) -> Result<Project> {
        ensure!(!path.as_os_str().is_empty(), "empty project path");
        let _guard = hostfs::lock_dir(&self.dir, true)?;
        let mut projects = self.list()?;
        if init_git {
            fs::create_dir_all(path)?;
        }
        let root = path.canonicalize()?;
        ensure!(root.is_dir(), "project must be directory");
        if let Some(p) = projects.iter().find(|p| p.root_path == root) {
            return Ok(p.clone());
        }
        ensure!(
            !projects.iter().any(|p| Some(p.id.as_str()) == id),
            "duplicate project id"
        );
        let now = Utc::now();
        let mut p = Project {
            id: id
                .filter(|s| !s.is_empty())
                .map(str::to_owned)
                .unwrap_or_else(|| format!("project_{}", uuid::Uuid::new_v4().simple())),
            title: root
                .file_name()
                .unwrap_or_default()
                .to_string_lossy()
                .into(),
            default_base_branch: String::new(),
            root_path: root.clone(),
            setup_script: String::new(),
            dev_command: String::new(),
            custom_instructions: String::new(),
            env: BTreeMap::new(),
            created_at: now,
            updated_at: now,
        };
        settings.apply(&mut p)?;
        if init_git && Repository::discover(&root).is_err() {
            ensure!(
                fs::read_dir(&root)?.next().is_none(),
                "git initialization requires an empty directory"
            );
            let branch = if p.default_base_branch.is_empty() {
                "main"
            } else {
                &p.default_base_branch
            };
            git::run(&root, &["check-ref-format", "--branch", branch])?;
            git::run(&root, &["init", &format!("--initial-branch={branch}")])?;
            fs::write(root.join("README.md"), format!("# {}\n", p.title))?;
            fs::write(
                root.join(".gitignore"),
                ".DS_Store\n.env\n.env.local\nnode_modules/\n",
            )?;
            git::run(&root, &["add", "--", "README.md", ".gitignore"])?;
            let mut args = vec![];
            for (key, fallback) in [
                ("user.name", "user.name=Vantry"),
                ("user.email", "user.email=vantry@localhost"),
            ] {
                if git::line(&root, &["config", "--get", key])
                    .unwrap_or_default()
                    .is_empty()
                {
                    args.extend(["-c", fallback]);
                }
            }
            args.extend(["commit", "-m", "Initial commit"]);
            git::run(&root, &args)?;
        }
        if p.default_base_branch.is_empty() {
            p.default_base_branch = git::line(
                &root,
                &["symbolic-ref", "--short", "refs/remotes/origin/HEAD"],
            )
            .or_else(|_| git::line(&root, &["symbolic-ref", "--short", "HEAD"]))
            .unwrap_or_else(|_| "main".into());
        }
        projects.push(p.clone());
        hostfs::write_json(&self.dir.join("projects.json"), &projects)?;
        Ok(p)
    }
    pub fn update(&self, id: &str, changes: Settings) -> Result<Project> {
        let _guard = hostfs::lock_dir(&self.dir, true)?;
        let mut projects = self.list()?;
        let p = projects
            .iter_mut()
            .find(|p| p.id == id)
            .context("project not found")?;
        changes.apply(p)?;
        p.updated_at = Utc::now();
        let out = p.clone();
        hostfs::write_json(&self.dir.join("projects.json"), &projects)?;
        Ok(out)
    }
    pub fn delete(&self, id: &str) -> Result<()> {
        let _guard = hostfs::lock_dir(&self.dir, true)?;
        let mut list = self.list()?;
        let i = list
            .iter()
            .position(|p| p.id == id)
            .context("project not found")?;
        list.remove(i);
        hostfs::write_json(&self.dir.join("projects.json"), &list)
    }
    pub fn find_by_path(&self, path: &Path) -> Result<Project> {
        let path = path.canonicalize()?;
        let list = self.list()?;
        if let Some(p) = list
            .iter()
            .filter(|p| path.starts_with(&p.root_path))
            .max_by_key(|p| p.root_path.components().count())
        {
            return Ok(p.clone());
        }
        let common = Repository::discover(&path)?.common_dir()?;
        list.into_iter()
            .find(|p| {
                Repository::discover(&p.root_path).is_ok_and(|repo| {
                    repo.root() == p.root_path && repo.common_dir().ok().as_ref() == Some(&common)
                })
            })
            .context("project not found")
    }
    pub fn recent(&self) -> Result<Vec<PathBuf>> {
        Ok(hostfs::read_json(&self.dir.join("recent-workspaces.json"))?.unwrap_or_default())
    }
    pub fn remember(&self, path: &Path) -> Result<()> {
        let path = path.canonicalize()?;
        ensure!(path.is_dir(), "workspace must be directory");
        let _guard = hostfs::lock_dir(&self.dir, true)?;
        let mut list = self.recent()?;
        list.retain(|p| *p != path);
        list.insert(0, path);
        hostfs::write_json(&self.dir.join("recent-workspaces.json"), &list)
    }
}
