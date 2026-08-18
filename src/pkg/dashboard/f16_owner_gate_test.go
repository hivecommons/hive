package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Audit F16 (2026-08-13). Three dashboard surfaces performed privileged
// mutations with NO owner gate, so any authenticated read-write member could
// reach them:
//
//   - PUT /api/config/governor/security — the body carries agentSandboxEnabled,
//     so a read-write member could TURN OFF THE AGENT SANDBOX. Every sibling
//     governor-config handler was already owner-gated; this one was the outlier.
//   - POST /api/agents and POST /api/agents/import — create paths that write an
//     agent file and start a live process. The asymmetry proved the intent:
//     handleAgentDelete in the same file WAS gated.
//   - POST /api/plan/{epicID}/{approve,reject,child} — the plan-review gate that
//     decompose.go relies on to withhold an epic's children "until a human
//     approves the plan".
//
// These tests assert the INVARIANT at the source level rather than one
// handler's behaviour, because the failure mode for this class of bug in this
// repo is a merge silently dropping a gate, not a logic bug. Finding F14
// regressed exactly that way: the fix commit is an ancestor of v4, yet the gate
// was gone at HEAD because a v2->v4 sync merge resolved the conflict in favour
// of the older side. A behavioural test on a single handler does not catch that.
//
// Shape follows api_contribute_owner_gate_test.go (#3713).

// ownerGatedF16Handlers maps each handler this fix gated to the file it lives
// in. Adding a handler here without gating it fails the test, and vice versa.
var ownerGatedF16Handlers = map[string]string{
	"handleGovernorSecurity": "api_governor_security.go",
	"handleAgentCreate":      "api_agents.go",
	"handleAgentImport":      "api_agents.go",
	"handlePlanApprove":      "plan_handlers.go",
	"handlePlanReject":       "plan_handlers.go",
	"handlePlanChild":        "plan_handlers.go",
}

// f16HandlerBody returns the source of the named handler, from its func line to
// the first line that is exactly "}".
func f16HandlerBody(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "func (s *Server) "+name+"(")
	if i < 0 {
		t.Fatalf("handler %s not found — it was renamed or removed; update this test deliberately, do not delete the case", name)
	}
	j := strings.Index(src[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("could not find the end of %s", name)
	}
	return src[i : i+j]
}

func f16ReadSource(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return string(raw)
}

// TestF16PrivilegedHandlersAreOwnerGated is the F16 regression.
func TestF16PrivilegedHandlersAreOwnerGated(t *testing.T) {
	for name, file := range ownerGatedF16Handlers {
		body := f16HandlerBody(t, f16ReadSource(t, file), name)
		if !strings.Contains(body, "requireOwnerRole(w, r)") {
			t.Errorf("%s (%s) has no requireOwnerRole gate — a read-write member can reach a privileged mutation (audit F16). "+
				"If a merge dropped this, restore the gate rather than relaxing the test.", name, file)
		}
		// Weaker gates admit RoleReadWrite (or better) and are NOT sufficient
		// for these surfaces. Catch a downgrade as well as an outright removal.
		for _, weak := range []string{
			"s.requireContributorWrite(w, r)",
			"requireMergerOrOwnerRole(w, r)",
		} {
			if strings.Contains(body, weak) {
				t.Errorf("%s (%s) gates on %s, which admits a non-owner role; this surface is owner-only", name, file, weak)
			}
		}
	}
}

// TestF16AgentSandboxToggleIsOwnerOnly pins the single highest-impact item
// explicitly, so the reason this fix exists survives even if someone prunes the
// table above. handleGovernorSecurity is the only handler that writes
// cfg.AgentSandbox.Enabled from request input.
func TestF16AgentSandboxToggleIsOwnerOnly(t *testing.T) {
	body := f16HandlerBody(t, f16ReadSource(t, "api_governor_security.go"), "handleGovernorSecurity")
	if !strings.Contains(body, "AgentSandboxEnabled") {
		t.Skip("handleGovernorSecurity no longer accepts agentSandboxEnabled; the sandbox toggle moved — re-point this test")
	}
	if !strings.Contains(body, "requireOwnerRole(w, r)") {
		t.Error("handleGovernorSecurity accepts agentSandboxEnabled but is not owner-gated — " +
			"a read-write member can disable the agent sandbox (audit F16)")
	}
}

// --- POSITIVE CONTROLS -------------------------------------------------------
//
// Without these, a blanket "add requireOwnerRole to every handler in these
// files" would satisfy the tests above while breaking legitimate workflows.
// Over-gating is a real cost, not a safe default — #3713 deliberately left
// handleContributorRequeue un-gated for exactly this reason.

// TestF16ReadOnlyPlanAndAgentReadsStayUngated asserts the GET handlers that sit
// beside the gated mutations are still reachable by a read-only member.
func TestF16ReadOnlyPlanAndAgentReadsStayUngated(t *testing.T) {
	for name, file := range map[string]string{
		"handlePlanTree":   "plan_handlers.go",
		"handleAgentsList": "api_agents.go",
	} {
		body := f16HandlerBody(t, f16ReadSource(t, file), name)
		if strings.Contains(body, "requireOwnerRole(w, r)") {
			t.Errorf("%s (%s) must NOT be owner-gated — it is a read-only GET that the "+
				"review UI calls for every viewer; owner-gating it blanks the plan/agent views "+
				"for non-owners", name, file)
		}
	}
}

// TestF16PlanFromIssueStaysUngated is the substantive control: a mutating
// handler in a file this fix touched that must stay reachable by a normal
// contributor. handlePlanFromIssue mints a DRAFT epic — decompose.go withholds
// a draft epic's children from Ready(), so proposing a plan releases no work on
// its own. It is the "ask for a plan" action, and the owner gate belongs on
// approve (which releases the children), not here.
func TestF16PlanFromIssueStaysUngated(t *testing.T) {
	body := f16HandlerBody(t, f16ReadSource(t, "plan_handlers.go"), "handlePlanFromIssue")
	if strings.Contains(body, "requireOwnerRole(w, r)") {
		t.Error("handlePlanFromIssue must NOT be owner-gated — proposing a plan creates a " +
			"draft epic whose children stay gated until approve; gating it here would make " +
			"planning owner-only end to end and break the contributor workflow")
	}
}

// --- COUNT FLOORS ------------------------------------------------------------

// TestF16OwnerGateCountFloor is the blunt instrument that survives renames: if
// a file's gate count drops below what this fix established, something removed
// a gate even if the handler names changed. Cheap, and exactly the signal a
// sync merge would trip.
func TestF16OwnerGateCountFloor(t *testing.T) {
	// Minimum requireOwnerRole call sites per file after F16.
	// api_agents.go: create + import + delete (delete predates this fix).
	// plan_handlers.go: approve + reject + child.
	// api_governor_security.go: security.
	want := map[string]int{
		"api_governor_security.go": 1,
		"api_agents.go":            3,
		"plan_handlers.go":         3,
	}
	gate := regexp.MustCompile(`requireOwnerRole\(w, r\)`)
	for file, min := range want {
		got := len(gate.FindAllString(f16ReadSource(t, file), -1))
		if got < min {
			t.Errorf("%s has %d requireOwnerRole gates, want at least %d — a gate was removed "+
				"(audit F14 regressed exactly this way, via a sync merge)", file, got, min)
		}
	}
}
