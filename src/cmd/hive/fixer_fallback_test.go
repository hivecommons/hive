package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/review"
)

// Only status reads are implemented: fallback selection must never interrupt
// or restart the busy primary, even when its transcript has gone silent.
type fixerFallbackRuntime struct {
	crux.Runtime
	statuses map[string]*crux.Process
}

func (f *fixerFallbackRuntime) IsPaused(name string) bool {
	return f.statuses[name] != nil && f.statuses[name].Paused
}

func (f *fixerFallbackRuntime) GetStatusFast(name string) (*crux.Process, error) {
	if st := f.statuses[name]; st != nil {
		return st, nil
	}
	return nil, fmt.Errorf("unknown agent %s", name)
}

func TestFixerUndeliverable(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		fallbacks []string
		threshold int
		kicks     int
		activity  time.Time
		want      bool
	}{
		{"unset", nil, 0, 10, now.Add(-time.Hour), false},
		{"threshold alone", nil, 1, 10, now.Add(-time.Hour), false},
		{"blank list", []string{" "}, 0, 10, now.Add(-time.Hour), false},
		{"below default", []string{"fixer"}, 0, 2, now, false},
		{"at default", []string{"fixer"}, 0, 3, now, true},
		{"below override", []string{"fixer"}, 5, 4, now, false},
		{"at override", []string{"fixer"}, 5, 5, now, true},
		{"silent busy", []string{"fixer"}, 0, 1, now.Add(-time.Hour), true},
		{"idle silent", []string{"fixer"}, 0, 0, now.Add(-time.Hour), false},
		{"unknown transcript", []string{"fixer"}, 0, 1, time.Time{}, false},
		{"future transcript", []string{"fixer"}, 0, 1, now.Add(time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.ReviewConfig{FixerFallbackAgents: tc.fallbacks, FixerFallbackAfterUndeliverable: tc.threshold}
			st := &crux.Process{KicksUndeliverable: tc.kicks, LastTranscriptActivity: tc.activity}
			st.Config.BusyNoActivityThreshold = time.Hour
			if got := fixerUndeliverable(cfg, st, now); got != tc.want {
				t.Fatalf("fixerUndeliverable = %v, want %v", got, tc.want)
			}
		})
	}
}

func fallbackTestFleet() (*config.Config, *fixerFallbackRuntime) {
	cfg := reviewSwarmConfig()
	cfg.Review.FixerFallbackAgents = []string{" paused ", "disabled", "on-demand", "non-kick", "off-cadence", "unknown", "busy", "backoff", "fixer", "spare"}
	mgr := &fixerFallbackRuntime{statuses: map[string]*crux.Process{
		"scanner": {KicksUndeliverable: 3},
		"paused":  {Paused: true},
		"busy":    {KicksUndeliverable: 1},
		"backoff": {ProviderErrorBackoffUntil: time.Now().Add(time.Hour)},
		"fixer":   {},
		"spare":   {},
	}}
	for _, name := range []string{"paused", "on-demand", "non-kick", "off-cadence", "unknown", "busy", "backoff", "fixer", "spare"} {
		cfg.Agents[name] = config.AgentConfig{Enabled: true}
	}
	ac := cfg.Agents["on-demand"]
	ac.OnDemand = true
	cfg.Agents["on-demand"] = ac
	kickEnabled := false
	cfg.Agents["non-kick"] = config.AgentConfig{Enabled: true, Channels: []config.ChannelConfig{{Type: "kick", Enabled: &kickEnabled}}}
	cfg.Governor.Modes = map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"off-cadence": "paused"}},
	}
	return cfg, mgr
}

