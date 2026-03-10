use std::path::{Path, PathBuf};

use tokio::io::AsyncWriteExt;

pub mod restore;

/// Storage paths for a given bundle.
pub struct BundlePaths {
    pub archive: PathBuf,  // {app_id}/{bundle_id}.tar.gz
    pub unpacked: PathBuf, // {app_id}/{bundle_id}/
    pub library: PathBuf,  // {app_id}/{bundle_id}_lib/
}

impl BundlePaths {
    pub fn for_bundle(base: &Path, app_id: &str, bundle_id: &str) -> Self {
        let app_dir = base.join(app_id);
        Self {
            archive: app_dir.join(format!("{bundle_id}.tar.gz")),
            unpacked: app_dir.join(bundle_id),
            library: app_dir.join(format!("{bundle_id}_lib")),
        }
    }
}

/// Write the uploaded tar.gz to a temp file, then atomically rename.
/// Creates the app directory if it doesn't exist.
pub async fn write_archive(
    base: &Path,
    app_id: &str,
    bundle_id: &str,
    data: bytes::Bytes,
) -> Result<BundlePaths, BundleError> {
    let paths = BundlePaths::for_bundle(base, app_id, bundle_id);
    let app_dir = base.join(app_id);
    tokio::fs::create_dir_all(&app_dir)
        .await
        .map_err(|e| BundleError::Storage(format!("create app dir: {e}")))?;

    // Write to temp file in the same directory (same filesystem for rename)
    let temp_path = app_dir.join(format!(".{bundle_id}.tar.gz.tmp"));
    let result = async {
        let mut file = tokio::fs::File::create(&temp_path)
            .await
            .map_err(|e| BundleError::Storage(format!("create temp file: {e}")))?;
        file.write_all(&data)
            .await
            .map_err(|e| BundleError::Storage(format!("write temp file: {e}")))?;
        file.flush()
            .await
            .map_err(|e| BundleError::Storage(format!("flush temp file: {e}")))?;

        // Atomic rename
        tokio::fs::rename(&temp_path, &paths.archive)
            .await
            .map_err(|e| BundleError::Storage(format!("rename archive: {e}")))?;

        Ok(paths)
    }
    .await;

    // Clean up temp file on any failure
    if result.is_err() {
        let _ = tokio::fs::remove_file(&temp_path).await;
    }

    result
}

/// Unpack the tar.gz archive into {bundle_id}/ directory.
pub async fn unpack_archive(paths: &BundlePaths) -> Result<(), BundleError> {
    let archive_path = paths.archive.clone();
    let unpack_dir = paths.unpacked.clone();

    // Run in a blocking task — tar decompression is CPU-bound
    tokio::task::spawn_blocking(move || {
        let file = std::fs::File::open(&archive_path)
            .map_err(|e| BundleError::Unpack(format!("open archive: {e}")))?;
        let decoder = flate2::read::GzDecoder::new(file);
        let mut archive = tar::Archive::new(decoder);

        std::fs::create_dir_all(&unpack_dir)
            .map_err(|e| BundleError::Unpack(format!("create unpack dir: {e}")))?;

        archive
            .unpack(&unpack_dir)
            .map_err(|e| BundleError::Unpack(format!("unpack: {e}")))?;

        Ok(())
    })
    .await
    .map_err(|e| BundleError::Unpack(format!("spawn_blocking: {e}")))?
}

/// Create the library output directory for dependency restoration.
pub async fn create_library_dir(paths: &BundlePaths) -> Result<(), BundleError> {
    tokio::fs::create_dir_all(&paths.library)
        .await
        .map_err(|e| BundleError::Storage(format!("create library dir: {e}")))?;
    Ok(())
}

/// Delete a bundle's files (archive, unpacked dir, library dir).
/// Best-effort — logs errors but does not fail.
pub async fn delete_bundle_files(paths: &BundlePaths) {
    for path in [&paths.archive, &paths.unpacked, &paths.library] {
        if path.exists() {
            let result = if path.is_dir() {
                tokio::fs::remove_dir_all(path).await
            } else {
                tokio::fs::remove_file(path).await
            };
            if let Err(e) = result {
                tracing::warn!(path = %path.display(), error = %e, "failed to delete bundle file");
            }
        }
    }
}

