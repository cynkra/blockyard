package ui

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/appname"
	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
)

// uploadMode describes how uploaded files should be packaged for the bundle pipeline.
type uploadMode int

const (
	modeFiles   uploadMode = iota // individual files → tar.gz
	modeTarGz                     // single .tar.gz → passthrough
	modeZip                       // single .zip → repackage as tar.gz
)

// detectMode inspects the file headers and returns the appropriate packaging mode.
func detectMode(files []*multipart.FileHeader) uploadMode {
	if len(files) == 1 {
		name := strings.ToLower(files[0].Filename)
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			return modeTarGz
		}
		if strings.HasSuffix(name, ".zip") {
			return modeZip
		}
	}
	return modeFiles
}

// packageFiles streams individual multipart files into a tar.gz archive.
// The returned reader is backed by an io.Pipe — callers must read it to
// completion or close it to avoid leaking the writer goroutine.
func packageFiles(files []*multipart.FileHeader) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)
		var err error
		defer func() {
			tw.Close()
			gw.Close()
			pw.CloseWithError(err) //nolint:errcheck // best-effort
		}()
		for _, fh := range files {
			var f multipart.File
			f, err = fh.Open()
			if err != nil {
				return
			}
			err = tw.WriteHeader(&tar.Header{
				Name: fh.Filename,
				Size: fh.Size,
				Mode: 0644,
			})
			if err != nil {
				f.Close()
				return
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return
			}
		}
	}()
	return pr
}

// passThroughArchive returns a reader for a single tar.gz upload.
// The caller must close the returned ReadCloser when done.
func passThroughArchive(fh *multipart.FileHeader) (io.ReadCloser, error) {
	return fh.Open()
}

// repackageZip reads a zip archive and streams its entries as tar.gz.
// The returned reader is backed by an io.Pipe — callers must read it to
// completion or close it to avoid leaking the writer goroutine.
func repackageZip(fh *multipart.FileHeader) (io.ReadCloser, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	// multipart.File implements io.ReaderAt.
	ra, ok := f.(io.ReaderAt)
	if !ok {
		f.Close()
		return nil, fmt.Errorf("uploaded file does not support random access")
	}
	zr, err := zip.NewReader(ra, fh.Size)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("invalid zip archive: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)
		var writeErr error
		defer func() {
			tw.Close()
			gw.Close()
			pw.CloseWithError(writeErr) //nolint:errcheck // best-effort
			f.Close()
		}()
		for _, zf := range zr.File {
			// Guard against path traversal.
			clean := filepath.Clean(zf.Name)
			if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
				writeErr = fmt.Errorf("zip entry %q escapes root", zf.Name)
				return
			}
			if zf.FileInfo().IsDir() {
				continue
			}
			writeErr = tw.WriteHeader(&tar.Header{
				Name: clean,
				Size: int64(zf.UncompressedSize64), //nolint:gosec // G115: zip entries > 8 EiB are not realistic
				Mode: 0644,
			})
			if writeErr != nil {
				return
			}
			var rc io.ReadCloser
			rc, writeErr = zf.Open()
			if writeErr != nil {
				return
			}
			// Limit decompressed size to guard against zip bombs.
			// The bundle pipeline enforces its own 2 GiB limit downstream.
			_, writeErr = io.Copy(tw, io.LimitReader(rc, 2<<30)) //nolint:gosec // G110: bounded by LimitReader
			rc.Close()
			if writeErr != nil {
				return
			}
		}
	}()
	return pr, nil
}

// archiveReader returns an io.ReadCloser for the bundle archive based on the
// detected upload mode.
func archiveReader(files []*multipart.FileHeader, mode uploadMode) (io.ReadCloser, error) {
	switch mode {
	case modeTarGz:
		return passThroughArchive(files[0])
	case modeZip:
		return repackageZip(files[0])
	default:
		return packageFiles(files), nil
	}
}

// newAppForm serves the "New App" modal fragment.
func (ui *UI) newAppForm(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanCreateApp() {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["new_app_modal.html"].Execute(w, nil); err != nil {
			slog.Error("render new_app_modal", "err", err)
		}
	}
}

