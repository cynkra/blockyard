package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Manifest Validate ---

func TestManifestValidate_PinnedValid(t *testing.T) {
	m := Manifest{
		Version:  1,
		Metadata: Metadata{Entrypoint: "app.R"},
		Packages: map[string]Package{
			"shiny": {Package: "shiny", Version: "1.9.1", Source: "Repository"},
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestManifestValidate_UnpinnedValid(t *testing.T) {
	m := Manifest{
		Version:     1,
		Metadata:    Metadata{Entrypoint: "app.R"},
		Description: map[string]string{"Imports": "shiny"},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestManifestValidate_BothPackagesAndDescription(t *testing.T) {
	m := Manifest{
		Version:     1,
		Metadata:    Metadata{Entrypoint: "app.R"},
		Packages:    map[string]Package{"shiny": {Package: "shiny", Version: "1.9.1", Source: "Repository"}},
		Description: map[string]string{"Imports": "shiny"},
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected error for both packages and description")
	}
	if !strings.Contains(err.Error(), "both packages and description") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestManifestValidate_UnknownVersion(t *testing.T) {
	m := Manifest{
		Version:  99,
		Metadata: Metadata{Entrypoint: "app.R"},
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected error for unknown version")
	}
	if !strings.Contains(err.Error(), "unsupported manifest version") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestManifestValidate_MissingEntrypoint(t *testing.T) {
	m := Manifest{Version: 1, Metadata: Metadata{}}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected error for missing entrypoint")
	}
	if !strings.Contains(err.Error(), "entrypoint") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestManifestIsPinned(t *testing.T) {
	pinned := Manifest{Packages: map[string]Package{"x": {}}}
	if !pinned.IsPinned() {
		t.Error("expected IsPinned() = true")
	}
	unpinned := Manifest{Description: map[string]string{"Imports": "x"}}
	if unpinned.IsPinned() {
		t.Error("expected IsPinned() = false")
	}
}

func TestManifestBuildMode(t *testing.T) {
	pinned := Manifest{Packages: map[string]Package{"x": {}}}
	if pinned.BuildMode() != BuildModePinned {
		t.Error("expected BuildModePinned")
	}
	unpinned := Manifest{}
	if unpinned.BuildMode() != BuildModeUnpinned {
		t.Error("expected BuildModeUnpinned")
	}
}

func TestBuildModeString(t *testing.T) {
	if BuildModePinned.String() != "pinned" {
		t.Error("expected 'pinned'")
	}
	if BuildModeUnpinned.String() != "unpinned" {
		t.Error("expected 'unpinned'")
	}
}

// --- Manifest Read / Write round-trip ---

func TestManifestReadWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	m := &Manifest{
		Version:  1,
		RVersion: "4.4.2",
		Metadata: Metadata{AppMode: "shiny", Entrypoint: "app.R"},
		Repositories: []Repository{
			{Name: "CRAN", URL: "https://p3m.dev/cran/latest"},
		},
		Description: map[string]string{"Imports": "shiny, ggplot2"},
		Files:       map[string]FileInfo{"app.R": {Checksum: "abc123"}},
	}

	if err := m.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.RVersion != "4.4.2" {
		t.Errorf("RVersion = %q, want %q", got.RVersion, "4.4.2")
	}
	if got.Metadata.AppMode != "shiny" {
		t.Errorf("AppMode = %q, want %q", got.Metadata.AppMode, "shiny")
	}
	if got.Description["Imports"] != "shiny, ggplot2" {
		t.Errorf("Description.Imports = %q", got.Description["Imports"])
	}
	if len(got.Repositories) != 1 || got.Repositories[0].Name != "CRAN" {
		t.Errorf("Repositories = %v", got.Repositories)
	}
}

// --- Package Validate ---

func TestPackageValidate_CRAN(t *testing.T) {
	p := Package{Package: "shiny", Version: "1.9.1", Source: "Repository"}
	if err := p.Validate(); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}

func TestPackageValidate_Bioconductor(t *testing.T) {
	p := Package{Package: "GenomicRanges", Version: "1.56.0", Source: "Bioconductor"}
	if err := p.Validate(); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}

func TestPackageValidate_GitHub(t *testing.T) {
	p := Package{
		Package:        "mypkg",
		Source:         "GitHub",
		RemoteUsername: "owner",
		RemoteRepo:     "mypkg",
		RemoteSha:      "abc123",
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}

func TestPackageValidate_GitHubMissingSha(t *testing.T) {
	p := Package{
		Package:        "mypkg",
		Source:         "GitHub",
		RemoteUsername: "owner",
		RemoteRepo:     "mypkg",
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error for missing RemoteSha")
	}
	if !strings.Contains(err.Error(), "RemoteSha") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPackageValidate_Git(t *testing.T) {
	p := Package{Package: "mypkg", Source: "git", RemoteUrl: "https://git.example.com/repo.git"}
	if err := p.Validate(); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}

func TestPackageValidate_GitMissingUrl(t *testing.T) {
	p := Package{Package: "mypkg", Source: "git"}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error for missing RemoteUrl")
	}
	if !strings.Contains(err.Error(), "RemoteUrl") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPackageValidate_MissingSource(t *testing.T) {
	p := Package{Package: "mypkg"}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error for missing Source")
	}
}

func TestPackageValidate_UnsupportedSource(t *testing.T) {
	p := Package{Package: "mypkg", Source: "Local"}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error for unsupported Source")
	}
	if !strings.Contains(err.Error(), "Local") {
		t.Errorf("expected Source name in error: %v", err)
	}
}

// --- FromRenvLock ---

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, data, 0o644)
}

func TestFromRenvLock_BasicCRAN(t *testing.T) {
	dir := t.TempDir()
	lock := map[string]any{
		"R": map[string]any{
			"Version": "4.4.2",
			"Repositories": []map[string]string{
				{"Name": "CRAN", "URL": "https://p3m.dev/cran/2026-03-18"},
			},
		},
		"Packages": map[string]map[string]string{
			"shiny": {"Package": "shiny", "Version": "1.9.1", "Source": "Repository"},
		},
	}
	lockPath := filepath.Join(dir, "renv.lock")
	writeJSON(t, lockPath, lock)

	m, err := FromRenvLock(lockPath,
		Metadata{AppMode: "shiny", Entrypoint: "app.R"},
		map[string]FileInfo{"app.R": {Checksum: "abc"}})
	if err != nil {
		t.Fatalf("FromRenvLock: %v", err)
	}

	if m.RVersion != "4.4.2" {
		t.Errorf("RVersion = %q", m.RVersion)
	}
	if !m.IsPinned() {
		t.Error("expected pinned manifest")
	}
	pkg, ok := m.Packages["shiny"]
	if !ok {
		t.Fatal("shiny not in Packages")
	}
	if pkg.Version != "1.9.1" {
		t.Errorf("shiny.Version = %q", pkg.Version)
	}
}

func TestFromRenvLock_GitHubPackage(t *testing.T) {
	dir := t.TempDir()
	lock := map[string]any{
		"R": map[string]any{
			"Version":      "4.4.2",
			"Repositories": []map[string]string{},
		},
		"Packages": map[string]map[string]string{
			"mypkg": {
				"Package":        "mypkg",
				"Version":        "0.3.1",
				"Source":         "GitHub",
				"RemoteType":     "github",
				"RemoteHost":     "api.github.com",
				"RemoteUsername": "owner",
				"RemoteRepo":     "mypkg",
				"RemoteRef":      "main",
				"RemoteSha":      "abc123def456",
			},
		},
	}
	lockPath := filepath.Join(dir, "renv.lock")
	writeJSON(t, lockPath, lock)

	m, err := FromRenvLock(lockPath,
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{})
	if err != nil {
		t.Fatalf("FromRenvLock: %v", err)
	}

	pkg := m.Packages["mypkg"]
	if pkg.RemoteUsername != "owner" {
		t.Errorf("RemoteUsername = %q", pkg.RemoteUsername)
	}
	if pkg.RemoteSha != "abc123def456" {
		t.Errorf("RemoteSha = %q", pkg.RemoteSha)
	}
}

func TestFromRenvLock_Repositories(t *testing.T) {
	dir := t.TempDir()
	lock := map[string]any{
		"R": map[string]any{
			"Version": "4.4.2",
			"Repositories": []map[string]string{
				{"Name": "CRAN", "URL": "https://p3m.dev/cran/2026-03-18"},
				{"Name": "BioCsoft", "URL": "https://bioconductor.org/packages/3.19/bioc"},
			},
		},
		"Packages": map[string]map[string]string{
			"shiny": {"Package": "shiny", "Version": "1.9.1", "Source": "Repository"},
		},
	}
	lockPath := filepath.Join(dir, "renv.lock")
	writeJSON(t, lockPath, lock)

	m, err := FromRenvLock(lockPath,
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Repositories) != 2 {
		t.Fatalf("expected 2 repositories, got %d", len(m.Repositories))
	}
	if m.Repositories[0].Name != "CRAN" {
		t.Errorf("first repo = %q", m.Repositories[0].Name)
	}
}

func TestFromRenvLock_V2Format(t *testing.T) {
	// v2 lockfiles embed full DESCRIPTION per package — extra fields should be dropped.
	dir := t.TempDir()
	lock := map[string]any{
		"R": map[string]any{
			"Version":      "4.4.2",
			"Repositories": []map[string]string{},
		},
		"Packages": map[string]map[string]string{
			"shiny": {
				"Package":     "shiny",
				"Version":     "1.9.1",
				"Source":      "Repository",
				"Title":       "Web Application Framework for R",
				"Authors@R":   "person(\"Joe\", role = \"aut\")",
				"Description": "Makes it easy to build interactive web apps.",
			},
		},
	}
	lockPath := filepath.Join(dir, "renv.lock")
	writeJSON(t, lockPath, lock)

	m, err := FromRenvLock(lockPath,
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{})
	if err != nil {
		t.Fatal(err)
	}
	// Extra fields should have been silently dropped.
	if m.Packages["shiny"].Version != "1.9.1" {
		t.Errorf("Version = %q", m.Packages["shiny"].Version)
	}
}

func TestFromRenvLock_MissingRemoteSha(t *testing.T) {
	dir := t.TempDir()
	lock := map[string]any{
		"R": map[string]any{
			"Version":      "4.4.2",
			"Repositories": []map[string]string{},
		},
		"Packages": map[string]map[string]string{
			"mypkg": {
				"Package":        "mypkg",
				"Source":         "GitHub",
				"RemoteUsername": "owner",
				"RemoteRepo":     "mypkg",
			},
		},
	}
	lockPath := filepath.Join(dir, "renv.lock")
	writeJSON(t, lockPath, lock)

	_, err := FromRenvLock(lockPath,
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{})
	if err == nil {
		t.Fatal("expected error for missing RemoteSha")
	}
}

func TestFromRenvLock_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "renv.lock")
	os.WriteFile(lockPath, []byte("{invalid json"), 0o644)

	_, err := FromRenvLock(lockPath,
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// --- FromDescription ---

func TestFromDescription_ImportsOnly(t *testing.T) {
	dir := t.TempDir()
	descPath := filepath.Join(dir, "DESCRIPTION")
	os.WriteFile(descPath, []byte("Package: myapp\nVersion: 0.1.0\nImports: shiny, ggplot2\n"), 0o644)

	m, err := FromDescription(descPath,
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{},
		[]Repository{{Name: "CRAN", URL: "https://p3m.dev/cran/latest"}})
	if err != nil {
		t.Fatalf("FromDescription: %v", err)
	}
	if m.IsPinned() {
		t.Error("expected unpinned manifest")
	}
	if m.Description["Imports"] != "shiny, ggplot2" {
		t.Errorf("Imports = %q", m.Description["Imports"])
	}
}

func TestFromDescription_WithRemotes(t *testing.T) {
	dir := t.TempDir()
	descPath := filepath.Join(dir, "DESCRIPTION")
	os.WriteFile(descPath, []byte("Package: myapp\nImports: shiny\nRemotes: blockr-org/blockr\n"), 0o644)

	m, err := FromDescription(descPath,
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Description["Remotes"] != "blockr-org/blockr" {
		t.Errorf("Remotes = %q", m.Description["Remotes"])
	}
}

func TestFromDescription_ContinuationLines(t *testing.T) {
	dir := t.TempDir()
	descPath := filepath.Join(dir, "DESCRIPTION")
	content := "Package: myapp\nImports:\n    shiny (>= 1.8.0),\n    ggplot2,\n    DT\n"
	os.WriteFile(descPath, []byte(content), 0o644)

	m, err := FromDescription(descPath,
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	imp := m.Description["Imports"]
	if !strings.Contains(imp, "shiny") || !strings.Contains(imp, "DT") {
		t.Errorf("Imports = %q", imp)
	}
}

func TestFromDescription_MissingFile(t *testing.T) {
	_, err := FromDescription("/nonexistent/DESCRIPTION",
		Metadata{Entrypoint: "app.R"},
		map[string]FileInfo{}, nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// --- parseDCF ---

func TestParseDCF_BasicFields(t *testing.T) {
	data := []byte("Package: myapp\nVersion: 0.1.0\nTitle: My App\n")
	fields := parseDCF(data)
	if fields["Package"] != "myapp" {
		t.Errorf("Package = %q", fields["Package"])
	}
	if fields["Version"] != "0.1.0" {
		t.Errorf("Version = %q", fields["Version"])
	}
}

// --- PakRef / PakRefs / RepoLines ---

func TestPakRef(t *testing.T) {
	tests := []struct {
		name string
		pkg  Package
		want string
	}{
		{"Repository", Package{Package: "shiny", Version: "1.9.1", Source: "Repository"}, "shiny@1.9.1"},
		{"Bioconductor", Package{Package: "Biobase", Version: "2.60.0", Source: "Bioconductor"}, "bioc::Biobase@2.60.0"},
		{"GitHub", Package{Package: "dplyr", Source: "GitHub", RemoteUsername: "tidyverse", RemoteRepo: "dplyr", RemoteSha: "abc123"}, "tidyverse/dplyr@abc123"},
		{"GitLab", Package{Package: "x", Source: "GitLab", RemoteUsername: "u", RemoteRepo: "r", RemoteSha: "def"}, "gitlab::u/r@def"},
		{"Bitbucket", Package{Package: "x", Source: "Bitbucket", RemoteUsername: "u", RemoteRepo: "r", RemoteSha: "eee"}, "bitbucket::u/r@eee"},
		{"git", Package{Package: "x", Source: "git", RemoteUrl: "https://example.com/repo.git"}, "git::https://example.com/repo.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pkg.PakRef(); got != tt.want {
				t.Fatalf("PakRef() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPakRefs_Pinned(t *testing.T) {
	m := Manifest{
		Packages: map[string]Package{
			"shiny": {Package: "shiny", Version: "1.9.1", Source: "Repository"},
		},
	}
	refs := m.PakRefs()
	if len(refs) != 1 || refs[0] != "shiny@1.9.1" {
		t.Fatalf("PakRefs() = %v", refs)
	}
}

func TestPakRefs_Unpinned(t *testing.T) {
	m := Manifest{
		Description: map[string]string{"Imports": "shiny"},
	}
	refs := m.PakRefs()
	if len(refs) != 1 || refs[0] != "deps::/app" {
		t.Fatalf("PakRefs() = %v", refs)
	}
}

func TestRepoLines(t *testing.T) {
	m := Manifest{
		Repositories: []Repository{
			{Name: "CRAN", URL: "https://cloud.r-project.org"},
			{Name: "BioCsoft", URL: "https://bioconductor.org/packages/release/bioc"},
		},
	}
	lines := m.RepoLines()
	if len(lines) != 2 {
		t.Fatalf("RepoLines() = %v", lines)
	}
	if lines[0] != "CRAN=https://cloud.r-project.org" {
		t.Fatalf("lines[0] = %q", lines[0])
	}
}

func TestRepoLines_Empty(t *testing.T) {
	m := Manifest{}
	if lines := m.RepoLines(); len(lines) != 0 {
		t.Fatalf("expected empty, got %v", lines)
	}
}

func TestParseDCF_ContinuationLines(t *testing.T) {
	data := []byte("Imports:\n    shiny,\n    ggplot2\n")
	fields := parseDCF(data)
	if !strings.Contains(fields["Imports"], "shiny") {
		t.Errorf("Imports = %q", fields["Imports"])
	}
	if !strings.Contains(fields["Imports"], "ggplot2") {
		t.Errorf("Imports = %q", fields["Imports"])
	}
}

func TestParseDCF_EmptyLines(t *testing.T) {
	data := []byte("Package: myapp\n\nVersion: 0.1.0\n")
	fields := parseDCF(data)
	if fields["Package"] != "myapp" {
		t.Errorf("Package = %q", fields["Package"])
	}
	if fields["Version"] != "0.1.0" {
		t.Errorf("Version = %q", fields["Version"])
	}
}
