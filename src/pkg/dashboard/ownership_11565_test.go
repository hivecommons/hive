package dashboard

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

var ownershipNow11565 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func ownershipEnv11565(level int) ownershipEnv {
	env := ownershipEnv{
		now:                   ownershipNow11565,
		hiveLevel:             level,
		staleDays:             defaultStaleDays,
		sentinelLabel:         config.DefaultSentinelLabel,
		reviewerLaneAvailable: func(string) bool { return true },
		escalatedAt:           func(string, int) time.Time { return ownershipNow11565 },
	}
	// Every lane runs in its launched mode; push rights come from the lane's
	// default mode at the repository's level, as in newOwnershipEnv.
	env.laneCanOpenPR = func(lane, repo string) bool {
		return agent.DefaultAgentMode(lane, env.levelFor(repo)).CanPush()
	}
	return env
}

func freshIssue11565(labels ...string) issueOwnershipInput {
	return issueOwnershipInput{repo: "o/r", number: 1, labels: labels, actionable: true, band: "ready", createdAt: ownershipNow11565.Add(-time.Hour), updatedAt: ownershipNow11565.Add(-time.Hour)}
}

func nonActionable11565(bucket, reason string, labels ...string) issueOwnershipInput {
	return issueOwnershipInput{repo: "o/r", number: 1, labels: labels, bucket: bucket, bucketReason: reason, createdAt: ownershipNow11565.Add(-time.Hour), updatedAt: ownershipNow11565.Add(-time.Hour)}
}

func hivePR11565(labels ...string) github.PullRequest {
	return github.PullRequest{Repo: "o/r", Number: 2, HiveAgent: "scanner", Labels: labels, CreatedAt: ownershipNow11565.Add(-time.Hour), UpdatedAt: ownershipNow11565.Add(-time.Hour)}
}

func personPR11565(labels ...string) github.PullRequest {
	return github.PullRequest{Repo: "o/r", Number: 3, Author: "alice", Labels: labels, CreatedAt: ownershipNow11565.Add(-time.Hour), UpdatedAt: ownershipNow11565.Add(-time.Hour)}
}

// want11565 is the expected identity for one level: "" means not your turn.
type want11565 struct{ verb, code string }

var notYours11565 = want11565{}

func checkOwnership11565(t *testing.T, got Ownership, want want11565) {
	t.Helper()
	if want == notYours11565 {
		if got.YourTurn || got.Verb != "" || got.ReasonCode != "" {
			t.Fatalf("got %+v, want not your turn", got)
		}
		return
	}
	if !got.YourTurn || got.Verb != want.verb || got.ReasonCode != want.code || got.Reason == "" {
		t.Fatalf("got %+v, want your turn %s/%s with a reason", got, want.verb, want.code)
	}
}

