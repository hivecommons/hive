package dashboard

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watchdog"
)

// ── RFC #4665: watchdog settings on the Health tab + observed conditions ──────

func watchdogUIHTML(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	return string(b)
}

// TestWatchdogSettingsRenderOnHealthTab asserts the settings surface exists on
// the Health tab — beside the escalation breaker, not on a tab of its own —
// and that every control the operator needs is present.
func TestWatchdogSettingsRenderOnHealthTab(t *testing.T) {
	html := watchdogUIHTML(t)

	// The section is reached from renderGovHealth, so it lands on Health.
	if !strings.Contains(html, "${renderWatchdogSection(h.watchdog)}") {
		t.Error("renderGovHealth must render the watchdog section — the settings belong beside escalation, not on a new tab")
	}

	for _, snippet := range []string{
		// Mode control, dirty-tracked into the health bag.
		`data-arg0="health" data-arg1="watchdogMode"`,
		// The throttle and the two give-up/recovery rules.
		`data-arg0="health" data-arg1="watchdogProbeIntervalS"`,
		`data-arg0="health" data-arg1="watchdogCrashLoopAfter"`,
		`data-arg0="health" data-arg1="watchdogHealthyReset"`,
		// Its own write-through endpoint.
		`'/api/config/governor/watchdog'`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}

	// All three modes must be offered, each explained.
	for _, mode := range []string{"observe", "heal", "off"} {
		if !strings.Contains(html, `['`+mode+`',`) {
			t.Errorf("mode %q must be selectable", mode)
		}
	}

	// The backoff ladder is DISPLAY ONLY: no input may write it, or an
	// operator can type a 1-second cap.
	if strings.Contains(html, `data-arg1="watchdogBackoff"`) {
		t.Error("the backoff ladder must be read-only — an editable ladder invites a 1s cap")
	}
}

// TestWatchdogUIHasNoUndefinedCallees is the renderAll()/ReferenceError guard:
// every function the watchdog UI calls must actually be defined.
func TestWatchdogUIHasNoUndefinedCallees(t *testing.T) {
	html := watchdogUIHTML(t)
	for _, fn := range []string{
		"renderWatchdogSection",
		"watchdogConditionsRow",
		"hasWatchdogKeys",
		"splitWatchdogKeys",
		"markDirty",
		"esc",
	} {
		declared := regexp.MustCompile(`(?m)^\s*(function\s+` + fn + `\s*\(|const\s+` + fn + `\s*=)`)
		if !declared.MatchString(html) {
			t.Errorf("watchdog UI calls %q but it is never declared", fn)
		}
	}
	// Top-level declarations sit at 4-space indent, matching the file's other
	// script-scope functions.
	for _, fn := range []string{"renderWatchdogSection", "watchdogConditionsRow", "hasWatchdogKeys", "splitWatchdogKeys"} {
		if !regexp.MustCompile(`(?m)^ {4}function ` + fn + `\(`).MatchString(html) {
			t.Errorf("%q must be declared at top level (4-space indent)", fn)
		}
	}
}

// TestWatchdogObserveModeIsVisuallyDistinct is the core honesty assertion: a
// condition implying an action must not read as an action taken. Heal mode is
// the positive control — the same surface says the opposite thing there.
func TestWatchdogObserveModeIsVisuallyDistinct(t *testing.T) {
	html := watchdogUIHTML(t)

	// Observe mode: the banner and the per-agent note must both say no action
	// was taken.
	for _, snippet := range []string{
		"Observe only — no agent is being restarted or paused.",
		"observe mode — reported only, no restart or pause was performed",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("observe mode must be unmistakable; missing %q", snippet)
		}
	}

	// Positive control: heal mode states the opposite, so the observe text is
	// a real distinction rather than the only thing the UI ever says.
	if !strings.Contains(html, "Healing is active.") {
		t.Error("heal mode must be distinguished from observe (positive control)")
	}

	// The per-agent note is gated on the mode, not shown unconditionally.
	if !strings.Contains(html, `const observing = a.watchdogMode === 'observe';`) {
		t.Error("the per-agent conditions row must branch on the watchdog's actual mode")
	}

	// The fleet-wide kill switch overrides a saved heal, so the UI must say the
	// stored value is not the value in force.
	if !strings.Contains(html, "Fleet-wide pause active.") {
		t.Error("an engaged kill switch must be surfaced — a saved 'heal' that is not in force is a lie")
	}
}

