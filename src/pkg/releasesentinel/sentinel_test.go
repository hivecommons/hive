package releasesentinel

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tag1 = "v1.2.3"
	tag2 = "v1.2.4"
)

// fakeSource is a scripted forge. runs is keyed by the SHA queried, so a test
// can make the forge (wrongly) return runs for a stale SHA as well.
type fakeSource struct {
	tag        Tag
	hasTag     bool
	tagErr     error
	runs       map[string][]Run
	runsErr    error
	details    map[int64]RunDetails
	detailsErr error
	release    map[string]ReleaseInfo
	releaseErr error
	runQueries []string
}

func (f *fakeSource) CurrentTag(context.Context) (Tag, bool, error) {
	return f.tag, f.hasTag, f.tagErr
}

func (f *fakeSource) Runs(_ context.Context, sha string) ([]Run, error) {
	f.runQueries = append(f.runQueries, sha)
	if f.runsErr != nil {
		return nil, f.runsErr
	}
	return f.runs[sha], nil
}

func (f *fakeSource) Details(_ context.Context, id int64) (RunDetails, error) {
	if f.detailsErr != nil {
		return RunDetails{}, f.detailsErr
	}
	if d, ok := f.details[id]; ok {
		return d, nil
	}
	// Default: a normal code failure with one job.
	return RunDetails{JobCount: 1, FailedJobs: []string{"test / go test"}, Evidence: []string{"FAIL: TestSomething"}}, nil
}

func (f *fakeSource) Release(_ context.Context, tag string) (ReleaseInfo, error) {
	if f.releaseErr != nil {
		return ReleaseInfo{}, f.releaseErr
	}
	return f.release[tag], nil
}

type fakeDispatcher struct {
	reqs []RepairRequest
	err  error
}

func (d *fakeDispatcher) DispatchRepair(_ context.Context, req RepairRequest) error {
	if d.err != nil {
		return d.err
	}
	d.reqs = append(d.reqs, req)
	return nil
}

type fakeEscalator struct{ got []Escalation }

func (e *fakeEscalator) Escalate(_ context.Context, esc Escalation) { e.got = append(e.got, esc) }

type memStore struct {
	records map[string]*Record
	loadErr error
	saveErr error
	saves   int
}

func (m *memStore) Load() (map[string]*Record, error) {
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	out := map[string]*Record{}
	for k, v := range m.records {
		cp := *v
		out[k] = &cp
	}
	return out, nil
}

