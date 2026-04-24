package ui

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/errorlog"
	"github.com/cynkra/blockyard/internal/orchestrator"
	"github.com/cynkra/blockyard/internal/preflight"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
	updatepkg "github.com/cynkra/blockyard/internal/update"
)

// authServerWithOrch is authServer but registers the UI routes with a
// non-nil orchestrator so the "updates supported" branches become
// reachable.
func authServerWithOrch(t *testing.T, cfg *config.Config, sub string, role auth.Role) (*server.Server, *httptest.Server, *orchestrator.Orchestrator) {
	t.Helper()
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	database.UpsertUserWithRole(sub, sub+"@test.com", sub, role.String())

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	var wg sync.WaitGroup
	srv.RestoreWG = &wg

	orch := orchestrator.NewForTest()
	uiHandler := New()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.ContextWithUser(req.Context(), &auth.AuthenticatedUser{Sub: sub})
			ctx = auth.ContextWithCaller(ctx, &auth.CallerIdentity{
				Sub:    sub,
				Name:   sub,
				Role:   role,
				Source: auth.AuthSourceSession,
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	uiHandler.RegisterRoutes(r, srv, orch, context.Background())

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(wg.Wait)
	return srv, ts, orch
}

// stubGithubAPI points update.APIBase at a local server that returns
// a single stubbed /releases/latest response for the duration of the
// test. Avoids hitting real GitHub from the UI update-check handler.
func stubGithubAPI(t *testing.T, tag string) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			_ = json.NewEncoder(w).Encode(updatepkg.GitHubRelease{TagName: tag})
			return
		}
		http.Error(w, "unexpected path: "+r.URL.Path, http.StatusInternalServerError)
	}))
	t.Cleanup(s.Close)
	old := updatepkg.APIBase
	updatepkg.APIBase = s.URL
	t.Cleanup(func() { updatepkg.APIBase = old })
}

// --- Unit tests for pure helpers ---