// TestOwnershipLevelTable11565 checks the "Ownership by level" table of
// #11507 cell by cell for L1–L6. A nil entry marks a "—" cell (the situation
// cannot occur at that level) and is not checked.
func TestOwnershipLevelTable11565(t *testing.T) {
	yours := func(verb, code string) *want11565 { return &want11565{verb, code} }
	not := &notYours11565
	all := func(w *want11565) [6]*want11565 { return [6]*want11565{w, w, w, w, w, w} }
	tests := []struct {
		name   string
		decide func(env ownershipEnv) Ownership
		levels [6]*want11565
	}{
		{
			name:   "unclaimed issue filed by a person",
			decide: func(env ownershipEnv) Ownership { return decideIssueOwnership(freshIssue11565(), env) },
			levels: all(not),
		},
		{
			name: "agent-filed unacknowledged issue, default self-authorization hold",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(freshIssue11565("agent/strategist"), env)
			},
			levels: [6]*want11565{nil, nil, yours(OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged), yours(OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged), yours(OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged), not},
		},
		{
			name: "agent-filed unacknowledged issue, hold configured on",
			decide: func(env ownershipEnv) Ownership {
				env.selfAuthorizationHold = func(string) bool { return true }
				return decideIssueOwnership(freshIssue11565("agent/strategist"), env)
			},
			levels: [6]*want11565{nil, nil, yours(OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged), yours(OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged), yours(OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged), yours(OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged)},
		},
		{
			name: "untrusted reporter awaiting admission",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(nonActionable11565(repoWorkBucketReporterTriage, "Awaiting maintainer triage label"), env)
			},
			levels: all(yours(OwnershipVerbAdmit, OwnershipReasonUntrustedReporter)),
		},
		{
			name: "parked needs-decision",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(nonActionable11565(repoWorkBucketFiltered, "needs-decision", "needs-decision"), env)
			},
			levels: all(yours(OwnershipVerbDecide, OwnershipReasonParkedNeedsDecision)),
		},
		{
			name: "parked needs-direction",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(nonActionable11565(repoWorkBucketFiltered, "needs-direction", "needs-direction"), env)
			},
			levels: all(yours(OwnershipVerbSteer, OwnershipReasonParkedNeedsDirection)),
		},
		{
			name: "parked needs-human",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(nonActionable11565(repoWorkBucketFiltered, "needs-human", "needs-human"), env)
			},
			levels: all(yours(OwnershipVerbReview, OwnershipReasonParkedNeedsHuman)),
		},
		{
			name: "parked needs-spec under a renamed label",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(nonActionable11565(repoWorkBucketFiltered, "needs-spec", "wants-a-spec"), env)
			},
			levels: all(yours(OwnershipVerbSpec, OwnershipReasonParkedNeedsSpec)),
		},
		{
			name: "agent PR held by the level gate",
			decide: func(env ownershipEnv) Ownership {
				pr := hivePR11565("hold")
				pr.HoldSources = []string{github.HoldSourceLevelGate}
				return decidePROwnership(pr, true, env)
			},
			levels: [6]*want11565{nil, nil, yours(OwnershipVerbReview, OwnershipReasonLevelGateHold), yours(OwnershipVerbReview, OwnershipReasonLevelGateHold), yours(OwnershipVerbReview, OwnershipReasonLevelGateHold), yours(OwnershipVerbReview, OwnershipReasonLevelGateHold)},
		},
		{
			name: "PR with a hold whose sources include person",
			decide: func(env ownershipEnv) Ownership {
				pr := personPR11565("hold")
				pr.HoldSources = []string{github.HoldSourcePerson, github.HoldSourceLevelGate}
				return decidePROwnership(pr, true, env)
			},
			levels: all(not),
		},
		{
			name:   "hive PR with needs-human, adjudicator has not passed, just escalated",
			decide: func(env ownershipEnv) Ownership { return decidePROwnership(hivePR11565("needs-human"), false, env) },
			levels: [6]*want11565{nil, nil, yours(OwnershipVerbReview, OwnershipReasonHivePRNeedsHuman), yours(OwnershipVerbReview, OwnershipReasonHivePRNeedsHuman), not, not},
		},
		{
			name: "hive PR with needs-human after the adjudicator wait",
			decide: func(env ownershipEnv) Ownership {
				env.escalatedAt = func(string, int) time.Time { return ownershipNow11565.Add(-ownershipAdjudicatorWait) }
				return decidePROwnership(hivePR11565("needs-human"), false, env)
			},
			levels: [6]*want11565{nil, nil, yours(OwnershipVerbReview, OwnershipReasonHivePRNeedsHuman), yours(OwnershipVerbReview, OwnershipReasonHivePRNeedsHuman), yours(OwnershipVerbReview, OwnershipReasonAdjudicatorWaitElapsed), yours(OwnershipVerbReview, OwnershipReasonAdjudicatorWaitElapsed)},
		},
		{
			name: "hive PR the adjudicator recommended closing",
			decide: func(env ownershipEnv) Ownership {
				return decidePROwnership(hivePR11565("needs-human", "reviewer-recommend-close"), false, env)
			},
			levels: [6]*want11565{nil, nil, nil, nil, yours(OwnershipVerbClose, OwnershipReasonAdjudicatorRecommendedClose), yours(OwnershipVerbClose, OwnershipReasonAdjudicatorRecommendedClose)},
		},
		{
			name: "hive PR re-escalated after an adjudicator pass",
			decide: func(env ownershipEnv) Ownership {
				return decidePROwnership(hivePR11565("needs-human", "reviewer-passed"), false, env)
			},
			levels: [6]*want11565{nil, nil, nil, nil, yours(OwnershipVerbReview, OwnershipReasonAdjudicatorReescalated), yours(OwnershipVerbReview, OwnershipReasonAdjudicatorReescalated)},
		},
		{
			name:   "person's PR with needs-human",
			decide: func(env ownershipEnv) Ownership { return decidePROwnership(personPR11565("needs-human"), false, env) },
			levels: all(yours(OwnershipVerbReview, OwnershipReasonPersonPRNeedsHuman)),
		},
		{
			name: "sentinel-flagged PR",
			decide: func(env ownershipEnv) Ownership {
				return decidePROwnership(personPR11565("sentinel-alert"), false, env)
			},
			levels: all(yours(OwnershipVerbInspect, OwnershipReasonSentinelFlagged)),
		},
		{
			name: "issue Hive verified as done but could not close",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(freshIssue11565("hive/already-done"), env)
			},
			levels: all(yours(OwnershipVerbClose, OwnershipReasonVerifiedDone)),
		},
		{
			name: "issue covered by an open PR",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(nonActionable11565(repoWorkBucketClaimedByPR, "Open hive-authored PR #9 already claims this issue", "hive/covered-by-pr"), env)
			},
			levels: all(not),
		},
		{
			name: "blocked, Epic, 2-discussing and needs-triage stay in the agent queue",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(freshIssue11565("blocked", "Epic", "2-discussing", "needs-triage"), env)
			},
			levels: all(not),
		},
		{
			name: "needs-reporter-confirmation within the stale period",
			decide: func(env ownershipEnv) Ownership {
				return decideIssueOwnership(nonActionable11565(repoWorkBucketFiltered, "needs-reporter-confirmation", "needs-reporter-confirmation"), env)
			},
			levels: all(not),
		},
		{
			name: "needs-reporter-confirmation after the stale period",
			decide: func(env ownershipEnv) Ownership {
				in := nonActionable11565(repoWorkBucketFiltered, "needs-reporter-confirmation", "needs-reporter-confirmation")
				in.createdAt = ownershipNow11565.AddDate(0, 0, -30)
				in.updatedAt = ownershipNow11565.AddDate(0, 0, -20)
				return decideIssueOwnership(in, env)
			},
			levels: all(yours(OwnershipVerbClose, OwnershipReasonReporterConfirmationStale)),
		},
	}
	for _, tt := range tests {
		for i, want := range tt.levels {
			if want == nil {
				continue
			}
			level := i + 1
			t.Run(tt.name+"/L"+string(rune('0'+level)), func(t *testing.T) {
				checkOwnership11565(t, tt.decide(ownershipEnv11565(level)), *want)
			})
		}
	}
}