/// Enforce retention: keep at most `retention` bundles per app, plus the
/// active bundle (never deleted). Returns IDs of deleted bundles.
pub async fn enforce_retention(
    pool: &sqlx::SqlitePool,
    base: &Path,
    app_id: &str,
    active_bundle_id: Option<&str>,
    retention: u32,
) -> Vec<String> {
    let bundles = match crate::db::sqlite::list_bundles_by_app(pool, app_id).await {
        Ok(b) => b,
        Err(e) => {
            tracing::warn!(app_id, error = %e, "failed to list bundles for retention");
            return vec![];
        }
    };

    // Bundles are ordered newest-first. Keep the first `retention` plus
    // any bundle that is the active one.
    let mut to_delete = Vec::new();
    let mut kept = 0u32;
    for bundle in &bundles {
        let is_active = active_bundle_id == Some(bundle.id.as_str());
        if is_active || kept < retention {
            if !is_active {
                kept += 1;
            }
            continue;
        }
        to_delete.push(bundle.clone());
    }

    let mut deleted_ids = Vec::new();
    for bundle in to_delete {
        let paths = BundlePaths::for_bundle(base, app_id, &bundle.id);
        delete_bundle_files(&paths).await;
        if let Err(e) = crate::db::sqlite::delete_bundle(pool, &bundle.id).await {
            tracing::warn!(bundle_id = bundle.id, error = %e, "failed to delete bundle row");
        } else {
            deleted_ids.push(bundle.id);
        }
    }

    deleted_ids
}

#[derive(Debug, thiserror::Error)]
pub enum BundleError {
    #[error("storage error: {0}")]
    Storage(String),
    #[error("unpack error: {0}")]
    Unpack(String),
    #[error("restore error: {0}")]
    Restore(String),
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_test_targz(dir: &Path) -> PathBuf {
        let tar_path = dir.join("test.tar.gz");
        let file = std::fs::File::create(&tar_path).unwrap();
        let encoder = flate2::write::GzEncoder::new(file, flate2::Compression::default());
        let mut archive = tar::Builder::new(encoder);

        let app_r = b"library(shiny)\nshinyApp(ui, server)";
        let mut header = tar::Header::new_gnu();
        header.set_size(app_r.len() as u64);
        header.set_mode(0o644);
        header.set_cksum();
        archive
            .append_data(&mut header, "app.R", &app_r[..])
            .unwrap();

        archive.into_inner().unwrap().finish().unwrap();
        tar_path
    }

    #[tokio::test]
    async fn write_and_unpack_archive() {
        let tmp = tempfile::TempDir::new().unwrap();
        let tar_data = tokio::fs::read(make_test_targz(tmp.path())).await.unwrap();

        let paths = write_archive(
            tmp.path(),
            "app-1",
            "bundle-1",
            bytes::Bytes::from(tar_data),
        )
        .await
        .unwrap();

        assert!(paths.archive.exists());

        unpack_archive(&paths).await.unwrap();
        assert!(paths.unpacked.join("app.R").exists());
    }

    #[tokio::test]
    async fn delete_bundle_files_works() {
        let tmp = tempfile::TempDir::new().unwrap();
        let tar_data = tokio::fs::read(make_test_targz(tmp.path())).await.unwrap();
        let paths = write_archive(
            tmp.path(),
            "app-1",
            "bundle-1",
            bytes::Bytes::from(tar_data),
        )
        .await
        .unwrap();
        unpack_archive(&paths).await.unwrap();
        create_library_dir(&paths).await.unwrap();

        delete_bundle_files(&paths).await;
        assert!(!paths.archive.exists());
        assert!(!paths.unpacked.exists());
        assert!(!paths.library.exists());
    }
}
