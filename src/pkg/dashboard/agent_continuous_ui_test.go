package dashboard

import (
	"strings"
	"testing"
)

func TestContinuousModeControlsAndBadgesRender(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-key="continuous"`,
		`data-key="continuousCooldown"`,
		`Continuous</b> — re-kick when a session ends`,
		`∞ continuous`,
		`Continuous mode — re-kick after session end plus cool-down`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard HTML missing %q", want)
		}
	}
}
