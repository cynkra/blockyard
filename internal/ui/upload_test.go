package ui

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/config"
)

// --- Packaging helper tests ---

func TestDetectModeSingleTarGz(t *testing.T) {
	fh := &multipart.FileHeader{Filename: "app.tar.gz"}
	if m := detectMode([]*multipart.FileHeader{fh}); m != modeTarGz {
		t.Errorf("got %d, want modeTarGz", m)
	}
}

func TestDetectModeSingleTgz(t *testing.T) {
	fh := &multipart.FileHeader{Filename: "app.tgz"}
	if m := detectMode([]*multipart.FileHeader{fh}); m != modeTarGz {
		t.Errorf("got %d, want modeTarGz", m)
	}
}

func TestDetectModeSingleZip(t *testing.T) {
	fh := &multipart.FileHeader{Filename: "app.zip"}
	if m := detectMode([]*multipart.FileHeader{fh}); m != modeZip {
		t.Errorf("got %d, want modeZip", m)
	}
}

func TestDetectModeMultipleFiles(t *testing.T) {
	files := []*multipart.FileHeader{
		{Filename: "app.R"},
		{Filename: "helpers.R"},
	}
	if m := detectMode(files); m != modeFiles {
		t.Errorf("got %d, want modeFiles", m)
	}
}

func TestDetectModeSingleR(t *testing.T) {
	fh := &multipart.FileHeader{Filename: "app.R"}
	if m := detectMode([]*multipart.FileHeader{fh}); m != modeFiles {
		t.Errorf("got %d, want modeFiles", m)
	}
}

// --- Handler tests ---

func uploadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := oidcConfig()
	cfg.Storage.BundleServerPath = t.TempDir()
	cfg.Storage.MaxBundleSize = 10 << 20 // 10 MiB
	cfg.Storage.BundleRetention = 5
	cfg.Docker.PakVersion = "stable"
	return cfg
}

func buildMultipartForm(t *testing.T, name string, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if name != "" {
		mw.WriteField("name", name)
	}
	for fname, data := range files {
		fw, err := mw.CreateFormFile("files", fname)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(data)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func makeTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, data := range files {
		tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0644})
		tw.Write(data)
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func makeZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		fw, _ := zw.Create(name)
		fw.Write(data)
	}
	zw.Close()
	return buf.Bytes()
}

func TestNewAppFormRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	resp, err := http.Get(ts.URL + "/ui/apps/new")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Fragment handlers return 403 for unauthenticated requests;
	// the frontend handles session expiry via the htmx:beforeSwap overlay.
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403", resp.StatusCode)
	}
}

func TestNewAppFormRendersForPublisher(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "pub-user", auth.RolePublisher)

	resp, err := http.Get(ts.URL + "/ui/apps/new")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "new-app-modal") {
		t.Error("expected modal dialog in response")
	}
}

func TestNewAppFormForbiddenForViewer(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "view-user", auth.RoleViewer)

	resp, err := http.Get(ts.URL + "/ui/apps/new")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403", resp.StatusCode)
	}
}

func TestCreateAppUploadSuccess(t *testing.T) {
	_, ts := authServer(t, uploadConfig(t), "pub-user", auth.RolePublisher)

	archive := makeTarGz(t, map[string][]byte{"app.R": []byte("library(shiny)\n")})
	body, ct := buildMultipartForm(t, "test-app", map[string][]byte{"app.tar.gz": archive})

	resp, err := noRedirectClient().Post(ts.URL+"/ui/apps/new", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
	}
	redirect := resp.Header.Get("HX-Redirect")
	if redirect != "/app/test-app/" {
		t.Errorf("HX-Redirect = %q, want /app/test-app/", redirect)
	}
}

