package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// Sidebar navigation used to issue a single scrollIntoView and hope the page
// stayed put. Sections above the target re-render on every status tick,
// lazy panels fill in and collapsed sections resize, so the scroll finished
// at a stale Y and the reader had to click the item again. These tests pin
// the shared settle helper that both the section and agent-detail scrolls
// now route through, and the render-time anchor that pins the nav target.

func TestSidebarNavScrollRoutesThroughSettleHelper(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function ocSettleScroll(targetY, apply, settleMs, anchorEl)",
		"const OC_NAV_SETTLE_MS = 2500;",
		"let _ocNavAnchorEl = null;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — sidebar scroll settle helper regressed", snippet)
		}
	}
	section := jsFunc(t, html, "ocScrollSectionIntoView")
	if !strings.Contains(section, "ocSettleScroll(targetY, () => el.scrollIntoView({ block: 'start', behavior: 'smooth' }), OC_NAV_SETTLE_MS, el);") {
		t.Errorf("ocScrollSectionIntoView does not hold its anchor through the settle helper:\n%s", section)
	}
	agent := jsFunc(t, html, "ocScrollToAgentDetail")
	if !strings.Contains(agent, "ocSettleScroll(targetY,") {
		t.Errorf("ocScrollToAgentDetail bypasses the shared settle helper:\n%s", agent)
	}
	capture := jsFunc(t, html, "captureScrollAnchor")
	if !strings.Contains(capture, "anchor.el = navEl; anchor.top = navEl.getBoundingClientRect().top;") {
		t.Errorf("captureScrollAnchor does not pin the in-flight sidebar target:\n%s", capture)
	}
	if !strings.Contains(html, "#lifecycle-section { scroll-margin-top:") {
		t.Error("lifecycle-section lost its sticky-navbar scroll-margin-top")
	}
}

// TestSidebarNavScrollSettleReanchorsAndCancels executes ocSettleScroll
// against a tiny browser stand-in: a layout shift above the target must
// re-issue the scroll, a shift inside tolerance must not, user wheel input
// must cancel the hold (and release the render anchor), and the deadline
// must end the loop on its own.
func TestSidebarNavScrollSettleReanchorsAndCancels(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — ocSettleScroll was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
const OC_SCROLL_TOLERANCE_PX = 4;
const OC_SCROLL_CANCEL_EVENTS = ['wheel', 'touchstart', 'mousedown', 'keydown'];
let _ocScrollSettleCancel = null;
let _ocNavAnchorEl = null;
let now = 0;
const performance = { now(){ return now; } };
let rafs = [];
function requestAnimationFrame(fn){ rafs.push(fn); }
function runRafs(){ const q = rafs.slice(); rafs = []; q.forEach(fn => fn()); }
const listeners = {};
const window = {
  scrollY: 0, innerHeight: 800,
  addEventListener(type, fn){ listeners[type] = fn; },
  removeEventListener(type, fn){ if (listeners[type] === fn) delete listeners[type]; },
};
const document = { documentElement: { scrollHeight: 10000 }, body: { scrollHeight: 10000 } };
let top = 1000;
const el = { id: 'target' };
let applies = 0;
function assert(cond, msg){ if (!cond) throw new Error(msg); }
` + jsFunc(t, html, "ocMaxScrollY") + "\n" + jsFunc(t, html, "ocSettleScroll") + `
ocSettleScroll(() => top, () => { applies++; }, 2500, el);
assert(applies === 1, 'initial scroll not issued');
assert(_ocNavAnchorEl === el, 'nav anchor not exposed during hold');
assert(typeof listeners.wheel === 'function', 'wheel cancel listener missing');
runRafs();
assert(applies === 1, 'settle re-scrolled without drift');
top += 3;
runRafs();
assert(applies === 1, 'settle re-scrolled inside tolerance');
top += 120;
runRafs();
assert(applies === 2, 'layout drift above the target did not re-anchor');
runRafs();
assert(applies === 2, 'settle re-scrolled twice for one drift');
listeners.wheel();
assert(_ocNavAnchorEl === null, 'user input did not release the render anchor');
assert(_ocScrollSettleCancel === null, 'user input did not clear the active hold');
top += 500;
runRafs();
assert(applies === 2, 'settle kept correcting after user input');

applies = 0; rafs = [];
ocSettleScroll(() => top, () => { applies++; }, 2500, el);
now = 2600;
runRafs();
assert(_ocNavAnchorEl === null, 'deadline did not release the render anchor');
assert(rafs.length === 0, 'settle loop kept running past its deadline');

// Clamp: a target past the bottom of the page is anchored at max scroll, so
// growth below the fold is not mistaken for drift.
applies = 0; rafs = []; now = 0;
top = 20000;
ocSettleScroll(() => top, () => { applies++; }, 2500, el);
top = 25000;
runRafs();
assert(applies === 1, 'drift beyond max scroll re-anchored needlessly');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node ocSettleScroll failed: %v\n%s", err, out)
	}
}
