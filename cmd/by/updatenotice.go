package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cynkra/blockyard/internal/cliconfig"
	"github.com/cynkra/blockyard/internal/update"
)

const updateCheckInterval = 24 * time.Hour

// updateCache is persisted to disk to throttle GitHub API calls.
type updateCache struct {
	LatestVersion string    `json:"latest_version"`
	Channel       string    `json:"channel"`
	CheckedAt     time.Time `json:"checked_at"`
}

func updateCachePath() string {
	return filepath.Join(cliconfig.Dir(), "update-check.json")
}

func loadUpdateCache() *updateCache {
	data, err := os.ReadFile(updateCachePath())
	if err != nil {
		return nil
	}
	var c updateCache
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

func saveUpdateCache(c *updateCache) {
	dir := cliconfig.Dir()
	os.MkdirAll(dir, 0o700)
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	os.WriteFile(updateCachePath(), data, 0o600)
}

// updateResult is sent from the background goroutine to the post-run hook.
type updateResult struct {
	result *update.Result
	err    error
}

// updateNoticeState holds the goroutine channel for a pending background check.
var updateNoticeState struct {
	ch chan updateResult
}

// startUpdateCheck kicks off a background update check if the cache is stale.
// Called from PersistentPreRun so the HTTP request runs concurrently with the
// actual command.
func startUpdateCheck() {
	cache := loadUpdateCache()
	channel := update.InferChannel(version)

	// If the cache is fresh and no update is available, skip entirely.
	if cache != nil && cache.Channel == channel &&
		time.Since(cache.CheckedAt) < updateCheckInterval &&
		cache.LatestVersion == version {
		return
	}

	// If the cache is fresh and shows an update, we'll print from cache
	// in finishUpdateCheck — no need for a network call.
	if cache != nil && cache.Channel == channel &&
		time.Since(cache.CheckedAt) < updateCheckInterval {
		return
	}

	// Cache is stale or missing — start a background check.
	ch := make(chan updateResult, 1)
	updateNoticeState.ch = ch
	go func() {
		res, err := update.CheckLatest(channel, version)
		ch <- updateResult{result: res, err: err}
	}()
}

// finishUpdateCheck collects the background check result (if any) and prints
// a one-line notice to stderr when an update is available.
// Called from PersistentPostRunE after the command finishes.
func finishUpdateCheck() {
	channel := update.InferChannel(version)

	if updateNoticeState.ch != nil {
		// A background check is running — collect the result.
		ur := <-updateNoticeState.ch
		updateNoticeState.ch = nil
		if ur.err == nil && ur.result != nil {
			saveUpdateCache(&updateCache{
				LatestVersion: ur.result.LatestVersion,
				Channel:       ur.result.Channel,
				CheckedAt:     time.Now(),
			})
			if ur.result.UpdateAvailable {
				printUpdateNotice(ur.result.LatestVersion)
			}
		}
		return
	}

	// No background check — use cached result.
	cache := loadUpdateCache()
	if cache != nil && cache.Channel == channel && cache.LatestVersion != version {
		printUpdateNotice(cache.LatestVersion)
	}
}

func printUpdateNotice(latest string) {
	fmt.Fprintf(os.Stderr,
		"A newer version of by is available: %s (current: %s). Run 'by self-update' to upgrade.\n",
		latest, version)
}
