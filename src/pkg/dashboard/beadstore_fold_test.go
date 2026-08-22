package dashboard

import (
	"strings"
	"testing"
	"time"
)

// Bead-store LOAD failures fold into AdvisoryError so the hub's stale gate
// fires immediately with the cause. Live incident (fma /
// llm-d-fast-model-actuation): /data/beads worker dirs were group-owned by a
// foreign gid, the server hit EACCES on every store at startup and silently
// dropped them, every digest built empty, and the advisory aged for 3 days
// with agents visibly working — nothing on the hub said why.
func TestAdvisoryState_FoldsBeadStoreFailuresForParticipant(t *testing.T) {
	s := newAdvisoryTestServer()
	s.deps = &Dependencies{BeadStoreLoadFailures: 3}
	s.RecordAdvisoryPost(3) // participant with a fresh post time

	_, _, errMsg := s.AdvisoryState()
	if errMsg == "" {
		t.Fatalf("an advisory participant with dropped bead stores must surface it as AdvisoryError")
	}
	if !strings.Contains(errMsg, "3 bead store(s) failed to load") {
		t.Fatalf("fold must name the dropped-store count, got %q", errMsg)
	}
}

// Participation gate: a hive that has never posted a digest must not be
// false-alarmed as advisory-stale by a bead-store failure — the failure still
// surfaces through dependency-admission coverage on the spoke, but the
// advisory pill belongs to advisory participants only.
func TestAdvisoryState_BeadStoreFoldSkipsNonParticipant(t *testing.T) {
	s := newAdvisoryTestServer()
	s.deps = &Dependencies{BeadStoreLoadFailures: 3}

	postedAt, _, errMsg := s.AdvisoryState()
	if !postedAt.IsZero() {
		t.Fatalf("precondition: a fresh server has never posted")
	}
	if errMsg != "" {
		t.Fatalf("a non-participant must not have bead-store failures folded in, got %q", errMsg)
	}
}

// Precedence: a real post error and the inference-auth cause are both more
// specific than the bead-store fold and must win.
func TestAdvisoryState_BeadStoreFoldIsOutrankedBySpecificCauses(t *testing.T) {
	s := newAdvisoryTestServer()
	s.deps = &Dependencies{BeadStoreLoadFailures: 1}
	s.RecordAdvisoryPost(3)

	s.RecordAdvisoryError("403 Resource not accessible by integration")
	if _, _, errMsg := s.AdvisoryState(); errMsg != "403 Resource not accessible by integration" {
		t.Fatalf("a recorded post error must outrank the bead-store fold, got %q", errMsg)
	}

	s.RecordAdvisoryError("") // clear; now test inference precedence
	setInferenceAuthForTest(t, "litellm inference backend auth failed (401)", time.Now())
	if _, _, errMsg := s.AdvisoryState(); !strings.Contains(errMsg, "inference backend auth failed") {
		t.Fatalf("the inference-auth fold must outrank the bead-store fold, got %q", errMsg)
	}
}

// Self-heal: a restart that loads every store reports zero failures, so the
// fold disappears without operator action.
func TestAdvisoryState_BeadStoreFoldSelfHealsAtZero(t *testing.T) {
	s := newAdvisoryTestServer()
	s.deps = &Dependencies{BeadStoreLoadFailures: 0}
	s.RecordAdvisoryPost(3)

	if _, _, errMsg := s.AdvisoryState(); errMsg != "" {
		t.Fatalf("zero load failures must leave AdvisoryError empty, got %q", errMsg)
	}
}
