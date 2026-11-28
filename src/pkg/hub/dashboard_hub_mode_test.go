package hub

import (
	"os/exec"
	"strings"
	"testing"
)

// dashOpenTag returns the opening tag of the element carrying id.
func dashOpenTag(t *testing.T, html, id string) string {
	t.Helper()
	at := strings.Index(html, `id="`+id+`"`)
	if at < 0 {
		t.Fatalf("dashboard is missing id=%q", id)
	}
	start := strings.LastIndex(html[:at], "<")
	end := strings.Index(html[at:], ">")
	return html[start : at+end+1]
}

// TestDashboardSelfHostedModeHidesHostedFleetControls pins #20199: when the
// hub reports hub_mode "self-hosted" (#20196) every hosted-fleet control is
// hidden and Register your own hive is the only way to add a hive. With the
// mode unset nothing changes: the hiding rule only applies under the
// hub-self-hosted root class, which applyHubMode never sets for hosted.
func TestDashboardSelfHostedModeHidesHostedFleetControls(t *testing.T) {
	html := dashScript(t)

	if !strings.Contains(html, ".hub-self-hosted .hosted-fleet-only { display: none !important; }") {
		t.Fatal("dashboard must hide .hosted-fleet-only only under the hub-self-hosted root class")
	}
	for _, id := range []string{
		"btn-add-hive",           // + Add Hosted Hive
		"btn-request-hive",       // request a hosted hive
		"cluster-health-section", // Cluster Health
		"scale-controls-summary", // wave / workers / per-cluster / kubectl summary
		"scale-globals",          // the four fleet knobs
	} {
		if tag := dashOpenTag(t, html, id); !strings.Contains(tag, "hosted-fleet-only") {
			t.Errorf("%s must carry hosted-fleet-only: %s", id, tag)
		}
	}
	if tag := dashOpenTag(t, html, "btn-register-hive"); strings.Contains(tag, "hosted-fleet-only") {
		t.Errorf("Register your own hive must stay visible in self-hosted mode: %s", tag)
	}
	for _, want := range []string{
		`<div class="hosted-fleet-only" style="font-size:0.85rem;color:var(--text);margin:var(--sp-5) var(--sp-0) var(--sp-4);font-weight:600">Per-cluster limits</div>`,
		`<div class="table-wrap hosted-fleet-only"><table`,
		`<div class="hosted-fleet-only" style="display:flex;align-items:center;gap:var(--sp-5);margin-top:14px">`,
		`'<a class="hv-btn btn-primary btn-primary hosted-fleet-only" href="/get-started" style="text-decoration:none" title="We host the hive for you">Request a hive</a>'`,
		`(_hubSelfHosted ? '' : renderUsageSection('By cluster', d.byCluster, 'cluster'))`,
		`(_hubSelfHosted ? '' : '<th onclick="sortUsers(\'saas_quota\')" style="cursor:pointer">Quota ⇅</th>')`,
		`(_hubSelfHosted ? '' : '<td><input type="number" min="0" max="10" value="' + (u.saas_quota || 0) + '"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard is missing the self-hosted gate: %s", want)
		}
	}
	// The optional feature panels share the Scale Controls section and must
	// stay reachable in self-hosted mode.
	for _, id := range []string{"scale-controls-section", "customers-share-setting", "ask-setting"} {
		if tag := dashOpenTag(t, html, id); strings.Contains(tag, "hosted-fleet-only") {
			t.Errorf("%s must not be hidden in self-hosted mode: %s", id, tag)
		}
	}
	if !strings.Contains(npsHubJSFunc(t, html, "loadUser"), "applyHubMode(hubSelfHosted(data));") {
		t.Error("loadUser must apply the hub mode from /api/auth/user")
	}
	if !strings.Contains(npsHubJSFunc(t, html, "loadClusterHealth"), "if (!_isAdmin || _hubSelfHosted) return;") {
		t.Error("loadClusterHealth must not poll cluster health on a self-hosted hub")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; hubSelfHosted/applyHubMode behavior was NOT executed by this run")
	}
	script := "var _hubSelfHosted = false;\nvar HUB_MODE_SELF_HOSTED = 'self-hosted';\n" +
		npsHubJSFunc(t, html, "hubSelfHosted") + "\n" + npsHubJSFunc(t, html, "applyHubMode") + `
const classes = new Set();
globalThis.document = { documentElement: { classList: {
  toggle: (c, on) => { if (on) classes.add(c); else classes.delete(c); },
} } };
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }
check('unset mode is hosted', hubSelfHosted({authenticated: true}) === false);
check('hosted mode is hosted', hubSelfHosted({hub_mode: 'hosted'}) === false);
check('unknown mode is hosted', hubSelfHosted({hub_mode: 'saas'}) === false);
check('missing payload is hosted', hubSelfHosted(null) === false);
check('self-hosted mode', hubSelfHosted({hub_mode: 'self-hosted'}) === true);
check('self-hosted mode is case-insensitive', hubSelfHosted({hub_mode: 'Self-Hosted'}) === true);
applyHubMode(hubSelfHosted({authenticated: true}));
check('unset mode leaves the root class off', !classes.has('hub-self-hosted') && _hubSelfHosted === false);
applyHubMode(hubSelfHosted({hub_mode: 'self-hosted'}));
check('self-hosted sets the root class', classes.has('hub-self-hosted') && _hubSelfHosted === true);
applyHubMode(hubSelfHosted({hub_mode: 'hosted'}));
check('hosted clears the root class', !classes.has('hub-self-hosted') && _hubSelfHosted === false);
if (fails) process.exit(1);
`
	runDashboardJS(t, node, script)
}
