package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #7099: the browser re-applied the ACMM pack-roster filter inside
// applyACMMVisibility with NO "active outside pack" escape hatch, so an agent
// the server deliberately published in status.agents (running/enabled but
// outside the level's roster — e.g. a custom agent named "review") had its
// sidebar nav item AND its card hidden on every status render. That put the
// terminal link, Login button, restart and config gear out of reach for the
// exact agent the watchdog was raising a re-auth alert about.
//
// The fix (option 1 in the issue) deletes the redundant client pack filter and
// lets status.agents — already gated server-side WITH the exception — be
// authoritative. This test EXECUTES applyACMMVisibility against a DOM stub and
// pins the user-visible invariant: an out-of-pack agent's nav item and card
// must remain visible after the function runs.
//
// Skips loudly when node is unavailable: a silently skipped behavioural test is
// the "fails green" shape the repo's structure tests exist to avoid.
func TestApplyACMMVisibilityKeepsOutOfPackAgentVisible7099(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the client visibility rule was NOT executed by this run")
	}

	html := indexHTML(t)
	script := jsFunc(t, html, "applyACMMVisibility") + "\n" + clientPackVisibilityAssertions

	dir := t.TempDir()
	path := filepath.Join(dir, "visibility.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("client pack-visibility behaviour check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

// clientPackVisibilityAssertions builds a minimal DOM whose only agents are
// "supervisor" (in the pack roster) and "review" (a custom agent in NO roster,
// running — the #7099 scenario). It runs applyACMMVisibility at L5 with a pack
// roster that EXCLUDES review, then asserts review's nav item and card are not
// display:none. If the redundant client pack filter is ever re-introduced, the
// review elements are hidden and these checks fail.
const clientPackVisibilityAssertions = `
function makeEl(attrs, sels) {
  return {
    style: { display: '' },
    _attrs: attrs || {},
    _sels: sels || [],
    getAttribute(k) { return (k in this._attrs) ? this._attrs[k] : null; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    matchesSel(sel) { return this._sels.indexOf(sel) !== -1; },
  };
}

const navSupervisor = makeEl({ 'data-agent-nav': 'supervisor' }, ['.oc-nav-item', '[data-agent-nav]']);
const navReview = makeEl({ 'data-agent-nav': 'review' }, ['.oc-nav-item', '[data-agent-nav]']);
const cardSupervisor = makeEl({ 'data-agent': 'supervisor' }, ['.agent-card[data-agent]']);
const cardReview = makeEl({ 'data-agent': 'review' }, ['.agent-card[data-agent]']);

const registry = {
  '.oc-nav-group': [],
  '.oc-nav-item': [navSupervisor, navReview],
  '[data-agent-nav]': [navSupervisor, navReview],
  '.agent-card[data-agent]': [cardSupervisor, cardReview],
};

global.document = {
  getElementById() { return makeEl(); },
  querySelectorAll(sel) { return registry[sel] || []; },
};

// L5, pack roster that surfaces supervisor but NOT the custom "review" agent —
// exactly what the server sends as acmmPackAgents. status.agents (from which
// the nav/cards were built) already includes review via the escape hatch.
applyACMMVisibility(5, ['supervisor']);

let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}

check('out-of-pack agent nav item stays visible', navReview.style.display !== 'none');
check('out-of-pack agent card stays visible', cardReview.style.display !== 'none');
check('in-pack agent nav item stays visible', navSupervisor.style.display !== 'none');
check('in-pack agent card stays visible', cardSupervisor.style.display !== 'none');

process.exit(fails ? 1 : 0);
`
