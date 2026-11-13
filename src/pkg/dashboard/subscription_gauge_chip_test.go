package dashboard

import (
	"strings"
	"testing"
)

// TestSubscriptionGaugeChipWiring pins the opt-in header chip: hidden by
// default, shown only for the owner with dashboard.subscription_gauge_header
// on, fed by the shared loader (no fetch of its own), and opening the
// Overview gauge on click.
func TestSubscriptionGaugeChipWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="oc-subscription-gauge"`,
		`class="nav-chip nav-chip--status subscription-gauge-chip"`,
		`data-action="openSubscriptionGaugeOverview"`,
		`.subscription-gauge-chip[hidden] { display: none; }`,
		`#oc-subscription-gauge .nav-chip__label,`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("header usage chip missing %q", want)
		}
	}
	i := strings.Index(html, `id="oc-subscription-gauge"`)
	tagStart := strings.LastIndex(html[:i], "<button")
	tagEnd := strings.Index(html[i:], ">") + i
	if tag := html[tagStart:tagEnd]; !strings.Contains(tag, " hidden") {
		t.Fatalf("header usage chip must be hidden by default: %s", tag)
	}

	chip := jsFunc(t, html, "renderSubscriptionGaugeChip")
	for _, want := range []string{"subscriptionGaugeOwner()", "subscriptionGaugeHeaderEnabled()", "el.hidden = true", "subscriptionGaugeDialSvg(", "compact: true", "Claude: no reading"} {
		if !strings.Contains(chip, want) {
			t.Fatalf("renderSubscriptionGaugeChip missing %q:\n%s", want, chip)
		}
	}
	if strings.Contains(chip, "fetch(") || strings.Contains(chip, "innerHTML") {
		t.Fatalf("the chip must reuse the shared readings and build DOM without innerHTML:\n%s", chip)
	}
	header := jsFunc(t, html, "subscriptionGaugeHeaderEnabled")
	if !strings.Contains(header, "dashboardFeatureEnabled('subscription_gauge_header')") {
		t.Fatalf("the chip must follow dashboard.subscription_gauge_header:\n%s", header)
	}
	surfaces := jsFunc(t, html, "renderSubscriptionGaugeSurfaces")
	if !strings.Contains(surfaces, "renderSubscriptionGaugeChip()") {
		t.Fatalf("shared render must update the chip:\n%s", surfaces)
	}
	open := jsFunc(t, html, "openSubscriptionGaugeOverview")
	for _, want := range []string{"ocNavigate('overview-section')", "overview-usage-gauge", "scrollIntoView"} {
		if !strings.Contains(open, want) {
			t.Fatalf("openSubscriptionGaugeOverview missing %q:\n%s", want, open)
		}
	}
}
