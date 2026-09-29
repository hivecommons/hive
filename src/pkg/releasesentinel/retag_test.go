package releasesentinel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	shaC      = "cccccccccccccccccccccccccccccccccccccccc"
	fixPRNum  = 42
	fixBranch = "main"
)

type fakeFixes struct {
	branch    string
	branchErr error
	pr        FixPR
	ok        bool
	err       error
	// what MergedFixPR was asked
	branches    []string
	sinces      []time.Time
	branchCalls int
}

func (f *fakeFixes) DefaultBranch(context.Context) (string, error) {
	f.branchCalls++
	return f.branch, f.branchErr
}

func (f *fakeFixes) MergedFixPR(_ context.Context, branch, _ string, since time.Time) (FixPR, bool, error) {
	f.branches = append(f.branches, branch)
	f.sinces = append(f.sinces, since)
	return f.pr, f.ok, f.err
}

type fakeRetagger struct {
	reqs []RetagRequest
	err  error
}

func (r *fakeRetagger) Retag(_ context.Context, req RetagRequest) error {
	r.reqs = append(r.reqs, req)
	return r.err
}

// retagHarness is a harness whose tag v1.2.3 (shaA) has a failing run and a
// merged, marked fix PR (#42, merge commit shaB) ready to be retagged to.
type retagHarness struct {
	*harness
	fixes    *fakeFixes
	retagger *fakeRetagger
}

func newRetagHarness(t *testing.T) *retagHarness {
	t.Helper()
	h := newHarness(t, nil)
	h.opts.RetagEnabled = true
	h.src.runs[shaA] = []Run{failing(1, shaA)}
	return &retagHarness{
		harness:  h,
		fixes:    &fakeFixes{branch: fixBranch, pr: FixPR{Number: fixPRNum, MergeSHA: shaB, Commits: []string{shaB}}},
		retagger: &fakeRetagger{},
	}
}

func (h *retagHarness) sentinel() *Sentinel {
	return h.harness.sentinel().WithRetag(h.fixes, h.retagger)
}

func (h *retagHarness) eval(t *testing.T) Result {
	t.Helper()
	res, err := h.sentinel().Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return res
}

func TestRetag_MovesTagToMergedFixThenReEvaluatesCIThere(t *testing.T) {
	h := newRetagHarness(t)
	res := h.eval(t)
	if res.Action != ActionRoundStarted || !h.disp.reqs[0].Retag {
		t.Fatalf("round 1: action=%s retag=%v", res.Action, h.disp.reqs[0].Retag)
	}
	roundStart := h.clk.t

	// The agent's fix PR merged 20 minutes later.
	h.fixes.ok = true
	h.clk.t = h.clk.t.Add(20 * time.Minute)
	res = h.eval(t)
	if res.Action != ActionRetagged {
		t.Fatalf("action = %s, want retagged", res.Action)
	}
	if len(h.retagger.reqs) != 1 {
		t.Fatalf("retag calls = %d, want 1", len(h.retagger.reqs))
	}
	req := h.retagger.reqs[0]
	if req.Tag != tag1 || req.OldSHA != shaA || req.NewSHA != shaB || req.Branch != fixBranch || req.Repo != "acme/widgets" ||
		len(req.FixCommits) != 1 || req.FixCommits[0] != shaB || req.AllowIntervening {
		t.Fatalf("retag request = %+v", req)
	}
	if len(h.fixes.sinces) != 1 || !h.fixes.sinces[0].Equal(roundStart) {
		t.Fatalf("fix PR looked up since %v, want the round start %v", h.fixes.sinces, roundStart)
	}
	rec := res.Record
	if rec.SHA != shaB || rec.State != StateAwaitingCI || rec.RetagPR != fixPRNum || rec.RetagFromSHA != shaA || rec.Round != 1 || rec.BlockingRuns != nil {
		t.Fatalf("record after retag = %+v", rec)
	}

	// The forge now reports the tag on shaB, and shaB's CI fails too: the
	// sentinel evaluated the NEW commit and starts round 2 against it.
	h.src.tag = Tag{Name: tag1, SHA: shaB}
	h.src.runs[shaB] = []Run{failing(9, shaB)}
	h.fixes.ok = false
	res = h.eval(t)
	if got := h.src.runQueries[len(h.src.runQueries)-1]; got != shaB {
		t.Fatalf("runs queried for %s after the retag, want %s", got, shaB)
	}
	if res.Action != ActionRoundStarted || res.Record.Round != 2 || res.Record.RoundSHA != shaB {
		t.Fatalf("after retag: action=%s record=%+v", res.Action, res.Record)
	}
	if len(h.disp.reqs) != 2 || h.disp.reqs[1].SHA != shaB {
		t.Fatalf("round 2 request = %+v", h.disp.reqs)
	}

	// And when shaB's CI is green with a published release, the release is green.
	h.src.runs[shaB] = []Run{passing(9, shaB)}
	h.src.release[tag1] = ReleaseInfo{Exists: true}
	if res := h.eval(t); res.Action != ActionGreen {
		t.Fatalf("action = %s, want green", res.Action)
	}
}