func TestCreateAppUploadZip(t *testing.T) {
	_, ts := authServer(t, uploadConfig(t), "pub-user", auth.RolePublisher)

	archive := makeZip(t, map[string][]byte{"app.R": []byte("library(shiny)\n")})
	body, ct := buildMultipartForm(t, "zip-app", map[string][]byte{"app.zip": archive})

	resp, err := noRedirectClient().Post(ts.URL+"/ui/apps/new", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("HX-Redirect"); got != "/app/zip-app/" {
		t.Errorf("HX-Redirect = %q, want /app/zip-app/", got)
	}
}

func TestCreateAppUploadIndividualFiles(t *testing.T) {
	_, ts := authServer(t, uploadConfig(t), "pub-user", auth.RolePublisher)

	body, ct := buildMultipartForm(t, "files-app", map[string][]byte{
		"app.R": []byte("library(shiny)\n"),
	})

	resp, err := noRedirectClient().Post(ts.URL+"/ui/apps/new", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("HX-Redirect"); got != "/app/files-app/" {
		t.Errorf("HX-Redirect = %q, want /app/files-app/", got)
	}
}

func TestCreateAppUploadDuplicateName(t *testing.T) {
	srv, ts := authServer(t, uploadConfig(t), "pub-user", auth.RolePublisher)
	srv.DB.CreateApp("dupe-app", "pub-user")

	archive := makeTarGz(t, map[string][]byte{"app.R": []byte("1\n")})
	body, ct := buildMultipartForm(t, "dupe-app", map[string][]byte{"app.tar.gz": archive})

	resp, err := http.Post(ts.URL+"/ui/apps/new", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	respBody := readBody(t, resp)
	if !strings.Contains(respBody, "already exists") {
		t.Errorf("expected 'already exists' error, got: %s", respBody)
	}
}

func TestCreateAppUploadNoFiles(t *testing.T) {
	_, ts := authServer(t, uploadConfig(t), "pub-user", auth.RolePublisher)

	body, ct := buildMultipartForm(t, "no-files", map[string][]byte{})

	resp, err := http.Post(ts.URL+"/ui/apps/new", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	respBody := readBody(t, resp)
	if !strings.Contains(respBody, "No files") {
		t.Errorf("expected 'No files' error, got: %s", respBody)
	}
}

func TestCreateAppUploadMissingEntrypoint(t *testing.T) {
	srv, ts := authServer(t, uploadConfig(t), "pub-user", auth.RolePublisher)

	archive := makeTarGz(t, map[string][]byte{"helpers.R": []byte("1\n")})
	body, ct := buildMultipartForm(t, "no-entry", map[string][]byte{"bundle.tar.gz": archive})

	resp, err := http.Post(ts.URL+"/ui/apps/new", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	respBody := readBody(t, resp)
	if !strings.Contains(respBody, "app.R or server.R") {
		t.Errorf("expected entrypoint error, got: %s", respBody)
	}

	// App should have been rolled back.
	app, _ := srv.DB.GetAppByName("no-entry")
	if app != nil {
		t.Error("expected app to be rolled back after failed upload")
	}
}

func TestCreateAppUploadForbiddenForViewer(t *testing.T) {
	_, ts := authServer(t, uploadConfig(t), "view-user", auth.RoleViewer)

	archive := makeTarGz(t, map[string][]byte{"app.R": []byte("1\n")})
	body, ct := buildMultipartForm(t, "viewer-app", map[string][]byte{"app.tar.gz": archive})

	resp, err := http.Post(ts.URL+"/ui/apps/new", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403", resp.StatusCode)
	}
}

func TestCreateAppUploadInvalidName(t *testing.T) {
	_, ts := authServer(t, uploadConfig(t), "pub-user", auth.RolePublisher)

	archive := makeTarGz(t, map[string][]byte{"app.R": []byte("1\n")})
	body, ct := buildMultipartForm(t, "Bad-Name!", map[string][]byte{"app.tar.gz": archive})

	resp, err := http.Post(ts.URL+"/ui/apps/new", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	respBody := readBody(t, resp)
	if !strings.Contains(respBody, "alert-error") {
		t.Errorf("expected error alert, got: %s", respBody)
	}
}

// --- Packaging helper integration tests ---

func TestPackageFilesProducesValidTarGz(t *testing.T) {
	// Build a multipart form and extract the file headers.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", "app.R")
	fw.Write([]byte("library(shiny)\n"))
	fw2, _ := mw.CreateFormFile("files", "helpers.R")
	fw2.Write([]byte("helper <- 1\n"))
	mw.Close()

	mr := multipart.NewReader(&buf, mw.Boundary())
	form, err := mr.ReadForm(10 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer form.RemoveAll()

	rc := packageFiles(form.File["files"])
	defer rc.Close()

	// Decompress and verify entries.
	gr, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatal("gzip open:", err)
	}
	tr := tar.NewReader(gr)

	names := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("tar next:", err)
		}
		names[hdr.Name] = true
	}

	if !names["app.R"] || !names["helpers.R"] {
		t.Errorf("expected app.R and helpers.R in tar, got %v", names)
	}
}
