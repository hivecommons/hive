package dashboard

import (
	"strings"
	"testing"
)

func TestProviderRotationHeadroomRendersOnlyWhenEnabled(t *testing.T) {
	body := jsFunctionBody(t, indexHTML(t), "function renderGovFeatures(f, am, rv)")

	enabledGate := "${rotationOn ? `"
	idx := strings.Index(body, enabledGate)
	if idx < 0 {
		t.Fatalf("renderGovFeatures has no rotationOn gated block")
	}
	gated := body[idx:]
	if !strings.Contains(gated, `id="rotation-headroom-host"`) || !strings.Contains(body, "if (rotationOn) loadRotationHeadroom") {
		t.Fatalf("provider rotation enabled block must render and fetch live headroom")
	}

	beforeGate := body[:idx]
	if strings.Contains(beforeGate, `id="rotation-headroom-host"`) {
		t.Fatalf("headroom readout escaped the rotationEnabled-only block")
	}
}
