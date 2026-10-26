package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/docs#469 as the hosted hive's status listed it on 2026-10-26:
// a dependabot PR, open on the tracker, merged by the automerge sweep at
// 18:25:46Z and still listed an hour later.
var (
	docs469ListedAt = time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
	docs469MergedAt = time.Date(2026, 10, 26, 18, 25, 46, 0, time.UTC)
)

func docs469Fixture(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "docs", Full: "hivecommons/docs", PRs: 1,
		OpenPrs: []any{
			github.PullRequest{Repo: "hivecommons/docs", Number: 469, Title: "build(deps): bump mkdocs", Author: "dependabot[bot]",
				State: "open", HeadSHA: "abc123", UpdatedAt: docs469ListedAt},
		},
	}}
	s.status = status
	return s
}

func trackerListsPR(t *testing.T, s *Server, repo string, number int) bool {
	t.Helper()
	s.statusMu.RLock()
	cur := s.status
	s.statusMu.RUnlock()
	for _, it := range s.buildStageTracker(cur, s.stageStore, docs469MergedAt.Add(time.Hour)).Items {
		if it.Kind == "pr" && it.Number == number && it.Repo == repo {
			return true
		}
	}
	return false
}

// The hive's own merge must take the row off the tracker and out of the
// governor's cached snapshot at once, without a webhook delivery, a closed
// sweep or a re-enumeration.
func TestHiveMergeEvictsTrackerRowWithoutWebhook(t *testing.T) {
	s := docs469Fixture(t)
	var patched []github.WebhookItemSnapshot
	s.deps = &Dependencies{ApplyWebhookItems: func(snaps []github.WebhookItemSnapshot) {
		patched = append(patched, snaps...)
	}}
	if !trackerListsPR(t, s, "hivecommons/docs", 469) {
		t.Fatal("fixture: docs#469 must be listed before the merge")
	}

	if !s.EvictHiveClosedItem("hivecommons/docs", 469, true, true, github.AuditActionPRMerged, docs469MergedAt) {
		t.Fatal("EvictHiveClosedItem reported no row removed")
	}
	if trackerListsPR(t, s, "hivecommons/docs", 469) {
		t.Fatal("docs#469 still listed on the tracker after the hive merged it")
	}
	s.statusMu.RLock()
	prs := s.status.Repos[0].PRs
	s.statusMu.RUnlock()
	if prs != 0 {
		t.Fatalf("repo PR count = %d after eviction, want 0", prs)
	}
	if len(patched) != 1 {
		t.Fatalf("governor snapshot received %d patches, want 1", len(patched))
	}
	got := patched[0]
	if got.Repo != "hivecommons/docs" || got.Number != 469 || !got.IsPR || got.Open() || !got.Merged {
		t.Fatalf("governor patch = %+v, want closed+merged PR hivecommons/docs#469", got)
	}
	if got.DeliveryID != hiveWriteDeliveryPrefix+github.AuditActionPRMerged {
		t.Fatalf("patch DeliveryID = %q, want hive-write audit marker", got.DeliveryID)
	}
	if !got.UpdatedAt.Equal(docs469MergedAt) {
		t.Fatalf("patch UpdatedAt = %v, want merge instant %v", got.UpdatedAt, docs469MergedAt)
	}

	// The eviction is remembered for replay, so a status publish from an
	// enumeration that began before the merge cannot bring the row back.
	stale := docs469Fixture(t)
	stale.statusMu.RLock()
	staleStatus := stale.status
	stale.statusMu.RUnlock()
	s.UpdateStatus(staleStatus)
	if trackerListsPR(t, s, "hivecommons/docs", 469) {
		t.Fatal("stale publish resurrected docs#469 after the hive merged it")
	}
}

// A second call for the same merge, an unknown repo, or an item the status
// never listed is a no-op rather than an error.
func TestHiveMergeEvictIsIdempotentAndIgnoresUnlisted(t *testing.T) {
	s := docs469Fixture(t)
	if !s.EvictHiveClosedItem("hivecommons/docs", 469, true, true, github.AuditActionPRMerged, docs469MergedAt) {
		t.Fatal("first eviction removed nothing")
	}
	if s.EvictHiveClosedItem("hivecommons/docs", 469, true, true, github.AuditActionPRMerged, docs469MergedAt) {
		t.Fatal("second eviction of the same PR reported a removal")
	}
	if s.EvictHiveClosedItem("hivecommons/other", 1, true, true, github.AuditActionPRMerged, docs469MergedAt) {
		t.Fatal("eviction for an unlisted repo reported a removal")
	}
	if s.EvictHiveClosedItem("", 469, true, true, github.AuditActionPRMerged, docs469MergedAt) {
		t.Fatal("eviction with an empty repo reported a removal")
	}
	if s.EvictHiveClosedItem("hivecommons/docs", 0, true, true, github.AuditActionPRMerged, docs469MergedAt) {
		t.Fatal("eviction with number 0 reported a removal")
	}
	var nilSrv *Server
	if nilSrv.EvictHiveClosedItem("hivecommons/docs", 469, true, true, github.AuditActionPRMerged, docs469MergedAt) {
		t.Fatal("nil server reported a removal")
	}
}
