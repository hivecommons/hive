package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func seedCIFailureWebhookServer(t *testing.T) (*Server, *[]github.CIFailureSignal, *[]string) {
	t.Helper()
	s := newGitHubWebhookTestServer(t)
	s.deps.GHClient.SetAppBotLogin("hive-bot[bot]")
	s.deps.Config = &config.Config{Project: config.ProjectConfig{Org: "webhookorg", Repos: []string{"webhookrepo"}}}
	github.SeedPRDetailCacheForTest("webhookorg/webhookrepo", 41, "hive-bot[bot]", "agent PR", "feedface", "main")
	github.SeedPRDetailCacheForTest("webhookorg/webhookrepo", 42, "octocat", "human PR", "c0ffee", "main")

	var kicks []github.CIFailureSignal
	var nudges []string
	s.deps.CIFailureKick = func(sig github.CIFailureSignal) CIFailureKickOutcome {
		kicks = append(kicks, sig)
		return CIFailureKickOutcome{Agent: "scanner", Kicked: true}
	}
	s.deps.SweepNudge = func(repo string, number int) { nudges = append(nudges, repo+"#"+strconv.Itoa(number)) }
	return s, &kicks, &nudges
}

func checkRunFailureBody(sha, name string) []byte {
	return []byte(`{"action":"completed","repository":{"full_name":"webhookorg/webhookrepo"},"check_run":{"head_sha":"` + sha + `","name":"` + name + `","conclusion":"failure","details_url":"https://ci/` + name + `","pull_requests":[]}}`)
}

