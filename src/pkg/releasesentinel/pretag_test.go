package releasesentinel

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

const releaseWorkflow = "Tagged Release"

type fakePreTag struct {
	runs  []Run
	err   error
	calls int
	names [][]string
}

func (f *fakePreTag) LatestReleaseWorkflowRuns(_ context.Context, names []string) ([]Run, error) {
	f.calls++
	f.names = append(f.names, names)
	return f.runs, f.err
}

// policyEvidence is the #5875 failure: no commit can fix it.
var policyEvidence = RunDetails{JobCount: 1, FailedJobs: []string{"release / open release PR"},
	Evidence: []string{"GraphQL: GitHub Actions is not permitted to create or approve pull requests (createPullRequest)"}}

func releaseRun(id int64, sha, conclusion string) Run {
	return Run{ID: id, Name: releaseWorkflow, HeadSHA: sha, Status: "completed", Conclusion: conclusion, URL: fmt.Sprintf("https://example.test/runs/%d", id)}
}

type preTagHarness struct {
	*harness
	pre *fakePreTag
}

func newPreTagHarness(t *testing.T, store Store) *preTagHarness {
	t.Helper()
	h := newHarness(t, store)
	h.opts.ReleaseWorkflows = []string{releaseWorkflow}
	// No release tag yet: the failure is before tagging.
	h.src.hasTag = false
	return &preTagHarness{harness: h, pre: &fakePreTag{}}
}

func (h *preTagHarness) evaluate() (Result, error) {
	return h.sentinel().WithPreTag(h.pre).Evaluate(context.Background())
}

func (h *preTagHarness) eval(t *testing.T) Result {
	t.Helper()
	res, err := h.evaluate()
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return res
}

func TestPreTag_PolicyFailureEscalatesWithoutDispatch(t *testing.T) {
	h := newPreTagHarness(t, nil)
	h.pre.runs = []Run{releaseRun(50, shaB, "failure")}
	h.src.details[50] = policyEvidence

	res := h.eval(t)
	if res.Record != nil {
		t.Fatalf("no tag exists, yet a release record came back: %+v", res.Record)
	}
	if len(res.PreTagEscalated) != 1 || res.PreTagEscalated[0] != releaseWorkflow {
		t.Fatalf("PreTagEscalated = %v", res.PreTagEscalated)
	}
	if len(h.disp.reqs) != 0 {
		t.Fatalf("a pre-tag policy failure dispatched %d round(s); it must go to a human only", len(h.disp.reqs))
	}
	if len(h.esc.got) != 1 {
		t.Fatalf("escalations = %d, want 1", len(h.esc.got))
	}
	esc := h.esc.got[0]
	if esc.Reason != EscalationPolicy || esc.Workflow != releaseWorkflow || esc.Tag != "" || esc.SHA != shaB || esc.Round != 0 || len(esc.Blocking) != 1 {
		t.Fatalf("escalation = %+v", esc)
	}
	if got := h.pre.names[0]; len(got) != 1 || got[0] != releaseWorkflow {
		t.Fatalf("release workflows asked for = %v", got)
	}

	// Persisted: a restart does not page again for the same run.
	records, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := records[preTagKey(releaseWorkflow)]
	if rec == nil || rec.Kind != KindPreTag || rec.State != StateFailed || !rec.Escalated || rec.RunID != 50 || rec.Workflow != releaseWorkflow {
		t.Fatalf("pre-tag record = %+v", rec)
	}
	h.eval(t)
	if len(h.esc.got) != 1 {
		t.Fatalf("same run escalated again: %d", len(h.esc.got))
	}
}

