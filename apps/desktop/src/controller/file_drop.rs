//! File-manager drops offer URLs. A drag out of a browser can offer bytes instead.
use std::{collections::HashSet, io::Read};

use super::*;
use crate::bridge::workbench::MAX_ATTACHMENT_BYTES;

fn classify(event: &FileDropEvent) -> (Vec<(PathBuf, String)>, Vec<String>) {
    let mut seen = HashSet::new();
    let mut images = Vec::new();
    let mut files = Vec::new();
    for (index, uri) in event.uris.iter().enumerate() {
        let Some(path) = local_file_path(uri) else {
            continue;
        };
        if !seen.insert(path.clone()) {
            continue;
        }
        let mime = event
            .mime_types
            .get(index)
            .filter(|mime| !mime.is_empty())
            .map(String::as_str)
            .unwrap_or_else(|| nsh_core::platform::file_mime_type(uri));
        if mime.starts_with("image/") {
            images.push((path, mime.to_owned()));
        } else {
            files.push(uri.clone());
        }
    }
    (images, files)
}

pub(crate) fn drop_files(event: &FileDropEvent) {
    let Some(bridge) = workbench() else { return };
    let key = state().with_untracked(|s| s.draft_key());
    let (paths, uris) = classify(event);
    paste_file_uris(&uris);
    if paths.is_empty() && event.files.is_empty() {
        return;
    }
    let contents = event.files.clone();
    state().update(|s| s.begin_attachment_import(&key));
    spawn_for_window(async move {
        let mut images = Vec::new();
        for (path, mime) in paths {
            let name = path
                .file_name()
                .unwrap_or_default()
                .to_string_lossy()
                .into_owned();
            let result = tokio::task::spawn_blocking(move || {
                if !path.is_file() {
                    return Err(std::io::Error::other("只能附加普通文件"));
                }
                let file = std::fs::File::open(path)?;
                let mut bytes = Vec::new();
                file.take((8 << 20) + 1).read_to_end(&mut bytes)?;
                Ok::<_, std::io::Error>(bytes)
            })
            .await;
            match result {
                Ok(Ok(bytes)) => images.push(PastedFile::new(name, mime, bytes)),
                Ok(Err(err)) => warn(format!("无法读取 {name}：{err}")),
                Err(err) => warn(format!("无法读取 {name}：{err}")),
            }
        }
        for file in contents {
            if file.status != nsh_core::picker::PickedStatus::Read
                || file.len > MAX_ATTACHMENT_BYTES
            {
                warn(format!("附件过大：{}", file.name));
            } else if file.mime.starts_with("image/") {
                images.push(PastedFile::new(
                    file.name,
                    file.mime,
                    file.data.bytes().to_vec(),
                ));
            } else {
                import_bytes(&key, &bridge, file.name, file.data.bytes()).await;
            }
        }
        paste_images_for(&key, &bridge, &images);
        state().update(|s| s.finish_attachment_import(&key));
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn drop_classifies_images_and_deduplicates_decoded_local_urls() {
        let root = std::env::temp_dir();
        let image = root.join("diagram 1.PNG");
        let image_uri = url::Url::from_file_path(&image).unwrap().to_string();
        let file_uri = url::Url::from_file_path(root.join("notes.txt"))
            .unwrap()
            .to_string();
        let mut event = FileDropEvent::default();
        event.uris = vec![
            image_uri.replace("diagram", "%64iagram"),
            file_uri.clone(),
            image_uri,
            file_uri.clone(),
            "https://localhost/image.png".into(),
        ];
        let (images, files) = classify(&event);
        assert_eq!(images, [(image, "image/png".into())]);
        assert_eq!(files, [file_uri]);
    }
}
