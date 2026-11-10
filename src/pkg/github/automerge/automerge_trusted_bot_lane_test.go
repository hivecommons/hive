package automerge

// The trusted-bot lane must honor every bot login the operator enabled in
// Settings (auto_merge.trusted_bot_authors): claim the PR itself, never
// bounce it to the contributor lane as app-authored, treat the review
// swarm's verdict as advisory while trusted_bot_skip_review is on, and name
// an absent configured required check instead of parking the PR at a bare
// "needs a person".

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// TestTrustedBotLaneClaimsEveryConfiguredBot iterates every Settings toggle
// (config.KnownBotAuthors) plus a custom login the operator typed in: each
// is owned by the trusted-bot lane and handed off by the contributor lane
// under its own reason, never contributor-lane:app-authored.
func TestTrustedBotLaneClaimsEveryConfiguredBot(t *testing.T) {
	custom := "KubeStellar-Console[bot]"
	configured := append(append([]string{}, config.KnownBotAuthors...), custom)
	am := config.AutoMergeConfig{TrustedBotAuthors: configured}
	c := newTrustedBotEngine(t, am.TrustedBotAuthorSet)
	for _, login := range configured {
		for _, spelled := range []string{login, strings.ToUpper(login)} {
			lane, ok := c.sweepLaneForAuthor(spelled)
			if !ok || lane != laneTrustedBot {
				t.Errorf("sweepLaneForAuthor(%q) = (%q, %v), want (%q, true)", spelled, lane, ok, laneTrustedBot)
			}
			pr := selfAuthoredListPR(func(pr *gh.PullRequest) { pr.User = &gh.User{Login: gh.Ptr(spelled)} })
			if got := c.prefilterSelfAuthoredPR(pr); got != "" {
				t.Errorf("prefilterSelfAuthoredPR(%q) = %q, want admitted", spelled, got)
			}
			if got := c.prefilterContributorLanePR(pr, ContributorLanePolicy{Enabled: true}); got != trustedBotLaneSkipReason {
				t.Errorf("prefilterContributorLanePR(%q) = %q, want %q", spelled, got, trustedBotLaneSkipReason)
			}
			if got := c.selfAuthoredSweepTier(pr); got != laneTrustedBot {
				t.Errorf("selfAuthoredSweepTier(%q) = %q, want %q", spelled, got, laneTrustedBot)
			}
		}
	}
	// The App's own PR keeps the historical contributor-lane hand-off reason.
	appPR := selfAuthoredListPR(nil)
	if got := c.prefilterContributorLanePR(appPR, ContributorLanePolicy{Enabled: true}); got != contributorLaneReasonAppAuthored {
		t.Errorf("App PR contributor-lane prefilter = %q, want %q", got, contributorLaneReasonAppAuthored)
	}
	// A bot the operator did not enable is nobody's: not the trusted-bot lane's.
	if lane, ok := c.sweepLaneForAuthor("mergify[bot]"); ok {
		t.Errorf("unconfigured bot admitted to lane %q", lane)
	}
}

// The Settings toggles are read through the live config on every call: a
// reload that adds or removes a login takes effect without a restart.
func TestTrustedBotLaneReadsLiveConfig(t *testing.T) {
	cfg := &config.Config{}
	c := newTrustedBotEngine(t, func() map[string]bool { return cfg.AutoMerge.TrustedBotAuthorSet() })
	if lane, ok := c.sweepLaneForAuthor("renovate[bot]"); ok {
		t.Fatalf("renovate admitted before being enabled: lane %q", lane)
	}
	cfg.AutoMerge.TrustedBotAuthors = []string{"dependabot[bot]", "renovate[bot]"}
	if lane, ok := c.sweepLaneForAuthor("renovate[bot]"); !ok || lane != laneTrustedBot {
		t.Fatalf("renovate not admitted after enabling: (%q, %v)", lane, ok)
	}
	cfg.AutoMerge.TrustedBotAuthors = []string{}
	if _, ok := c.sweepLaneForAuthor("dependabot[bot]"); ok {
		t.Fatal("explicit empty list must disable the lane")
	}
}

func TestTrustedBotSkipsReviewDefault(t *testing.T) {
	if !(config.AutoMergeConfig{}).TrustedBotSkipsReview() {
		t.Fatal("trusted_bot_skip_review must default to true")
	}
	off := false
	if (config.AutoMergeConfig{TrustedBotSkipReview: &off}).TrustedBotSkipsReview() {
		t.Fatal("trusted_bot_skip_review: false must be honored")
	}
}

