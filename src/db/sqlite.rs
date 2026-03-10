use sqlx::SqlitePool;
use uuid::Uuid;

/// App record as stored in SQLite.
#[derive(Debug, Clone, sqlx::FromRow, serde::Serialize)]
pub struct AppRow {
    pub id: String,
    pub name: String,
    pub active_bundle: Option<String>,
    pub max_workers_per_app: Option<i64>,
    pub max_sessions_per_worker: i64,
    pub memory_limit: Option<String>,
    pub cpu_limit: Option<f64>,
    pub created_at: String,
    pub updated_at: String,
}

/// Bundle record as stored in SQLite.
#[derive(Debug, Clone, sqlx::FromRow, serde::Serialize)]
pub struct BundleRow {
    pub id: String,
    pub app_id: String,
    pub status: String,
    pub path: String,
    pub uploaded_at: String,
}

pub async fn create_app(pool: &SqlitePool, name: &str) -> Result<AppRow, sqlx::Error> {
    let id = Uuid::new_v4().to_string();
    let now = chrono::Utc::now().to_rfc3339();
    sqlx::query_as::<_, AppRow>(
        "INSERT INTO apps (id, name, max_sessions_per_worker, created_at, updated_at)
         VALUES (?, ?, 1, ?, ?)
         RETURNING *",
    )
    .bind(&id)
    .bind(name)
    .bind(&now)
    .bind(&now)
    .fetch_one(pool)
    .await
}

pub async fn get_app(pool: &SqlitePool, id: &str) -> Result<Option<AppRow>, sqlx::Error> {
    sqlx::query_as::<_, AppRow>("SELECT * FROM apps WHERE id = ?")
        .bind(id)
        .fetch_optional(pool)
        .await
}

pub async fn get_app_by_name(pool: &SqlitePool, name: &str) -> Result<Option<AppRow>, sqlx::Error> {
    sqlx::query_as::<_, AppRow>("SELECT * FROM apps WHERE name = ?")
        .bind(name)
        .fetch_optional(pool)
        .await
}

/// Resolve an app by ID or name. Tries ID first, then falls back to name.
pub async fn resolve_app(
    pool: &SqlitePool,
    id_or_name: &str,
) -> Result<Option<AppRow>, sqlx::Error> {
    if let Some(app) = get_app(pool, id_or_name).await? {
        return Ok(Some(app));
    }
    get_app_by_name(pool, id_or_name).await
}

pub async fn list_apps(pool: &SqlitePool) -> Result<Vec<AppRow>, sqlx::Error> {
    sqlx::query_as::<_, AppRow>("SELECT * FROM apps ORDER BY created_at DESC")
        .fetch_all(pool)
        .await
}

pub async fn delete_app(pool: &SqlitePool, id: &str) -> Result<bool, sqlx::Error> {
    let result = sqlx::query("DELETE FROM apps WHERE id = ?")
        .bind(id)
        .execute(pool)
        .await?;
    Ok(result.rows_affected() > 0)
}

pub async fn create_bundle(
    pool: &SqlitePool,
    id: &str,
    app_id: &str,
    path: &str,
) -> Result<BundleRow, sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();
    sqlx::query_as::<_, BundleRow>(
        "INSERT INTO bundles (id, app_id, status, path, uploaded_at)
         VALUES (?, ?, 'pending', ?, ?)
         RETURNING *",
    )
    .bind(id)
    .bind(app_id)
    .bind(path)
    .bind(&now)
    .fetch_one(pool)
    .await
}

pub async fn list_bundles_by_app(
    pool: &SqlitePool,
    app_id: &str,
) -> Result<Vec<BundleRow>, sqlx::Error> {
    sqlx::query_as::<_, BundleRow>(
        "SELECT * FROM bundles WHERE app_id = ? ORDER BY uploaded_at DESC",
    )
    .bind(app_id)
    .fetch_all(pool)
    .await
}

pub async fn delete_bundle(pool: &SqlitePool, id: &str) -> Result<bool, sqlx::Error> {
    let result = sqlx::query("DELETE FROM bundles WHERE id = ?")
        .bind(id)
        .execute(pool)
        .await?;
    Ok(result.rows_affected() > 0)
}

pub async fn set_active_bundle(
    pool: &SqlitePool,
    app_id: &str,
    bundle_id: &str,
) -> Result<bool, sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();
    let result = sqlx::query("UPDATE apps SET active_bundle = ?, updated_at = ? WHERE id = ?")
        .bind(bundle_id)
        .bind(&now)
        .bind(app_id)
        .execute(pool)
        .await?;
    Ok(result.rows_affected() > 0)
}