func (m *memStore) Save(r map[string]*Record) error {
	m.saves++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.records = r
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type harness struct {
	src   *fakeSource
	disp  *fakeDispatcher
	esc   *fakeEscalator
	store Store
	clk   *clock
	opts  Options
}

func newHarness(t *testing.T, store Store) *harness {
	t.Helper()
	if store == nil {
		store = NewFileStore(filepath.Join(t.TempDir(), "release-sentinel.json"))
	}
	clk := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	return &harness{
		src: &fakeSource{
			tag: Tag{Name: tag1, SHA: shaA}, hasTag: true,
			runs:    map[string][]Run{},
			details: map[int64]RunDetails{},
			release: map[string]ReleaseInfo{},
		},
		disp:  &fakeDispatcher{},
		esc:   &fakeEscalator{},
		store: store,
		clk:   clk,
		opts:  Options{Repo: "acme/widgets", MaxRounds: 3, RoundTimeout: time.Hour},
	}
}

// sentinel builds a fresh Sentinel over the harness's store, which is what a
// hive restart does: nothing survives but the state file.
func (h *harness) sentinel() *Sentinel {
	o := h.opts
	o.Now = h.clk.now
	return New(o, h.src, h.store, h.disp, h.esc)
}

func (h *harness) eval(t *testing.T) Result {
	t.Helper()
	res, err := h.sentinel().Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return res
}

func failing(id int64, sha string) Run {
	return Run{ID: id, Name: fmt.Sprintf("wf-%d", id), HeadSHA: sha, Status: "completed", Conclusion: "failure", URL: fmt.Sprintf("https://example.test/runs/%d", id)}
}

func passing(id int64, sha string) Run {
	return Run{ID: id, Name: fmt.Sprintf("wf-%d", id), HeadSHA: sha, Status: "completed", Conclusion: "success"}
}

func TestEvaluate_StaleSHANeverTriggersARound(t *testing.T) {
	h := newHarness(t, nil)
	// The forge hands back a FAILED run whose head SHA is not the one the tag
	// points at (the tag moved after the run started). It must be ignored.
	h.src.runs[shaA] = []Run{failing(1, shaB), failing(2, shaB)}

	for i := 0; i < 3; i++ {
		res := h.eval(t)
		if res.StaleRuns != 2 {
			t.Fatalf("pass %d: StaleRuns = %d, want 2", i, res.StaleRuns)
		}
		if res.Action != ActionWaiting {
			t.Fatalf("pass %d: action = %s, want waiting", i, res.Action)
		}
		if res.Record.State != StateAwaitingCI || res.Record.Round != 0 {
			t.Fatalf("pass %d: record = %+v, want awaiting_ci round 0", i, res.Record)
		}
		h.clk.t = h.clk.t.Add(2 * time.Hour)
	}
	if len(h.disp.reqs) != 0 || len(h.esc.got) != 0 {
		t.Fatalf("stale runs dispatched %d round(s) and %d escalation(s); want none", len(h.disp.reqs), len(h.esc.got))
	}
}

func TestEvaluate_StaleSHAAfterTagMoveNeverTriggersARound(t *testing.T) {
	h := newHarness(t, nil)
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	if res := h.eval(t); res.Action != ActionRoundStarted {
		t.Fatalf("first failure: action = %s", res.Action)
	}

	// The tag is re-pointed at shaB. The forge still reports the OLD failure
	// (for shaA) when asked about shaB: stale, so no second round.
	h.src.tag = Tag{Name: tag1, SHA: shaB}
	h.src.runs[shaB] = []Run{failing(1, shaA), {ID: 3, Name: "wf-3", HeadSHA: shaB, Status: "in_progress"}}
	res := h.eval(t)
	if res.Action != ActionWaiting || res.StaleRuns != 1 {
		t.Fatalf("after tag move: action=%s stale=%d, want waiting/1", res.Action, res.StaleRuns)
	}
	if res.Record.SHA != shaB || res.Record.State != StateAwaitingCI || res.Record.Round != 1 {
		t.Fatalf("after tag move: record = %+v", res.Record)
	}
	if len(h.disp.reqs) != 1 {
		t.Fatalf("dispatched %d rounds, want exactly 1", len(h.disp.reqs))
	}
	if got := h.src.runQueries[len(h.src.runQueries)-1]; got != shaB {
		t.Fatalf("runs queried for %s, want the tag's current SHA %s", got, shaB)
	}
}

func TestEvaluate_BlockingFailureStartsRound(t *testing.T) {
	h := newHarness(t, nil)
	h.opts.Agent = "release-fixer"
	h.src.runs[shaA] = []Run{failing(7, shaA), passing(8, shaA)}
	res := h.eval(t)
	if res.Action != ActionRoundStarted {
		t.Fatalf("action = %s, want round_started", res.Action)
	}
	if res.Record.State != StateFixing || res.Record.Round != 1 || res.Record.RoundSHA != shaA {
		t.Fatalf("record = %+v", res.Record)
	}
	if len(res.Record.BlockingRuns) != 1 || res.Record.BlockingRuns[0].ID != 7 {
		t.Fatalf("blocking runs = %+v", res.Record.BlockingRuns)
	}
	if len(h.disp.reqs) != 1 {
		t.Fatalf("dispatches = %d", len(h.disp.reqs))
	}
	req := h.disp.reqs[0]
	if req.Agent != "release-fixer" || req.Round != 1 || req.MaxRounds != 3 || req.Tag != tag1 || req.SHA != shaA || req.Repo != "acme/widgets" {
		t.Fatalf("request = %+v", req)
	}
	if !req.Deadline.Equal(h.clk.t.Add(time.Hour)) {
		t.Fatalf("deadline = %s", req.Deadline)
	}
	if len(req.Blocking) != 1 || len(req.Blocking[0].Evidence) == 0 {
		t.Fatalf("request carries no evidence: %+v", req.Blocking)
	}
}

func TestEvaluate_SameSHAWhileFixingDoesNotRedispatch(t *testing.T) {
	h := newHarness(t, nil)
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	h.eval(t)
	h.clk.t = h.clk.t.Add(30 * time.Minute) // inside the round timeout
	res := h.eval(t)
	if res.Action != ActionWaiting || res.Record.State != StateFixing {
		t.Fatalf("action=%s state=%s, want waiting/fixing", res.Action, res.Record.State)
	}
	if len(h.disp.reqs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(h.disp.reqs))
	}
}

