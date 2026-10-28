package dashboard

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestDelegatedOrphanDoesNotDeduplicateDeadDecision(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, adoptionChildClosed: true}
	mock.priorComments = []string{
		delegatedecision.CommentBody(delegatedecision.Decision{Verdict: delegatedecision.VerdictAdoptSupersede, Rule: "contributor-offline"}, "h1", "writer"),
		"<!-- hive:adopted mode=supersede superseded_by=90 head=feedfacefeedfacefeedfacefeedfacefeedface -->",
	}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"requires_human"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Status != delegatedJobStatusDone || job.Result == nil || job.Result.Action == rowActionNoop {
		t.Fatalf("orphan was a noop: %+v", job)
	}
	if !strings.Contains(job.Result.Outcome, "Re-adopting: hive's earlier copy #90") || !strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("re-adoption not queued / explained: %+v labels=%v", job.Result, mock.added)
	}
}

func TestTrackerOrphanAndLoopGuardArePlain(t *testing.T) {
	now := time.Now()
	s, store, status := adoptionFixture(t, now, "adopt", nil)
	key := stagelabels.Key("hivecommons/hive", 14524)
	store.MarkAdopted(key, "feed", 90, now)
	store.Observe(key, stagelabels.NeedsDecision, "requires_human")
	store.NoteAdoptionOrphan(key, 90, now, false)
	pr := status.Repos[0].OpenPrs[0].(FrontendPR)
	pr.Labels = []string{"stage/085-needs-decision", "needs-human", "author/clanker", "role/contributor"}
	status.Repos[0].OpenPrs[0] = pr
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if !it.AdoptChildClosed || it.Caption != "Adopted → #90 (closed) · recovering" {
		t.Fatalf("orphan hidden: %+v", it)
	}
	store.MarkAdoptionEscalated(key, "feed", "adoption-child-closed-twice")
	it = trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if !strings.Contains(it.Caption, "adoption-child-closed-twice") {
		t.Fatalf("loop guard hidden: %+v", it)
	}
}
