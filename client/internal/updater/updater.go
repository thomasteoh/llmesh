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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	checkInterval     = time.Hour
	initialDelay      = 30 * time.Second // wait for connection to establish before first check
	manifestSizeLimit = 64 * 1024        // 64 KB — manifest should be tiny
	maxBinarySize     = 512 << 20        // 512 MB — generous ceiling; prevents disk-fill
)

// requireHTTPS rejects non-HTTPS update URLs. Fetching a manifest or binary over
// plain HTTP would let an on-path attacker serve arbitrary code to the worker,
// so auto-update is only allowed over TLS.
func requireHTTPS(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid update URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing update over insecure scheme %q (https required)", u.Scheme)
	}
	return nil
}

// parseSemver parses a vMAJOR.MINOR.PATCH string (pre-release/build suffix
// ignored). Returns ok=false when the string is not parseable.
func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// isNewer reports whether latest is a strictly higher version than current.
// Unparseable versions return false so the updater never downgrades or installs
// a sideways/garbage version pushed by a compromised manifest.
func isNewer(latest, current string) bool {
	lv, ok1 := parseSemver(latest)
	cv, ok2 := parseSemver(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if lv[i] != cv[i] {
			return lv[i] > cv[i]
		}
	}
	return false
}

// manifest is the JSON structure returned by the update URL.
type manifest struct {
	Version   string `json:"version"` // e.g. "v1.2.3"
	BinaryURL string `json:"url"`     // direct download link for the new binary
	SHA256    string `json:"sha256"`  // hex-encoded SHA-256 of the binary; required
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
	if err := requireHTTPS(manifestURL); err != nil {
		// Auto-update requires HTTPS; a ws:// router yields an http:// manifest
		// URL. Bail out once instead of failing every check.
		log.Info("updater: disabled — update endpoint is not HTTPS", "url", manifestURL)
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
	if !isNewer(m.Version, currentVersion) {
		return // not a strictly newer version — ignore (also blocks downgrades)
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
	if err := requireHTTPS(url); err != nil {
		return nil, err
	}
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
	if err := validateManifest(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// validateManifest enforces that a manifest carries everything needed to apply
// a verifiable update. Fails closed: a missing sha256 makes the update
// unverifiable and is rejected rather than installed on trust.
func validateManifest(m *manifest) error {
	if m.Version == "" || m.BinaryURL == "" {
		return fmt.Errorf("manifest missing version or url field")
	}
	if strings.TrimSpace(m.SHA256) == "" {
		return fmt.Errorf("manifest missing sha256 — refusing unverifiable update")
	}
	return nil
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

	// Only download over HTTPS — otherwise an on-path attacker controls the
	// binary that replaces this process.
	if err := requireHTTPS(m.BinaryURL); err != nil {
		return err
	}

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
	// Cap the copy so a malicious or runaway server cannot fill the disk.
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBinarySize)); err != nil {
		tmp.Close()
		return fmt.Errorf("write binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// sha256 presence is guaranteed by fetchManifest; verify it (fail closed).
	got := hex.EncodeToString(h.Sum(nil))
	want := strings.ToLower(strings.TrimSpace(m.SHA256))
	if got != want {
		return fmt.Errorf("SHA-256 mismatch: got %s, want %s", got, want)
	}
	log.Info("updater: binary integrity verified", "sha256", got)

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