func TestEvaluate_RoundCapEnforced(t *testing.T) {
	h := newHarness(t, nil)
	h.src.runs[shaA] = []Run{failing(1, shaA)}

	// Every round times out still red. MaxRounds=3 means exactly three
	// dispatches, then failed + one escalation, then silence forever.
	for pass := 0; pass < 10; pass++ {
		h.eval(t)
		h.clk.t = h.clk.t.Add(time.Hour)
	}
	if len(h.disp.reqs) != h.opts.MaxRounds {
		t.Fatalf("dispatched %d rounds, want exactly MaxRounds=%d", len(h.disp.reqs), h.opts.MaxRounds)
	}
	for i, r := range h.disp.reqs {
		if r.Round != i+1 {
			t.Fatalf("dispatch %d carried round %d", i, r.Round)
		}
	}
	if len(h.esc.got) != 1 || h.esc.got[0].Reason != EscalationRoundCap || h.esc.got[0].Round != h.opts.MaxRounds {
		t.Fatalf("escalations = %+v, want one round_cap at round %d", h.esc.got, h.opts.MaxRounds)
	}
	recs, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := recs[tag1]
	if rec.State != StateFailed || !rec.Escalated || rec.EscalationReason != EscalationRoundCap {
		t.Fatalf("record = %+v", rec)
	}
	if res := h.eval(t); res.Action != ActionTerminal {
		t.Fatalf("after failed: action = %s, want terminal", res.Action)
	}
}

func TestEvaluate_RoundCapAppliesToNewFailuresToo(t *testing.T) {
	h := newHarness(t, nil)
	h.opts.MaxRounds = 1
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	h.eval(t) // round 1

	// A fix moved the tag; the new SHA fails too. The cap is already spent.
	h.src.tag = Tag{Name: tag1, SHA: shaB}
	h.src.runs[shaB] = []Run{failing(2, shaB)}
	res := h.eval(t)
	if res.Action != ActionEscalated || res.Record.State != StateFailed {
		t.Fatalf("action=%s state=%s, want escalated/failed", res.Action, res.Record.State)
	}
	if len(h.disp.reqs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(h.disp.reqs))
	}
	if len(h.esc.got) != 1 || len(h.esc.got[0].Blocking) != 1 {
		t.Fatalf("escalation must carry the new evidence: %+v", h.esc.got)
	}
}

