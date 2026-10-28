package dashboard

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15052: "Let hive decide" on an adoption whose hive copy
// was closed unmerged.

const orphanAdoptedMarker = automerge.AdoptionMarkerPrefix + " by=h1 reason=contributor-offline mode=supersede superseded_by=90 head=feedfacefeedfacefeedfacefeedfacefeedface -->\n@someone the hive has adopted this PR."

func orphanPriorDecision() string {
	return delegatedecision.CommentBody(delegatedecision.Decision{Verdict: delegatedecision.VerdictAdoptSupersede, Rule: "contributor-offline", Rationale: "r"}, "h1", "writer")
}

// The recorded contributor-offline decision has a closed, unmerged copy
// behind it: not "no change" but a fresh adoption request, said plainly.
func TestDelegatedDecisionOrphanedAdoptionReadopts(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", supersederState: "closed"}
	mock.priorComments = []string{orphanPriorDecision(), orphanAdoptedMarker}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"contributor-lane:decision-requested","caption":"Needs human decision"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionAdopted {
		t.Fatalf("result = %+v status=%s err=%s", job.Result, job.Status, job.Error)
	}
	if job.Result.Outcome != "Re-adopting: hive's earlier copy #90 was closed by mistake; opening a fresh copy." {
		t.Fatalf("outcome = %q", job.Result.Outcome)
	}
	if job.Decision == nil || job.Decision.Rule != delegatedRuleAdoptionOrphaned || job.Decision.Verdict != delegatedecision.VerdictAdoptSupersede {
		t.Fatalf("decision = %+v", job.Decision)
	}
	if !strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("re-adoption not queued: %v", mock.added)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "closed without merging") || !strings.Contains(mock.comments[0], "#90") {
		t.Fatalf("decision comment: %v", mock.comments)
	}
	if len(mock.edits) != 0 {
		t.Fatalf("original PR edited: %v", mock.edits)
	}
}

// A copy still open, or merged, is not an orphan: the recorded decision
// stands and a repeat click is the usual no-op (or, past the effect
// timeout, the plain-words "hasn't taken effect" of hivecommons/hive#14822)
// — never a re-adoption.
func TestDelegatedDecisionAdoptionWithLiveCopyIsNoop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  string
		merged bool
	}{{"open", "open", false}, {"merged", "closed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", supersederState: tc.state, supersederMerged: tc.merged}
			mock.priorComments = []string{orphanPriorDecision(), orphanAdoptedMarker}
			srv := newDelegatedServer(t, mock)
			w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"contributor-lane:decision-requested"}`, nil)
			job := decodeDelegatedJob(t, w)
			if job.Result == nil || (job.Result.Action != rowActionNoop && job.Result.Action != rowActionNoEffect) || job.Result.Rule != "contributor-offline" {
				t.Fatalf("result = %+v", job.Result)
			}
			if job.Decision == nil || job.Decision.Rule == delegatedRuleAdoptionOrphaned || strings.Contains(job.Result.Outcome, "Re-adopting") {
				t.Fatalf("re-adopted a standing adoption: decision=%+v result=%+v", job.Decision, job.Result)
			}
			if len(mock.added) != 0 || strings.Contains(strings.Join(mock.comments, ","), "closed without merging") {
				t.Fatalf("wrote on a standing adoption: comments=%v added=%v", mock.comments, mock.added)
			}
		})
	}
}

// The store says the copies were closed twice within the hour: no third
// copy; a recommendation names the loop and leaves the park.
func TestDelegatedDecisionOrphanedAdoptionLoopedRecommends(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", supersederState: "closed"}
	mock.priorComments = []string{orphanPriorDecision(), orphanAdoptedMarker}
	srv := newDelegatedServer(t, mock)
	store := stagelabels.NewStore("", nil)
	key := stagelabels.Key("testorg/testrepo", 8)
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC)
	store.Observe(key, stagelabels.NeedsDecision, stagelabels.LaneReasonAdoptionChildClosedTwice)
	store.MarkAdopted(key, "feedfacefeedfacefeedfacefeedfacefeedface", 90, now)
	store.MarkAdoptionEscalated(key, "feedfacefeedfacefeedfacefeedfacefeedface", stagelabels.LaneReasonAdoptionChildClosedTwice)
	srv.stageStore = store
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"adoption-child-closed-twice"}`, nil)
	job := decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionRecommended || !strings.Contains(job.Result.Outcome, "twice within an hour") {
		t.Fatalf("result = %+v", job.Result)
	}
	if strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("a third copy was requested: %v", mock.added)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "not opening a third") {
		t.Fatalf("recommendation: %v", mock.comments)
	}
}

func TestOrphanedAdoptionDecisionText(t *testing.T) {
	d := orphanedAdoption{Dead: 14977}.decision("#14947")
	if d.Rule != delegatedRuleAdoptionOrphaned || len(d.Actions) != 2 || d.Actions[1].Kind != delegatedecision.ActionAdoptSupersede || d.Actions[1].Target != 14977 {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(d.Text, "earlier copy #14977 was closed without merging") {
		t.Fatalf("text = %q", d.Text)
	}
	if got := delegatedOrphanOutcome(14977); got != "Re-adopting: hive's earlier copy #14977 was closed by mistake; opening a fresh copy." {
		t.Fatalf("outcome = %q", got)
	}
}
