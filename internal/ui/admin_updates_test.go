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
	updatepkg "github.com/cynkra/blockyard/internal/update"
)

// authServerWithOrch is authServer but registers the UI routes with a
// non-nil orchestrator so the "updates supported" branches in the
// admin update handlers become reachable.
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

// --- adminVersionData helpers ---

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

// --- Admin /admin querystring helpers ---

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

// --- Admin Updates tab: GET /ui/admin/tab/version ---

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

// --- Admin Updates tab: POST /ui/admin/update/check ---

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

// --- Admin Updates tab: POST /ui/admin/update/start ---

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

// --- Admin Updates tab: GET /ui/admin/update/progress ---

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

// --- Admin system checks: /ui/system/run and /ui/system/banner ---

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

// --- Admin errors fragment: level-class helpers ---

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

func TestBuildAdminErrorsCoversAllLevels(t *testing.T) {
	// Existing tests exercise ERROR only; this one exercises WARN/INFO/
	// DEBUG so every branch of errorLevelBadgeClass is hit through the
	// real buildAdminErrors flow.
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

// --- parseDuration (used by the profile-page token creator) ---

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