func TestEvaluate_StatePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "release-sentinel.json")
	h := newHarness(t, NewFileStore(path))
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	h.eval(t) // round 1 dispatched
	h.clk.t = h.clk.t.Add(time.Hour)
	h.eval(t) // round 2 dispatched

	// "Restart": a brand-new store and sentinel over the same file, fresh
	// fakes for everything that lived in memory.
	h.store = NewFileStore(path)
	h.disp = &fakeDispatcher{}
	h.esc = &fakeEscalator{}
	h.clk.t = h.clk.t.Add(10 * time.Minute)
	res := h.eval(t)
	if res.Action != ActionWaiting || res.Record.Round != 2 || res.Record.State != StateFixing {
		t.Fatalf("after restart: action=%s record=%+v, want waiting in round 2", res.Action, res.Record)
	}
	if len(h.disp.reqs) != 0 {
		t.Fatalf("restart re-dispatched a round in flight")
	}

	// The cap is counted across the restart: one more round, then failed.
	h.clk.t = h.clk.t.Add(time.Hour)
	h.eval(t)
	h.clk.t = h.clk.t.Add(time.Hour)
	h.eval(t)
	if len(h.disp.reqs) != 1 || h.disp.reqs[0].Round != 3 {
		t.Fatalf("post-restart dispatches = %+v, want only round 3", h.disp.reqs)
	}
	if len(h.esc.got) != 1 || h.esc.got[0].Reason != EscalationRoundCap {
		t.Fatalf("escalations = %+v", h.esc.got)
	}
}

func TestEvaluate_PolicyFailureEscalatesWithoutDispatch(t *testing.T) {
	cases := map[string]RunDetails{
		"actions pr permission (#5875)": {JobCount: 1, FailedJobs: []string{"release / Open and merge a PR"}, Evidence: []string{
			"gh pr create failed: pull request create failed: GraphQL: GitHub Actions is not permitted to create or approve pull requests (createPullRequest)"}},
		"workflows permission (#6804)": {JobCount: 1, Evidence: []string{
			"! [remote rejected] HEAD -> release-gate/v4.29.2 (refusing to allow a GitHub App to create or update workflow `.github/workflows/dco-post-merge.yml` without `workflows` permission)"}},
		"403 permission denied": {JobCount: 1, Evidence: []string{"remote: Permission to acme/widgets.git denied to bot. fatal: unable to access: The requested URL returned error: 403"}},
		"no jobs ran (#7123)":   {JobCount: 0},
	}
	for name, details := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.src.runs[shaA] = []Run{failing(1, shaA), failing(2, shaA)}
			h.src.details[2] = details
			res := h.eval(t)
			if res.Action != ActionEscalated || res.Record.State != StateFailed {
				t.Fatalf("action=%s state=%s, want escalated/failed", res.Action, res.Record.State)
			}
			if res.Record.Round != 0 {
				t.Fatalf("policy failure consumed round %d; must escalate on the first round", res.Record.Round)
			}
			if len(h.disp.reqs) != 0 {
				t.Fatalf("policy failure dispatched a repair round: %+v", h.disp.reqs)
			}
			if len(h.esc.got) != 1 || h.esc.got[0].Reason != EscalationPolicy || len(h.esc.got[0].Blocking) != 2 {
				t.Fatalf("escalations = %+v", h.esc.got)
			}
			// And it stays escalated: later passes never dispatch.
			h.clk.t = h.clk.t.Add(24 * time.Hour)
			if res := h.eval(t); res.Action != ActionTerminal || len(h.disp.reqs) != 0 || len(h.esc.got) != 1 {
				t.Fatalf("after escalation: action=%s dispatches=%d escalations=%d", res.Action, len(h.disp.reqs), len(h.esc.got))
			}
		})
	}
}