// TestWatchdogConditionsRenderOnAgentCard asserts the observed condition set is
// surfaced next to the agent it describes.
func TestWatchdogConditionsRenderOnAgentCard(t *testing.T) {
	html := watchdogUIHTML(t)
	if !strings.Contains(html, "${watchdogConditionsRow(a)}") {
		t.Error("the agent card must render the watchdog's observed conditions")
	}
	// Conditions are read from the payload field the server actually sends.
	if !strings.Contains(html, "a.conditions || []") {
		t.Error("the conditions row must read the conditions payload field")
	}
	// An agent the watchdog has not swept renders nothing rather than an
	// empty widget claiming health it never observed.
	if !strings.Contains(html, "if (!conds.length) return '';") {
		t.Error("an unswept agent must render no conditions rather than implying health")
	}
}

// ── #7254: watchdog activity strip on the Health tab ─────────────────────────

// TestWatchdogActivityStripRendersOnHealthTab asserts the activity strip lands
// directly under the mode banner, carries every element id the fill code
// targets, is fetched when the Health tab opens, and reads the endpoint that
// serves it.
func TestWatchdogActivityStripRendersOnHealthTab(t *testing.T) {
	html := watchdogUIHTML(t)

	// The shell renders inside the watchdog section, between the mode banners
	// and the mode select — under the banner, as #7254 specifies.
	banner := strings.Index(html, "${pausedBanner}${healBanner}${observeBanner}")
	shell := strings.Index(html, "${renderWatchdogActivityShell()}")
	modeSelect := strings.Index(html, `data-arg1="watchdogMode"`)
	if banner < 0 || shell < 0 || modeSelect < 0 {
		t.Fatalf("banner/shell/mode markers not all found: %d %d %d", banner, shell, modeSelect)
	}
	if !(banner < shell && shell < modeSelect) {
		t.Errorf("activity strip must sit directly under the mode banner and above the mode select (banner=%d shell=%d select=%d)", banner, shell, modeSelect)
	}

	// Every id the fill code targets is declared once, in the id table.
	for _, id := range []string{
		"watchdog-activity",
		"watchdog-activity-count",
		"watchdog-activity-spark",
		"watchdog-activity-actions",
		"watchdog-liveness",
		"watchdog-promotion-hint",
	} {
		if !strings.Contains(html, `'`+id+`'`) {
			t.Errorf("index.html must declare the strip element id %q", id)
		}
	}
	// The promotion hint renders beside the mode select (its own div, under
	// the select, inside the same config-field).
	if !strings.Contains(html, `<div id="watchdog-promotion-hint"`) {
		t.Error("the promotion hint container must render next to the mode select")
	}

	// Fetched when the Health tab opens, from the endpoint this PR adds, with
	// the 30-day window the issue specifies.
	if !strings.Contains(html, "if (tabId === 'Health') { loadEscalationConfig(); loadWatchdogActivity(); }") {
		t.Error("opening the Health tab must load the watchdog activity readout")
	}
	if !strings.Contains(html, "`/api/watchdog/activity?days=${WATCHDOG_ACTIVITY_DAYS}`") {
		t.Error("the strip must read GET /api/watchdog/activity")
	}
	if !strings.Contains(html, "const WATCHDOG_ACTIVITY_DAYS = 30;") {
		t.Error("the strip's window must be the 30 days #7254 specifies")
	}
	// Saving a mode change invalidates the cached readout, so the headline
	// ("would have taken" vs "taken") follows the mode just saved.
	if !strings.Contains(html, "invalidateWatchdogActivity();") {
		t.Error("the watchdog save path must invalidate the cached activity readout")
	}
}

// TestWatchdogActivityStripIsHonestAboutMode is the same honesty rule the
// banners follow: in observe the count is what the watchdog WOULD have done;
// in heal it is what it did. Both wordings must exist, gated on `acting`.
func TestWatchdogActivityStripIsHonestAboutMode(t *testing.T) {
	html := watchdogUIHTML(t)
	for _, snippet := range []string{
		"the watchdog WOULD have taken in ${a.days} d",
		"taken by the watchdog in ${a.days} d",
		"Safe to promote to Heal",
		"Not yet safe to promote",
		// Heal mode shows the last action instead of a promotion verdict.
		"No action taken in ${a.days} d.",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("activity strip is missing %q", snippet)
		}
	}
	// Severity colouring per #7254: observed muted, restart amber, pause /
	// give-up red — and the severe check spans the -observed twins.
	if !strings.Contains(html, "return b === 'crashloop-pause' || b === 'giveup';") {
		t.Error("severity must key on the crash-loop pause and give-up verdicts")
	}
	// The histogram must not carry zeros forward: a zero day is the news.
	if !strings.Contains(html, "return axisSparkSvg(daily, d => d.count || 0") {
		t.Error("the daily histogram must render through axisSparkSvg (no zero carry-forward)")
	}
}

