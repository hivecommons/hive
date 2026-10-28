package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A review-backlog spin-off blocked on a PR names it: the payload carries
// blocked_on_pr (repo, number, URL, state from the caches), the caption says
// "Blocked on PR #N" rather than "No stage recorded yet", and the state is
// read from the status snapshot's open PRs or the recent-merge ring — never
// a GitHub call (hivecommons/hive#14863 wore a bare grey `blocked` pill).
func TestStageTrackerBlockedOnPRFromMarker(t *testing.T) {
	now := time.Date(2026, 10, 28, 13, 18, 0, 0, time.UTC)
	s := newTestServer()
	filed := now.Add(-7 * time.Hour)
	body := "Spun off from the review backlog.\n\n**Blocked on PR #14862.**\n" + github.BlockedOnMarker("hivecommons/hive", 14862)
	status := minimalPayload()
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: 14862, Title: "The blocker", Author: "pat", State: "open"}}},
		NonActionableIssues: []any{
			// Sidelined by the enumeration: only the reason survives.
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 14863, Title: "Review backlog: a", Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked on hivecommons/hive#14862"},
			// Blocked on a PR in another repo nothing lists: state unknown.
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 14868, Title: "Review backlog: b", Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked on hivecommons/pluk#7"},
			// Bare label, no marker: no blocked_on_pr, caption unchanged.
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 14900, Title: "Just blocked", Labels: []string{"blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked"},
			// Marker in the body but the label already lifted: not blocked.
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 14901, Title: "Unblocked", Labels: []string{"agent/reviewer"}, CreatedAt: filed, Bucket: "filtered", Reason: "Exempt label"},
		},
		ActionableIssues: []any{
			// Actionable copy with the body at hand (a webhook re-read).
			github.Issue{Number: 14926, Title: "Review backlog: c", Body: strings.ReplaceAll(body, "14862", "14923"), Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: filed},
		},
	}}
	s.UpdateStatus(status)
	tr := s.buildStageTracker(status, nil, now)

	it := trackerItem(t, tr, "issue", 14863)
	if it.BlockedOnPR == nil || it.BlockedOnPR.Repo != "hivecommons/hive" || it.BlockedOnPR.Number != 14862 {
		t.Fatalf("14863 blocked_on_pr = %+v", it.BlockedOnPR)
	}
	if it.BlockedOnPR.URL != "https://github.com/hivecommons/hive/pull/14862" || it.BlockedOnPR.State != blockedOnPRStateOpen {
		t.Fatalf("14863 blocked_on_pr url/state = %q %q", it.BlockedOnPR.URL, it.BlockedOnPR.State)
	}
	if !hasMod(it, modBlocked) || it.Caption != "Blocked on PR #14862 — lifts when it merges" || it.Actor != "PR #14862" {
		t.Fatalf("14863 mods=%v caption=%q actor=%q", it.Modifiers, it.Caption, it.Actor)
	}
	if strings.Contains(it.Caption, "No stage recorded yet") {
		t.Fatalf("default caption survived: %q", it.Caption)
	}

	it = trackerItem(t, tr, "issue", 14868)
	if it.BlockedOnPR == nil || it.BlockedOnPR.Repo != "hivecommons/pluk" || it.BlockedOnPR.Number != 7 || it.BlockedOnPR.State != "" {
		t.Fatalf("14868 blocked_on_pr = %+v", it.BlockedOnPR)
	}
	if it.Caption != "Blocked on PR hivecommons/pluk#7 — lifts when it merges" {
		t.Fatalf("14868 caption = %q (foreign repo keeps the owner/repo)", it.Caption)
	}

	it = trackerItem(t, tr, "issue", 14900)
	if it.BlockedOnPR != nil || !hasMod(it, modBlocked) || it.Caption != "No stage recorded yet" {
		t.Fatalf("bare blocked label: blocked_on_pr=%+v mods=%v caption=%q", it.BlockedOnPR, it.Modifiers, it.Caption)
	}
	it = trackerItem(t, tr, "issue", 14901)
	if it.BlockedOnPR != nil || hasMod(it, modBlocked) {
		t.Fatalf("lifted label must not read as blocked: %+v", it)
	}

	it = trackerItem(t, tr, "issue", 14926)
	if it.BlockedOnPR == nil || it.BlockedOnPR.Number != 14923 || it.BlockedOnPR.State != "" {
		t.Fatalf("14926 (body marker, unlisted blocker) blocked_on_pr = %+v", it.BlockedOnPR)
	}

	// The merged state comes from the recent-merge ring.
	tr.RecentlyMerged = []StageTrackerMerged{{Repo: "hivecommons/hive", Number: 14923, MergedAt: now.Add(-time.Minute)}}
	resolve := s.stageTrackerPRStateResolver(status, tr.RecentlyMerged)
	if got := resolve("hivecommons/hive", 14923); got != blockedOnPRStateMerged {
		t.Fatalf("merged blocker state = %q", got)
	}
	if got := resolve("hivecommons/hive", 14862); got != blockedOnPRStateOpen {
		t.Fatalf("open blocker state = %q", got)
	}
	var merged StageTrackerItem
	merged.Modifiers = []string{modBlocked}
	merged.Repo = "hivecommons/hive"
	stageTrackerBlockedOnPR(&merged, "", "blocked on hivecommons/hive#14923", "", resolve)
	if merged.BlockedOnPR == nil || merged.BlockedOnPR.State != blockedOnPRStateMerged || !strings.Contains(merged.Caption, "merged; the blocked sweep lifts the label next") {
		t.Fatalf("merged caption = %q (%+v)", merged.Caption, merged.BlockedOnPR)
	}
	closed := StageTrackerItem{Repo: "hivecommons/hive", Modifiers: []string{modBlocked}}
	stageTrackerBlockedOnPR(&closed, "", "blocked on hivecommons/hive#1", "", func(string, int) string { return blockedOnPRStateClosed })
	if !strings.Contains(closed.Caption, "closed unmerged; the blocked sweep re-triages it next") {
		t.Fatalf("closed caption = %q", closed.Caption)
	}

	// JSON shape the SPA reads.
	raw, _ := json.Marshal(trackerItem(t, tr, "issue", 14863))
	for _, want := range []string{`"blocked_on_pr":{`, `"repo":"hivecommons/hive"`, `"number":14862`, `"url":"https://github.com/hivecommons/hive/pull/14862"`, `"state":"open"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("payload lacks %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"blocked_on":`) {
		t.Fatalf("the GitHub blocked-by list must stay separate: %s", raw)
	}
}

