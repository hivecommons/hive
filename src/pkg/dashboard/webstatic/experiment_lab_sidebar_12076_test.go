package webstatic

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestStaticExperimentLabSidebarEntry pins the #12076 fix on the client: the
// Experiment Lab sidebar entry exists, is gated on the dashboard.experiment_lab
// feature flag (reactively, via applyDashboardFeatureVisibility on every
// status poll) AND on ACMM L3+ (ACMM_NOUS_MIN_LEVEL in applyACMMVisibility),
// links to the nous-section card, and reads "Experiment Lab (nous)" — the same
// "<Section> (<codename>)" convention as "Inception (spektacular)" — in the
// sidebar, the section <h2> and the section-card title maps alike.
func TestStaticExperimentLabSidebarEntry(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	for _, want := range []string{
		// Sidebar entry: flag-gated, hidden until the status poll flips it, 🧪 icon, (nous) suffix.
		`<a class="oc-nav-item" data-section="nous-section" data-feature="experiment_lab" hidden data-action="ocNavigate" data-arg0="nous-section"><span class="oc-nav-emoji">🧪</span><span class="oc-nav-text">Experiment Lab (nous)</span></a>`,
		// Section card it navigates to, same flag gate.
		`<div id="nous-section" data-dashboard-section="nous-section" data-feature="experiment_lab" hidden`,
		// Header and card titles in lockstep with the sidebar label.
		`<span class="section-chevron">▼</span> 🧪 Experiment Lab</h2>`,
		`'nous-section': { title: '🧪 Experiment Lab (nous)', summary: '—' },`,
		`'nous-section': { title: '🧪 Experiment Lab (nous)', summary: '—', settingsHtml: sectionSettingsGear('Experiment Lab', 'openNousConfig', 'Experiment Lab Configuration') },`,
		// Reactive reveal: the feature pass runs on every status payload.
		"window._lastStatus = data;\n      applyDashboardFeatureVisibility();",
		`document.querySelectorAll('[data-feature="experiment_lab"]').forEach(el => {`,
		// The ACMM pass requires both gates: flag on AND L3+.
		`const ACMM_NOUS_MIN_LEVEL = 3;`,
		`const nousVisible = experimentLabEnabled() && level >= ACMM_NOUS_MIN_LEVEL;`,
		`nousVisible ? show(nousSection, '') : hide(nousSection);`,
		`nousVisible ? show(nousSidebarCfg, '') : hide(nousSidebarCfg);`,
		`if (section === 'nous-section' && !nousVisible) hide(item);`,
		// The toggle says what it does, always states the L3 requirement, and
		// surfaces the current level + an in-app notice/toast (no native dialog)
		// when flipped on below L3.
		`<span class="config-toggle-label">Experiment Lab (nous) ' + yamlHint('dashboard.experiment_lab') + ' <span class="text-muted">Adds the 🧪 Experiment Lab (nous) section and its sidebar entry on the next status refresh — no reload needed.</span></span></div>' +
        experimentLabToggleHelpHTML(currentACMMLevelForSettings(), 'dashboard') +`,
		// The Features tab's "Nous / Experiment Lab" card shares the same helper.
		"${experimentLabToggleHelpHTML(currentACMMLevelForSettings(), 'features')}",
		"requires ACMM Level ${EXPERIMENT_LAB_MIN_ACMM_LEVEL} or higher; goals, bounds and schedule live in the Experiment Lab panel, not here.",
		`const EXPERIMENT_LAB_MIN_ACMM_LEVEL = 3;`,
		`const requirement = 'Requires ACMM Level ' + EXPERIMENT_LAB_MIN_ACMM_LEVEL + ' or higher.';`,
		`'Your hive is at Level ' + level + ' — Experiment Lab will appear once it reaches Level ' + EXPERIMENT_LAB_MIN_ACMM_LEVEL + '.'`,
		`if (section === 'dashboard' && key === 'experiment_lab') noteExperimentLabToggle(isOn);`,
		`noteExperimentLabSaved(values);`,
		`id="experiment-lab-acmm-notice' + sfx + '" data-experiment-lab-acmm-notice="1" hidden`,
		// Settings search finds the entry by either name.
		`label: 'Experiment Lab (nous)', key: 'dashboard.experiment_lab'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}

	// The L4 threshold must not come back, and nothing may hide the nous entry
	// on the flag alone (which would bypass the L3 gate).
	for _, forbidden := range []string{
		`const ACMM_NOUS_MIN_LEVEL = 4;`,
		`alert('Experiment Lab`,
		`confirm('Experiment Lab`,
		`'nous-section' && !experimentLabEnabled()) hide(item);`,
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("index.html regressed the Experiment Lab ACMM gate via %q (#12076: flag AND L3+)", forbidden)
		}
	}
}

// TestStaticCodenamedSidebarLabelsShareOneConvention keeps the Inception and
// Experiment Lab entries on the same "<Section> (<lowercase codename>)" shape so
// neither drifts to "Inception · Spektacular" or "Nous Experiment Lab".
func TestStaticCodenamedSidebarLabelsShareOneConvention(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	re := regexp.MustCompile(`<a class="oc-nav-item[^>]*data-section="([^"]+)"[^>]*>\s*<span class="oc-nav-emoji">[^<]+</span><span class="oc-nav-text">([^<]+)</span>`)
	labels := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		labels[m[1]] = strings.TrimSpace(m[2])
	}
	codenamed := regexp.MustCompile(`^[A-Z][A-Za-z ]+ \([a-z]+\)$`)
	for section, want := range map[string]string{
		"inception-section": "Inception (spektacular)",
		"nous-section":      "Experiment Lab (nous)",
	} {
		got, ok := labels[section]
		if !ok {
			t.Fatalf("sidebar has no entry for %s", section)
		}
		if got != want {
			t.Errorf("sidebar label for %s = %q, want %q", section, got, want)
		}
		if !codenamed.MatchString(got) {
			t.Errorf("sidebar label %q for %s does not follow the \"<Section> (<codename>)\" convention", got, section)
		}
	}
}