// TestWatchdogActivityUIHasNoUndefinedCallees extends the ReferenceError guard
// to the strip's helpers.
func TestWatchdogActivityUIHasNoUndefinedCallees(t *testing.T) {
	html := watchdogUIHTML(t)
	for _, fn := range []string{
		"renderWatchdogActivityShell",
		"loadWatchdogActivity",
		"fillWatchdogActivity",
		"invalidateWatchdogActivity",
		"watchdogActivitySparkSvg",
		"watchdogLivenessTable",
		"watchdogPromotionHintHTML",
		"watchdogActivityColor",
		"axisSparkSvg",
		"relativeAge",
		"fmtSparkVal",
	} {
		declared := regexp.MustCompile(`(?m)^\s*(async\s+)?(function\s+` + fn + `\s*\(|const\s+` + fn + `\s*=)`)
		if !declared.MatchString(html) {
			t.Errorf("watchdog activity UI calls %q but it is never declared", fn)
		}
	}
}

// TestWatchdogConfigPayloadReportsResolvedSettings asserts the Health payload
// carries the settings actually IN FORCE, so a hive that never wrote a
// governor.watchdog block shows real values rather than blanks.
func TestWatchdogConfigPayloadReportsResolvedSettings(t *testing.T) {
	cfg := &config.Config{}
	got := watchdogConfigPayload(cfg)
	if got == nil {
		t.Fatal("payload must be built for a config with no watchdog block")
	}

	if got["mode"] != string(watchdog.DefaultMode) {
		t.Errorf("mode = %v, want the resolved default %q", got["mode"], watchdog.DefaultMode)
	}
	if got["crashLoopAfter"] != watchdog.DefaultCrashLoopAfter {
		t.Errorf("crashLoopAfter = %v, want the resolved default %d", got["crashLoopAfter"], watchdog.DefaultCrashLoopAfter)
	}
	if got["probeIntervalS"] != int(watchdog.DefaultProbeInterval.Seconds()) {
		t.Errorf("probeIntervalS = %v, want %v", got["probeIntervalS"], watchdog.DefaultProbeInterval.Seconds())
	}
	ladder, ok := got["backoff"].([]string)
	if !ok || len(ladder) == 0 {
		t.Fatalf("backoff ladder must be reported for display, got %v", got["backoff"])
	}
	if ladder[0] != "1m0s" {
		t.Errorf("backoff ladder starts at %q, want the RFC's 1m", ladder[0])
	}

	// An explicitly configured mode is reported as configured.
	cfg.Governor.Watchdog.Mode = "heal"
	if got := watchdogConfigPayload(cfg); got["mode"] != "heal" {
		t.Errorf("mode = %v, want heal", got["mode"])
	}
}

// TestValidateGovernorWatchdogRejectsFeatureDefeatingValues covers the bounds
// that keep a typed value from turning the feature off rather than tuning it.
func TestValidateGovernorWatchdogRejectsFeatureDefeatingValues(t *testing.T) {
	cases := []struct {
		name           string
		mode           string
		probe          int
		crashLoop      int
		healthyReset   string
		wantErr        bool
		wantErrKeyword string
	}{
		{name: "all empty is a no-op", wantErr: false},
		{name: "valid full set", mode: "heal", probe: 300, crashLoop: 5, healthyReset: "30m"},
		{name: "unknown mode refused", mode: "healz", wantErr: true, wantErrKeyword: "mode"},
		{name: "probe interval too low", probe: 1, wantErr: true, wantErrKeyword: "probe interval"},
		{name: "probe interval too high", probe: 999999, wantErr: true, wantErrKeyword: "probe interval"},
		{name: "crash loop zero would give up before trying", crashLoop: -1, wantErr: true, wantErrKeyword: "crash-loop"},
		{name: "crash loop absurdly high", crashLoop: 5000, wantErr: true, wantErrKeyword: "crash-loop"},
		{name: "healthy reset unparseable", healthyReset: "soon", wantErr: true, wantErrKeyword: "duration"},
		{name: "healthy reset too short to prevent laundering", healthyReset: "1s", wantErr: true, wantErrKeyword: "healthy reset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGovernorWatchdog(tc.mode, tc.probe, tc.crashLoop, tc.healthyReset)
			if tc.wantErr && err == nil {
				t.Fatal("want an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantErr && tc.wantErrKeyword != "" && !strings.Contains(err.Error(), tc.wantErrKeyword) {
				t.Fatalf("error %q must name the offending field (%q)", err, tc.wantErrKeyword)
			}
		})
	}
}
