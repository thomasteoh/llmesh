package admin

import (
	"os/exec"
	"testing"
)

// The portal's forms are posted by admin.js and the result swapped in, so the
// behaviour that matters for a create — that the response body reaches the page
// — lives in the script, not in a handler. The Go tests can only pin what the
// handlers return.
//
// testdata/submit_action.mjs loads the real admin.js into a vm with a DOM stub
// and drives submitAction against canned responses. Skipped rather than failed
// when node is absent: it is a supplement to the Go tests, not a build
// dependency.
func TestPortalScript_SubmitAction(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping the portal script test")
	}

	out, err := exec.Command(node, "testdata/submit_action.mjs", "static/admin.js").CombinedOutput()
	if err != nil {
		t.Fatalf("submitAction behaved unexpectedly:\n%s", out)
	}
	t.Logf("\n%s", out)
}