// TestTrustedBotLaneVerdictAdvisory: a requires_human / decision-requested /
// changes_requested swarm verdict on a trusted bot's PR is advisory, so it
// neither routes to 085 nor blocks; the self-authored lane and an operator
// who turned the knob off keep the verdict as-is.
func TestTrustedBotLaneVerdictAdvisory(t *testing.T) {
	requiresHuman := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionQuestion: "major bump of yaml action; is this wanted?"}
	decision := ContributorLaneVerdict{Configured: true, Verdict: "approve", DecisionRequested: true, DecisionFinding: "risk-class"}
	changes := ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}
	approved := ContributorLaneVerdict{Configured: true, Verdict: "approve", Approved: true}
	answered := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionAnswered: true, DecisionAnsweredBy: "alice"}

	var logs bytes.Buffer
	c := newTrustedBotEngine(t, func() map[string]bool { return map[string]bool{"dependabot[bot]": true} })
	c.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	for _, tc := range []struct {
		name     string
		lane     string
		skip     func() bool
		in       ContributorLaneVerdict
		advisory bool
	}{
		{name: "requires_human on trusted-bot", lane: laneTrustedBot, in: requiresHuman, advisory: true},
		{name: "decision requested on trusted-bot", lane: laneTrustedBot, in: decision, advisory: true},
		{name: "changes_requested on trusted-bot", lane: laneTrustedBot, in: changes, advisory: true},
		{name: "nil knob defaults on", lane: laneTrustedBot, skip: nil, in: requiresHuman, advisory: true},
		{name: "approved stays approved", lane: laneTrustedBot, in: approved},
		{name: "answered verdict left alone", lane: laneTrustedBot, in: answered},
		{name: "knob off keeps the hold", lane: laneTrustedBot, skip: func() bool { return false }, in: requiresHuman},
		{name: "self-authored lane unaffected", lane: "self-authored", in: requiresHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.trustedBotSkipReview = tc.skip
			if tc.skip == nil && tc.name != "nil knob defaults on" {
				c.trustedBotSkipReview = func() bool { return true }
			}
			logs.Reset()
			got := c.trustedBotLaneVerdict("acme/widget", 7, "sha7", tc.lane, tc.in)
			d := stageDetail{Verdict: got, VerdictKnown: true}
			if tc.advisory {
				if verdictNeedsHuman(d) || verdictBlocks(d) || got.Verdict != trustedBotAdvisoryVerdict || got.DecisionRequested {
					t.Fatalf("verdict not advisory: %+v", got)
				}
				if !strings.Contains(logs.String(), "review verdict advisory") || !strings.Contains(logs.String(), `"tier":"trusted-bot"`) {
					t.Fatalf("downgrade not logged with tier: %s", logs.String())
				}
				return
			}
			if got.Verdict != tc.in.Verdict || got.DecisionRequested != tc.in.DecisionRequested || got.Approved != tc.in.Approved {
				t.Fatalf("verdict changed: got %+v want %+v", got, tc.in)
			}
			if logs.Len() != 0 {
				t.Fatalf("unexpected log: %s", logs.String())
			}
		})
	}
}

