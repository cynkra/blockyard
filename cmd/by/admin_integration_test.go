//go:build cli_test

package main

import (
	"testing"
)

// TestCLI_Admin exercises the `by admin` subcommands against the
// same mock-API harness as the other CLI integration tests.
// admin_test.go covers cobra wiring; this file covers behaviour.
func TestCLI_Admin(t *testing.T) {
	t.Run("status_idle", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/admin/update/status", 200, map[string]string{
			"state": "idle",
		})
		r := run(t, m, "admin", "status")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "idle")
	})

	t.Run("status_json", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/admin/update/status", 200, map[string]any{
			"state":   "running",
			"task_id": "task-7",
		})
		r := run(t, m, "admin", "status", "--json")
		assertExit(t, r, 0)
		got := r.jsonMap()
		if got["state"] != "running" {
			t.Errorf("state = %v, want running", got["state"])
		}
		if got["task_id"] != "task-7" {
			t.Errorf("task_id = %v, want task-7", got["task_id"])
		}
	})

	t.Run("update_blocked_when_running", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/admin/update/status", 200, map[string]string{
			"state": "running",
		})
		r := run(t, m, "admin", "update", "--yes")
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "update already in progress")
	})

	t.Run("update_yes_streams_progress", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/admin/update/status", 200, map[string]string{
			"state": "idle",
		})
		m.on("POST", "/api/v1/admin/update", 202, map[string]string{
			"task_id": "task-up",
		})
		m.onText("GET", "/api/v1/tasks/task-up/logs", "Pulling image...\nDone.\n")
		m.on("GET", "/api/v1/tasks/task-up", 200, map[string]string{
			"status": "completed",
		})
		r := run(t, m, "admin", "update", "--yes", "--channel", "main")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Pulling image")

		body := bodyJSON(t, m.reqTo("POST", "/api/v1/admin/update"))
		if body["channel"] != "main" {
			t.Errorf("channel = %v, want main", body["channel"])
		}
	})

	t.Run("update_failed_streams_then_errors", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/admin/update/status", 200, map[string]string{
			"state": "idle",
		})
		m.on("POST", "/api/v1/admin/update", 202, map[string]string{
			"task_id": "task-up",
		})
		m.onText("GET", "/api/v1/tasks/task-up/logs", "Pulling image...\nERROR: tag missing\n")
		m.on("GET", "/api/v1/tasks/task-up", 200, map[string]string{
			"status": "failed",
		})
		r := run(t, m, "admin", "update", "--yes")
		assertExit(t, r, 1)
	})

	t.Run("update_json_returns_task_id", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/admin/update/status", 200, map[string]string{
			"state": "idle",
		})
		m.on("POST", "/api/v1/admin/update", 202, map[string]string{
			"task_id": "task-up",
		})
		r := run(t, m, "admin", "update", "--yes", "--json")
		assertExit(t, r, 0)
		got := r.jsonMap()
		if got["task_id"] != "task-up" {
			t.Errorf("task_id = %v, want task-up", got["task_id"])
		}
	})

	t.Run("rollback_yes_streams_progress", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/admin/rollback", 202, map[string]string{
			"task_id": "task-rb",
		})
		m.onText("GET", "/api/v1/tasks/task-rb/logs", "Reverting...\nDone.\n")
		m.on("GET", "/api/v1/tasks/task-rb", 200, map[string]string{
			"status": "completed",
		})
		r := run(t, m, "admin", "rollback", "--yes")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Reverting")
	})

	t.Run("rollback_unsupported_returns_error", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/admin/rollback", 501, map[string]string{
			"error":   "not_supported",
			"message": "rollback not supported by this backend",
		})
		r := run(t, m, "admin", "rollback", "--yes")
		assertExit(t, r, 1)
	})

	t.Run("rollback_json_returns_task_id", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/admin/rollback", 202, map[string]string{
			"task_id": "task-rb",
		})
		r := run(t, m, "admin", "rollback", "--yes", "--json")
		assertExit(t, r, 0)
		got := r.jsonMap()
		if got["task_id"] != "task-rb" {
			t.Errorf("task_id = %v, want task-rb", got["task_id"])
		}
	})
}
