package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

// MakeBundle returns a valid tar.gz containing a single app.R file.
func MakeBundle(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte("library(shiny)\nshinyApp(ui, server)")
	hdr := &tar.Header{
		Name: "app.R",
		Mode: 0o644,
		Size: int64(len(content)),
	}
	tw.WriteHeader(hdr)
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// MakeBundleWithoutEntrypoint returns a valid tar.gz that has no app.R.
func MakeBundleWithoutEntrypoint(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte("not an entrypoint")
	hdr := &tar.Header{
		Name: "README.md",
		Mode: 0o644,
		Size: int64(len(content)),
	}
	tw.WriteHeader(hdr)
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// MakeTraversalBundle returns a tar.gz with a path traversal entry.
func MakeTraversalBundle(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte("evil")
	hdr := &tar.Header{
		Name: "../../evil",
		Mode: 0o644,
		Size: int64(len(content)),
	}
	tw.WriteHeader(hdr)
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}
