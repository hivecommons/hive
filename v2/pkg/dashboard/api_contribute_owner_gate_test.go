package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Contributor trust, agent-role grants, revoke and delete are OWNER-only. They
// were gated by #3460 (f575b06c) and then silently REVERTED: that commit is an
// ancestor of v4, yet every gate was back to requireContributorWrite at HEAD —
// a v2->v4 sync merge resolved the conflict in favour of the older side.
// Audit F14 (2026-08-13) re-reproduced it.
//
// requireContributorWrite admits RoleOwner OR RoleReadWrite, so the regression
// let any read-write member grant trust, hand out agent roles, and delete other
// contributors.
//
// This test asserts the INVARIANT rather than the behaviour of one handler,
// because the failure mode is a merge dropping the gate, not a logic bug. A
// behavioural test on a single handler would not have caught the revert.

var ownerGatedContributorHandlers = []string{
	"handleContributorTrust",
	"handleContributorAgentRoleGrants",
	"handleContributorRevoke",
	"handleContributorDelete",
}

// handlerBody returns the source of the named handler, from its func line to
// the first line that is exactly "}".
func handlerBody(t *testing.T, src, name string) string {
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

// TestContributorManagementHandlersAreOwnerGated is the F14 regression.
func TestContributorManagementHandlersAreOwnerGated(t *testing.T) {
	raw, err := os.ReadFile("api_contribute.go")
	if err != nil {
		t.Fatalf("read api_contribute.go: %v", err)
	}
	src := string(raw)

	for _, name := range ownerGatedContributorHandlers {
		body := handlerBody(t, src, name)
		if !strings.Contains(body, "requireOwnerRole(w, r)") {
			t.Errorf("%s has no requireOwnerRole gate — a read-write member can reach it (audit F14). "+
				"If a merge dropped this, restore it rather than relaxing the test.", name)
		}
		if strings.Contains(body, "s.requireContributorWrite(w, r)") {
			t.Errorf("%s still gates on requireContributorWrite, which admits RoleReadWrite; "+
				"contributor management is owner-only", name)
		}
	}
}

// TestContributorRequeueStaysContributorWrite is the positive control. #3460
// deliberately did NOT gate requeue — it is a normal contributor action. Without
// this case, "add requireOwnerRole everywhere in the file" would satisfy the
// test above while breaking a legitimate workflow.
func TestContributorRequeueStaysContributorWrite(t *testing.T) {
	raw, err := os.ReadFile("api_contribute.go")
	if err != nil {
		t.Fatalf("read api_contribute.go: %v", err)
	}
	body := handlerBody(t, string(raw), "handleContributorRequeue")
	if !strings.Contains(body, "s.requireContributorWrite(w, r)") {
		t.Error("handleContributorRequeue must stay on requireContributorWrite — " +
			"requeue is a contributor action, not an owner action")
	}
	if strings.Contains(body, "requireOwnerRole(w, r)") {
		t.Error("handleContributorRequeue must NOT be owner-gated")
	}
}

// TestOwnerGateCountFloor is the blunt instrument that survives renames: if the
// count ever drops below the number of owner-gated handlers, something removed
// a gate. Cheap, and it is exactly the signal a sync merge would trip.
func TestOwnerGateCountFloor(t *testing.T) {
	raw, err := os.ReadFile("api_contribute.go")
	if err != nil {
		t.Fatalf("read api_contribute.go: %v", err)
	}
	got := len(regexp.MustCompile(`requireOwnerRole\(w, r\)`).FindAllString(string(raw), -1))
	if want := len(ownerGatedContributorHandlers); got < want {
		t.Errorf("api_contribute.go has %d requireOwnerRole gates, want at least %d — "+
			"a gate was removed (audit F14 regressed exactly this way)", got, want)
	}
}
