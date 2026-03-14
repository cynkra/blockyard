package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/cynkra/blockyard/internal/telemetry"
)

// Action identifies the type of audit event.
type Action string

const (
	ActionAppCreate         Action = "app.create"
	ActionAppUpdate         Action = "app.update"
	ActionAppDelete         Action = "app.delete"
	ActionAppStart          Action = "app.start"
	ActionAppStop           Action = "app.stop"
	ActionBundleUpload      Action = "bundle.upload"
	ActionBundleRestoreOK   Action = "bundle.restore.success"
	ActionBundleRestoreFail Action = "bundle.restore.fail"
	ActionAccessGrant       Action = "access.grant"
	ActionAccessRevoke      Action = "access.revoke"
	ActionCredentialEnroll  Action = "credential.enroll"
	ActionUserLogin         Action = "user.login"
	ActionUserLogout        Action = "user.logout"
	ActionRoleMappingSet    Action = "role_mapping.set"
	ActionRoleMappingDelete Action = "role_mapping.delete"
)

// Entry is a single audit log record.
type Entry struct {
	Timestamp string         `json:"ts"`
	Action    Action         `json:"action"`
	Actor     string         `json:"actor"`
	Target    string         `json:"target,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
	SourceIP  string         `json:"source_ip,omitempty"`
}

// Log is an append-only audit log backed by a JSON Lines file.
// Writes are buffered via a channel and flushed by a background goroutine.
type Log struct {
	entries chan Entry
}

const bufferSize = 1000

// New creates an audit log. The background writer must be started with
// Run(). If path is empty, returns nil.
func New(path string) *Log {
	if path == "" {
		return nil
	}
	return &Log{
		entries: make(chan Entry, bufferSize),
	}
}

// Emit sends an entry to the background writer. Non-blocking — if the
// buffer is full, the entry is dropped and a warning is logged.
func (l *Log) Emit(entry Entry) {
	if l == nil {
		return
	}
	entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)

	select {
	case l.entries <- entry:
	default:
		telemetry.AuditEntriesDropped.Inc()
		slog.Warn("audit log buffer full, dropping entry",
			"action", entry.Action, "actor", entry.Actor)
	}
}

// Run is the background goroutine that appends entries to the log file.
// Blocks until ctx is cancelled. Drains remaining entries before exit.
func (l *Log) Run(ctx context.Context, path string) {
	if l == nil {
		<-ctx.Done()
		return
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		slog.Error("failed to open audit log", "path", path, "error", err)
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)

	for {
		select {
		case <-ctx.Done():
			// Drain remaining entries
			for {
				select {
				case entry := <-l.entries:
					enc.Encode(entry)
				default:
					return
				}
			}
		case entry := <-l.entries:
			enc.Encode(entry)
		}
	}
}
