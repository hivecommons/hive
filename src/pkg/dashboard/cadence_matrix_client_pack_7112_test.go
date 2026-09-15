package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #7112: the Governor cadence matrix filtered its rows by acmmPackAgents — the
// RAW ACMM pack roster — with no "active outside pack" escape hatch, so a
// running agent the server deliberately publishes in status.agents (e.g. a
// custom "review" agent, running, outside the level roster) had its cadence row
// dropped even after #7110 restored its nav item and card. The operator could
// reach the agent's terminal but could not see when it was kicked.
//
// The fix makes renderCadenceMatrix follow status.agents (window._lastAgents) —
// the set the server already gated WITH the exception (buildAgentsWithHidden) —
// rather than re-deriving the raw roster. This test EXECUTES renderCadenceMatrix
// against a fixture and pins three invariants at once:
//
//  1. every agent present in status.agents that has a cadence row keeps it
//     (the matrix's filter source never omits a status.agents member);
//  2. a running out-of-pack agent ("review") keeps its cadence row;
//  3. a genuinely pack-inactive "ghost" ("linter", not in status.agents) is
//     STILL suppressed — the fix does not disable pack filtering wholesale.
//
// Mutating the implementation:
//   - reinstating the unconditional acmmPackAgents packSet filter drops the
//     "review" row → invariants 1 & 2 fail (this reproduces #7112);
//   - removing the status.agents filter entirely surfaces the "linter" ghost
//     row → invariant 3 fails.
//
// Skips loudly when node is unavailable: a silently skipped behavioural test is
// the "fails green" shape the repo's structure tests exist to avoid.
func TestRenderCadenceMatrixKeepsOutOfPackRunningRow7112(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the cadence-matrix filter rule was NOT executed by this run")
	}

	html := indexHTML(t)
	script := jsFunc(t, html, "renderCadenceMatrix") + "\n" + cadenceMatrixPackAssertions

	dir := t.TempDir()
	path := filepath.Join(dir, "cadence_matrix.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("cadence-matrix pack-filter behaviour check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

// cadenceMatrixPackAssertions stubs renderCadenceMatrix's render-only helpers
// (esc/cliChip/modelChip/_sortAgentsBySidebar) so the extracted function runs in
// isolation, then drives it with the #7112 scenario:
//
//   - window._lastAgents (status.agents) = supervisor (pack), review
//     (out-of-pack, running). "linter" is a pack-inactive ghost the server did
//     NOT surface, so it is absent here.
//   - window._lastStatus.acmmPackAgents = the RAW roster (supervisor only) —
//     it contains neither review nor linter, exactly as buildACMMPackAgents
//     publishes it.
//   - the cadence matrix (server buildCadenceMatrix output) has a row for
//     supervisor, review AND the ghost linter.
//
// The produced HTML must contain rows for supervisor and review and must NOT
// contain a row for linter.
const cadenceMatrixPackAssertions = `
function esc(s) { return String(s == null ? '' : s); }
function cliChip() { return ''; }
function modelChip() { return ''; }
function _sortAgentsBySidebar(agents) { return { sorted: agents.slice() }; }

global.window = {
  _lastAgents: [
    { name: 'supervisor', busy: 'working' },
    { name: 'review', busy: 'working' },
  ],
  _lastStatus: { acmmPackAgents: ['supervisor'] },
};

const modes = ['idle', 'quiet', 'busy', 'surge'];
const cell = { idle: 'off', quiet: 'off', busy: 'off', surge: 'off' };
const matrix = [
  Object.assign({ agent: 'supervisor' }, cell),
  Object.assign({ agent: 'review' }, cell),
  Object.assign({ agent: 'linter' }, cell),
];

const out = renderCadenceMatrix(matrix, 'idle', modes);

let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}

check('in-pack agent keeps its cadence row', out.indexOf('data-agent="supervisor"') !== -1);
check('out-of-pack running agent keeps its cadence row', out.indexOf('data-agent="review"') !== -1);
check('pack-inactive ghost agent row stays suppressed', out.indexOf('data-agent="linter"') === -1);

process.exit(fails ? 1 : 0);
`