func TestRetag_ToggleOffNeverMovesATag(t *testing.T) {
	h := newRetagHarness(t)
	h.opts.RetagEnabled = false
	h.fixes.ok = true
	for i := 0; i < 3; i++ {
		res := h.eval(t)
		if res.Action == ActionRetagged || res.Record.SHA != shaA {
			t.Fatalf("pass %d: toggle off moved the tag: %+v", i, res)
		}
		h.clk.t = h.clk.t.Add(10 * time.Minute)
	}
	if len(h.retagger.reqs) != 0 || len(h.fixes.branches) != 0 || h.fixes.branchCalls != 0 {
		t.Fatalf("toggle off consulted the retag path: retags=%d lookups=%d", len(h.retagger.reqs), len(h.fixes.branches))
	}
	if h.disp.reqs[0].Retag {
		t.Fatal("toggle off still promised a retag in the kick")
	}
}

func TestRetag_EnabledWithoutRetaggerIsOff(t *testing.T) {
	h := newRetagHarness(t)
	h.fixes.ok = true
	// RetagEnabled but nothing wired: the plain sentinel.
	s := h.harness.sentinel()
	for i := 0; i < 2; i++ {
		res, err := s.Evaluate(context.Background())
		if err != nil || res.Action == ActionRetagged {
			t.Fatalf("pass %d: res=%+v err=%v", i, res, err)
		}
	}
	if len(h.retagger.reqs) != 0 || h.disp.reqs[0].Retag {
		t.Fatal("an unwired retagger was used or promised")
	}
}

func TestRetag_NoMergedFixYetWaits(t *testing.T) {
	h := newRetagHarness(t)
	h.eval(t)
	h.clk.t = h.clk.t.Add(time.Minute)
	res := h.eval(t)
	if res.Action != ActionWaiting || res.Record.State != StateFixing || len(h.retagger.reqs) != 0 {
		t.Fatalf("no fix yet: action=%s state=%s retags=%d", res.Action, res.Record.State, len(h.retagger.reqs))
	}
	if h.fixes.branchCalls != 1 || h.fixes.branches[0] != fixBranch {
		t.Fatalf("branch not resolved from the default branch: calls=%d branches=%v", h.fixes.branchCalls, h.fixes.branches)
	}
}

func TestRetag_ConfiguredBranchAndInterveningPassThrough(t *testing.T) {
	h := newRetagHarness(t)
	h.opts.ReleaseBranch = " release-1.2 "
	h.opts.RetagAllowIntervening = true
	h.eval(t)
	h.fixes.ok = true
	h.eval(t)
	if h.fixes.branchCalls != 0 {
		t.Fatal("configured release branch still asked the forge for the default branch")
	}
	if len(h.retagger.reqs) != 1 || h.retagger.reqs[0].Branch != "release-1.2" || !h.retagger.reqs[0].AllowIntervening {
		t.Fatalf("retag request = %+v", h.retagger.reqs)
	}
}

func TestRetag_RefusalIsRecordedAndNeverRetried(t *testing.T) {
	h := newRetagHarness(t)
	h.eval(t)
	h.fixes.ok = true
	h.retagger.err = refused("fix commit is not a fast-forward")
	res := h.eval(t)
	if res.Action != ActionWaiting || res.Record.SHA != shaA || res.Record.State != StateFixing {
		t.Fatalf("refused retag: action=%s record=%+v", res.Action, res.Record)
	}
	if res.Record.RetagRefusedPR != fixPRNum || !strings.Contains(res.Record.RetagNote, "fast-forward") || !strings.Contains(res.RetagRefused, "#42") {
		t.Fatalf("refusal not recorded: record=%+v refused=%q", res.Record, res.RetagRefused)
	}
	h.clk.t = h.clk.t.Add(time.Minute)
	h.retagger.err = nil
	res = h.eval(t)
	if len(h.retagger.reqs) != 1 || res.Action == ActionRetagged {
		t.Fatalf("refused fix PR was retried: retags=%d action=%s", len(h.retagger.reqs), res.Action)
	}
	// A DIFFERENT merged fix PR is a new candidate.
	h.fixes.pr = FixPR{Number: fixPRNum + 1, MergeSHA: shaC, Commits: []string{shaC}}
	if res := h.eval(t); res.Action != ActionRetagged || res.Record.SHA != shaC {
		t.Fatalf("new fix PR: action=%s record=%+v", res.Action, res.Record)
	}
}