func TestReEngagementFallsBackAndReturnsToRecoveredPrimary(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg, mgr := fallbackTestFleet()
	o := escalation.Observation{Repo: "acme/widgets", Number: 7, Agent: "scanner"}
	kicker := &fakeKicker{}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	if !deliverReEngagement(cfg, store, o, "abc", kicker, agentAvailability(cfg, mgr), logger) {
		t.Fatal("repair was not delivered around the busy primary")
	}
	if len(kicker.kicks) != 1 || kicker.kicks[0].agent != "fixer" || !strings.Contains(kicker.kicks[0].message, "FIX-BEFORE-NEW") {
		t.Fatalf("expected first available fallback to receive repair, got %+v", kicker.kicks)
	}
	if !strings.Contains(logs.String(), "routed to fallback fixer") || !strings.Contains(logs.String(), "agent=fixer") {
		t.Fatalf("missing routing diagnostic: %s", logs.String())
	}

	*clock = clock.Add(escalation.MaxReEngageCooldown + time.Minute)
	mgr.statuses["scanner"].KicksUndeliverable = 0
	if !deliverReEngagement(cfg, store, o, "abc", kicker, agentAvailability(cfg, mgr), logger) || kicker.kicks[1].agent != "scanner" {
		t.Fatalf("recovered primary did not regain repair routing: %+v", kicker.kicks)
	}

	*clock = clock.Add(escalation.MaxReEngageCooldown + time.Minute)
	mgr.statuses["scanner"].KicksUndeliverable = 3
	mgr.statuses["fixer"].KicksUndeliverable = 1
	mgr.statuses["spare"].Paused = true
	if deliverReEngagement(cfg, store, o, "abc", kicker, agentAvailability(cfg, mgr), logger) {
		t.Fatal("repair reported delivered while all fixers were unavailable")
	}
	if len(kicker.kicks) != 2 || store.ReEngagements(o.Repo, o.Number) != 2 {
		t.Fatal("undeliverable repair spent budget or sent a kick")
	}
}

func TestReviewFixerFallbackWiring(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kicks     int
		fallbacks bool
		want      string
	}{
		{"unset preserves busy primary", 10, false, "scanner"},
		{"below threshold", 2, true, "scanner"},
		{"fallback", 3, true, "fixer"},
		{"recovered primary", 0, true, "scanner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectReviewPaths(t)
			if err := review.WriteArtifact("", review.Artifact{Items: []review.Aggregate{{
				Repo: "hivecommons/hive", Number: 4321, HeadSHA: reviewTestSHA, Verdict: review.VerdictChangesRequested,
			}}}); err != nil {
				t.Fatal(err)
			}
			cfg, mgr := fallbackTestFleet()
			if !tc.fallbacks {
				cfg.Review.FixerFallbackAgents = nil
			}
			mgr.statuses["scanner"].KicksUndeliverable = tc.kicks
			plan := planReviewDispatch(cfg, actionableWithPR("hive-bot"), mgr, nil, discardLogger())
			if len(plan.FixKicks) != 1 || plan.FixKicks[0].Agent != tc.want {
				t.Fatalf("fix kicks = %+v, want one for %s", plan.FixKicks, tc.want)
			}
			if len(plan.State.Fixes) != 1 || plan.State.Fixes[0].Agent != tc.want || plan.State.Fixes[0].Attempts != 1 {
				t.Fatalf("fallback not reflected in dispatch accounting: %+v", plan.State.Fixes)
			}
			if tc.want == "fixer" {
				// Recovery must not duplicate work already delivered to the
				// fallback. It only changes routing for the next eligible fix.
				state := review.ConfirmDelivered(plan.State, plan.FixKicks, plan.FixKicks)
				if err := review.WriteDispatchState("", state); err != nil {
					t.Fatal(err)
				}
				mgr.statuses["scanner"].KicksUndeliverable = 0
				again := planReviewDispatch(cfg, actionableWithPR("hive-bot"), mgr, nil, discardLogger())
				if len(again.FixKicks) != 0 || len(again.State.Fixes) != 1 || again.State.Fixes[0].Agent != "fixer" {
					t.Fatalf("recovery duplicated the pending fallback fix: %+v", again)
				}
			}
		})
	}
}

func TestReviewFixerFallbackAllBusyRetriesWithoutHumanHold(t *testing.T) {
	redirectReviewPaths(t)
	if err := review.WriteArtifact("", review.Artifact{Items: []review.Aggregate{{
		Repo: "hivecommons/hive", Number: 4321, HeadSHA: reviewTestSHA, Verdict: review.VerdictChangesRequested,
	}}}); err != nil {
		t.Fatal(err)
	}
	cfg, mgr := fallbackTestFleet()
	mgr.statuses["fixer"].KicksUndeliverable = 1
	mgr.statuses["spare"].Paused = true
	plan := planReviewDispatch(cfg, actionableWithPR("hive-bot"), mgr, nil, discardLogger())
	if len(plan.FixKicks) != 0 || len(plan.State.Fixes) != 0 || len(plan.State.Human) != 0 {
		t.Fatalf("busy fleet sent a repair or created a human hold: %+v", plan)
	}
	mgr.statuses["scanner"].KicksUndeliverable = 0
	plan = planReviewDispatch(cfg, actionableWithPR("hive-bot"), mgr, nil, discardLogger())
	if len(plan.FixKicks) != 1 || plan.FixKicks[0].Agent != "scanner" {
		t.Fatalf("repair did not retry after recovery: %+v", plan.FixKicks)
	}
}
