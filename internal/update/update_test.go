package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestClassifyVersion(t *testing.T) {
	tests := []struct {
		in       string
		wantKind Kind
		wantSHA  string
	}{
		{"v1.2.3", KindSemver, ""},
		{"1.2.3", KindSemver, ""},
		{"0.0.2", KindSemver, ""},
		// git describe outputs
		{"v0.0.2-3-gabc1234", KindSHA, "abc1234"},
		{"v0.0.2-3-gabc1234-dirty", KindSHA, "abc1234"},
		{"abc1234", KindSHA, "abc1234"},
		{"abc1234-dirty", KindSHA, "abc1234"},
		{"abcdef0123456789abcdef0123456789abcdef01", KindSHA, "abcdef0123456789abcdef0123456789abcdef01"},
		// Legacy main+ format
		{"main+abc1234", KindSHA, "abc1234"},
		// Anything else
		{"dev", KindUnknown, ""},
		{"", KindUnknown, ""},
		{"some-random-string", KindUnknown, ""},
		// 6 hex is too short to be a SHA
		{"abc123", KindUnknown, ""},
	}
	for _, tt := range tests {
		gotKind, gotSHA := classifyVersion(tt.in)
		if gotKind != tt.wantKind || gotSHA != tt.wantSHA {
			t.Errorf("classifyVersion(%q) = (%v, %q), want (%v, %q)",
				tt.in, gotKind, gotSHA, tt.wantKind, tt.wantSHA)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b [3]int
		want int
	}{
		{[3]int{1, 0, 0}, [3]int{1, 0, 0}, 0},
		{[3]int{1, 0, 0}, [3]int{1, 0, 1}, -1},
		{[3]int{1, 0, 1}, [3]int{1, 0, 0}, 1},
		{[3]int{1, 1, 0}, [3]int{1, 0, 5}, 1},
		{[3]int{2, 0, 0}, [3]int{1, 99, 99}, 1},
		{[3]int{0, 0, 1}, [3]int{0, 0, 2}, -1},
	}
	for _, tt := range tests {
		if got := compareSemver(tt.a, tt.b); got != tt.want {
			t.Errorf("compareSemver(%v, %v) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// fakeRemote is a minimal stub of the GitHub endpoints we hit. It
// records request paths so tests can assert which calls happened.
type fakeRemote struct {
	server      *httptest.Server
	latestTag   string         // /releases/latest TagName
	mainRelName string         // /releases/tags/main Name
	compareResp *CompareResult // /compare/{base}...{head} payload
	compare404  bool           // when true, /compare returns 404
	called      map[string]int // path → count
}

func newFakeRemote() *fakeRemote {
	f := &fakeRemote{called: map[string]int{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.called[r.URL.Path]++
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			json.NewEncoder(w).Encode(GitHubRelease{TagName: f.latestTag})
		case strings.HasSuffix(r.URL.Path, "/releases/tags/main"):
			json.NewEncoder(w).Encode(GitHubRelease{Name: f.mainRelName})
		case strings.HasPrefix(r.URL.Path, "/compare/"):
			if f.compare404 {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(f.compareResp)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	return f
}

func (f *fakeRemote) Close() { f.server.Close() }

// install points the package APIBase at the fake server for the
// duration of the test.
func (f *fakeRemote) install(t *testing.T) {
	t.Helper()
	old := APIBase
	APIBase = f.server.URL
	t.Cleanup(func() { APIBase = old })
}

func TestCheckLatest_Stable_Semver_UpdateAvailable(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.latestTag = "v1.5.0"
	f.install(t)

	res, err := CheckLatest("1.4.0", "stable")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateUpdateAvailable {
		t.Errorf("State = %q, want %q", res.State, StateUpdateAvailable)
	}
	if res.LatestVersion != "1.5.0" {
		t.Errorf("LatestVersion = %q", res.LatestVersion)
	}
	if res.Channel != "stable" {
		t.Errorf("Channel = %q, want %q", res.Channel, "stable")
	}
}

func TestCheckLatest_Stable_Semver_UpToDate(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.latestTag = "v1.5.0"
	f.install(t)

	res, _ := CheckLatest("1.5.0", "stable")
	if res.State != StateUpToDate {
		t.Errorf("State = %q, want %q", res.State, StateUpToDate)
	}
}

func TestCheckLatest_Stable_Semver_Ahead(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.latestTag = "v1.4.0"
	f.install(t)

	res, _ := CheckLatest("v1.5.0", "stable")
	if res.State != StateAhead {
		t.Errorf("State = %q, want %q", res.State, StateAhead)
	}
	if res.Detail == "" {
		t.Error("expected non-empty Detail describing the ahead-of-release state")
	}
}

// SHA-build running on stable channel: no numeric comparison, just
// string-equality against the latest tag. Different → update available.
func TestCheckLatest_Stable_SHA_UpdateAvailable(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.latestTag = "v1.5.0"
	f.install(t)

	res, _ := CheckLatest("abc1234", "stable")
	if res.State != StateUpdateAvailable {
		t.Errorf("State = %q, want %q", res.State, StateUpdateAvailable)
	}
	if res.LatestVersion != "1.5.0" {
		t.Errorf("LatestVersion = %q, want 1.5.0", res.LatestVersion)
	}
}

func TestCheckLatest_Main_SHA_Behind(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.mainRelName = "v1.5.0-3-gfeedbee"
	f.compareResp = &CompareResult{Status: "ahead", AheadBy: 3}
	f.install(t)

	res, _ := CheckLatest("abc1234", "main")
	if res.State != StateUpdateAvailable {
		t.Errorf("State = %q, want %q (target ahead means we are behind)", res.State, StateUpdateAvailable)
	}
	if !strings.Contains(res.Detail, "3 commits behind") {
		t.Errorf("Detail = %q, expected 3-commits-behind note", res.Detail)
	}
	if res.LatestVersion != "v1.5.0-3-gfeedbee" {
		t.Errorf("LatestVersion = %q, want %q", res.LatestVersion, "v1.5.0-3-gfeedbee")
	}
}

func TestCheckLatest_Main_SHA_Ahead(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.mainRelName = "v1.4.0-1-gabc1234"
	f.compareResp = &CompareResult{Status: "behind", BehindBy: 2}
	f.install(t)

	res, _ := CheckLatest("def5678", "main")
	if res.State != StateAhead {
		t.Errorf("State = %q, want %q", res.State, StateAhead)
	}
}

func TestCheckLatest_Main_SHA_Identical(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.mainRelName = "v1.4.0-1-gabc1234"
	f.compareResp = &CompareResult{Status: "identical"}
	f.install(t)

	res, _ := CheckLatest("abc1234", "main")
	if res.State != StateUpToDate {
		t.Errorf("State = %q, want %q", res.State, StateUpToDate)
	}
}

func TestCheckLatest_Main_SHA_Diverged(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.mainRelName = "v1.4.0-1-gabc1234"
	f.compareResp = &CompareResult{Status: "diverged", AheadBy: 2, BehindBy: 1}
	f.install(t)

	res, _ := CheckLatest("def5678", "main")
	if res.State != StateDiverged {
		t.Errorf("State = %q, want %q", res.State, StateDiverged)
	}
}

func TestCheckLatest_Main_SHA_LocalNotFound(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.mainRelName = "v1.4.0-1-gabc1234"
	f.compare404 = true
	f.install(t)

	res, _ := CheckLatest("def5678", "main")
	if res.State != StateLocalNotFound {
		t.Errorf("State = %q, want %q", res.State, StateLocalNotFound)
	}
}

// Semver current on main channel: skips the commit-graph path and
// falls back to string equality.
func TestCheckLatest_Main_Semver_UpdateAvailable(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.mainRelName = "v1.5.0-2-gabc1234"
	f.install(t)

	res, _ := CheckLatest("1.4.0", "main")
	if res.State != StateUpdateAvailable {
		t.Errorf("State = %q, want %q", res.State, StateUpdateAvailable)
	}
	// Did not hit the compare API.
	for path := range f.called {
		if strings.HasPrefix(path, "/compare/") {
			t.Errorf("unexpected /compare call for semver-on-main path")
		}
	}
}

func TestCheckLatest_DevBuild_NoUpdateOffer(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.latestTag = "v0.0.2"
	f.install(t)

	res, _ := CheckLatest("dev", "stable")
	if res.State != StateDevBuild {
		t.Errorf("State = %q, want %q", res.State, StateDevBuild)
	}
	// The channel's latest is fetched for informational display,
	// not as an "update" offer.
	if res.LatestVersion != "0.0.2" {
		t.Errorf("LatestVersion = %q, want 0.0.2 (informational)", res.LatestVersion)
	}
}

func TestCheckLatest_NoRemote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	res, err := CheckLatest("1.4.0", "stable")
	if err != nil {
		t.Fatalf("expected error folded into result, got %v", err)
	}
	if res.State != StateNoRemote {
		t.Errorf("State = %q, want %q", res.State, StateNoRemote)
	}
}

func TestSetRepo(t *testing.T) {
	old := APIBase
	defer func() { APIBase = old }()

	SetRepo("acme/widget")
	if !strings.HasSuffix(APIBase, "/repos/acme/widget") {
		t.Errorf("APIBase = %q, expected /repos/acme/widget suffix", APIBase)
	}
}

// FetchInstallTarget for the main channel returns the rolling tag
// "main" without consulting GitHub — the orchestrator pulls :main
// and digest-compares to determine "already up to date." This is
// the contract that issue #360 establishes; regression here would
// re-introduce the broken per-commit tag.
func TestFetchInstallTarget_Main(t *testing.T) {
	// Install a fake remote that fails any call. The main channel
	// must not hit GitHub at all.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("main channel should not call GitHub: %s", r.URL.Path)
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	target, err := FetchInstallTarget("main", "abc1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != "main" {
		t.Errorf("target = %q, want %q", target, "main")
	}
}

func TestFetchInstallTarget_StableUpdateAvailable(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.latestTag = "v1.5.0"
	f.install(t)

	target, err := FetchInstallTarget("stable", "1.4.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != "1.5.0" {
		t.Errorf("target = %q, want %q", target, "1.5.0")
	}
}

func TestFetchInstallTarget_StableAlreadyCurrent(t *testing.T) {
	f := newFakeRemote()
	defer f.Close()
	f.latestTag = "v1.5.0"
	f.install(t)

	target, err := FetchInstallTarget("stable", "1.5.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != "" {
		t.Errorf("target = %q, want empty (already current)", target)
	}
}

func TestFetchRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("expected Accept header, got %q", r.Header.Get("Accept"))
		}
		json.NewEncoder(w).Encode(GitHubRelease{
			TagName: "v1.2.3",
			Assets:  []GitHubAsset{{Name: "blockyard-linux-amd64.tar.gz"}},
		})
	}))
	defer srv.Close()

	rel, err := FetchRelease(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName = %q", rel.TagName)
	}
	if len(rel.Assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(rel.Assets))
	}
}

func TestAddGitHubAuth_WithToken(t *testing.T) {
	old := os.Getenv("GITHUB_TOKEN")
	os.Setenv("GITHUB_TOKEN", "test-token-123")
	defer func() {
		if old == "" {
			os.Unsetenv("GITHUB_TOKEN")
		} else {
			os.Setenv("GITHUB_TOKEN", old)
		}
	}()

	req, _ := http.NewRequest("GET", "https://example.com", nil)
	AddGitHubAuth(req)

	if got := req.Header.Get("Authorization"); got != "Bearer test-token-123" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestAddGitHubAuth_NoToken(t *testing.T) {
	old := os.Getenv("GITHUB_TOKEN")
	os.Unsetenv("GITHUB_TOKEN")
	defer func() {
		if old != "" {
			os.Setenv("GITHUB_TOKEN", old)
		}
	}()

	req, _ := http.NewRequest("GET", "https://example.com", nil)
	AddGitHubAuth(req)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("expected no Authorization header, got %q", got)
	}
}