func TestRetag_TransientErrorIsRetried(t *testing.T) {
	h := newRetagHarness(t)
	h.eval(t)
	h.fixes.ok = true
	h.retagger.err = errors.New("network down")
	res := h.eval(t)
	if res.Action != ActionWaiting || res.Record.SHA != shaA || !strings.Contains(res.RetagError, "network down") {
		t.Fatalf("transient failure: action=%s err=%q record=%+v", res.Action, res.RetagError, res.Record)
	}
	if res.Record.RetagRefusedPR != 0 {
		t.Fatal("a transient failure was recorded as a final refusal")
	}
	h.retagger.err = nil
	if res := h.eval(t); res.Action != ActionRetagged || len(h.retagger.reqs) != 2 {
		t.Fatalf("retry: action=%s retags=%d", res.Action, len(h.retagger.reqs))
	}
}

func TestRetag_TransientFailureDoesNotBlockTheRoundCap(t *testing.T) {
	h := newRetagHarness(t)
	h.opts.MaxRounds = 1
	h.eval(t)
	h.fixes.ok = true
	h.retagger.err = errors.New("push rejected by a tag ruleset")
	h.clk.t = h.clk.t.Add(2 * time.Hour) // past the round timeout, at the cap
	res := h.eval(t)
	if res.Action != ActionEscalated || res.Record.State != StateFailed || len(h.esc.got) != 1 {
		t.Fatalf("a failing retag kept the round alive: action=%s record=%+v", res.Action, res.Record)
	}
}

func TestRetag_LookupErrors(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(h *retagHarness){
		"default branch error": func(h *retagHarness) { h.fixes.branchErr = boom },
		"empty default branch": func(h *retagHarness) { h.fixes.branch = "" },
		"merged PR error":      func(h *retagHarness) { h.fixes.err = boom },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRetagHarness(t)
			h.eval(t)
			mutate(h)
			res := h.eval(t)
			if res.RetagError == "" {
				t.Fatal("lookup failure was not reported")
			}
			if len(h.retagger.reqs) != 0 {
				t.Fatal("retagged despite a lookup failure")
			}
		})
	}
}

func TestRetag_MergedPRWithoutMergeSHAIsIgnored(t *testing.T) {
	h := newRetagHarness(t)
	h.eval(t)
	h.fixes.ok = true
	h.fixes.pr.MergeSHA = ""
	if res := h.eval(t); res.Action != ActionWaiting || len(h.retagger.reqs) != 0 {
		t.Fatalf("action=%s retags=%d", res.Action, len(h.retagger.reqs))
	}
}

func TestRetag_LaggingTagReadDoesNotReopenOldFailure(t *testing.T) {
	h := newRetagHarness(t)
	h.eval(t)
	h.fixes.ok = true
	h.eval(t) // retag shaA -> shaB
	h.fixes.ok = false

	// The tag listing still shows shaA (a lagging read) and shaA is still red.
	h.clk.t = h.clk.t.Add(time.Minute)
	res := h.eval(t)
	if res.Action != ActionWaiting || res.Record.SHA != shaB || len(h.disp.reqs) != 1 {
		t.Fatalf("lagging read: action=%s record=%+v dispatches=%d", res.Action, res.Record, len(h.disp.reqs))
	}

	// Past the settle window the old commit is taken at face value.
	h.clk.t = h.clk.t.Add(retagSettleWindow)
	res = h.eval(t)
	if res.Record.SHA != shaA {
		t.Fatalf("past the settle window the tag read was still ignored: %+v", res.Record)
	}
}

func TestFixMarker(t *testing.T) {
	if FixMarker(tag1) != "Release-Sentinel: v1.2.3" {
		t.Fatalf("marker = %q", FixMarker(tag1))
	}
	body := "Fixes the release job.\n\n  release-sentinel: V1.2.3  \nmore"
	if !HasFixMarker(body, tag1) {
		t.Fatal("marker line with whitespace and other case not found")
	}
	for _, b := range []string{"", "Release-Sentinel: v1.2.4", "see Release-Sentinel: v1.2.3 inline", "Release-Sentinel: v1.2.30"} {
		if HasFixMarker(b, tag1) {
			t.Errorf("%q matched the v1.2.3 marker", b)
		}
	}
}

func TestRefusedWrapsSentinel(t *testing.T) {
	err := refused("x %d", 1)
	if !errors.Is(err, ErrRetagRefused) || !strings.Contains(err.Error(), "x 1") {
		t.Fatalf("err = %v", err)
	}
}
