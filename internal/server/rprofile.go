package server

import (
	"os"
	"path/filepath"
	"sync"
)

// SHINY_PORT is unset after reading so Shiny does not mistake the
// worker for a Shiny Server deployment and emit a version warning.
const rProfileContent = `local({
  host <- Sys.getenv("SHINY_HOST", "127.0.0.1")
  port <- Sys.getenv("SHINY_PORT", "3838")
  options(shiny.host = host, shiny.port = as.integer(port))
  Sys.unsetenv(c("SHINY_HOST", "SHINY_PORT"))
})
`

var (
	rProfileOnce sync.Once
	rProfilePath string
	rProfileErr  error
)

// EnsureRProfile writes the blockyard R profile to dir and returns
// the path. The file bridges SHINY_HOST and SHINY_PORT env vars to
// the corresponding R options so bundles don't have to.
// Safe to call from multiple goroutines; the file is written once.
func EnsureRProfile(dir string) (string, error) {
	rProfileOnce.Do(func() {
		rProfilePath = filepath.Join(dir, ".blockyard-rprofile.R")
		rProfileErr = os.WriteFile(rProfilePath, []byte(rProfileContent), 0o644) //nolint:gosec // G306: must be world-readable; workers run as different UIDs
	})
	return rProfilePath, rProfileErr
}