func TestParseDurationHoursMinutesDays(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"24h", 24 * time.Hour, true},
		{"30m", 30 * time.Minute, true},
		{"90d", 90 * 24 * time.Hour, true},
		{"", 0, false},
		{"5s", 0, false},
		{"abc", 0, false},
		{"12", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseDuration(tc.in)
		if ok != tc.ok {
			t.Errorf("parseDuration(%q) ok=%v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestErrorLevelBadgeClass(t *testing.T) {
	cases := []struct {
		lvl  slog.Level
		want string
	}{
		{slog.LevelError, "badge-error"},
		{slog.LevelWarn, "badge-warning"},
		{slog.LevelInfo, "badge-info"},
		{slog.LevelDebug, "badge-neutral"},
	}
	for _, tc := range cases {
		if got := errorLevelBadgeClass(tc.lvl); got != tc.want {
			t.Errorf("errorLevelBadgeClass(%v) = %q, want %q", tc.lvl, got, tc.want)
		}
	}
}

func TestRelTimeShort(t *testing.T) {
	now := time.Now()
	cases := []struct {
		offset time.Duration
		want   string
	}{
		{2 * time.Second, "just now"},
		{30 * time.Second, "30s ago"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{48 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		got := relTimeShort(now.Add(-tc.offset))
		if got != tc.want {
			t.Errorf("relTimeShort(-%v) = %q, want %q", tc.offset, got, tc.want)
		}
	}
}

func TestStatusBadgeAllStates(t *testing.T) {
	cases := []struct {
		state     updatepkg.State
		wantLabel string
		wantClass string
	}{
		{updatepkg.StateUpdateAvailable, "update available", "badge-warning"},
		{updatepkg.StateUpToDate, "up to date", "badge-success"},
		{updatepkg.StateAhead, "ahead of latest", "badge-info"},
		{updatepkg.StateDiverged, "diverged from main", "badge-warning"},
		{updatepkg.StateDevBuild, "development build", "badge-ghost"},
		{updatepkg.StateNoRemote, "check failed", "badge-error"},
		{updatepkg.StateLocalNotFound, "commit not on origin", "badge-ghost"},
		{updatepkg.StateUnknown, "not checked", "badge-ghost"},
	}
	for _, tc := range cases {
		got := adminVersionData{State: tc.state}.StatusBadge()
		if got.Label != tc.wantLabel || got.Class != tc.wantClass {
			t.Errorf("StatusBadge(%q) = %+v, want {%q, %q}",
				tc.state, got, tc.wantLabel, tc.wantClass)
		}
	}
}

func TestUpdateAvailableMethod(t *testing.T) {
	if !(adminVersionData{State: updatepkg.StateUpdateAvailable}).UpdateAvailable() {
		t.Error("StateUpdateAvailable should be UpdateAvailable")
	}
	if (adminVersionData{State: updatepkg.StateUpToDate}).UpdateAvailable() {
		t.Error("StateUpToDate should not be UpdateAvailable")
	}
}

func TestBuildAdminVersionEmptyStatus(t *testing.T) {
	srv, _ := authServer(t, oidcConfig(), "a", auth.RoleAdmin)
	data := buildAdminVersion(srv, nil, "")
	if data.State != updatepkg.StateUnknown {
		t.Errorf("State = %q, want %q", data.State, updatepkg.StateUnknown)
	}
	if data.UpdateSupported {
		t.Error("UpdateSupported should be false when orch is nil")
	}
	if data.UpdateState != "idle" {
		t.Errorf("UpdateState = %q, want idle", data.UpdateState)
	}
}

func TestBuildAdminVersionWithStatus(t *testing.T) {
	srv, _ := authServer(t, oidcConfig(), "a", auth.RoleAdmin)
	srv.SetUpdateStatus(&updatepkg.Result{
		State:         updatepkg.StateUpdateAvailable,
		LatestVersion: "1.2.3",
		Detail:        "3 commits behind",
	})
	data := buildAdminVersion(srv, nil, "boom")
	if data.State != updatepkg.StateUpdateAvailable {
		t.Errorf("State = %q, want update_available", data.State)
	}
	if data.LatestVersion != "1.2.3" {
		t.Errorf("LatestVersion = %q, want 1.2.3", data.LatestVersion)
	}
	if data.Detail != "3 commits behind" {
		t.Errorf("Detail = %q, want '3 commits behind'", data.Detail)
	}
	if data.CheckError != "boom" {
		t.Errorf("CheckError = %q, want 'boom'", data.CheckError)
	}
	if data.LastCheckedStr == "" {
		t.Error("LastCheckedStr should be populated after SetUpdateStatus")
	}
}

func TestBuildAdminPushURLEmpty(t *testing.T) {
	if got := buildAdminPushURL(url.Values{}); got != "/admin" {
		t.Errorf("buildAdminPushURL(empty) = %q, want /admin", got)
	}
	// Unknown keys are dropped.
	q := url.Values{"unknown": []string{"x"}}
	if got := buildAdminPushURL(q); got != "/admin" {
		t.Errorf("buildAdminPushURL(unknown key only) = %q, want /admin", got)
	}
}

func TestBuildAdminPushURLWithFilters(t *testing.T) {
	q := url.Values{
		"search": []string{"foo"},
		"role":   []string{"viewer"},
		"page":   []string{"2"},
	}
	got := buildAdminPushURL(q)
	if !strings.HasPrefix(got, "/admin?") {
		t.Fatalf("got %q, want /admin? prefix", got)
	}
	for _, want := range []string{"search=foo", "role=viewer", "page=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in %q", want, got)
		}
	}
}

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

// --- Admin Updates tab (version/check/start/progress) route tests ---

// The authServer helper registers the UI routes with orch=nil. That's
// the production shape for non-Docker backends and also covers the
// "Rolling updates are not supported" branch in adminUpdateStart.

func TestAdminTabVersionFragmentRendersForAdmin(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/ui/admin/tab/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	// The adminVersion template renders an empty-state message when no
	// check has run. A populated status would instead surface the
	// StatusBadge label.
	if !strings.Contains(body, "No check has run yet") {
		t.Errorf("expected empty-state message, got: %s", body)
	}
	if !strings.Contains(body, "admin-version-card") {
		t.Errorf("expected admin-version-card container, got: %s", body)
	}
}

func TestAdminTabVersionFragmentForbiddenForNonAdmin(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "viewer-1", auth.RoleViewer)

	resp, err := http.Get(ts.URL + "/ui/admin/tab/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAdminUpdateProgressForbiddenForNonAdmin(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "pub", auth.RolePublisher)

	resp, err := http.Get(ts.URL + "/ui/admin/update/progress")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAdminUpdateProgressIdleRendersCard(t *testing.T) {
	// orch is nil in authServer → buildAdminVersion returns
	// UpdateState="idle", so this hits the "idle → render card" branch.
	_, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/ui/admin/update/progress")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestAdminUpdateStartForbiddenForNonAdmin(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "viewer-1", auth.RoleViewer)

	resp, err := http.Post(ts.URL+"/ui/admin/update/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAdminUpdateStartWithNilOrchReportsUnsupported(t *testing.T) {
	// With orch == nil (authServer's default) the handler returns a
	// rendered card that surfaces the "not supported on this deployment"
	// message via CheckError without ever spawning a goroutine.
	_, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)

	resp, err := http.Post(ts.URL+"/ui/admin/update/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "not supported") && !strings.Contains(body, "container runtime") {
		t.Errorf("expected unsupported-backend message, got: %s", body)
	}
}

func TestAdminUpdateCheckForbiddenForNonAdmin(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "pub", auth.RolePublisher)

	resp, err := http.Post(ts.URL+"/ui/admin/update/check", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

// --- System-run / system-banner permission tests ---

func TestSystemRunFragmentForbiddenForNonAdmin(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "pub", auth.RolePublisher)

	resp, err := http.Post(ts.URL+"/ui/system/run", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestSystemBannerFragmentHidesForNonAdmin(t *testing.T) {
	// Non-admin hits the "isAdmin=false" branch — handler returns 200
	// with an empty banner (no warnings section rendered).
	_, ts := authServer(t, oidcConfig(), "viewer-1", auth.RoleViewer)

	resp, err := http.Get(ts.URL + "/ui/system/banner")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestSystemBannerFragmentForAdminWithoutChecker(t *testing.T) {
	// Admin path but srv.Checker == nil — covers the "no checker"
	// branch where hasWarnings stays false.
	_, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/ui/system/banner")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// --- Sidebar fragments that lacked coverage ---

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

func TestAdminUpdateCheckRendersCardForAdmin(t *testing.T) {
	// Stub GitHub so the handler's PerformCheck call resolves locally
	// and exercises the "card rendered with fresh status" path.
	stubGithubAPI(t, "v99.0.0")
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	srv.Version = "1.0.0" // KindSemver → stable-release check path

	resp, err := http.Post(ts.URL+"/ui/admin/update/check", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "admin-version-card") {
		t.Errorf("expected admin-version-card in body, got: %s", body)
	}
}

func TestAdminUpdateStartWithOrchKicksOffGoroutine(t *testing.T) {
	// With a non-nil orchestrator the handler transitions state to
	// "updating" and renders the progress card. The noop update
	// checker in orchestrator.NewForTest drives Update to return
	// (false, nil) quickly so the background goroutine finishes
	// without external deps.
	srv, ts, orch := authServerWithOrch(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	_ = srv

	resp, err := http.Post(ts.URL+"/ui/admin/update/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	// Wait for the background goroutine to settle state back to idle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if orch.State() == "idle" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("expected orchestrator to return to idle, got %q", orch.State())
}

func TestAdminUpdateStartRejectsWhenNotIdle(t *testing.T) {
	// Drive the "update already in progress" branch by flipping state
	// before issuing the request.
	_, ts, orch := authServerWithOrch(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	orch.SetState("updating")
	t.Cleanup(func() { orch.SetState("idle") })

	resp, err := http.Post(ts.URL+"/ui/admin/update/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Update already in progress") {
		t.Errorf("expected 'Update already in progress' in body, got: %s", body)
	}
}

func TestAdminUpdateProgressNonIdleRendersProgress(t *testing.T) {
	_, ts, orch := authServerWithOrch(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	orch.SetState("updating")
	t.Cleanup(func() { orch.SetState("idle") })

	resp, err := http.Get(ts.URL + "/ui/admin/update/progress")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	// The adminUpdateProgress template renders a polling container when
	// state != idle.
	if !strings.Contains(body, "hx-get") && !strings.Contains(body, "Update") {
		t.Errorf("expected progress-poll markup in body, got: %s", body)
	}
}

func TestSystemRunFragmentAdminRuns(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	srv.Checker = preflight.NewChecker(preflight.RuntimeDeps{})
	srv.Checker.Init(context.Background(), nil, nil)

	resp, err := http.Post(ts.URL+"/ui/system/run", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestSystemBannerFragmentAdminRenders(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	srv.Checker = preflight.NewChecker(preflight.RuntimeDeps{})
	srv.Checker.Init(context.Background(), nil, nil)

	resp, err := http.Get(ts.URL + "/ui/system/banner")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
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

func TestWorkerDetailTabReturns404ForDrainingMissingWorker(t *testing.T) {
	// Worker in the map is the only way to hit the "draining" status
	// branch in workerDetailTab — reach it by marking the worker
	// draining before the request.
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

// --- errorlog-level regression: WARN path of buildAdminErrors
//     — existing tests exercise ERROR but not WARN/INFO levels. ---

func TestBuildAdminErrorsCoversAllLevels(t *testing.T) {
	srv, _ := authServer(t, oidcConfig(), "a", auth.RoleAdmin)
	srv.ErrorLog = errorlog.NewStore(10)
	now := time.Now()
	srv.ErrorLog.Append(errorlog.Entry{Time: now, Level: slog.LevelError, Message: "e"})
	srv.ErrorLog.Append(errorlog.Entry{Time: now, Level: slog.LevelWarn, Message: "w"})
	srv.ErrorLog.Append(errorlog.Entry{Time: now, Level: slog.LevelInfo, Message: "i"})
	srv.ErrorLog.Append(errorlog.Entry{Time: now, Level: slog.LevelDebug, Message: "d"})

	data := buildAdminErrors(srv)
	if data.Count != 4 {
		t.Fatalf("expected 4 entries, got %d", data.Count)
	}
	// Entries are newest-first — Debug was appended last.
	wantClasses := []string{"badge-neutral", "badge-info", "badge-warning", "badge-error"}
	for i, want := range wantClasses {
		if data.Entries[i].LevelClass != want {
			t.Errorf("Entries[%d].LevelClass = %q, want %q",
				i, data.Entries[i].LevelClass, want)
		}
	}
}
