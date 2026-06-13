// client/internal/updater/updater.go
//
// In-place binary update for llmesh-client. When auto_update is enabled and
// a manifest URL is configured, the client polls every hour for a new version.
// If a newer version is found and the client is idle (zero active jobs), it
// downloads the new binary, replaces itself, and re-execs the process.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	checkInterval  = time.Hour
	initialDelay   = 30 * time.Second // wait for connection to establish before first check
	manifestSizeLimit = 64 * 1024     // 64 KB — manifest should be tiny
)

// manifest is the JSON structure returned by the update URL.
type manifest struct {
	Version   string `json:"version"` // e.g. "v1.2.3"
	BinaryURL string `json:"url"`     // direct download link for the new binary
	SHA256    string `json:"sha256"`  // hex-encoded SHA-256 of the binary; verified when present
}

// Run starts the update check loop. Blocks until ctx is cancelled.
// manifestURL is the URL of the JSON manifest. currentVersion is the running
// binary's version string (set via -ldflags at build time; "dev" skips updates).
// autoUpdate enables the periodic hourly check in addition to portal-triggered updates.
// triggerCh receives a value when the router requests an immediate update check.
// isIdle reports whether the client currently has zero in-flight jobs.
func Run(ctx context.Context, manifestURL, currentVersion string, autoUpdate bool, isIdle func() bool, triggerCh <-chan struct{}, log *slog.Logger) {
	if manifestURL == "" {
		return
	}
	if currentVersion == "dev" {
		log.Info("updater: skipping updates for dev build")
		return
	}

	doCheck := func() {
		tryUpdate(ctx, manifestURL, currentVersion, isIdle, log)
	}

	var tickerC <-chan time.Time
	if autoUpdate {
		// Initial delay — give the router connection time to establish before checking.
		select {
		case <-time.After(initialDelay):
		case <-ctx.Done():
			return
		}
		doCheck()
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		tickerC = ticker.C
	}

	for {
		select {
		case <-tickerC:
			doCheck()
		case _, ok := <-triggerCh:
			if !ok {
				return
			}
			log.Info("updater: portal-triggered update check")
			doCheck()
		case <-ctx.Done():
			return
		}
	}
}

func tryUpdate(ctx context.Context, manifestURL, currentVersion string, isIdle func() bool, log *slog.Logger) {
	m, err := fetchManifest(ctx, manifestURL)
	if err != nil {
		log.Warn("updater: manifest fetch failed", "error", err)
		return
	}
	if m.Version == currentVersion {
		return // already running the latest version
	}
	log.Info("updater: new version available", "current", currentVersion, "latest", m.Version)

	if !isIdle() {
		log.Info("updater: deferring — client not idle", "latest", m.Version)
		return
	}

	if err := applyUpdate(ctx, m, isIdle, log); err != nil {
		log.Error("updater: failed", "error", err, "latest", m.Version)
	}
	// applyUpdate only returns on failure; success calls syscall.Exec (no return).
}

func fetchManifest(ctx context.Context, url string) (*manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest server returned %d", resp.StatusCode)
	}
	var m manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, manifestSizeLimit)).Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Version == "" || m.BinaryURL == "" {
		return nil, fmt.Errorf("manifest missing version or url field")
	}
	return &m, nil
}

func applyUpdate(ctx context.Context, m *manifest, isIdle func() bool, log *slog.Logger) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}

	// Create temp file in the same directory as the binary so that os.Rename is
	// atomic (same filesystem). A cross-filesystem rename would fail.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".llmesh-client-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename; cleans up on any failure path

	log.Info("updater: downloading new binary", "version", m.Version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.BinaryURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download server returned %d", resp.StatusCode)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if m.SHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		want := strings.ToLower(strings.TrimSpace(m.SHA256))
		if got != want {
			return fmt.Errorf("SHA-256 mismatch: got %s, want %s", got, want)
		}
		log.Info("updater: binary integrity verified", "sha256", got)
	} else {
		log.Warn("updater: manifest has no sha256 field — skipping integrity check")
	}

	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("chmod binary: %w", err)
	}

	// Re-check idle after the download — jobs may have arrived while downloading.
	if !isIdle() {
		log.Info("updater: aborting — jobs started during download", "version", m.Version)
		return nil
	}

	log.Info("updater: replacing binary and re-executing", "version", m.Version, "path", exe)
	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}
	// syscall.Exec replaces this process image with the new binary.
	// It does not return on success.
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("re-exec: %w", err)
	}
	return nil // unreachable
}
