package dashboard

import (
	"strings"
	"testing"
)

func TestAdvisoryUpdateIntervalUIWiring(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	page := string(raw)
	for _, want := range []string{
		"When it updates",
		"Update interval (s)",
		"a.update_interval_s === undefined ? 0 : a.update_interval_s",
		`data-change-action="gh75"`,
		"markDirty('advisory','update_interval_s',Number(this.value)||0)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Advisory tab missing update-interval wiring %q", want)
		}
	}
}