func postCIFailure(t *testing.T, s *Server, event string, body []byte) []ciFailureReaction {
	t.Helper()
	const secret = "s3cr3t"
	rec := postGitHubWebhook(s, event, signGitHubWebhookForTest(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		CIFailure []ciFailureReaction `json:"ci_failure"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.CIFailure
}

// A failed check on a hive-authored PR head kicks the owning agent through
// the hook and writes the activity line; the same head+check a second time
// is a duplicate and does nothing; a contributor PR head nudges the sweep
// (contributor lane / stage 072) instead, once per head within the cooldown.
func TestGitHubWebhookCIFailure_RoutesAndDedups(t *testing.T) {
	t.Setenv(githubWebhookSecretEnvVar, "s3cr3t")
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	t.Cleanup(github.ResetCIFailureSignalsForTest(clock))
	t.Cleanup(github.ResetWebhookTrackerForTest())
	s, kicks, nudges := seedCIFailureWebhookServer(t)

	got := postCIFailure(t, s, "check_run", checkRunFailureBody("feedface", "build-gate"))
	if len(got) != 1 || got[0].Number != 41 || got[0].Route != "agent" || got[0].Outcome != "kicked" || got[0].Agent != "scanner" {
		t.Fatalf("agent reaction = %+v", got)
	}
	if len(*kicks) != 1 || (*kicks)[0].Check != "build-gate" || (*kicks)[0].DetailsURL != "https://ci/build-gate" || !(*kicks)[0].HiveAuthored {
		t.Fatalf("kick hook got %+v", *kicks)
	}
	var sawLine bool
	for _, e := range s.audit.Recent(50) {
		if e.Action == auditActionCIFailureKick && strings.Contains(e.Detail, "CI failed on #41 (build-gate) — kicked agent scanner") {
			sawLine = true
		}
	}
	if !sawLine {
		t.Fatalf("activity log missing the CI failure line: %+v", s.audit.Recent(50))
	}

	// Same head + check again (GitHub redelivery / check_suite fan-out):
	// deduplicated, no second kick. The cache entry is gone after the first
	// delivery, so this exercises the number-only path too.
	github.SeedPRDetailCacheForTest("webhookorg/webhookrepo", 41, "hive-bot[bot]", "agent PR", "feedface", "main")
	got = postCIFailure(t, s, "check_run", checkRunFailureBody("feedface", "build-gate"))
	if len(got) != 1 || got[0].Outcome != "duplicate" || len(*kicks) != 1 {
		t.Fatalf("duplicate reaction = %+v, kicks = %d", got, len(*kicks))
	}

	// Contributor PR: nudge the sweep, never the kick hook.
	status := []byte(`{"sha":"c0ffee","state":"failure","context":"ci/test","target_url":"https://ci/3","repository":{"full_name":"webhookorg/webhookrepo"}}`)
	got = postCIFailure(t, s, "status", status)
	if len(got) != 1 || got[0].Number != 42 || got[0].Route != "contributor" || got[0].Outcome != "nudged" {
		t.Fatalf("contributor reaction = %+v", got)
	}
	if len(*nudges) != 1 || !strings.HasPrefix((*nudges)[0], "webhookorg/webhookrepo#") || len(*kicks) != 1 {
		t.Fatalf("nudges = %v, kicks = %d", *nudges, len(*kicks))
	}
	// A different check on the same contributor head within the cooldown is
	// a new signal but no second nudge.
	github.SeedPRDetailCacheForTest("webhookorg/webhookrepo", 42, "octocat", "human PR", "c0ffee", "main")
	got = postCIFailure(t, s, "check_run", checkRunFailureBody("c0ffee", "lint"))
	if len(got) != 1 || got[0].Outcome != "cooldown" || len(*nudges) != 1 {
		t.Fatalf("cooldown reaction = %+v, nudges = %v", got, *nudges)
	}
	now = now.Add(config.DefaultCIFailureKickCooldownS*time.Second + time.Second)
	github.SeedPRDetailCacheForTest("webhookorg/webhookrepo", 42, "octocat", "human PR", "c0ffee", "main")
	got = postCIFailure(t, s, "check_run", checkRunFailureBody("c0ffee", "e2e"))
	if len(got) != 1 || got[0].Outcome != "nudged" || len(*nudges) != 2 {
		t.Fatalf("post-cooldown reaction = %+v, nudges = %v", got, *nudges)
	}

	// /api/status exposes the store.
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "webhook-test"}
	s.statusMu.Unlock()
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status?fields=ci_failure_signals", nil))
	var st struct {
		Signals github.CIFailureSignalsStatus `json:"ci_failure_signals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Signals.Count != 4 || st.Signals.Last == nil || st.Signals.Last.Check != "e2e" || st.Signals.Kicks != 2 {
		t.Fatalf("/api/status ci_failure_signals = %+v", st.Signals)
	}
}

// Repo pause and the config flag stop the reaction (the signal is still
// recorded for the sweep); a head no open PR matches counts as unmatched.
func TestGitHubWebhookCIFailure_PauseFlagAndUnmatched(t *testing.T) {
	t.Setenv(githubWebhookSecretEnvVar, "s3cr3t")
	t.Cleanup(github.ResetCIFailureSignalsForTest(nil))
	t.Cleanup(github.ResetWebhookTrackerForTest())
	s, kicks, nudges := seedCIFailureWebhookServer(t)

	s.deps.Config.Project.PausedRepos = []config.RepoPause{{Repo: "webhookrepo"}}
	got := postCIFailure(t, s, "check_run", checkRunFailureBody("feedface", "build-gate"))
	if len(got) != 1 || got[0].Outcome != "repo paused" || len(*kicks) != 0 {
		t.Fatalf("paused repo reaction = %+v, kicks = %d", got, len(*kicks))
	}
	s.deps.Config.Project.PausedRepos = nil

	off := false
	s.deps.Config.Governor.CIFailureWebhookKick = &off
	github.SeedPRDetailCacheForTest("webhookorg/webhookrepo", 42, "octocat", "human PR", "c0ffee", "main")
	got = postCIFailure(t, s, "check_run", checkRunFailureBody("c0ffee", "lint"))
	if len(got) != 1 || got[0].Outcome != "disabled" || len(*nudges) != 0 {
		t.Fatalf("disabled reaction = %+v, nudges = %v", got, *nudges)
	}
	if snap := github.CIFailureSignals().Snapshot(); snap.Count != 2 {
		t.Fatalf("signals must still be recorded for the sweep: %+v", snap)
	}
	s.deps.Config.Governor.CIFailureWebhookKick = nil

	got = postCIFailure(t, s, "status", []byte(`{"sha":"deadbeef","state":"error","context":"ci","repository":{"full_name":"webhookorg/webhookrepo"}}`))
	if len(got) != 0 {
		t.Fatalf("unmatched head must produce no reaction: %+v", got)
	}
	if snap := github.CIFailureSignals().Snapshot(); snap.Unmatched != 1 {
		t.Fatalf("unmatched = %d, want 1", snap.Unmatched)
	}
	// A green check never reacts.
	green := []byte(`{"action":"completed","repository":{"full_name":"webhookorg/webhookrepo"},"check_run":{"head_sha":"feedface","name":"build-gate","conclusion":"success","pull_requests":[{"number":41}]}}`)
	if got := postCIFailure(t, s, "check_run", green); len(got) != 0 {
		t.Fatalf("success must not react: %+v", got)
	}
}
