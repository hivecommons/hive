package dashboard

import (
	"testing"
	"time"
)

// setInferenceAuthForTest points the package-global provider at a fixed value
// for the duration of a test and restores it after, so parallel-free tests do
// not leak state into each other.
func setInferenceAuthForTest(t *testing.T, errMsg string, since time.Time) {
	t.Helper()
	prev := getInferenceAuthFn()
	SetInferenceAuthProvider(func() (string, time.Time) { return errMsg, since })
	t.Cleanup(func() { SetInferenceAuthProvider(prev) })
}

// Part A: an inference-auth failure folds into AdvisoryError so the hub's stale
// gate fires immediately — but ONLY on an advisory-participating hive (one that
// has posted at least once).
func TestAdvisoryState_FoldsInferenceAuthForParticipant(t *testing.T) {
	s := newAdvisoryTestServer()
	s.RecordAdvisoryPost(3) // now an advisory participant with a fresh post time
	setInferenceAuthForTest(t, "litellm inference backend auth failed (401): invalid proxy token", time.Now())

	_, _, errMsg := s.AdvisoryState()
	if errMsg == "" {
		t.Fatalf("an advisory participant with an inference-auth failure must surface it as AdvisoryError")
	}
}

// The fold must NOT fabricate an AdvisoryError on a hive that has never posted
// a digest — otherwise a pure PR/merge hive with no advisory agents would be
// false-alarmed as stale by an inference blip on some other path.
func TestAdvisoryState_DoesNotFoldForNonParticipant(t *testing.T) {
	s := newAdvisoryTestServer() // never posted → not an advisory participant
	setInferenceAuthForTest(t, "litellm inference backend auth failed (401): invalid proxy token", time.Now())

	postedAt, _, errMsg := s.AdvisoryState()
	if !postedAt.IsZero() {
		t.Fatalf("precondition: a fresh server has never posted")
	}
	if errMsg != "" {
		t.Fatalf("a non-participant must not have inference-auth folded into AdvisoryError, got %q", errMsg)
	}
}

// A real advisory-post error takes precedence over the inference-auth fold —
// the more specific cause wins.
func TestAdvisoryState_RealErrorTakesPrecedenceOverFold(t *testing.T) {
	s := newAdvisoryTestServer()
	s.RecordAdvisoryPost(3)
	s.RecordAdvisoryError("403 Resource not accessible by integration")
	setInferenceAuthForTest(t, "litellm inference backend auth failed (401): invalid proxy token", time.Now())

	_, _, errMsg := s.AdvisoryState()
	if errMsg != "403 Resource not accessible by integration" {
		t.Fatalf("a recorded advisory-post error must win over the inference-auth fold, got %q", errMsg)
	}
}

// Self-heal: when inference recovers the provider reports empty, so the fold
// disappears and a fresh-post participant reads healthy again.
func TestAdvisoryState_FoldSelfHealsWhenInferenceRecovers(t *testing.T) {
	s := newAdvisoryTestServer()
	s.RecordAdvisoryPost(3)
	setInferenceAuthForTest(t, "", time.Time{}) // recovered: provider reports nothing

	_, _, errMsg := s.AdvisoryState()
	if errMsg != "" {
		t.Fatalf("a recovered inference backend must leave AdvisoryError empty, got %q", errMsg)
	}
}

// Part B: the dedicated inference-auth field is reported straight from the
// provider, independent of advisory participation.
func TestInferenceAuthState_ReportsProviderVerbatim(t *testing.T) {
	s := newAdvisoryTestServer() // NOT an advisory participant
	since := time.Now()
	cause := "litellm inference backend auth failed (401): invalid proxy token"
	setInferenceAuthForTest(t, cause, since)

	got, gotSince := s.InferenceAuthState()
	if got != cause {
		t.Fatalf("InferenceAuthState must report the provider cause verbatim, got %q", got)
	}
	if !gotSince.Equal(since) {
		t.Fatalf("InferenceAuthState must report the provider since time, got %v want %v", gotSince, since)
	}
}

func TestInferenceAuthState_EmptyWhenHealthy(t *testing.T) {
	s := newAdvisoryTestServer()
	setInferenceAuthForTest(t, "", time.Time{})
	if got, _ := s.InferenceAuthState(); got != "" {
		t.Fatalf("a healthy inference backend must report no error, got %q", got)
	}
}
