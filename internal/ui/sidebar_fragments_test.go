package ui

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

// --- validateUITagName (shared between the settings tab and the
//     sidebar tag-assign endpoint) ---

func TestValidateUITagNameValid(t *testing.T) {
	for _, name := range []string{"a", "tag", "my-tag", "abc-123", "x9"} {
		if err := validateUITagName(name); err != nil {
			t.Errorf("expected %q to be valid, got %v", name, err)
		}
	}
}

func TestValidateUITagNameInvalid(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"", "1–63"},
		{strings.Repeat("a", 64), "1–63"},
		{"Tag", "lowercase letters"},
		{"tag_name", "lowercase letters"},
		{"9tag", "must start with a lowercase letter"},
		{"-tag", "must start with a lowercase letter"},
		{"tag-", "must not end with a hyphen"},
	}
	for _, tc := range cases {
		err := validateUITagName(tc.name)
		if err == nil {
			t.Errorf("expected error for %q", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("validateUITagName(%q) = %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

// --- workerDetailTab: GET /ui/apps/{name}/tab/runtime/worker/{wid} ---

func TestWorkerDetailTabReturns404ForMissingWorker(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("wd-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/wd-app/tab/runtime/worker/no-such-worker")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for missing worker, got %d", resp.StatusCode)
	}
}

func TestWorkerDetailTabReturns404ForUnknownApp(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/ui/apps/no-app/tab/runtime/worker/any-wid")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestWorkerDetailTabRendersForActiveWorker(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("wd-real", "owner")
	srv.Workers.Set("wd-real-worker", server.ActiveWorker{
		AppID:     app.ID,
		StartedAt: time.Now().Add(-2 * time.Minute),
	})
	// Session with a known display name.
	srv.DB.UpsertUserWithRole("sess-user", "s@example.com", "Session User", "viewer")
	srv.Sessions.Set("sess-x", session.Entry{
		WorkerID:   "wd-real-worker",
		UserSub:    "sess-user",
		LastAccess: time.Now(),
	})
	// Anonymous session too.
	srv.Sessions.Set("sess-anon", session.Entry{
		WorkerID:   "wd-real-worker",
		UserSub:    "",
		LastAccess: time.Now(),
	})

	resp, err := http.Get(ts.URL + "/ui/apps/wd-real/tab/runtime/worker/wd-real-worker")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "wd-real-worker") {
		t.Errorf("expected worker id in body, got: %s", body)
	}
	if !strings.Contains(body, "Session User") {
		t.Errorf("expected 'Session User' in sessions list, got: %s", body)
	}
	if !strings.Contains(body, "anonymous") {
		t.Errorf("expected 'anonymous' label for empty-sub session, got: %s", body)
	}
}

func TestWorkerDetailTabRendersDrainingStatus(t *testing.T) {
	// A draining worker exercises the Draining branch in
	// workerDetailTab's status string.
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("wd-drain", "owner")
	srv.Workers.Set("drain-w", server.ActiveWorker{
		AppID:     app.ID,
		Draining:  true,
		StartedAt: time.Now(),
	})

	resp, err := http.Get(ts.URL + "/ui/apps/wd-drain/tab/runtime/worker/drain-w")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "draining") && !strings.Contains(body, "Draining") {
		t.Errorf("expected draining status in body, got: %s", body)
	}
}

// --- deploymentLogFragment: GET /ui/deployments/{bundleID}/logs ---

func TestDeploymentLogFragmentEmptyState(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("dl-app", "u")
	// Bundle with no build log.
	bundleID := "bun-dl-" + app.ID[:8]
	srv.DB.CreateBundle(bundleID, app.ID, "u", false)

	resp, err := http.Get(ts.URL + "/ui/deployments/" + bundleID + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "No build log available") {
		t.Errorf("expected empty-state message, got: %s", body)
	}
}

func TestDeploymentLogFragmentRendersLog(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("dl2-app", "u")
	bundleID := "bun-dl2-" + app.ID[:8]
	srv.DB.CreateBundle(bundleID, app.ID, "u", false)
	// Populate the build log.
	if err := srv.DB.InsertBundleLog(bundleID, "pak: building <script>"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/ui/deployments/" + bundleID + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "pak: building") {
		t.Errorf("expected log content, got: %s", body)
	}
	// HTML escaping must be applied so user-influenced log text can't
	// inject markup.
	if strings.Contains(body, "<script>") {
		t.Errorf("expected HTML-escaped output, got raw tag: %s", body)
	}
}

// --- searchUsersFragment: GET /ui/users/search ---

func TestSearchUsersFragmentEmptyQuery(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/ui/users/search")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if strings.TrimSpace(body) != "" {
		t.Errorf("expected empty body for empty query, got: %s", body)
	}
}

func TestSearchUsersFragmentRendersMatches(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	srv.DB.UpsertUserWithRole("alice-sub", "alice@example.com", "Alice", "viewer")
	srv.DB.UpsertUserWithRole("bob-sub", "bob@example.com", "Bob", "viewer")

	resp, err := http.Get(ts.URL + "/ui/users/search?q=ali")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Alice") {
		t.Error("expected Alice in search results")
	}
	if strings.Contains(body, "Bob") {
		t.Error("did not expect Bob for query 'ali'")
	}
	if !strings.Contains(body, `role="option"`) {
		t.Error("expected combobox option markup")
	}
}

// --- createAndAssignTag: POST /ui/apps/{name}/tags ---

func TestCreateAndAssignTagEmptyName(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("tag-app", "owner")

	resp, err := http.Post(
		ts.URL+"/ui/apps/tag-app/tags",
		"application/x-www-form-urlencoded",
		strings.NewReader("name="),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "required") {
		t.Errorf("expected 'required' in body, got: %s", body)
	}
}

func TestCreateAndAssignTagInvalidName(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("tag-app-2", "owner")

	resp, err := http.Post(
		ts.URL+"/ui/apps/tag-app-2/tags",
		"application/x-www-form-urlencoded",
		strings.NewReader("name=BAD_NAME"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid name, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "lowercase") {
		t.Errorf("expected validation message, got: %s", body)
	}
}

func TestCreateAndAssignTagCreatesNewTag(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("tag-app-3", "owner")

	resp, err := http.Post(
		ts.URL+"/ui/apps/tag-app-3/tags",
		"application/x-www-form-urlencoded",
		strings.NewReader("name=fresh-tag"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("HX-Trigger") != "tagAdded" {
		t.Errorf("expected HX-Trigger=tagAdded, got %q", resp.Header.Get("HX-Trigger"))
	}
	tags, _ := srv.DB.ListTags()
	found := false
	for _, tg := range tags {
		if tg.Name == "fresh-tag" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'fresh-tag' to exist in tag table after assignment")
	}
}

func TestCreateAndAssignTagReusesExistingTag(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("tag-app-4", "owner")

	// Pre-create the tag.
	if _, err := srv.DB.CreateTag("existing-tag"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		ts.URL+"/ui/apps/tag-app-4/tags",
		"application/x-www-form-urlencoded",
		strings.NewReader("name=existing-tag"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	// No duplicate tag row should exist.
	tags, _ := srv.DB.ListTags()
	count := 0
	for _, tg := range tags {
		if tg.Name == "existing-tag" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 'existing-tag' row, got %d", count)
	}
}