// TestTrustedBotLaneReviewGateAdvisory drives the whole self-authored path
// for a dependabot PR under an enforcing review gate whose swarm verdict is
// requires_human (the live kubestellar/docs#7562 shape): the PR merges, the
// evaluation is recorded unenforced, and the stage tracker never holds a
// requires_human verdict. With trusted_bot_skip_review off the gate holds.
func TestTrustedBotLaneReviewGateAdvisory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		skip     bool
		wantHold bool
	}{
		{name: "skip review (default)", skip: true},
		{name: "skip review off", skip: false, wantHold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			fx := selfSweepFixture{author: "dependabot[bot]", mergeApplied: true, mergeCalls: &merges}
			api := newSelfSweepGuardAPI(t, fx)
			defer api.Close()
			var rec reviewGateRecords
			c := newReviewGateSweepClient(api.URL, ReviewGate{
				Mode:    func() string { return ReviewGateModeEnforce },
				Verdict: func(string, int, string) ReviewGateVerdict { return reviewGateRequiresHuman },
			}, &rec)
			c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
			skip := tc.skip
			c.trustedBotSkipReview = func() bool { return skip }
			c.stageTracker = NewStageTracker()
			c.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }
			c.contributorLaneVerdict = func(string, int, string) ContributorLaneVerdict {
				return ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionQuestion: "major bump"}
			}
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil {
				t.Fatalf("trySweepSelfAuthoredPR error: %v", err)
			}
			noted := c.stageTracker.details[stageKey("acme", "widget", 7)]
			if tc.wantHold {
				if reason != autoMergeReasonReviewGatePrefix+ReviewGateOutcomeRequiresHuman || merges != 0 {
					t.Fatalf("reason = %q merges = %d; want review-gate hold", reason, merges)
				}
				if !verdictNeedsHuman(noted) {
					t.Fatalf("stage verdict should still need a human when the knob is off: %+v", noted.Verdict)
				}
				return
			}
			if reason != "" || merges != 1 {
				t.Fatalf("reason = %q merges = %d; want merge", reason, merges)
			}
			if verdictNeedsHuman(noted) {
				t.Fatalf("stage tracker holds a requires_human verdict for a trusted bot: %+v", noted.Verdict)
			}
			if len(rec.evals) != 1 || rec.evals[0].Lane != laneTrustedBot || rec.evals[0].Enforced || rec.evals[0].Outcome != ReviewGateOutcomeRequiresHuman {
				t.Fatalf("evaluations = %+v; want one unenforced trusted-bot evaluation recording requires-human", rec.evals)
			}
		})
	}
}

// TestTrustedBotLaneReviewVerdictHoldAdvisory: the review-verdict hold
// ledger (#12374) says requires_human (or reject) for a PR authored by a
// configured trusted bot and no person has released it. With
// trusted_bot_skip_review on (default) the trusted-bot lane logs the hold
// as advisory and merges; with it off the hold stands, and the App's own
// PR is held regardless.
func TestTrustedBotLaneReviewVerdictHoldAdvisory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		author     string
		skip       bool
		verdict    string
		wantReason string
	}{
		{name: "trusted bot, requires_human, default", author: "dependabot[bot]", skip: true, verdict: "requires_human"},
		{name: "trusted bot, reject, default", author: "github-actions[bot]", skip: true, verdict: "reject"},
		{name: "trusted bot, knob off", author: "dependabot[bot]", skip: false, verdict: "requires_human", wantReason: autoMergeReasonReviewRequiresHuman},
		{name: "app PR unaffected by the knob", author: "", skip: true, verdict: "requires_human", wantReason: autoMergeReasonReviewRequiresHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{author: tc.author, mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			srv := newReviewHoldFront(t, api.URL, &reviewHoldFront{})
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.trustedBotAuthors = func() map[string]bool {
				return map[string]bool{"dependabot[bot]": true, "github-actions[bot]": true}
			}
			skip := tc.skip
			c.trustedBotSkipReview = func() bool { return skip }
			c.reviewVerdictHold = verdictHoldFor("acme/widget", ReviewVerdictHold{Held: true, Verdict: tc.verdict, HeadSHA: "sha7", RecordedAt: reviewHoldVerdictAt})
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			wantMerges := 0
			if tc.wantReason == "" {
				wantMerges = 1
			}
			if merges != wantMerges {
				t.Fatalf("merges = %d, want %d", merges, wantMerges)
			}
		})
	}
}

