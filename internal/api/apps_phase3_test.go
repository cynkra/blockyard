package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// --- UpdateApp rename branches ---

func TestUpdateAppRenameByOwnerSucceeds(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "rename-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"name":"renamed-app"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	// Verify the new name is canonical.
	req = authReq("GET", ts.URL+"/api/v1/apps/renamed-app", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected renamed app to resolve, got %d", resp2.StatusCode)
	}
}

func TestUpdateAppRenameByCollaboratorForbidden(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "collab-rename")
	id := created["id"].(string)

	srv.DB.UpsertUserWithRole("collab", "c@test", "Collab", "publisher")
	srv.DB.GrantAppAccess(id, "collab", "user", "collaborator", "admin")
	collabToken := createTestPAT(t, srv.DB, "collab")

	req, _ := http.NewRequest("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"name":"collab-renamed"}`))
	req.Header.Set("Authorization", "Bearer "+collabToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 (rename requires owner/admin), got %d", resp.StatusCode)
	}
}

func TestUpdateAppRenameInvalidName(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "rename-invalid")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"name":"INVALID_NAME"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid rename name, got %d", resp.StatusCode)
	}
}

func TestUpdateAppRenameConflictReturnsServerError(t *testing.T) {
	// db.RenameApp's pre-insert uniqueness check returns a plain
	// fmt.Errorf that doesn't satisfy IsUniqueConstraintError. The
	// handler's fallback branch wraps it as a 500; this test pins
	// that current behavior so regressions surface a clear diff.
	_, ts := testServer(t)
	createApp(t, ts, "taken-name")
	created := createApp(t, ts, "rename-clash")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"name":"taken-name"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500 for rename conflict, got %d", resp.StatusCode)
	}
}

// --- UpdateApp HX-Request branches ---

func TestUpdateAppHXRequestSetsTrigger(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "hx-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"title":"Hello"}`))
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("HX-Trigger"); got != "appUpdated" {
		t.Errorf("HX-Trigger = %q, want appUpdated", got)
	}
}

func TestUpdateAppHXInvalidCronReturns422(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "cron-hx-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"refresh_schedule":"not a cron"}`))
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for HX-Request with bad cron, got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "invalid cron") {
		t.Errorf("expected 'invalid cron' message, got: %s", b)
	}
}

// --- UpdateApp form-encoded branch ---

func TestUpdateAppFormEncodedFieldsApplied(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "form-app")
	id := created["id"].(string)

	form := url.Values{}
	form.Set("title", "from form")
	form.Set("description", "form desc")
	form.Set("memory_limit", "256m")
	form.Set("max_workers_per_app", "5")
	form.Set("max_sessions_per_worker", "10")
	form.Set("pre_warmed_sessions", "2")
	form.Set("cpu_limit", "1.5")

	req, _ := http.NewRequest("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["title"] != "from form" {
		t.Errorf("title = %v, want 'from form'", out["title"])
	}
	if out["description"] != "form desc" {
		t.Errorf("description = %v, want 'form desc'", out["description"])
	}
	if out["memory_limit"] != "256m" {
		t.Errorf("memory_limit = %v, want 256m", out["memory_limit"])
	}
}

// --- RollbackApp branches ---

func TestRollbackAppFormEncoded(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "rb-form-app")
	id := created["id"].(string)

	// Create two bundles, activate the first.
	b1 := "bun-rbf-1-" + id[:8]
	b2 := "bun-rbf-2-" + id[:8]
	srv.DB.CreateBundle(b1, id, "admin", false)
	srv.DB.UpdateBundleStatus(b1, "ready")
	srv.DB.CreateBundle(b2, id, "admin", false)
	srv.DB.UpdateBundleStatus(b2, "ready")
	srv.DB.ActivateBundle(id, b1)

	form := url.Values{}
	form.Set("bundle_id", b2)
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/rollback",
		strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestRollbackAppHXRequestSetsTrigger(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "rb-hx-app")
	id := created["id"].(string)
	b1 := "bun-rbhx-1-" + id[:8]
	b2 := "bun-rbhx-2-" + id[:8]
	srv.DB.CreateBundle(b1, id, "admin", false)
	srv.DB.UpdateBundleStatus(b1, "ready")
	srv.DB.CreateBundle(b2, id, "admin", false)
	srv.DB.UpdateBundleStatus(b2, "ready")
	srv.DB.ActivateBundle(id, b1)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback",
		strings.NewReader(`{"bundle_id":"`+b2+`"}`))
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("HX-Trigger"), "bundleRolledBack") {
		t.Errorf("expected bundleRolledBack in HX-Trigger, got %q", resp.Header.Get("HX-Trigger"))
	}
}

