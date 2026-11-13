package dashboard

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// #18111: the pane-poll quota flag (crux.Process.QuotaExhausted) must reach
// the /api/status agent row for every backend, not only gateway agents whose
// classifier sets BackendAuthStatus "quota".
func TestBuildFrontendStatus_QuotaExhaustedReachesAgentRow(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "copilot", Model: "gpt-5", Enabled: true},
				"quality": {Backend: "copilot", Model: "gpt-5", Enabled: true},
			},
		},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	statuses := map[string]*crux.Process{
		"scanner": {
			Name:           "scanner",
			Config:         cfg.Agents["scanner"],
			State:          crux.StateRunning,
			OutputBuffer:   cruxtest.NewRingBuffer(10),
			QuotaExhausted: true,
		},
		"quality": {
			Name:         "quality",
			Config:       cfg.Agents["quality"],
			State:        crux.StateRunning,
			OutputBuffer: cruxtest.NewRingBuffer(10),
		},
	}

	payload := BuildFrontendStatus(gov.GetState(), nil, statuses, cfg, nil, gov, nil, nil, nil, nil)
	rows := map[string]FrontendAgent{}
	for _, a := range payload.Agents {
		rows[a.Name] = a
	}
	if !rows["scanner"].QuotaExhausted {
		t.Fatal("scanner row QuotaExhausted = false, want true from crux.Process.QuotaExhausted")
	}
	if rows["scanner"].BackendAuthStatus != "" {
		t.Fatalf("scanner BackendAuthStatus = %q, want empty: the quota flag must not rewrite the classifier verdict", rows["scanner"].BackendAuthStatus)
	}
	if rows["quality"].QuotaExhausted {
		t.Fatal("quality row QuotaExhausted = true, want false")
	}

	raw, err := json.Marshal(rows["scanner"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"quotaExhausted":true`) {
		t.Fatalf("scanner row JSON missing quotaExhausted:true: %s", raw)
	}
	raw, err = json.Marshal(rows["quality"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "quotaExhausted") {
		t.Fatalf("quality row JSON must omit quotaExhausted when false: %s", raw)
	}
}

// #18111: an out-of-quota agent is red and says "out of quota" on the top-bar
// tile, the collapsed tile and the agent card; login problems outrank it and
// disabled/paused/stopped agents keep their own look.
func TestAgentOutOfQuotaTileStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: agent out-of-quota tile states were not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
` + jsFunc(t, html, "agentIsDisabled") + `
` + jsFunc(t, html, "_agentNeedsLogin") + `
` + jsFunc(t, html, "agentOutOfQuota") + `
` + jsFunc(t, html, "agentCollapsedAuthInfo") + `
` + jsFunc(t, html, "agentCollapsedStateLabel") + `
` + jsFunc(t, html, "agentOutOfQuotaNoteHtml") + `
const live = { name: 'scanner', state: 'running', enabled: true };
let auth = agentCollapsedAuthInfo({ ...live, quotaExhausted: true }, false);
assert.deepEqual(auth, { label: 'out of quota', status: 'error', needsLogin: false, quota: true });
assert.equal(agentCollapsedStateLabel(false, false, auth), 'out of quota');

auth = agentCollapsedAuthInfo({ ...live, backendAuthStatus: 'quota' }, false);
assert.equal(auth.label, 'out of quota');
assert.equal(auth.status, 'error');

auth = agentCollapsedAuthInfo({ ...live, quotaExhausted: true, needsLogin: true }, false);
assert.equal(auth.label, 'logged out');
assert.equal(auth.quota, undefined);

auth = agentCollapsedAuthInfo({ ...live, enabled: false, quotaExhausted: true }, false);
assert.equal(auth.status, 'ok', 'disabled agent must not be painted red for quota');
assert.equal(auth.quota, undefined);

auth = agentCollapsedAuthInfo({ ...live, paused: true, quotaExhausted: true }, false);
assert.equal(auth.status, 'ok', 'paused agent must not be painted red for quota');

auth = agentCollapsedAuthInfo({ ...live, state: 'stopped', quotaExhausted: true }, true);
assert.equal(auth.quota, undefined, 'stopped agent keeps its stopped look');

auth = agentCollapsedAuthInfo({ ...live }, false);
assert.deepEqual(auth, { label: 'authenticated', status: 'ok', needsLogin: false });

assert.equal(agentOutOfQuota(null), false);
assert.equal(agentOutOfQuota({ backendAuthStatus: 'token-expired' }), false);

let note = agentOutOfQuotaNoteHtml({ backendAuthLastError: 'You have exceeded your monthly quota' });
assert.match(note, /Out of provider quota: this agent cannot run until the quota resets or it is moved to another provider\./);
assert.match(note, /You have exceeded your monthly quota/);
assert.doesNotMatch(note, /log ?in/i);
note = agentOutOfQuotaNoteHtml({});
assert.doesNotMatch(note, /agent-quota-note-detail/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("agent out-of-quota tile states failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestAgentOutOfQuotaRenderContracts(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		// card: quota state after disabled/login, never over paused/off/stopped
		"const outOfQuota = agentOutOfQuota(a) && !isDisabled && !needsLogin && !isStarting && !isPaused && !isOff && !isStopped;",
		"needsLogin ? 'needs-login' : outOfQuota ? 'out-of-quota' :",
		"isStopped ? 'stopped' : outOfQuota ? 'out-of-quota' :",
		"${outOfQuota ? agentOutOfQuotaNoteHtml(a) : ''}",
		// tiles
		"auth.quota ? 'out-of-quota' : ''",
		"`login: ${auth.quota ? 'authenticated' : auth.label}`",
		"...(auth.quota ? ['provider: out of quota'] : [])",
		// CSS uses existing red tokens
		".agent-card.out-of-quota { border-left-color: var(--red); }",
		".dot.out-of-quota { background: var(--status-error); }",
		".agent-collapsed-tile.out-of-quota .agent-tile-dot",
		".agent-navbar-tile.out-of-quota .agent-tile-dot",
		".agent-navbar-tile.stopped, .agent-navbar-tile.needs-login, .agent-navbar-tile.out-of-quota { border-color",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("out-of-quota render contract missing %q", want)
		}
	}
	if strings.Count(html, "auth.quota ? 'out-of-quota' : ''") != 2 {
		t.Error("both the collapsed tile and the top-bar tile must add the out-of-quota class")
	}
}
