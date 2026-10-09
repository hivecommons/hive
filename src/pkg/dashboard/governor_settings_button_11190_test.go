package dashboard

import (
	"strings"
	"testing"
)

func TestGovernorSettingsButtonStaysNextToHeaderControls(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, ".governor-card .dash-card-actions { margin-left: 0; }") {
		t.Fatal("Governor settings button must not be pushed to the far right of the card header")
	}
}