func TestPreTag_RepeatedPolicyFailuresPageOnceUntilRecovery(t *testing.T) {
	h := newPreTagHarness(t, nil)
	h.src.details[50] = policyEvidence
	h.src.details[51] = policyEvidence
	h.src.details[53] = policyEvidence

	h.pre.runs = []Run{releaseRun(50, shaA, "failure")}
	h.eval(t)
	// The hourly release run fails the same way again: still one page.
	h.pre.runs = []Run{releaseRun(51, shaB, "failure")}
	h.clk.t = h.clk.t.Add(time.Hour)
	h.eval(t)
	if len(h.esc.got) != 1 {
		t.Fatalf("escalations = %d after a repeat of the same block, want 1", len(h.esc.got))
	}
	// Someone fixed the setting: the next run succeeds.
	h.pre.runs = []Run{releaseRun(52, shaB, "success")}
	h.eval(t)
	records, _ := h.store.Load()
	if rec := records[preTagKey(releaseWorkflow)]; rec.State != StateGreen || rec.Escalated {
		t.Fatalf("after recovery: %+v", rec)
	}
	// A later block is a new incident and pages again.
	h.pre.runs = []Run{releaseRun(53, shaC, "failure")}
	h.eval(t)
	if len(h.esc.got) != 2 || len(h.disp.reqs) != 0 {
		t.Fatalf("escalations=%d dispatches=%d, want 2/0", len(h.esc.got), len(h.disp.reqs))
	}
}

func TestPreTag_FixableFailureIsLeftToCIPath(t *testing.T) {
	h := newPreTagHarness(t, nil)
	h.pre.runs = []Run{releaseRun(60, shaB, "failure")} // default details: a code failure
	res := h.eval(t)
	if len(res.PreTagFixable) != 1 || len(res.PreTagEscalated) != 0 {
		t.Fatalf("fixable=%v escalated=%v", res.PreTagFixable, res.PreTagEscalated)
	}
	if len(h.disp.reqs) != 0 || len(h.esc.got) != 0 {
		t.Fatalf("fixable pre-tag failure dispatched=%d escalated=%d; both must be 0", len(h.disp.reqs), len(h.esc.got))
	}
	records, _ := h.store.Load()
	if rec := records[preTagKey(releaseWorkflow)]; rec == nil || rec.State != StateAwaitingCI || rec.RunID != 60 {
		t.Fatalf("record = %+v", rec)
	}
	// The same run is not re-fetched.
	h.src.detailsErr = errors.New("must not be called")
	h.eval(t)
}

func TestPreTag_FixableAfterPolicyClearsEscalation(t *testing.T) {
	h := newPreTagHarness(t, nil)
	h.src.details[50] = policyEvidence
	h.pre.runs = []Run{releaseRun(50, shaB, "failure")}
	h.eval(t)
	h.pre.runs = []Run{releaseRun(51, shaB, "failure")}
	h.eval(t)
	records, _ := h.store.Load()
	if rec := records[preTagKey(releaseWorkflow)]; rec.State != StateAwaitingCI || rec.Escalated {
		t.Fatalf("record = %+v", rec)
	}
}

func TestPreTag_RunsOnTheTaggedCommitBelongToTheTag(t *testing.T) {
	h := newPreTagHarness(t, nil)
	h.src.hasTag = true // tag1 at shaA
	h.src.details[70] = policyEvidence
	h.pre.runs = []Run{releaseRun(70, shaA, "failure")}
	res := h.eval(t)
	if len(res.PreTagEscalated) != 0 {
		t.Fatalf("a run on the tagged commit was treated as pre-tag: %v", res.PreTagEscalated)
	}
}

func TestPreTag_SkipsInFlightAndUnnamedRuns(t *testing.T) {
	h := newPreTagHarness(t, nil)
	h.src.detailsErr = errors.New("must not be called")
	h.pre.runs = []Run{
		{ID: 1, Name: releaseWorkflow, HeadSHA: shaB, Status: "in_progress"},
		{ID: 2, Name: " ", HeadSHA: shaB, Status: "completed", Conclusion: "failure"},
		releaseRun(3, shaB, "success"), // green with no record: nothing to do
	}
	res := h.eval(t)
	if len(res.PreTagEscalated) != 0 || len(res.PreTagFixable) != 0 {
		t.Fatalf("res = %+v", res)
	}
	records, _ := h.store.Load()
	if len(records) != 0 {
		t.Fatalf("records = %+v, want none", records)
	}
}