func TestEvaluate_GreenRequiresPublishedRelease(t *testing.T) {
	h := newHarness(t, nil)
	h.src.runs[shaA] = []Run{
		passing(1, shaA),
		{ID: 2, Name: "cancelled", HeadSHA: shaA, Status: "completed", Conclusion: "cancelled"},
		{ID: 3, Name: "skipped", HeadSHA: shaA, Status: "completed", Conclusion: "skipped"},
		{ID: 4, Name: "neutral", HeadSHA: shaA, Status: "completed", Conclusion: "neutral"},
		{ID: 5, Name: "approval", HeadSHA: shaA, Status: "completed", Conclusion: "action_required"},
	}
	if res := h.eval(t); res.Action != ActionWaiting || res.Record.State != StateAwaitingCI {
		t.Fatalf("no release: action=%s state=%s, want waiting/awaiting_ci", res.Action, res.Record.State)
	}
	h.src.release[tag1] = ReleaseInfo{Exists: true, Draft: true}
	if res := h.eval(t); res.Action != ActionWaiting {
		t.Fatalf("draft release: action=%s, want waiting", res.Action)
	}
	h.src.release[tag1] = ReleaseInfo{Exists: true}
	res := h.eval(t)
	if res.Action != ActionGreen || res.Record.State != StateGreen {
		t.Fatalf("published: action=%s state=%s, want green", res.Action, res.Record.State)
	}
	if len(h.disp.reqs) != 0 {
		t.Fatalf("non-blocking conclusions dispatched a round")
	}
}

func TestEvaluate_PendingRunsBlockGreen(t *testing.T) {
	h := newHarness(t, nil)
	h.src.release[tag1] = ReleaseInfo{Exists: true}
	h.src.runs[shaA] = []Run{passing(1, shaA), {ID: 2, Name: "slow", HeadSHA: shaA, Status: "in_progress"}}
	if res := h.eval(t); res.Action != ActionWaiting {
		t.Fatalf("pending: action=%s", res.Action)
	}
	// No runs at all is not green either: CI has not reported yet.
	h.src.runs[shaA] = nil
	if res := h.eval(t); res.Action != ActionWaiting {
		t.Fatalf("no runs: action=%s", res.Action)
	}
}

func TestEvaluate_FixThenGreen(t *testing.T) {
	h := newHarness(t, nil)
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	h.eval(t)
	// The fix lands and the tag moves; the new SHA goes green and releases.
	h.src.tag = Tag{Name: tag1, SHA: shaB}
	h.src.runs[shaB] = []Run{passing(2, shaB)}
	h.src.release[tag1] = ReleaseInfo{Exists: true}
	res := h.eval(t)
	if res.Action != ActionGreen || res.Record.Round != 1 {
		t.Fatalf("action=%s record=%+v", res.Action, res.Record)
	}
	want := []State{StateFixing, StateAwaitingCI, StateGreen}
	if len(res.Record.History) != len(want) {
		t.Fatalf("history = %+v", res.Record.History)
	}
	for i, s := range want {
		if res.Record.History[i].To != s {
			t.Fatalf("history[%d].To = %s, want %s", i, res.Record.History[i].To, s)
		}
	}
}

func TestEvaluate_GreenWinsOverExpiredRoundAtCap(t *testing.T) {
	h := newHarness(t, nil)
	h.opts.MaxRounds = 1
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	h.eval(t)
	// A re-run on the same SHA passed and the release is published, but the
	// round deadline passed too. Green is the truth; no escalation.
	h.src.runs[shaA] = []Run{passing(1, shaA)}
	h.src.release[tag1] = ReleaseInfo{Exists: true}
	h.clk.t = h.clk.t.Add(2 * time.Hour)
	if res := h.eval(t); res.Action != ActionGreen {
		t.Fatalf("action=%s, want green", res.Action)
	}
	if len(h.esc.got) != 0 {
		t.Fatalf("escalated a green release")
	}
}

func TestEvaluate_TimedOutRoundWithoutFailureWaits(t *testing.T) {
	h := newHarness(t, nil)
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	h.eval(t)
	// Re-run in flight when the round expires: the round is spent, no new
	// dispatch until something actually fails again.
	h.src.runs[shaA] = []Run{{ID: 1, Name: "wf-1", HeadSHA: shaA, Status: "queued"}}
	h.clk.t = h.clk.t.Add(time.Hour)
	res := h.eval(t)
	if res.Action != ActionWaiting || res.Record.State != StateAwaitingCI {
		t.Fatalf("action=%s state=%s", res.Action, res.Record.State)
	}
	if len(h.disp.reqs) != 1 {
		t.Fatalf("dispatches = %d", len(h.disp.reqs))
	}
}

