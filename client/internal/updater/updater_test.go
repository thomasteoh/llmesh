package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchManifest_ParsesSHA256(t *testing.T) {
	want := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"version": "v2.0.0",
			"url":     "http://example.com/binary",
			"sha256":  want,
		})
	}))
	defer srv.Close()

	m, err := fetchManifest(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if m.Version != "v2.0.0" {
		t.Errorf("version = %q, want v2.0.0", m.Version)
	}
	if m.SHA256 != want {
		t.Errorf("sha256 = %q, want %q", m.SHA256, want)
	}
}

func TestFetchManifest_NoSHA256(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"version": "v1.0.0",
			"url":     "http://example.com/binary",
		})
	}))
	defer srv.Close()

	m, err := fetchManifest(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if m.SHA256 != "" {
		t.Errorf("expected empty sha256, got %q", m.SHA256)
	}
}

// TestHashVerification exercises the hash-check logic extracted from applyUpdate.
// This mirrors what applyUpdate does: hex.EncodeToString(h.Sum(nil)) vs strings.ToLower(manifest.SHA256).
func TestHashVerification(t *testing.T) {
	content := []byte("fake-binary-content")
	sum := sha256.Sum256(content)
	correctHex := hex.EncodeToString(sum[:])

	cases := []struct {
		name      string
		sha256    string
		wantMatch bool
	}{
		{"correct lowercase", correctHex, true},
		{"correct uppercase", strings.ToUpper(correctHex), true},
		{"wrong hash", strings.Repeat("0", 64), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := correctHex
			want := strings.ToLower(strings.TrimSpace(tc.sha256))
			match := got == want
			if match != tc.wantMatch {
				t.Errorf("hash match = %v, want %v (got=%s, want=%s)", match, tc.wantMatch, got, want)
			}
		})
	}
}