pub async fn update_app(
    pool: &SqlitePool,
    id: &str,
    max_workers_per_app: Option<Option<i64>>,
    max_sessions_per_worker: Option<i64>,
    memory_limit: Option<Option<String>>,
    cpu_limit: Option<Option<f64>>,
) -> Result<AppRow, sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();
    let mut app = get_app(pool, id).await?.ok_or(sqlx::Error::RowNotFound)?;

    if let Some(v) = max_workers_per_app {
        app.max_workers_per_app = v;
    }
    if let Some(v) = max_sessions_per_worker {
        app.max_sessions_per_worker = v;
    }
    if let Some(v) = memory_limit {
        app.memory_limit = v;
    }
    if let Some(v) = cpu_limit {
        app.cpu_limit = v;
    }

    sqlx::query_as::<_, AppRow>(
        "UPDATE apps SET
             max_workers_per_app = ?,
             max_sessions_per_worker = ?,
             memory_limit = ?,
             cpu_limit = ?,
             updated_at = ?
         WHERE id = ?
         RETURNING *",
    )
    .bind(app.max_workers_per_app)
    .bind(app.max_sessions_per_worker)
    .bind(&app.memory_limit)
    .bind(app.cpu_limit)
    .bind(&now)
    .bind(id)
    .fetch_one(pool)
    .await
}

pub async fn clear_active_bundle(pool: &SqlitePool, app_id: &str) -> Result<bool, sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();
    let result = sqlx::query("UPDATE apps SET active_bundle = NULL, updated_at = ? WHERE id = ?")
        .bind(&now)
        .bind(app_id)
        .execute(pool)
        .await?;
    Ok(result.rows_affected() > 0)
}

pub async fn fail_stale_bundles(pool: &SqlitePool) -> Result<u64, sqlx::Error> {
    let result = sqlx::query("UPDATE bundles SET status = 'failed' WHERE status = 'building'")
        .execute(pool)
        .await?;
    Ok(result.rows_affected())
}

