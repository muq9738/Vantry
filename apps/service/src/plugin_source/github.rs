//! Directory links identify one package within a GitHub repository. Fetch a
//! single revision so files from two pushes cannot end up in the same install.
use anyhow::{Context, Result, ensure};
use serde_json::Value;

pub struct GitHubDirectory {
    pub repo: String,
    pub revision: String,
    pub directory: String,
}
impl GitHubDirectory {
    pub fn parse(source: &str) -> Option<Self> {
        let url = reqwest::Url::parse(source.trim()).ok()?;
        if url.scheme() != "https"
            || url.host_str() != Some("github.com")
            || url.port().is_some()
            || !url.username().is_empty()
            || url.password().is_some()
        {
            return None;
        }
        let parts: Vec<_> = url.path().trim_matches('/').split('/').collect();
        let [owner, repo, "tree", revision, dirs @ ..] = parts.as_slice() else {
            return None;
        };
        if dirs.is_empty()
            || parts.iter().any(|p| {
                p.is_empty()
                    || matches!(*p, "." | "..")
                    || !p
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
            })
        {
            return None;
        }
        Some(Self {
            repo: format!("{owner}/{repo}"),
            revision: revision.to_string(),
            directory: dirs.join("/"),
        })
    }
    pub async fn commit(&self, client: &reqwest::Client) -> Result<String> {
        let meta: Value = client
            .get(format!(
                "https://api.github.com/repos/{}/commits/{}",
                self.repo, self.revision
            ))
            .timeout(std::time::Duration::from_secs(20))
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        let sha = meta["sha"].as_str().context("GitHub 未返回提交版本")?;
        ensure!(
            sha.len() == 40 && sha.bytes().all(|b| b.is_ascii_hexdigit()),
            "invalid GitHub commit"
        );
        Ok(sha.into())
    }
    pub fn raw(&self, commit: &str, file: &str) -> String {
        format!(
            "https://raw.githubusercontent.com/{}/{}/{}/{}",
            self.repo, commit, self.directory, file
        )
    }
}