func TestEvaluate_NewerTagSupersedes(t *testing.T) {
	h := newHarness(t, nil)
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	h.eval(t)
	h.src.tag = Tag{Name: tag2, SHA: shaB}
	h.src.runs[shaB] = []Run{{ID: 9, Name: "wf", HeadSHA: shaB, Status: "queued"}}
	res := h.eval(t)
	if len(res.Superseded) != 1 || res.Superseded[0] != tag1 {
		t.Fatalf("superseded = %v", res.Superseded)
	}
	recs, _ := h.store.Load()
	if recs[tag1].State != StateSuperseded {
		t.Fatalf("old record = %+v", recs[tag1])
	}
	if res.Record.Tag != tag2 || res.Record.State != StateAwaitingCI {
		t.Fatalf("new record = %+v", res.Record)
	}
	// A superseded release is never touched again even if it becomes current.
	h.src.tag = Tag{Name: tag1, SHA: shaA}
	if res := h.eval(t); res.Action != ActionTerminal {
		t.Fatalf("superseded record reopened: %s", res.Action)
	}
}

func TestEvaluate_IgnoredWorkflowsNeverBlock(t *testing.T) {
	h := newHarness(t, nil)
	h.opts.IgnoreWorkflows = []string{" Greetings ", ""}
	h.src.release[tag1] = ReleaseInfo{Exists: true}
	h.src.runs[shaA] = []Run{passing(1, shaA), {ID: 2, Name: "greetings", HeadSHA: shaA, Status: "completed", Conclusion: "failure"}}
	if res := h.eval(t); res.Action != ActionGreen {
		t.Fatalf("action=%s, want green", res.Action)
	}
}

func TestEvaluate_DispatchFailureDoesNotSpendRound(t *testing.T) {
	h := newHarness(t, nil)
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	h.disp.err = errors.New("agent paused")
	if _, err := h.sentinel().Evaluate(context.Background()); err == nil || !strings.Contains(err.Error(), "agent paused") {
		t.Fatalf("err = %v, want dispatch error", err)
	}
	recs, _ := h.store.Load()
	if recs[tag1].Round != 0 || recs[tag1].State != StateAwaitingCI {
		t.Fatalf("undelivered round was counted: %+v", recs[tag1])
	}
	h.disp.err = nil
	if res := h.eval(t); res.Action != ActionRoundStarted || res.Record.Round != 1 {
		t.Fatalf("retry: action=%s round=%d", res.Action, res.Record.Round)
	}
}