func TestOwnershipPrecedence11565(t *testing.T) {
	env := ownershipEnv11565(5)
	tests := []struct {
		name string
		got  Ownership
		want want11565
	}{
		{
			name: "person hold beats sentinel",
			got: func() Ownership {
				pr := hivePR11565("hold", "sentinel-alert")
				pr.HoldSources = []string{github.HoldSourcePerson}
				return decidePROwnership(pr, true, env)
			}(),
			want: notYours11565,
		},
		{
			name: "sentinel beats a level-gate hold",
			got: func() Ownership {
				pr := hivePR11565("hold", "sentinel-alert")
				pr.HoldSources = []string{github.HoldSourceLevelGate}
				return decidePROwnership(pr, true, env)
			}(),
			want: want11565{OwnershipVerbInspect, OwnershipReasonSentinelFlagged},
		},
		{
			name: "covered by an open PR beats a parking label",
			got:  decideIssueOwnership(nonActionable11565(repoWorkBucketClaimedByPR, "Open external PR #4 covers this issue", "needs-human"), env),
			want: notYours11565,
		},
		{
			name: "needs_human is checked before needs_decision",
			got: func() Ownership {
				in := freshIssue11565("needs-decision", "needs-human")
				in.held = true
				in.holdSources = []string{github.HoldSourceLevelGate}
				return decideIssueOwnership(in, env)
			}(),
			want: want11565{OwnershipVerbReview, OwnershipReasonParkedNeedsHuman},
		},
		{
			name: "parked beats agent-filed",
			got:  decideIssueOwnership(nonActionable11565(repoWorkBucketFiltered, "needs-spec", "needs-spec", "agent/architect"), env),
			want: want11565{OwnershipVerbSpec, OwnershipReasonParkedNeedsSpec},
		},
		{
			name: "reporter confirmation beats verified done",
			got:  decideIssueOwnership(nonActionable11565(repoWorkBucketFiltered, "needs-reporter-confirmation", "needs-reporter-confirmation", "hive/already-done"), env),
			want: notYours11565,
		},
		{
			name: "verified done beats agent-filed",
			got:  decideIssueOwnership(freshIssue11565("agent/scanner", "hive/already-done"), env),
			want: want11565{OwnershipVerbClose, OwnershipReasonVerifiedDone},
		},
		{
			name: "actionable likely-done whose lane opens PRs is not yours",
			got:  decideIssueOwnership(inLane11592(freshIssue11565("hive/likely-done"), "quality"), env),
			want: notYours11565,
		},
		{
			name: "held likely-done is verified done",
			got: func() Ownership {
				in := freshIssue11565("hive/likely-done")
				in.held = true
				in.holdSources = []string{github.HoldSourceLevelGate}
				return decideIssueOwnership(in, env)
			}(),
			want: want11565{OwnershipVerbClose, OwnershipReasonVerifiedDone},
		},
		{
			name: "likely-done routed to a lane that cannot open PRs is verified done",
			got:  decideIssueOwnership(inLane11592(freshIssue11565("hive/likely-done"), "supervisor"), env),
			want: want11565{OwnershipVerbClose, OwnershipReasonVerifiedDone},
		},
		{
			name: "covered-by-pr label with no open PR is verified done",
			got:  decideIssueOwnership(freshIssue11565("hive/covered-by-pr"), env),
			want: want11565{OwnershipVerbClose, OwnershipReasonVerifiedDone},
		},
		{
			name: "acknowledged agent proposal is not yours",
			got:  decideIssueOwnership(freshIssue11565("agent/scanner", "approved-direction"), env),
			want: notYours11565,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { checkOwnership11565(t, tt.got, tt.want) })
	}
}