// The hive's own unblock takes the same live path as a label webhook: the
// ItemRefreshHook drops the tracker cache and queues the issue for a
// targeted re-read whose snapshot patches the status row and is handed to
// the governor — the row re-derives now, not after the next enumeration
// (hivecommons/hive#14863 showed `blocked` 40 minutes after the hive
// removed the label).
func TestRefreshSelfEditedItemPatchesRowLikeAWebhook(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 41, 30, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s := newTestServer()
	status := minimalPayload()
	status.Repos = []FrontendRepo{{
		Name: "console", Full: "acme/console",
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "acme/console", Number: 8, Title: "Review backlog", Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: now.Add(-7 * time.Hour), Bucket: "filtered", Reason: "blocked on acme/console#5"},
		},
	}}
	s.UpdateStatus(status)
	if it := trackerItem(t, s.buildStageTracker(s.status, nil, now), "issue", 8); !hasMod(it, modBlocked) || it.BlockedOnPR == nil {
		t.Fatalf("precondition: row should be blocked on #5, got %+v", it)
	}

	var ghCalls []string
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ghCalls = append(ghCalls, r.URL.Path)
		if r.URL.Path != "/repos/acme/console/issues/8" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "title": "Review backlog", "state": "open",
			"updated_at": now.Format(time.RFC3339), "labels": []map[string]string{{"name": "agent/reviewer"}},
			"user": map[string]any{"login": "hive-app[bot]"}})
	}))
	t.Cleanup(ghSrv.Close)
	var handed []github.WebhookItemSnapshot
	s.deps = &Dependencies{
		GHClient:          github.NewClientForTest(ghSrv.URL, "acme", []string{"console"}, s.logger),
		Ctx:               context.Background(),
		Logger:            s.logger,
		Config:            &config.Config{},
		ApplyWebhookItems: func(snaps []github.WebhookItemSnapshot) { handed = append(handed, snaps...) },
	}
	timers := &fakeTimers{}
	s.webhookRefresh = newWebhookTrackerRefresher()
	s.webhookRefresh.newTimer = timers.newTimer

	s.RefreshSelfEditedItem("acme/console", 8, false)
	if timers.count() != 1 {
		t.Fatal("self edit must arm the targeted re-read")
	}
	s.FlushWebhookTrackerRefresh()
	if len(ghCalls) != 1 {
		t.Fatalf("GitHub calls = %v, want the single issue GET", ghCalls)
	}
	if len(handed) != 1 || handed[0].Number != 8 || handed[0].DeliveryID != selfEditDeliveryID || !handed[0].Open() {
		t.Fatalf("governor hook received %+v", handed)
	}
	it := trackerItem(t, s.buildStageTracker(s.status, nil, now), "issue", 8)
	if hasMod(it, modBlocked) || it.BlockedOnPR != nil {
		t.Fatalf("row still blocked after the hive's own unblock: mods=%v blocked_on_pr=%+v labels=%v", it.Modifiers, it.BlockedOnPR, it.Labels)
	}

	// Guards: nil receiver, empty repo, no number — all no-ops.
	var nilSrv *Server
	nilSrv.RefreshSelfEditedItem("acme/console", 8, false)
	s.RefreshSelfEditedItem("", 8, false)
	s.RefreshSelfEditedItem("acme/console", 0, false)
	if timers.count() != 1 {
		t.Fatalf("guards armed a timer: %d", timers.count())
	}
}
