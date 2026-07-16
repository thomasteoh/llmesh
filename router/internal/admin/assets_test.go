package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestAssetVersion checks the content hash is a stable, non-empty hex string.
// It is derived from the embedded assets, so it changes only when they do.
func TestAssetVersion(t *testing.T) {
	v := assetVersion()
	if !regexp.MustCompile(`^[a-f0-9]{12}$`).MatchString(v) {
		t.Fatalf("assetVersion %q is not a 12-char lowercase hex string", v)
	}
	if assetVersion() != v {
		t.Fatal("assetVersion not stable across calls")
	}
}

// TestStaticCacheHeaders verifies the static handler emits an ETag for
// revalidation, marks versioned (?v=) requests immutable, and returns 304 to a
// matching conditional request. This guards against a redeploy serving stale
// CSS/JS from a browser cache — the app-shell layout breaks when new markup
// pairs with an old cached stylesheet.
func TestStaticCacheHeaders(t *testing.T) {
	a := newTestAdmin(t)
	a.assetVersion = assetVersion()
	a.registerRoutes()

	etag := `"` + a.assetVersion + `"`

	// Versioned request: immutable, long-lived, with ETag.
	rr := httptest.NewRecorder()
	a.ServeHTTP(rr, httptest.NewRequest("GET", "/portal/static/admin.css?v="+a.assetVersion, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("versioned GET: status %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("ETag"); got != etag {
		t.Errorf("versioned GET ETag = %q, want %q", got, etag)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("versioned GET Cache-Control = %q, want immutable", cc)
	}

	// Unversioned request: must revalidate.
	rr = httptest.NewRecorder()
	a.ServeHTTP(rr, httptest.NewRequest("GET", "/portal/static/admin.css", nil))
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("unversioned GET Cache-Control = %q, want no-cache", cc)
	}

	// Conditional request with the matching ETag returns 304.
	req := httptest.NewRequest("GET", "/portal/static/admin.css?v="+a.assetVersion, nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	a.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("conditional GET: status %d, want 304", rr.Code)
	}
}

// TestRenderedPageUsesVersionedAsset confirms rendered pages reference the
// cache-busting asset URL rather than a bare static path.
func TestRenderedPageUsesVersionedAsset(t *testing.T) {
	a := newTestAdmin(t)
	a.assetVersion = assetVersion()
	if err := a.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := a.tmpls["login"].Execute(&buf, map[string]any{}); err != nil {
		t.Fatalf("render login: %v", err)
	}
	want := "/portal/static/admin.css?v=" + a.assetVersion
	if !strings.Contains(buf.String(), want) {
		t.Errorf("login page missing versioned asset URL %q", want)
	}
}
