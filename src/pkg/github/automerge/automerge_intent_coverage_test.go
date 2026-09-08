package automerge

// Additional coverage for the intent-tier gate (#6258) beyond
// automerge_intent_test.go: the nil-gate passthrough (no IntentGate
// installed, the pre-#6258 behavior), nil-safety of the Engine helper
// methods, the advisory-mode evidence-fetch-failure branch (logged, not
// withheld), and ListChangedFiles exercised directly for its own guard
// clauses and pagination/incomplete-list behavior.

import (
	"context"
	"net/http"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// No IntentGate installed on the Engine (the zero value / pre-#6258 state):
// the sweep must behave exactly as it did before the gate existed and merge
// without ever hitting the files endpoint.
func TestSelfAuthoredSweepNoIntentGateInstalledMergesWithoutEvidence(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{
		mergeApplied: true,
		mergeCalls:   &merges,
		// No files/body given; if the gate were consulted it would need
		// evidence it cannot get, so a merge here proves the gate was
		// skipped entirely rather than satisfied.
	}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL) // no SetIntentGate call: c.intentGate is nil
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
	if err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want clean merge with no gate installed", reason, err)
	}
	if event.MergeSHA != "merge7" || merges != 1 {
		t.Fatalf("event = %+v, merges = %d; want exactly one merge with no intent gate installed", event, merges)
	}
}

// selfMergeIntentGate itself must return ("", nil) immediately when the
// Engine carries no gate, without dereferencing pr.
func TestSelfMergeIntentGateNilGateNoop(t *testing.T) {
	c := newAutoMergeSweepClient("http://unused.invalid")
	reason, err := c.selfMergeIntentGate(context.Background(), "acme/widget", "acme", "widget", nil, "hive-app[bot]", nil)
	if reason != "" || err != nil {
		t.Fatalf("selfMergeIntentGate = (%q, %v), want (\"\", nil) with no gate installed", reason, err)
	}
}

// With intent.enforce off, a changed-file fetch failure is advisory: logged
// and the merge proceeds, exactly like writeMergeEligible's `if
// enforceIntent`. This is the mirror of
// TestSelfAuthoredSweepIntentGateFailsClosedWithoutEvidence, which only
// covers the enforce=true (fail-closed) side of the same branch.
func TestSelfAuthoredSweepIntentGateAdvisoryEvidenceErrorStillMerges(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{
		body:          testIntentLinkedBody,
		files:         []string{testIntentSourceFile},
		filesHTTPCode: http.StatusInternalServerError,
		mergeApplied:  true,
		mergeCalls:    &merges,
	}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	c := newIntentGateSweepClient(api.URL, false) // enforce=false
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
	if err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want advisory merge despite evidence-fetch failure", reason, err)
	}
	if event.MergeSHA != "merge7" || merges != 1 {
		t.Fatalf("event = %+v, merges = %d; want one merge when evidence fetch fails but enforce is off", event, merges)
	}
}

// enforced() must treat a nil *IntentGate, and a gate with a nil Enforce
// func, as "not enforced" rather than panicking.
func TestIntentGateEnforcedNilSafety(t *testing.T) {
	var nilGate *IntentGate
	if nilGate.enforced() {
		t.Fatalf("nil *IntentGate.enforced() = true, want false")
	}
	noFunc := &IntentGate{}
	if noFunc.enforced() {
		t.Fatalf("IntentGate with nil Enforce func .enforced() = true, want false")
	}
	on := &IntentGate{Enforce: func() bool { return true }}
	if !on.enforced() {
		t.Fatalf("IntentGate.enforced() = false, want true")
	}
}

// SetIntentGate and currentIntentGate on a nil *Engine must no-op / return
// nil rather than panicking, matching the nil-receiver pattern used
// elsewhere on Engine (e.g. StartSelfAuthoredAutoMergeSweep).
func TestIntentGateNilEngineSafety(t *testing.T) {
	var nilEngine *Engine
	nilEngine.SetIntentGate(&IntentGate{})
	if got := nilEngine.currentIntentGate(); got != nil {
		t.Fatalf("nil *Engine.currentIntentGate() = %+v, want nil", got)
	}
}

// currentIntentGate/SetIntentGate round-trip on a real Engine, including
// clearing a previously installed gate back to nil.
func TestIntentGateSetAndClear(t *testing.T) {
	c := newAutoMergeSweepClient("http://unused.invalid")
	if got := c.currentIntentGate(); got != nil {
		t.Fatalf("currentIntentGate() before install = %+v, want nil", got)
	}
	gate := &IntentGate{Enforce: func() bool { return true }}
	c.SetIntentGate(gate)
	if got := c.currentIntentGate(); got != gate {
		t.Fatalf("currentIntentGate() = %p, want installed gate %p", got, gate)
	}
	c.SetIntentGate(nil)
	if got := c.currentIntentGate(); got != nil {
		t.Fatalf("currentIntentGate() after clear = %+v, want nil", got)
	}
}

// ListChangedFiles's own guard clauses: a nil client or nil PR is refused
// directly, without any network call.
func TestListChangedFilesNilInputsRefused(t *testing.T) {
	ctx := context.Background()
	pr := &gh.PullRequest{Number: gh.Ptr(7)}

	if _, err := ListChangedFiles(ctx, nil, "acme", "widget", pr); err == nil {
		t.Fatalf("ListChangedFiles with nil client: err = nil, want error")
	}
	if _, err := ListChangedFiles(ctx, &gh.Client{}, "acme", "widget", nil); err == nil {
		t.Fatalf("ListChangedFiles with nil PR: err = nil, want error")
	}
}

// ListChangedFiles pages through multiple result pages and concatenates
// them, and accepts a complete list even when it spans more than one page.
func TestListChangedFilesPaginates(t *testing.T) {
	api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{testIntentSourceFile, testIntentDocsFile}})
	defer api.Close()
	client := newAutoMergeSweepClient(api.URL)
	pr := &gh.PullRequest{Number: gh.Ptr(7), ChangedFiles: gh.Ptr(2)}
	files, err := ListChangedFiles(context.Background(), client.gh, "acme", "widget", pr)
	if err != nil {
		t.Fatalf("ListChangedFiles returned error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("ListChangedFiles returned %d files, want 2", len(files))
	}
}

// A files-API transport error is surfaced directly (not swallowed) so the
// caller's fail-closed handling applies.
func TestListChangedFilesAPIErrorSurfaced(t *testing.T) {
	api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{testIntentSourceFile}, filesHTTPCode: http.StatusInternalServerError})
	defer api.Close()
	client := newAutoMergeSweepClient(api.URL)
	pr := &gh.PullRequest{Number: gh.Ptr(7), ChangedFiles: gh.Ptr(1)}
	if _, err := ListChangedFiles(context.Background(), client.gh, "acme", "widget", pr); err == nil {
		t.Fatalf("ListChangedFiles: err = nil, want error from files API failure")
	}
}