func TestOwnershipHoldSources11565(t *testing.T) {
	tests := []struct {
		name        string
		sources     []string
		want        want11565
		releaseHold bool
	}{
		{"level gate only offers release", []string{github.HoldSourceLevelGate}, want11565{OwnershipVerbReview, OwnershipReasonLevelGateHold}, true},
		{"outreach only", []string{github.HoldSourceOutreach}, want11565{OwnershipVerbReview, OwnershipReasonOutreachHold}, false},
		{"level gate and reporter trust", []string{github.HoldSourceLevelGate, github.HoldSourceReporterTrust}, want11565{OwnershipVerbReview, OwnershipReasonPolicyHold}, false},
		{"human merge path", []string{github.HoldSourceHumanMergePath}, want11565{OwnershipVerbReview, OwnershipReasonPolicyHold}, false},
		{"unknown only", []string{github.HoldSourceUnknown}, want11565{OwnershipVerbReview, OwnershipReasonHoldSourceUnknown}, false},
		{"level gate and unknown", []string{github.HoldSourceLevelGate, github.HoldSourceUnknown}, want11565{OwnershipVerbReview, OwnershipReasonHoldSourceUnknown}, false},
		{"person", []string{github.HoldSourcePerson, github.HoldSourceUnknown}, notYours11565, false},
		{"dashboard pause", []string{github.HoldSourceDashboardPause}, notYours11565, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := hivePR11565("hold")
			pr.HoldSources = tt.sources
			pr.UpdatedAt = ownershipNow11565.AddDate(0, 0, -60)
			got := decidePROwnership(pr, true, ownershipEnv11565(5))
			checkOwnership11565(t, got, tt.want)
			if (got.Action == OwnershipActionReleaseHold) != tt.releaseHold {
				t.Fatalf("action = %q, want release hold %v", got.Action, tt.releaseHold)
			}
			if tt.want.code == OwnershipReasonHoldSourceUnknown && !strings.Contains(got.Reason, "unknown") {
				t.Fatalf("reason %q does not say the source is unknown", got.Reason)
			}
		})
	}

	t.Run("held issue Hive's record does not attribute is a person's hold, also after the stale period", func(t *testing.T) {
		in := freshIssue11565()
		in.held = true
		in.holdSources = []string{github.HoldSourcePerson}
		in.updatedAt = ownershipNow11565.AddDate(0, 0, -60)
		in.createdAt = in.updatedAt
		checkOwnership11565(t, decideIssueOwnership(in, ownershipEnv11565(5)), notYours11565)
	})
	t.Run("sentinel-only held issue carries no hold source and reaches the sentinel rule", func(t *testing.T) {
		in := freshIssue11565(config.DefaultSentinelLabel)
		in.held = true
		checkOwnership11565(t, decideIssueOwnership(in, ownershipEnv11565(5)), want11565{OwnershipVerbInspect, OwnershipReasonSentinelFlagged})
	})
	t.Run("sentinel-only held PR carries no hold source and reaches the sentinel rule", func(t *testing.T) {
		pr := hivePR11565(config.DefaultSentinelLabel)
		checkOwnership11565(t, decidePROwnership(pr, true, ownershipEnv11565(5)), want11565{OwnershipVerbInspect, OwnershipReasonSentinelFlagged})
	})
	t.Run("dashboard-paused issue is not yours", func(t *testing.T) {
		in := freshIssue11565("needs-human")
		in.held = true
		in.holdSources = []string{github.HoldSourceDashboardPause}
		checkOwnership11565(t, decideIssueOwnership(in, ownershipEnv11565(5)), notYours11565)
	})
}