pub async fn update_bundle_status(
    pool: &SqlitePool,
    id: &str,
    status: &str,
) -> Result<bool, sqlx::Error> {
    let result = sqlx::query("UPDATE bundles SET status = ? WHERE id = ?")
        .bind(status)
        .bind(id)
        .execute(pool)
        .await?;
    Ok(result.rows_affected() > 0)
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn test_pool() -> SqlitePool {
        let pool = SqlitePool::connect(":memory:").await.unwrap();
        crate::db::run_migrations(&pool).await.unwrap();
        pool
    }

    #[tokio::test]
    async fn create_and_get_app() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        assert_eq!(app.name, "my-app");

        let fetched = get_app(&pool, &app.id).await.unwrap().unwrap();
        assert_eq!(fetched.id, app.id);
    }

    #[tokio::test]
    async fn get_app_by_name_works() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();

        let fetched = get_app_by_name(&pool, "my-app").await.unwrap().unwrap();
        assert_eq!(fetched.id, app.id);

        assert!(
            get_app_by_name(&pool, "nonexistent")
                .await
                .unwrap()
                .is_none()
        );
    }

    #[tokio::test]
    async fn resolve_app_by_id_and_name() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();

        // Resolve by ID
        let by_id = resolve_app(&pool, &app.id).await.unwrap().unwrap();
        assert_eq!(by_id.id, app.id);

        // Resolve by name
        let by_name = resolve_app(&pool, "my-app").await.unwrap().unwrap();
        assert_eq!(by_name.id, app.id);

        // Nonexistent returns None
        assert!(resolve_app(&pool, "nonexistent").await.unwrap().is_none());
    }

    #[tokio::test]
    async fn duplicate_name_fails() {
        let pool = test_pool().await;
        create_app(&pool, "my-app").await.unwrap();
        assert!(create_app(&pool, "my-app").await.is_err());
    }

    #[tokio::test]
    async fn list_apps_returns_all() {
        let pool = test_pool().await;
        create_app(&pool, "app-1").await.unwrap();
        create_app(&pool, "app-2").await.unwrap();

        let apps = list_apps(&pool).await.unwrap();
        assert_eq!(apps.len(), 2);
    }

    #[tokio::test]
    async fn delete_app_removes_row() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        assert!(delete_app(&pool, &app.id).await.unwrap());
        assert!(get_app(&pool, &app.id).await.unwrap().is_none());
    }

    #[tokio::test]
    async fn delete_nonexistent_returns_false() {
        let pool = test_pool().await;
        assert!(!delete_app(&pool, "nonexistent").await.unwrap());
    }

    #[tokio::test]
    async fn create_and_list_bundles() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        let bundle = create_bundle(&pool, "bundle-1", &app.id, "/tmp/bundle.tar.gz")
            .await
            .unwrap();
        assert_eq!(bundle.app_id, app.id);
        assert_eq!(bundle.status, "pending");

        let bundles = list_bundles_by_app(&pool, &app.id).await.unwrap();
        assert_eq!(bundles.len(), 1);
    }

    #[tokio::test]
    async fn update_bundle_status_works() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        let bundle = create_bundle(&pool, "bundle-1", &app.id, "/tmp/bundle.tar.gz")
            .await
            .unwrap();

        assert!(
            update_bundle_status(&pool, &bundle.id, "ready")
                .await
                .unwrap()
        );

        let bundles = list_bundles_by_app(&pool, &app.id).await.unwrap();
        assert_eq!(bundles[0].status, "ready");
    }

    #[tokio::test]
    async fn set_and_clear_active_bundle() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        let bundle = create_bundle(&pool, "b-1", &app.id, "/tmp/b.tar.gz")
            .await
            .unwrap();

        // Set active bundle
        assert!(set_active_bundle(&pool, &app.id, &bundle.id).await.unwrap());
        let fetched = get_app(&pool, &app.id).await.unwrap().unwrap();
        assert_eq!(fetched.active_bundle.as_deref(), Some(bundle.id.as_str()));

        // Clear active bundle
        assert!(clear_active_bundle(&pool, &app.id).await.unwrap());
        let fetched = get_app(&pool, &app.id).await.unwrap().unwrap();
        assert_eq!(fetched.active_bundle, None);
    }

    #[tokio::test]
    async fn set_active_bundle_nonexistent_app_returns_false() {
        let pool = test_pool().await;
        assert!(
            !set_active_bundle(&pool, "no-such-app", "b-1")
                .await
                .unwrap()
        );
    }

    #[tokio::test]
    async fn update_app_modifies_fields() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();

        let updated = update_app(
            &pool,
            &app.id,
            Some(Some(4)),                  // max_workers_per_app = 4
            Some(10),                       // max_sessions_per_worker = 10
            Some(Some("512m".to_string())), // memory_limit
            Some(Some(1.5)),                // cpu_limit
        )
        .await
        .unwrap();

        assert_eq!(updated.max_workers_per_app, Some(4));
        assert_eq!(updated.max_sessions_per_worker, 10);
        assert_eq!(updated.memory_limit.as_deref(), Some("512m"));
        assert_eq!(updated.cpu_limit, Some(1.5));
    }

    #[tokio::test]
    async fn update_app_partial_leaves_other_fields() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();

        // Only update max_sessions_per_worker, leave others as None (no change)
        let updated = update_app(&pool, &app.id, None, Some(5), None, None)
            .await
            .unwrap();

        assert_eq!(updated.max_sessions_per_worker, 5);
        assert_eq!(updated.max_workers_per_app, app.max_workers_per_app);
        assert_eq!(updated.memory_limit, app.memory_limit);
        assert_eq!(updated.cpu_limit, app.cpu_limit);
    }

    #[tokio::test]
    async fn update_app_nonexistent_returns_error() {
        let pool = test_pool().await;
        let result = update_app(&pool, "no-such-id", None, None, None, None).await;
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn fail_stale_bundles_marks_building_as_failed() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        create_bundle(&pool, "b-1", &app.id, "/tmp/b1.tar.gz")
            .await
            .unwrap();
        create_bundle(&pool, "b-2", &app.id, "/tmp/b2.tar.gz")
            .await
            .unwrap();

        // Set one to building, leave the other as pending
        update_bundle_status(&pool, "b-1", "building")
            .await
            .unwrap();

        let count = fail_stale_bundles(&pool).await.unwrap();
        assert_eq!(count, 1);

        let bundles = list_bundles_by_app(&pool, &app.id).await.unwrap();
        let b1 = bundles.iter().find(|b| b.id == "b-1").unwrap();
        let b2 = bundles.iter().find(|b| b.id == "b-2").unwrap();
        assert_eq!(b1.status, "failed");
        assert_eq!(b2.status, "pending"); // unchanged
    }

    #[tokio::test]
    async fn fail_stale_bundles_noop_when_none_building() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        create_bundle(&pool, "b-1", &app.id, "/tmp/b1.tar.gz")
            .await
            .unwrap();

        let count = fail_stale_bundles(&pool).await.unwrap();
        assert_eq!(count, 0);
    }

    #[tokio::test]
    async fn delete_bundle_removes_row() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        create_bundle(&pool, "b-1", &app.id, "/tmp/b.tar.gz")
            .await
            .unwrap();

        assert!(delete_bundle(&pool, "b-1").await.unwrap());
        let bundles = list_bundles_by_app(&pool, &app.id).await.unwrap();
        assert!(bundles.is_empty());
    }

    #[tokio::test]
    async fn delete_bundle_nonexistent_returns_false() {
        let pool = test_pool().await;
        assert!(!delete_bundle(&pool, "no-such-bundle").await.unwrap());
    }
}
