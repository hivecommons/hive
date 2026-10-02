package dashboard

import (
	"strings"
	"testing"
)

func TestFleetBreakerRendersAsSingleStatusChipButton(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	if strings.Contains(html, `<span id="fleet-breaker-pill" class="pill-passed">running</span>`) {
		t.Fatal("fleet breaker still renders a separate status pill before the toggle button")
	}
	for _, snippet := range []string{
		`<button type="button" id="fleet-breaker-btn2" class="nav-chip nav-chip--status nav-chip--ok fleet-breaker-chip"`,
		`data-action="toggleFleetBreaker"`,
		`aria-pressed="false"`,
		`<span id="fleet-breaker-dot" class="nav-chip__dot" aria-hidden="true">●</span>`,
		`<span id="fleet-breaker-pill" class="nav-chip__label">running</span>`,
		`btn.setAttribute('aria-label', canToggle`,
		`btn.disabled = !canToggle`,
		`btn.setAttribute('aria-disabled', canToggle ? 'false' : 'true')`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing fleet breaker chip snippet %q", snippet)
		}
	}
}