// TestRecheckRequiredCheckNeverReported: auto_merge.required_checks names
// "build-gate" but this repo never produces it. With other checks reporting
// green the head is gated on those; with one red it blocks on that check;
// with nothing else reporting the hold names the absent check. Only the
// config-sourced required set qualifies, and only on the trusted-bot lane.
func TestRecheckRequiredCheckNeverReported(t *testing.T) {
	type check struct{ name, status, conclusion string }
	for _, tc := range []struct {
		name       string
		author     string
		fromConfig bool
		observed   []check
		wantGreen  bool
		wantReason string
	}{
		{name: "other checks green", author: "dependabot[bot]", fromConfig: true, observed: []check{{"verify", "completed", "success"}, {"dco", "completed", "success"}}, wantGreen: true},
		{name: "another check red", author: "dependabot[bot]", fromConfig: true, observed: []check{{"verify", "completed", "failure"}}, wantReason: "required-check-failing:verify"},
		{name: "another check pending", author: "dependabot[bot]", fromConfig: true, observed: []check{{"verify", "in_progress", ""}}, wantReason: "check-pending"},
		{name: "nothing else reports", author: "dependabot[bot]", fromConfig: true, wantReason: requiredCheckNeverReportedPrefix + "build-gate"},
		{name: "protection-sourced set is not second-guessed", author: "dependabot[bot]", observed: []check{{"verify", "completed", "success"}}, wantReason: "required-check-missing:build-gate"},
		{name: "self-authored lane keeps waiting", author: testHiveAppBotLogin, fromConfig: true, observed: []check{{"verify", "completed", "success"}}, wantReason: "required-check-missing:build-gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main/protection/required_status_checks":
					http.Error(w, "forbidden", http.StatusForbidden)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main":
					http.Error(w, "forbidden", http.StatusForbidden)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/rules/branches/main":
					json.NewEncoder(w).Encode([]any{})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					runs := []map[string]string{}
					for _, c := range tc.observed {
						runs = append(runs, map[string]string{"name": c.name, "status": c.status, "conclusion": c.conclusion})
					}
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			var logs bytes.Buffer
			c := newAutoMergeSweepClient(api.URL)
			c.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
			c.SetRequiredChecks(map[string]bool{"build-gate": true})
			observed := map[string]bool{}
			for _, o := range tc.observed {
				observed[o.name] = true
			}
			ci := hgithub.CommitCIState{RequiredFromConfig: tc.fromConfig, MissingRequired: []string{"build-gate"}, Observed: observed, Reason: "required-check-missing:build-gate"}
			pr := &gh.PullRequest{User: &gh.User{Login: gh.Ptr(tc.author)}}
			green, reason, err := c.recheckRequiredCheckNeverReported(context.Background(), "acme/widget", "acme", "widget", "main", pr, 7, "sha", time.Time{}, ci, ci.Reason)
			if err != nil {
				t.Fatalf("recheck error: %v", err)
			}
			if green != tc.wantGreen || reason != tc.wantReason {
				t.Fatalf("recheck = (%v, %q), want (%v, %q)", green, reason, tc.wantGreen, tc.wantReason)
			}
			warned := strings.Contains(logs.String(), `required check \"build-gate\" never reported on acme/widget`)
			if wantWarn := tc.wantGreen || strings.HasPrefix(tc.wantReason, requiredCheckNeverReportedPrefix); warned != wantWarn {
				t.Fatalf("operator warning logged = %v, want %v: %s", warned, wantWarn, logs.String())
			}
		})
	}
}

// The never-reported warning is one line per repo per check per hour.
func TestWarnRequiredCheckNeverReportedOncePerHour(t *testing.T) {
	var logs bytes.Buffer
	c := newAutoMergeSweepClient("http://127.0.0.1:0")
	c.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	count := func() int { return strings.Count(logs.String(), "never reported on acme/widget") }
	c.warnRequiredCheckNeverReported("acme", "widget", "build-gate", "x")
	c.warnRequiredCheckNeverReported("acme", "widget", "build-gate", "x")
	if count() != 1 {
		t.Fatalf("warned %d times within the hour, want 1", count())
	}
	c.warnRequiredCheckNeverReported("acme", "widget", "other", "x")
	if count() != 2 {
		t.Fatalf("a different check must warn on its own: %d", count())
	}
	now = now.Add(requiredCheckNeverReportedWarnEvery)
	c.warnRequiredCheckNeverReported("acme", "widget", "build-gate", "x")
	if count() != 3 {
		t.Fatalf("after an hour the warning must repeat: %d", count())
	}
}

// The reason is a terminal hold (logged as "automerge hold", not re-polled
// as a skip) and the stage engine derives a named human gate from it.
func TestRequiredCheckNeverReportedIsTerminalHumanGate(t *testing.T) {
	reason := requiredCheckNeverReportedPrefix + "build-gate"
	if !isTerminalRequiredCheckHold(reason) {
		t.Fatal("required-check-never-reported must be a terminal hold")
	}
	if prstage.RequiredCheckNeverReportedPrefix != requiredCheckNeverReportedPrefix {
		t.Fatal("prefix drifted from prstage")
	}
}