func TestEvaluate_Errors(t *testing.T) {
	boom := errors.New("boom")
	t.Run("missing deps", func(t *testing.T) {
		if _, err := New(Options{}, nil, nil, nil, nil).Evaluate(context.Background()); !errors.Is(err, ErrMissingDependency) {
			t.Fatalf("err = %v", err)
		}
		var s *Sentinel
		if _, err := s.Evaluate(context.Background()); !errors.Is(err, ErrMissingDependency) {
			t.Fatalf("nil sentinel err = %v", err)
		}
	})
	t.Run("no tag", func(t *testing.T) {
		h := newHarness(t, nil)
		h.src.hasTag = false
		if res := h.eval(t); res.Action != ActionNone || res.Record != nil {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("tag error", func(t *testing.T) {
		h := newHarness(t, nil)
		h.src.tagErr = boom
		if _, err := h.sentinel().Evaluate(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("load error", func(t *testing.T) {
		h := newHarness(t, &memStore{loadErr: boom})
		if _, err := h.sentinel().Evaluate(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("save error", func(t *testing.T) {
		h := newHarness(t, &memStore{saveErr: boom})
		if _, err := h.sentinel().Evaluate(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("runs error", func(t *testing.T) {
		store := &memStore{}
		h := newHarness(t, store)
		h.src.runsErr = boom
		if _, err := h.sentinel().Evaluate(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if store.saves != 1 || store.records[tag1] == nil {
			t.Fatalf("state decided before the error was not persisted: saves=%d", store.saves)
		}
	})
	t.Run("details error", func(t *testing.T) {
		h := newHarness(t, nil)
		h.src.runs[shaA] = []Run{failing(1, shaA)}
		h.src.detailsErr = boom
		if _, err := h.sentinel().Evaluate(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if len(h.disp.reqs) != 0 {
			t.Fatalf("dispatched without evidence")
		}
	})
	t.Run("release error", func(t *testing.T) {
		h := newHarness(t, nil)
		h.src.runs[shaA] = []Run{passing(1, shaA)}
		h.src.releaseErr = boom
		if _, err := h.sentinel().Evaluate(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestOptionsDefaults(t *testing.T) {
	var o Options
	if o.maxRounds() != DefaultMaxRounds || o.roundTimeout() != DefaultRoundTimeout || o.agent() != DefaultAgent {
		t.Fatalf("defaults: %d %s %s", o.maxRounds(), o.roundTimeout(), o.agent())
	}
	if o.now().IsZero() {
		t.Fatal("now() without a clock returned zero")
	}
	if o.ignored("anything") {
		t.Fatal("empty ignore list ignored a workflow")
	}
}

func TestStateTerminal(t *testing.T) {
	for s, want := range map[State]bool{
		StateAwaitingCI: false, StateFixing: false,
		StateGreen: true, StateFailed: true, StateSuperseded: true,
	} {
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v", s, !want)
		}
	}
}

func TestHistoryIsBounded(t *testing.T) {
	r := &Record{State: StateAwaitingCI}
	now := time.Now()
	for i := 0; i < maxHistory*2; i++ {
		if i%2 == 0 {
			r.transition(now, StateFixing, "x")
		} else {
			r.transition(now, StateAwaitingCI, "y")
		}
	}
	if len(r.History) != maxHistory {
		t.Fatalf("history len = %d, want %d", len(r.History), maxHistory)
	}
	before := len(r.History)
	r.transition(now, r.State, "same state")
	if len(r.History) != before {
		t.Fatal("a self-transition was recorded in history")
	}
}

func TestPruneDropsOldestTerminalRecords(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := map[string]*Record{}
	for i := 0; i < maxRecords+5; i++ {
		recs[fmt.Sprintf("v0.0.%d", i)] = &Record{State: StateGreen, UpdatedAt: base.Add(time.Duration(i) * time.Hour)}
	}
	recs["v9.9.9"] = &Record{State: StateFixing, UpdatedAt: base}
	recs["v0.0.100"] = &Record{State: StateGreen, UpdatedAt: base} // ties with v0.0.0 by time
	out := prune(recs)
	if len(out) != maxRecords {
		t.Fatalf("len = %d, want %d", len(out), maxRecords)
	}
	if _, ok := out["v9.9.9"]; !ok {
		t.Fatal("prune dropped an active record")
	}
	if _, ok := out["v0.0.0"]; ok {
		t.Fatal("prune kept the oldest terminal record")
	}
	if _, ok := out[fmt.Sprintf("v0.0.%d", maxRecords+4)]; !ok {
		t.Fatal("prune dropped the newest terminal record")
	}
	small := map[string]*Record{"a": {State: StateGreen}}
	if len(prune(small)) != 1 {
		t.Fatal("prune touched a small map")
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA(shaA); got != shaA[:shortSHALen] {
		t.Fatalf("shortSHA = %q", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Fatalf("shortSHA short = %q", got)
	}
}
