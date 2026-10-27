package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestPRTerminalAuditEvictsAndReplays(t *testing.T) {
	for _, action := range []string{github.AuditActionPRMerged, github.AuditActionPRClosed} {
		t.Run(action, func(t *testing.T) {
			now := time.Now()
			s, _ := stageTrackerFixture(t, now)
			// The same number in the sidelined list must also disappear.
			s.status.Repos[0].NonActionablePrs = []any{github.RepoWorkPR{Repo: "acme/console", Number: 101}}
			old := s.status
			var handed []github.WebhookItemSnapshot
			s.deps = &Dependencies{ApplyWebhookItems: func(snaps []github.WebhookItemSnapshot) { handed = append(handed, snaps...) }}
			s.ApplyPRTerminalAudit(github.AuditRecord{Action: action, Repo: "acme/console", Target: 101})
			if len(handed) != 1 || handed[0].State != "closed" || !handed[0].IsPR || handed[0].Merged != (action == github.AuditActionPRMerged) {
				t.Fatalf("governor patch = %+v", handed)
			}
			assertGone := func() {
				t.Helper()
				for _, item := range s.buildStageTracker(s.status, s.stageStore, now).Items {
					if item.Kind == "pr" && item.Number == 101 {
						t.Fatal("terminal PR still on tracker")
					}
				}
			}
			assertGone()
			if len(old.Repos[0].NonActionablePrs) != 1 {
				t.Fatal("old snapshot mutated")
			}
			s.UpdateStatus(old)
			assertGone()
		})
	}
}

func TestPRTerminalAuditIgnoresNonTerminalOrInvalidRecords(t *testing.T) {
	s, _ := stageTrackerFixture(t, time.Now())
	old := s.status
	calls := 0
	s.deps = &Dependencies{ApplyWebhookItems: func([]github.WebhookItemSnapshot) { calls++ }}
	for _, rec := range []github.AuditRecord{
		{Action: "pr_merge_failed", Repo: "acme/console", Target: 101},
		{Action: github.AuditActionPRMerged, Target: 101},
		{Action: github.AuditActionPRClosed, Repo: "acme/console"},
	} {
		s.ApplyPRTerminalAudit(rec)
	}
	if calls != 0 || s.status != old {
		t.Fatal("non-terminal or invalid audit changed snapshots")
	}
}
