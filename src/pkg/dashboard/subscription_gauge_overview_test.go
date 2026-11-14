package dashboard

import (
	"strings"
	"testing"
)

// TestSubscriptionGaugeOverviewWiring pins the Overview fuel gauge: its host
// sits outside the re-rendered overview card, it is on by default with a
// per-browser hide checkbox for the owner, and it never fetches the
// owner-only headroom endpoint for anyone else.
func TestSubscriptionGaugeOverviewWiring(t *testing.T) {
	html := indexHTML(t)
	host := `<div class="overview-usage-gauge" id="overview-usage-gauge" role="region" aria-label="Claude subscription usage" hidden></div>`
	card := `<div class="overview-card" id="overview-card"></div>`
	hi, ci := strings.Index(html, host), strings.Index(html, card)
	if hi < 0 || ci < 0 || hi > ci {
		t.Fatalf("Overview gauge host must exist, hidden by default, before (and outside) #overview-card: host=%d card=%d", hi, ci)
	}

	defaults := jsFunc(t, html, "overviewDefaultChartState")
	if !strings.Contains(defaults, "showUsageGauge: true") {
		t.Fatalf("Overview gauge must be on by default:\n%s", defaults)
	}
	state := jsFunc(t, html, "overviewChartsState")
	if !strings.Contains(state, "showUsageGauge: saved.showUsageGauge !== false") {
		t.Fatalf("Overview gauge preference must default to on when unset:\n%s", state)
	}
	setter := jsFunc(t, html, "setOverviewUsageGauge")
	for _, want := range []string{"state.showUsageGauge = !!checked", "subscriptionGaugeRefresh()"} {
		if !strings.Contains(setter, want) {
			t.Fatalf("setOverviewUsageGauge missing %q:\n%s", want, setter)
		}
	}
	popover := jsFunc(t, html, "renderOverviewSettingsPopover")
	if !strings.Contains(popover, `subscriptionGaugeOwner()`) || !strings.Contains(popover, `data-change-action="setOverviewUsageGauge"`) || !strings.Contains(popover, "Show Claude usage gauge") {
		t.Fatalf("Overview settings must offer an owner-only Show Claude usage gauge checkbox:\n%s", popover)
	}

	wanted := jsFunc(t, html, "subscriptionGaugeWanted")
	if !strings.Contains(wanted, "subscriptionGaugeOwner() &&") {
		t.Fatalf("the gauge must be wanted only for the owner:\n%s", wanted)
	}
	owner := jsFunc(t, html, "subscriptionGaugeOwner")
	if !strings.Contains(owner, "dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')") {
		t.Fatalf("owner check changed:\n%s", owner)
	}
	fetchBody := jsFunctionBody(t, html, "async function subscriptionGaugeFetch(")
	gate := strings.Index(fetchBody, "if (!subscriptionGaugeWanted()) return;")
	call := strings.Index(fetchBody, "fetch('/api/providers/headroom')")
	if gate < 0 || call < 0 || gate > call {
		t.Fatalf("the headroom fetch must come after the owner gate:\n%s", fetchBody)
	}
	if n := strings.Count(html, "fetch('/api/providers/headroom')"); n != 2 {
		// One in the gauge loader, one in the existing rotation table loader.
		t.Fatalf("expected exactly 2 headroom fetch sites (gauge + rotation table), found %d", n)
	}

	overview := jsFunc(t, html, "renderSubscriptionGaugeOverview")
	for _, want := range []string{"subscriptionGaugeOwner()", "subscriptionGaugeOverviewEnabled()", "m.usesClaude", "textContent", "host.hidden = true"} {
		if !strings.Contains(overview, want) {
			t.Fatalf("renderSubscriptionGaugeOverview missing %q:\n%s", want, overview)
		}
	}
	if strings.Contains(overview, "innerHTML") {
		t.Fatalf("Overview gauge must build its DOM without innerHTML:\n%s", overview)
	}
	dial := jsFunc(t, html, "subscriptionGaugeDialSvg")
	if !strings.Contains(dial, "createElementNS") || strings.Contains(dial, "innerHTML") {
		t.Fatalf("fuel dial must be built with createElementNS, not innerHTML:\n%s", dial)
	}

	render := jsFunc(t, html, "render")
	if !strings.Contains(render, "subscriptionGaugeRefresh()") {
		t.Fatalf("render() must re-evaluate the gauge on every status frame")
	}
	auth := jsFunc(t, html, "showGHAuthState")
	if !strings.Contains(auth, "subscriptionGaugeRefresh()") {
		t.Fatalf("showGHAuthState must re-evaluate the gauge once the role resolves")
	}
}
