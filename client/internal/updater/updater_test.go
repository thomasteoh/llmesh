package updater

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireHTTPS(t *testing.T) {
	if err := requireHTTPS("https://example.com/manifest"); err != nil {
		t.Errorf("https should be accepted: %v", err)
	}
	for _, bad := range []string{"http://example.com/m", "ws://example.com/m", "file:///etc/passwd"} {
		if err := requireHTTPS(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestValidateManifest(t *testing.T) {
	ok := &manifest{Version: "v1.2.3", BinaryURL: "https://x/bin", SHA256: "abc"}
	if err := validateManifest(ok); err != nil {
		t.Errorf("valid manifest rejected: %v", err)
	}
	// Missing sha256 must fail closed rather than install unverified.
	if err := validateManifest(&manifest{Version: "v1", BinaryURL: "https://x/bin"}); err == nil {
		t.Error("manifest without sha256 should be rejected")
	}
	if err := validateManifest(&manifest{Version: "v1", SHA256: "abc"}); err == nil {
		t.Error("manifest without url should be rejected")
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.4", "v1.2.3", true},
		{"v1.3.0", "v1.2.9", true},
		{"v2.0.0", "v1.9.9", true},
		{"v1.2.3", "v1.2.3", false},  // same version — no update
		{"v1.2.2", "v1.2.3", false},  // downgrade — blocked
		{"v1.0.0", "v2.0.0", false},  // downgrade — blocked
		{"garbage", "v1.2.3", false}, // unparseable — never update
		{"v1.2.3", "dev", false},     // dev is not semver
	}
	for _, tc := range cases {
		if got := isNewer(tc.latest, tc.current); got != tc.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestFetchManifest_RejectsInsecureURL(t *testing.T) {
	// httptest serves over plain HTTP; fetchManifest must refuse before any
	// request is made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if _, err := fetchManifest(t.Context(), srv.URL); err == nil {
		t.Error("fetchManifest should reject a plain-HTTP manifest URL")
	}
}