func TestPreTag_OffWithoutWorkflowsOrSource(t *testing.T) {
	h := newPreTagHarness(t, nil)
	h.opts.ReleaseWorkflows = nil
	h.pre.runs = []Run{releaseRun(50, shaB, "failure")}
	if res := h.eval(t); res.Action != ActionNone || len(res.PreTagEscalated) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if h.pre.calls != 0 {
		t.Fatal("pre-tag source consulted with no release workflows configured")
	}
	// Workflows configured but no source supplied: also off.
	h.opts.ReleaseWorkflows = []string{releaseWorkflow}
	if res, err := h.sentinel().Evaluate(context.Background()); err != nil || res.Action != ActionNone {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestPreTag_ErrorDoesNotBlockTheTagPass(t *testing.T) {
	store := &memStore{}
	h := newPreTagHarness(t, store)
	h.src.hasTag = true
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	boom := errors.New("workflows listing down")
	h.pre.err = boom
	res, err := h.evaluate()
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the pre-tag error", err)
	}
	if res.Action != ActionRoundStarted || res.Record == nil || len(h.disp.reqs) != 1 || store.records[tag1] == nil {
		t.Fatalf("the tag pass did not run: res=%+v dispatches=%d", res, len(h.disp.reqs))
	}
}

func TestPreTag_Errors(t *testing.T) {
	boom := errors.New("boom")
	t.Run("list error with no tag is persisted and reported", func(t *testing.T) {
		store := &memStore{}
		h := newPreTagHarness(t, store)
		h.pre.err = boom
		if _, err := h.evaluate(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if store.saves != 1 {
			t.Fatalf("saves = %d", store.saves)
		}
	})
	t.Run("details error", func(t *testing.T) {
		h := newPreTagHarness(t, nil)
		h.pre.runs = []Run{releaseRun(50, shaB, "failure")}
		h.src.detailsErr = boom
		if _, err := h.evaluate(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if len(h.esc.got) != 0 {
			t.Fatal("escalated without evidence")
		}
	})
	t.Run("save error with no tag", func(t *testing.T) {
		h := newPreTagHarness(t, &memStore{saveErr: boom})
		if _, err := h.evaluate(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("save error with a tag joins the pre-tag error", func(t *testing.T) {
		h := newPreTagHarness(t, &memStore{saveErr: boom})
		h.src.hasTag = true
		pre := errors.New("pre")
		h.pre.err = pre
		_, err := h.evaluate()
		if !errors.Is(err, boom) || !errors.Is(err, pre) {
			t.Fatalf("err = %v, want both", err)
		}
	})
	t.Run("load error", func(t *testing.T) {
		h := newPreTagHarness(t, &memStore{loadErr: boom})
		if _, err := h.evaluate(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPreTag_RecordsAreNeverSupersededOrPruned(t *testing.T) {
	h := newPreTagHarness(t, nil)
	// A fixable pre-tag record is non-terminal (awaiting_ci).
	h.pre.runs = []Run{releaseRun(60, shaB, "failure")}
	h.eval(t)
	// Now a release tag exists: the tag pass must not supersede it.
	h.src.hasTag = true
	h.eval(t)
	records, _ := h.store.Load()
	if rec := records[preTagKey(releaseWorkflow)]; rec.State != StateAwaitingCI {
		t.Fatalf("pre-tag record was superseded: %+v", rec)
	}

	// prune never drops a pre-tag record, even the oldest terminal one.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := map[string]*Record{preTagKey(releaseWorkflow): {Kind: KindPreTag, State: StateFailed, UpdatedAt: old}}
	for i := 0; i < maxRecords+3; i++ {
		recs[fmt.Sprintf("v0.0.%d", i)] = &Record{State: StateGreen, UpdatedAt: old.Add(time.Duration(i+1) * time.Hour)}
	}
	out := prune(recs)
	if out[preTagKey(releaseWorkflow)] == nil || len(out) != maxRecords {
		t.Fatalf("prune: pre-tag kept=%v len=%d", out[preTagKey(releaseWorkflow)] != nil, len(out))
	}
}