func TestOwnershipAdjudicatorWait11565(t *testing.T) {
	escalated := ownershipNow11565.Add(-time.Hour)
	env := ownershipEnv11565(6)
	env.escalatedAt = func(string, int) time.Time { return escalated }

	checkOwnership11565(t, decidePROwnership(hivePR11565("needs-human"), false, env), notYours11565)

	later := env
	later.now = escalated.Add(ownershipAdjudicatorWait)
	checkOwnership11565(t, decidePROwnership(hivePR11565("needs-human"), false, later), want11565{OwnershipVerbReview, OwnershipReasonAdjudicatorWaitElapsed})

	unavailable := env
	unavailable.reviewerLaneAvailable = func(string) bool { return false }
	checkOwnership11565(t, decidePROwnership(hivePR11565("needs-human"), false, unavailable), want11565{OwnershipVerbReview, OwnershipReasonReviewerUnavailable})

	unknown := env
	unknown.escalatedAt = nil
	checkOwnership11565(t, decidePROwnership(hivePR11565("needs-human"), false, unknown), notYours11565)
}

func TestReviewerLaneAvailable11565(t *testing.T) {
	cfgWith := func(agents map[string]config.AgentConfig) *config.Config {
		level := 6
		return &config.Config{ACMMLevel: &level, Agents: agents}
	}
	adjudicator := config.AgentConfig{Enabled: true, Role: "reviewer"}
	advisory := config.AgentConfig{Enabled: true, Role: "reviewer", KickTemplate: "review the PR queue"}
	tests := []struct {
		name     string
		cfg      *config.Config
		statuses map[string]*agent.AgentProcess
		want     bool
	}{
		{"scheduled adjudicator", cfgWith(map[string]config.AgentConfig{"adjudicator": adjudicator}), nil, true},
		{"adjudicator absent, advisory reviewer running", cfgWith(map[string]config.AgentConfig{"reviewer": advisory}), nil, false},
		{"adjudicator paused at runtime", cfgWith(map[string]config.AgentConfig{"adjudicator": adjudicator, "reviewer": advisory}), map[string]*agent.AgentProcess{"adjudicator": {Paused: true}}, false},
		{"adjudicator disabled", cfgWith(map[string]config.AgentConfig{"adjudicator": {Role: "reviewer"}}), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewerLaneAvailable(tt.cfg, tt.statuses, "idle", "o/r"); got != tt.want {
				t.Fatalf("reviewerLaneAvailable = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOwnershipSafetyNet11565(t *testing.T) {
	stale := ownershipNow11565.AddDate(0, 0, -20)
	staleIssue := func(labels ...string) issueOwnershipInput {
		in := freshIssue11565(labels...)
		in.createdAt, in.updatedAt = stale, stale
		return in
	}
	stalePR := func(pr github.PullRequest) github.PullRequest {
		pr.CreatedAt, pr.UpdatedAt = stale, stale
		return pr
	}
	safetyNet := want11565{OwnershipVerbReview, OwnershipReasonStaleSafetyNet}
	tests := []struct {
		name string
		got  Ownership
		want want11565
	}{
		{"unclaimed issue at L3", decideIssueOwnership(inLane11592(staleIssue(), "quality"), ownershipEnv11565(3)), safetyNet},
		{"claimed band issue (human assignee) at L5", func() Ownership {
			in := inLane11592(staleIssue(), "quality")
			in.band = "in-progress"
			return decideIssueOwnership(in, ownershipEnv11565(5))
		}(), safetyNet},
		{"issue routed to a lane that cannot open PRs", decideIssueOwnership(inLane11592(staleIssue(), "scanner"), ownershipEnv11565(4)), notYours11565},
		{"issue with no lane", decideIssueOwnership(staleIssue(), ownershipEnv11565(4)), notYours11565},
		{"issue with a lane claim", decideIssueOwnership(inLane11592(staleIssue("hive/claimed-by-scanner"), "quality"), ownershipEnv11565(5)), notYours11565},
		{"actionable likely-done issue", decideIssueOwnership(inLane11592(staleIssue("hive/likely-done"), "quality"), ownershipEnv11565(5)), safetyNet},
		{"issue covered by an open PR", func() Ownership {
			in := nonActionable11565(repoWorkBucketClaimedByPR, "Open external PR #4 covers this issue")
			in.createdAt, in.updatedAt = stale, stale
			return decideIssueOwnership(in, ownershipEnv11565(5))
		}(), notYours11565},
		{"unheld hive PR at L3", decidePROwnership(stalePR(hivePR11565()), false, ownershipEnv11565(3)), safetyNet},
		{"hive PR at L2", decidePROwnership(stalePR(hivePR11565()), false, ownershipEnv11565(2)), notYours11565},
		{"draft hive PR", func() Ownership {
			pr := stalePR(hivePR11565())
			pr.Draft = true
			return decidePROwnership(pr, false, ownershipEnv11565(5))
		}(), notYours11565},
		{"person's PR", decidePROwnership(stalePR(personPR11565()), false, ownershipEnv11565(5)), notYours11565},
		{"person-held PR", func() Ownership {
			pr := stalePR(hivePR11565("hold"))
			pr.HoldSources = []string{github.HoldSourcePerson}
			return decidePROwnership(pr, true, ownershipEnv11565(5))
		}(), notYours11565},
		{"level-held PR keeps its hold reason", func() Ownership {
			pr := stalePR(hivePR11565("hold"))
			pr.HoldSources = []string{github.HoldSourceLevelGate}
			return decidePROwnership(pr, true, ownershipEnv11565(5))
		}(), want11565{OwnershipVerbReview, OwnershipReasonLevelGateHold}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { checkOwnership11565(t, tt.got, tt.want) })
	}

	got := decideIssueOwnership(inLane11592(staleIssue(), "quality"), ownershipEnv11565(3))
	if got.Reason != "untouched for 20 days" {
		t.Fatalf("safety-net reason = %q, want untouched for 20 days", got.Reason)
	}
	shorter := ownershipEnv11565(3)
	shorter.staleDays = 30
	checkOwnership11565(t, decideIssueOwnership(inLane11592(staleIssue(), "quality"), shorter), notYours11565)
}

func TestOwnershipFixedSets11565(t *testing.T) {
	if len(ownershipReasonVerbs) != 21 {
		t.Fatalf("reason codes = %d, want the 20 of #11507 and review_requires_human (#12329)", len(ownershipReasonVerbs))
	}
	verbs := map[string]bool{}
	for _, verb := range ownershipVerbOrder {
		verbs[verb] = true
	}
	if len(verbs) != 8 || ownershipVerbOrder[len(ownershipVerbOrder)-1] != OwnershipVerbClose {
		t.Fatalf("verb order = %v, want 8 verbs ending in CLOSE", ownershipVerbOrder)
	}
	for code, verb := range ownershipReasonVerbs {
		if !verbs[verb] {
			t.Fatalf("reason code %s maps to unknown verb %q", code, verb)
		}
	}
	if len(github.HoldSources) != 11 {
		t.Fatalf("hold sources = %v, want the 11 of #11507", github.HoldSources)
	}
}

func TestDecideRepoOwnershipInboxRank11565(t *testing.T) {
	day := func(n int) time.Time { return ownershipNow11565.AddDate(0, 0, -n) }
	repos := []FrontendRepo{
		{
			Name: "b", Full: "o/b",
			ActionableIssues: []any{github.Issue{Repo: "o/b", Number: 10, Labels: []string{"hive/already-done"}, CreatedAt: day(30), UpdatedAt: day(30)}},
			OpenPrs:          []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/b", Number: 11, Author: "alice", Labels: []string{"needs-human"}, CreatedAt: day(2), UpdatedAt: day(2)}}},
			NonActionableIssues: []any{
				github.RepoWorkIssue{Repo: "o/b", Number: 12, Labels: []string{"needs-decision"}, Bucket: repoWorkBucketFiltered, Reason: "needs-decision", CreatedAt: day(1), UpdatedAt: day(1)},
				github.RepoWorkIssue{Repo: "o/b", Number: 13, Labels: []string{"no-ai"}, Bucket: repoWorkBucketFiltered, Reason: "Exempt label", CreatedAt: day(1), UpdatedAt: day(1)},
			},
		},
		{
			Name: "a", Full: "o/a",
			OpenPrs:             []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/a", Number: 5, Author: "bob", Labels: []string{"needs-human"}, CreatedAt: day(2), UpdatedAt: day(2)}}},
			HeldPrs:             []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/a", Number: 6, HiveAgent: "scanner", Labels: []string{"hold", "sentinel-alert"}, CreatedAt: day(1), UpdatedAt: day(1)}}},
			NonActionableIssues: []any{github.RepoWorkIssue{Repo: "o/a", Number: 7, Bucket: repoWorkBucketFiltered, Reason: "needs-decision", CreatedAt: day(3), UpdatedAt: day(3)}},
		},
	}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))

	rank := func(repo int, kind string, number int) int {
		t.Helper()
		decision, ok := repos[repo].Ownership[ownershipKey(kind, number)]
		if !ok {
			t.Fatalf("no decision for repo %d %s#%d", repo, kind, number)
		}
		return decision.InboxRank
	}
	want := []struct {
		repo   int
		kind   string
		number int
	}{
		{1, ownershipKindPR, 6},     // INSPECT
		{1, ownershipKindIssue, 7},  // DECIDE, least recently active
		{0, ownershipKindIssue, 12}, // DECIDE
		{1, ownershipKindPR, 5},     // REVIEW, tie on activity: repo a first
		{0, ownershipKindPR, 11},    // REVIEW
		{0, ownershipKindIssue, 10}, // CLOSE always last
	}
	for i, w := range want {
		if got := rank(w.repo, w.kind, w.number); got != i+1 {
			t.Fatalf("%s#%d inbox_rank = %d, want %d", w.kind, w.number, got, i+1)
		}
	}
	if got := rank(0, ownershipKindIssue, 13); got != 0 {
		t.Fatalf("not-yours row has inbox_rank %d", got)
	}
}

func TestStatusWithOverviewBandsEmitsOwnership11565(t *testing.T) {
	s := newTestServer()
	status := &StatusPayload{Repos: []FrontendRepo{{
		Name: "r", Full: "o/r",
		ActionableIssues: []any{github.Issue{Repo: "o/r", Number: 1, CreatedAt: ownershipNow11565, UpdatedAt: ownershipNow11565}},
		OpenPrs:          []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 2, Author: "alice", Labels: []string{"needs-human"}, CreatedAt: ownershipNow11565, UpdatedAt: ownershipNow11565}}},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "o/r", Number: 3, Labels: []string{"needs-spec"}, Bucket: repoWorkBucketFiltered, Reason: "needs-spec"},
			github.RepoWorkIssue{Repo: "o/r", Number: 4, Labels: []string{"needs-reporter-confirmation"}, Bucket: repoWorkBucketFiltered, Reason: "needs-reporter-confirmation", CreatedAt: ownershipNow11565, UpdatedAt: ownershipNow11565},
		},
	}}}
	decideRepoOwnership(status.Repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(4))

	payload, err := json.Marshal(s.statusWithOverviewBands(status, ownershipNow11565))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		OverviewBands struct {
			Issues []BandSpec `json:"issues"`
		} `json:"overview_bands"`
		Repos []struct {
			Issues        []map[string]any `json:"actionableIssues"`
			PRs           []map[string]any `json:"openPrs"`
			NonActionable []map[string]any `json:"nonActionableIssues"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	repo := wire.Repos[0]
	ownership := func(row map[string]any) map[string]any {
		t.Helper()
		got, ok := row["ownership"].(map[string]any)
		if !ok {
			t.Fatalf("row %v has no ownership", row)
		}
		return got
	}
	if got := ownership(repo.Issues[0]); got["your_turn"] != false {
		t.Fatalf("unclaimed issue ownership = %v", got)
	}
	if got := ownership(repo.PRs[0]); got["verb"] != OwnershipVerbReview || got["reason_code"] != OwnershipReasonPersonPRNeedsHuman || got["inbox_rank"] != float64(2) {
		t.Fatalf("person PR ownership = %v", got)
	}
	parked := repo.NonActionable[0]
	if got := ownership(parked); got["verb"] != OwnershipVerbSpec || got["inbox_rank"] != float64(1) {
		t.Fatalf("parked issue ownership = %v", got)
	}
	if parked["band"] != "waiting" || parked["bucket"] != repoWorkBucketFiltered {
		t.Fatalf("parked row = %v, want band waiting and its bucket", parked)
	}
	reporter := repo.NonActionable[1]
	if _, ok := reporter["band"]; ok {
		t.Fatalf("reporter-confirmation row has a band: %v", reporter)
	}
	if got := ownership(reporter); got["your_turn"] != false {
		t.Fatalf("reporter-confirmation ownership = %v", got)
	}
	for _, band := range wire.OverviewBands.Issues {
		if band.Key == "waiting" && band.Count != 1 {
			t.Fatalf("waiting band count = %d, want the parked row", band.Count)
		}
	}
}

func TestRecordEscalationFirstSeenSurvivesRestart11565(t *testing.T) {
	dir := t.TempDir()
	oldState, oldLedger := ownershipStatePath, ownershipEscalationLedgerPath
	ownershipStatePath = filepath.Join(dir, "ownership-state.json")
	ownershipEscalationLedgerPath = filepath.Join(dir, "missing-ledger.json")
	t.Cleanup(func() { ownershipStatePath, ownershipEscalationLedgerPath = oldState, oldLedger })

	repos := func(labels ...string) []FrontendRepo {
		return []FrontendRepo{{Name: "r", Full: "o/r", OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 8, HiveAgent: "scanner", Labels: labels}}}}}
	}
	first := ownershipNow11565
	if got := recordEscalationFirstSeen(repos("needs-human"), first)("o/r", 8); !got.Equal(first) {
		t.Fatalf("first refresh escalation time = %v, want %v", got, first)
	}
	// A later refresh — including one after a restart, which reloads the
	// record from disk — keeps the first-seen time.
	if got := recordEscalationFirstSeen(repos("needs-human"), first.Add(3*time.Hour))("o/r", 8); !got.Equal(first) {
		t.Fatalf("later refresh escalation time = %v, want %v", got, first)
	}
	// Removing needs-human forgets it, so a re-escalation starts a new wait.
	if got := recordEscalationFirstSeen(repos(), first.Add(4*time.Hour))("o/r", 8); !got.IsZero() {
		t.Fatalf("escalation time after needs-human removed = %v, want zero", got)
	}
}

func TestDefaultWaitingLabelsMatchBrowser11565(t *testing.T) {
	m := regexp.MustCompile(`(?s)REPO_ISSUE_BAND_DEFAULTS = \{\s*waitingLabels: \[([^\]]*)\]`).FindStringSubmatch(indexHTML(t))
	if m == nil {
		t.Fatal("index.html has no REPO_ISSUE_BAND_DEFAULTS.waitingLabels")
	}
	var browser []string
	for _, part := range strings.Split(m[1], ",") {
		if label := strings.Trim(strings.TrimSpace(part), `'"`); label != "" {
			browser = append(browser, label)
		}
	}
	if strings.Join(browser, ",") != strings.Join(defaultIssueWaitingLabels, ",") {
		t.Fatalf("browser waiting labels %v != server %v", browser, defaultIssueWaitingLabels)
	}
	for _, label := range []string{"blocked", "2-discussing", "Epic", "needs-triage"} {
		info := IssueBand(github.Issue{Repo: "o/r", Number: 1, Labels: []string{label}}, false, config.DashboardIssueBandsConfig{}, ownershipNow11565)
		if info.Band == "waiting" {
			t.Fatalf("%s places an issue in the waiting band by default", label)
		}
	}
}