func TestRollbackAppMissingBundleID(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "rb-missing-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback",
		strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing bundle_id, got %d", resp.StatusCode)
	}
}

func TestRollbackAppBundleNotForApp(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "rb-cross-1")
	id1 := created["id"].(string)
	other := createApp(t, ts, "rb-cross-2")
	id2 := other["id"].(string)
	// Bundle belongs to id2.
	b := "bun-rbcross-" + id2[:8]
	srv.DB.CreateBundle(b, id2, "admin", false)
	srv.DB.UpdateBundleStatus(b, "ready")

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id1+"/rollback",
		strings.NewReader(`{"bundle_id":"`+b+`"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for cross-app bundle, got %d", resp.StatusCode)
	}
}

func TestRollbackAppBundleNotReady(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "rb-notready")
	id := created["id"].(string)
	b := "bun-rbnr-" + id[:8]
	srv.DB.CreateBundle(b, id, "admin", false)
	// Leave bundle in "pending" status.

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback",
		strings.NewReader(`{"bundle_id":"`+b+`"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for non-ready bundle, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not ready") {
		t.Errorf("expected 'not ready' in body, got: %s", body)
	}
}

func TestRollbackAppAlreadyActive(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "rb-active")
	id := created["id"].(string)
	b := "bun-rba-" + id[:8]
	srv.DB.CreateBundle(b, id, "admin", false)
	srv.DB.UpdateBundleStatus(b, "ready")
	srv.DB.ActivateBundle(id, b)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback",
		strings.NewReader(`{"bundle_id":"`+b+`"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "already active") {
		t.Errorf("expected 'already active', got: %s", body)
	}
}

func TestRollbackAppViewerForbidden(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "rb-viewer")
	id := created["id"].(string)
	b := "bun-rbv-" + id[:8]
	srv.DB.CreateBundle(b, id, "admin", false)
	srv.DB.UpdateBundleStatus(b, "ready")

	seedTestViewer(t, srv.DB)
	srv.DB.GrantAppAccess(id, "viewer", "user", "viewer", "admin")

	req := viewerReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback",
		strings.NewReader(`{"bundle_id":"`+b+`"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for viewer rollback, got %d", resp.StatusCode)
	}
}

func TestRollbackAppInvalidForm(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "rb-bad-form")
	id := created["id"].(string)

	// Form-encoded with empty bundle_id.
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/rollback",
		strings.NewReader("bundle_id="))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for empty form bundle_id, got %d", resp.StatusCode)
	}
}

// --- RestoreApp branches ---

func TestRestoreAppByNameSucceeds(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "restore-by-name")
	id := created["id"].(string)
	srv.DB.SoftDeleteApp(id)

	req := authReq("POST", ts.URL+"/api/v1/apps/restore-by-name/restore", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestRestoreAppNotDeletedIs404(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "restore-active")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for non-deleted app, got %d", resp.StatusCode)
	}
}

// --- StartApp / StopApp permission branches ---

func TestStartAppByViewerForbidden(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "start-viewer")
	id := created["id"].(string)
	seedTestViewer(t, srv.DB)
	srv.DB.GrantAppAccess(id, "viewer", "user", "viewer", "admin")

	req := viewerReq("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for viewer start, got %d", resp.StatusCode)
	}
}

