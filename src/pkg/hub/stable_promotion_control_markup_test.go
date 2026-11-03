package hub

import (
	"os"
	"strings"
	"testing"
)

// The stable channel row is a wrapping flex line, so anything rendered after
// the pause/resume button lands on its own line once the row is long. The
// button label already conveys the state, so the control must be the button
// alone — no trailing "auto: on"/"paused" chip.
func TestStablePromotionControlIsButtonOnly(t *testing.T) {
	src, err := os.ReadFile("assets/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(src)
	start := strings.Index(html, "function stablePromotionControlHTML(")
	if start < 0 {
		t.Fatal("stablePromotionControlHTML not found")
	}
	end := strings.Index(html[start:], "async function toggleStablePromotion(")
	if end < 0 {
		t.Fatal("stablePromotionControlHTML body end not found")
	}
	body := html[start : start+end]
	for _, stray := range []string{"'auto: on'", "'paused'", "</button><span"} {
		if strings.Contains(body, stray) {
			t.Errorf("stablePromotionControlHTML still renders a trailing status chip (%s); the button label is the state indicator", stray)
		}
	}
	if !strings.Contains(body, "'⏸ pause stable auto-promotion'") || !strings.Contains(body, "'▶ resume stable auto-promotion'") {
		t.Error("stablePromotionControlHTML lost its pause/resume button labels")
	}
}
