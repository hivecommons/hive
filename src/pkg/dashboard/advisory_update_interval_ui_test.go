package dashboard

import (
	"strings"
	"testing"
)

// TestAdvisoryUpdateIntervalUIWiring pins the Advisory tab's "When it updates"
// group (#4820) into the embedded page: the control exists, prefills from the
// GET payload's update_interval_s, and marks its edit dirty under the
// 'advisory' section via the CSP-safe data-action dispatch (markDirty with
// declarative args — no inline handler, no bespoke ghNN closure to drift).
func TestAdvisoryUpdateIntervalUIWiring(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	page := string(raw)
	for _, want := range []string{
		"When it updates",
		"Update interval (s)",
		`value="${a.update_interval_s || 0}"`,
		`data-arg0="advisory" data-arg1="update_interval_s" data-arg-types="s,s,N"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Advisory tab missing update-interval wiring %q", want)
		}
	}
}