func TestStopAppByViewerForbidden(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "stop-viewer")
	id := created["id"].(string)
	seedTestViewer(t, srv.DB)
	srv.DB.GrantAppAccess(id, "viewer", "user", "viewer", "admin")

	req := viewerReq("POST", ts.URL+"/api/v1/apps/"+id+"/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for viewer stop, got %d", resp.StatusCode)
	}
}

// --- CreateApp duplicate name ---

func TestCreateAppDuplicateNameReturnsConflict(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "dup-app")

	req := authReq("POST", ts.URL+"/api/v1/apps",
		strings.NewReader(`{"name":"dup-app"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 for duplicate name, got %d", resp.StatusCode)
	}
}

// --- ListApps (v1) publisher-owned path ---

func TestListAppsV1PublisherSeesOwnApp(t *testing.T) {
	srv, ts := testServerWithLifecycle(t)

	srv.DB.UpsertUserWithRole("pub-self", "p@test", "Pub Self", "publisher")
	pubToken := createTestPAT(t, srv.DB, "pub-self")

	req, _ := http.NewRequest("POST", ts.URL+"/apps",
		strings.NewReader(`{"name":"pub-own"}`))
	req.Header.Set("Authorization", "Bearer "+pubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", ts.URL+"/apps", nil)
	req.Header.Set("Authorization", "Bearer "+pubToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var apps []AppResponse
	json.NewDecoder(resp.Body).Decode(&apps)
	if len(apps) != 1 || apps[0].Name != "pub-own" {
		t.Errorf("expected publisher to see own app, got %+v", apps)
	}
}

func TestListAppsV1DeletedViewerForbidden(t *testing.T) {
	srv, ts := testServerWithLifecycle(t)
	seedTestViewer(t, srv.DB)

	req := viewerReq("GET", ts.URL+"/apps?deleted=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for viewer requesting deleted apps, got %d", resp.StatusCode)
	}
}

// --- AppLogs dead-worker branch ---

func TestAppLogsDeadWorkerFromLogStore(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "logs-dead")
	id := created["id"].(string)
	b := "bun-logs-" + id[:8]
	srv.DB.CreateBundle(b, id, "admin", false)
	srv.DB.UpdateBundleStatus(b, "ready")
	srv.DB.ActivateBundle(id, b)

	// Dead worker recorded in logstore only.
	deadWID := "dead-worker-1"
	sender := srv.LogStore.Create(deadWID, id)
	sender.Write("historical line 1")
	sender.Write("historical line 2")
	srv.LogStore.MarkEnded(deadWID)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs?worker_id="+deadWID+"&stream=false", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for dead-worker logs, got %d: %s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "historical line 1") {
		t.Errorf("expected buffered log lines, got: %s", body)
	}
}

func TestAppLogsWorkerNotForApp(t *testing.T) {
	srv, ts := testServer(t)
	a := createApp(t, ts, "logs-a")
	aID := a["id"].(string)
	b := createApp(t, ts, "logs-b")
	bID := b["id"].(string)

	// Worker belongs to app b, logs request targets app a.
	srv.LogStore.Create("cross-worker", bID)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+aID+"/logs?worker_id=cross-worker&stream=false", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for worker not belonging to app, got %d", resp.StatusCode)
	}
}

// --- AppLogs: sanity that the viewer path (CanDeploy false) returns 404 ---

func TestAppLogsViewerForbidden(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "logs-viewer")
	id := created["id"].(string)
	seedTestViewer(t, srv.DB)
	srv.DB.GrantAppAccess(id, "viewer", "user", "viewer", "admin")

	req := viewerReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs?worker_id=anything", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for viewer logs, got %d", resp.StatusCode)
	}
}

// --- Ensure NewUnstartedServer-based testServer also serves our paths
//     — smoke test used to anchor lifetime of helpers used above. ---

func TestCreateAppBadNameUnit(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps",
		strings.NewReader(`{"name":"INVALID_NAME"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}
