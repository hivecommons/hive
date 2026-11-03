package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestRefreshSelfEditedIssueDecisionDropsStalePark(t *testing.T) {
	for _, preserved := range []string{"", modBlocked, modHold} {
		t.Run("preserve_"+preserved, func(t *testing.T) {
			now := time.Now().UTC()
			t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
			s := newTestServer()
			store := stagelabels.NewStore("", func() time.Time { return now })
			store.Observe(stagelabels.Key("acme/console", 8), stagelabels.Actionable, "maintainer decision accepted")
			staleLabels := []string{"needs-human", stagelabels.IssueNeedsDecision.Label()}
			freshLabels := []map[string]string{{"name": github.HumanAckLabel}, {"name": stagelabels.Actionable.Label()}}
			if preserved != "" {
				staleLabels = append(staleLabels, preserved)
				freshLabels = append(freshLabels, map[string]string{"name": preserved})
			}
			status := minimalPayload()
			status.Repos = []FrontendRepo{{
				Name: "console", Full: "acme/console",
				NonActionableIssues: []any{github.RepoWorkIssue{
					Repo: "acme/console", Number: 8, Title: "Accepted direction",
					Labels: staleLabels, CreatedAt: now.Add(-time.Hour), Bucket: "filtered",
				}},
			}}
			s.UpdateStatus(status)
			before := trackerItem(t, s.buildStageTracker(s.status, store, now), "issue", 8)
			expectedStage := stagelabels.Actionable.Number()
			if preserved == modHold {
				expectedStage = stagelabels.None.Number()
			}
			if before.Stage != expectedStage || !hasMod(before, modNeedsHuman) {
				t.Fatalf("precondition: expected stage %d with stale human modifier, got %+v", expectedStage, before)
			}

			ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/console/issues/8" {
					t.Errorf("unexpected GitHub read: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 8, "title": "Accepted direction", "state": "open",
					"updated_at": now.Format(time.RFC3339), "labels": freshLabels,
					"user": map[string]string{"login": "hive-app[bot]"},
				})
			}))
			t.Cleanup(ghSrv.Close)
			s.deps = &Dependencies{
				GHClient: github.NewClientForTest(ghSrv.URL, "acme", []string{"console"}, s.logger),
				Ctx:      context.Background(), Logger: s.logger, Config: &config.Config{},
			}
			s.webhookRefresh = newWebhookTrackerRefresher()
			timers := &fakeTimers{}
			s.webhookRefresh.newTimer = timers.newTimer
			s.RefreshSelfEditedItem("acme/console", 8, false)
			s.FlushWebhookTrackerRefresh()
			after := trackerItem(t, s.buildStageTracker(s.status, store, now), "issue", 8)
			if hasMod(after, modNeedsHuman) || hasMod(after, modNeedsDecision) || after.DecisionNeeded != nil || after.NeedsHuman {
				t.Fatalf("accepted decision still asks a person: %+v", after)
			}
			if preserved != "" && !hasMod(after, preserved) {
				t.Fatalf("refresh lost genuine %s modifier: %+v", preserved, after)
			}
			// A later genuine park must still be shown; never suppress labels
			// merely because the older store record says Actionable.
			freshLabels = append(freshLabels, map[string]string{"name": "needs-human"})
			s.RefreshSelfEditedItem("acme/console", 8, false)
			s.FlushWebhookTrackerRefresh()
			reparked := trackerItem(t, s.buildStageTracker(s.status, store, now), "issue", 8)
			if !hasMod(reparked, modNeedsHuman) {
				t.Fatalf("refresh hid a later genuine human park: %+v", reparked)
			}
		})
	}
}