// createApp handles the combined create-app-and-upload-bundle POST.
func (ui *UI) createApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanCreateApp() {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		// Enforce bundle size limit on the raw body.
		r.Body = http.MaxBytesReader(w, r.Body, srv.Config.Storage.MaxBundleSize)

		if err := r.ParseMultipartForm(32 << 20); err != nil {
			renderUploadError(w, "Upload too large or invalid form data.")
			return
		}

		name := strings.TrimSpace(r.FormValue("name"))
		if err := appname.Validate(name); err != nil {
			renderUploadError(w, err.Error())
			return
		}

		files := r.MultipartForm.File["files"]
		if len(files) == 0 {
			renderUploadError(w, "No files selected.")
			return
		}

		// Check for duplicate app name.
		existing, err := srv.DB.GetAppByName(name)
		if err != nil {
			renderUploadError(w, "Internal error.")
			return
		}
		if existing != nil {
			renderUploadError(w, fmt.Sprintf("App name %q already exists.", name))
			return
		}

		// Create app.
		app, err := srv.DB.CreateApp(name, caller.Sub)
		if err != nil {
			renderUploadError(w, "Failed to create app.")
			return
		}
		slog.Info("app created", "app_id", app.ID, "name", app.Name, "owner", caller.Sub)
		if srv.AuditLog != nil {
			srv.AuditLog.Emit(audit.Entry{
				Action:   audit.ActionAppCreate,
				Actor:    caller.Sub,
				Target:   app.ID,
				Detail:   map[string]any{"name": app.Name},
				SourceIP: r.RemoteAddr,
			})
		}

		// From here on, roll back the app if anything fails.
		bundleID := uuid.New().String()
		taskID := uuid.New().String()
		paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, bundleID)

		cleanup := func() {
			bundle.DeleteFiles(paths)
			_ = srv.DB.HardDeleteApp(app.ID)
		}

		// Package uploaded files into a tar.gz reader.
		mode := detectMode(files)
		ar, err := archiveReader(files, mode)
		if err != nil {
			cleanup()
			renderUploadError(w, "Failed to read uploaded files.")
			return
		}

		if err := bundle.WriteArchive(paths, ar); err != nil {
			ar.Close()
			cleanup()
			renderUploadError(w, "Failed to store bundle.")
			return
		}
		ar.Close()

		if err := bundle.UnpackArchive(paths); err != nil {
			cleanup()
			renderUploadError(w, "Failed to unpack bundle.")
			return
		}

		if err := bundle.ValidateEntrypoint(paths); err != nil {
			cleanup()
			renderUploadError(w, "Missing entrypoint: the upload must contain app.R or server.R.")
			return
		}

		if err := bundle.CreateLibraryDir(paths); err != nil {
			cleanup()
			renderUploadError(w, "Internal error.")
			return
		}

		// Check manifest for pinned dependencies.
		bundlePinned := false
		manifestPath := filepath.Join(paths.Unpacked, "manifest.json")
		if m, mErr := manifest.Read(manifestPath); mErr == nil {
			bundlePinned = m.IsPinned()
		}

		if _, err := srv.DB.CreateBundle(bundleID, app.ID, caller.Sub, bundlePinned); err != nil {
			cleanup()
			renderUploadError(w, "Failed to create bundle.")
			return
		}

		// Spawn async restore.
		sender := srv.Tasks.Create(taskID, app.ID)
		bundle.SpawnRestore(bundle.RestoreParams{
			Backend:          srv.Backend,
			DB:               srv.DB,
			Tasks:            srv.Tasks,
			Sender:           sender,
			AppID:            app.ID,
			BundleID:         bundleID,
			Paths:            paths,
			Image:            server.AppImage(app, srv.Config.Docker.Image),
			PakVersion:       srv.Config.Docker.PakVersion,
			PakCachePath:     filepath.Join(srv.Config.Storage.BundleServerPath, ".pak-cache"),
			BuilderVersion:   srv.Version,
			BuilderCachePath: filepath.Join(srv.Config.Storage.BundleServerPath, ".by-builder-cache"),
			Retention:        srv.Config.Storage.BundleRetention,
			BasePath:         srv.Config.Storage.BundleServerPath,
			Store:            srv.PkgStore,
			AuditLog:         srv.AuditLog,
			AuditActor:       caller.Sub,
			WG:               srv.RestoreWG,
		})

		telemetry.BundlesUploaded.Inc()
		if srv.AuditLog != nil {
			srv.AuditLog.Emit(audit.Entry{
				Action:   audit.ActionBundleUpload,
				Actor:    caller.Sub,
				Target:   app.ID,
				Detail:   map[string]any{"bundle_id": bundleID},
				SourceIP: r.RemoteAddr,
			})
		}

		// Redirect to apps page — the new app appears in the grid.
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
	}
}

// renderUploadError writes an inline error alert for HTMX to swap into the modal.
func renderUploadError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<div class="alert alert-error text-sm"><span>%s</span></div>`, template.HTMLEscapeString(msg))
}
