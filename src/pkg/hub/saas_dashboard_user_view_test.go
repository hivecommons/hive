package hub

import (
	"strings"
	"testing"
)

// The user-first hub view (Request-a-hive, Register-your-own-hive,
// Contribute-first public hives, empty-state CTAs) lives entirely inside the
// dashboardHTML raw string, so there is no Go function to exercise. These tests
// pin the wiring: each affordance must reuse an EXISTING page/endpoint
// (/get-started, /get-started#self-host, /contribute) rather than a new
// backend, and a refactor that quietly drops one should turn CI red.

// TestDashboardRequestAndRegisterAffordances asserts the top button group
// carries a Request-a-hive affordance routing to the existing wizard
// (/get-started) and a distinct Register affordance routing to the existing
// self-host guide (/get-started#self-host).
func TestDashboardRequestAndRegisterAffordances(t *testing.T) {
	html := dashScript(t)

	// (1) Request a hive -> the EXISTING Request-a-Hive wizard.
	if !strings.Contains(html, `id="btn-request-hive"`) {
		t.Error(`missing the "Request a hive" button (id="btn-request-hive")`)
	}
	if !strings.Contains(html, `id="btn-request-hive" href="/get-started"`) {
		t.Error(`"Request a hive" must link to the existing wizard at /get-started`)
	}

	// (2) Register your own hive -> the EXISTING self-host guide.
	if !strings.Contains(html, `id="btn-register-hive"`) {
		t.Error(`missing the "Register your own hive" button (id="btn-register-hive")`)
	}
	if !strings.Contains(html, `id="btn-register-hive" href="/get-started#self-host"`) {
		t.Error(`"Register your own hive" must link to /get-started#self-host`)
	}

	// The Request-a-hive link only appears when the user has NO hosted quota;
	// with quota the working + Add Hosted Hive button takes over. Pin the
	// canCreate-driven visibility toggle so the two never show at once.
	for _, want := range []string{
		`requestBtn.style.display = canCreate ? 'none' : '';`,
		`addBtn.style.display = canCreate ? '' : 'none';`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing quota-driven visibility toggle: %s", want)
		}
	}
}

// TestPublicHivesContributeIsPrimary asserts Contribute is the PRIMARY action on
// public hives for everyone (no access required, since /contribute is public),
// with Request access demoted to a secondary link and Pending preserved.
func TestPublicHivesContributeIsPrimary(t *testing.T) {
	html := dashScript(t)

	// Contribute is built from the heartbeat-reported dashboard URL, not a
	// hardcoded host, and targets the PUBLIC /contribute endpoint.
	if !strings.Contains(html, `resolvedBase(h)`) {
		t.Error("public-hives Contribute must resolve the hive's reported dashboard URL")
	}
	if !strings.Contains(html, `cBase + '/contribute"`) {
		t.Error("public-hives Contribute must target the public /contribute endpoint")
	}
	if !strings.Contains(html, `var contributeAction = cBase`) {
		t.Error("Contribute must be the primary action computed for every row, not gated on access")
	}

	// The unavailable fallback (no reported dashboard URL) is preserved.
	if !strings.Contains(html, `Contribute unavailable`) {
		t.Error("public-hives must keep the 'Contribute unavailable' fallback when no dashboard URL")
	}

	// Request access is still available but SECONDARY, and Pending is preserved.
	if !strings.Contains(html, `dashRequestAccess(`) {
		t.Error("Request access must remain available as a secondary action")
	}
	if !strings.Contains(html, `>Pending</span>`) {
		t.Error("Pending access state must be preserved")
	}
}

// TestEmptyStateHasCTAs asserts the "No hives yet" empty state carries the two
// actionable CTAs (Request a hive, Register your own hive) plus the pointer to
// the Public Hives section, instead of the old flat text.
func TestEmptyStateHasCTAs(t *testing.T) {
	html := dashScript(t)

	i := strings.Index(html, "No hives yet")
	if i < 0 {
		t.Fatal("could not find the 'No hives yet' empty state")
	}
	// Bound the assertion to the empty-state block so an unrelated /get-started
	// link elsewhere cannot satisfy it.
	block := html[i:]
	if end := strings.Index(block, "return;"); end >= 0 {
		block = block[:end]
	}
	for _, want := range []string{
		`href="/get-started"`,           // Request a hive
		`href="/get-started#self-host"`, // Register your own hive
		`contribute to a public hive`,   // pointer to Public Hives section
	} {
		if !strings.Contains(block, want) {
			t.Errorf("empty state is missing CTA content: %s", want)
		}
	}
	// The old dead-end copy must be gone.
	if strings.Contains(block, "Log in to a local hive dashboard to see it here, or create a hosted hive.") {
		t.Error("empty state still shows the old flat text instead of actionable CTAs")
	}
}
