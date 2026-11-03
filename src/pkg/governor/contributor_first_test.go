package governor

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// contributorFirstGovernor builds a governor with "quality" marked
// contributor-first (grace = grace) and "scanner" ordinary, on a fixed clock
// the test advances through *now.
func contributorFirstGovernor(t *testing.T, grace time.Duration) (*Governor, *time.Time) {
	t.Helper()
	cfg, agents := standardConfig("quality", "scanner")
	q := agents["quality"]
	q.ContributorFirst = true
	q.ContributorFirstGrace = grace
	agents["quality"] = q
	g := New(cfg, agents, testLogger())
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	return g, &now
}

func coverage(logins ...string) map[string]ContributorCoverage {
	return map[string]ContributorCoverage{"quality": {Contributors: logins}}
}

func TestContributorFirst_CoveredLaneWithholdsKicks(t *testing.T) {
	g, _ := contributorFirstGovernor(t, 5*time.Minute)
	g.SetContributorCoverage(coverage("alice"))

	due := dueSet(g)
	if due["quality"] {
		t.Error("quality is covered by a contributor relay and must not be due")
	}
	if !due["scanner"] {
		t.Error("scanner has no contributor_first and must still be due")
	}

	state := g.GetState()
	cov, ok := state.CoveredLanes["quality"]
	if !ok || len(cov.Contributors) != 1 || cov.Contributors[0] != "alice" {
		t.Fatalf("CoveredLanes = %+v, want quality covered by alice", state.CoveredLanes)
	}
	if cov.Since.IsZero() || !cov.GraceUntil.IsZero() {
		t.Errorf("covered lane must carry Since and no GraceUntil, got %+v", cov)
	}
	// The budget vocabulary stays budget-only: standby dispatch keys on it.
	if len(state.SuppressedLanes) != 0 {
		t.Errorf("a covered lane must not be reported as budget-suppressed, got %v", state.SuppressedLanes)
	}
	if _, reasoned := state.LanePauseReasons["quality"]; reasoned {
		t.Errorf("a covered lane must not carry a lane pause reason, got %v", state.LanePauseReasons)
	}
	history := g.EvalHistory()
	if len(history) == 0 || history[len(history)-1].CoveredLanes["quality"].Contributors[0] != "alice" {
		t.Fatalf("eval history did not publish the covered lane: %+v", history)
	}
}

func TestContributorFirst_FlagOffIgnoresCoverage(t *testing.T) {
	cfg, agents := standardConfig("quality", "scanner")
	g := New(cfg, agents, testLogger())
	g.SetContributorCoverage(coverage("alice"))

	due := dueSet(g)
	if !due["quality"] {
		t.Error("without contributor_first a covered role must still be kicked")
	}
	if len(g.GetState().CoveredLanes) != 0 {
		t.Errorf("CoveredLanes must be empty without the flag, got %+v", g.GetState().CoveredLanes)
	}
}

func TestContributorFirst_GraceThenResume(t *testing.T) {
	g, now := contributorFirstGovernor(t, 5*time.Minute)
	start := *now
	g.SetContributorCoverage(coverage("alice"))
	if dueSet(g)["quality"] {
		t.Fatal("covered: quality must not be due")
	}

	// The relay disappears: the grace starts from the last sighting.
	*now = start.Add(time.Minute)
	g.SetContributorCoverage(nil)
	if dueSet(g)["quality"] {
		t.Error("within grace: quality must still be withheld")
	}
	cov := g.GetState().CoveredLanes["quality"]
	if len(cov.Contributors) != 0 {
		t.Errorf("in grace the lane has no contributors, got %v", cov.Contributors)
	}
	if want := start.Add(5 * time.Minute); !cov.GraceUntil.Equal(want) {
		t.Errorf("GraceUntil = %v, want %v (last sighting + grace)", cov.GraceUntil, want)
	}

	// The relay comes back inside the grace: coverage never ended, so Since
	// is the original start.
	*now = start.Add(3 * time.Minute)
	g.SetContributorCoverage(coverage("alice"))
	if dueSet(g)["quality"] {
		t.Error("relay returned within grace: quality must still be withheld")
	}
	if got := g.GetState().CoveredLanes["quality"].Since; !got.Equal(start) {
		t.Errorf("Since = %v, want the original start %v", got, start)
	}

	// Gone again, and this time the grace elapses.
	*now = start.Add(4 * time.Minute)
	g.SetContributorCoverage(nil)
	*now = start.Add(4*time.Minute + 5*time.Minute + time.Second)
	g.SetContributorCoverage(nil)
	if !dueSet(g)["quality"] {
		t.Error("after the grace elapsed quality must resume its cadence")
	}
	if len(g.GetState().CoveredLanes) != 0 {
		t.Errorf("CoveredLanes must be empty after the grace, got %+v", g.GetState().CoveredLanes)
	}
}

func TestContributorFirst_DefaultGraceWhenUnset(t *testing.T) {
	g, now := contributorFirstGovernor(t, 0)
	start := *now
	g.SetContributorCoverage(coverage("alice"))
	dueSet(g)
	*now = start.Add(time.Second)
	g.SetContributorCoverage(nil)
	if dueSet(g)["quality"] {
		t.Fatal("within the default grace quality must still be withheld")
	}
	cov := g.GetState().CoveredLanes["quality"]
	if want := start.Add(config.DefaultContributorFirstGrace); !cov.GraceUntil.Equal(want) {
		t.Errorf("GraceUntil = %v, want %v (default grace)", cov.GraceUntil, want)
	}
}

func TestContributorFirst_BudgetGateStillWins(t *testing.T) {
	g, _ := contributorFirstGovernor(t, 5*time.Minute)
	g.SetContributorCoverage(coverage("alice"))
	g.SetBudgetLimit(1000)
	g.SeedBudget(1000, nil, nil, time.Now())

	due := dueSet(g)
	if due["quality"] || due["scanner"] {
		t.Errorf("exhausted budget suppresses everything, got due=%v", due)
	}
	state := g.GetState()
	// A lane that is BOTH out of budget and covered still reads as paused for
	// budget — that is what standby dispatch keys on, and nothing here may
	// change its meaning.
	if state.LanePauseReasons["quality"] != budgetExhaustedPauseReason {
		t.Errorf("quality pause reason = %q, want %q", state.LanePauseReasons["quality"], budgetExhaustedPauseReason)
	}
	found := false
	for _, lane := range state.SuppressedLanes {
		if lane == "quality" {
			found = true
		}
	}
	if !found {
		t.Errorf("quality must still be budget-suppressed, got %v", state.SuppressedLanes)
	}
}

func TestContributorFirst_GatesEventAndResumeKicks(t *testing.T) {
	g, now := contributorFirstGovernor(t, 5*time.Minute)
	g.SetContributorCoverage(coverage("alice"))
	dueSet(g)

	if g.AgentEligibleForCELKick("quality") {
		t.Error("a CEL event kick must not bypass a covered lane")
	}
	if !g.AgentEligibleForCELKick("scanner") {
		t.Error("scanner is not covered and must stay CEL-eligible")
	}
	if ok, reason := g.AllowResumeKickReason("quality"); ok || reason != ResumeRefusalContributorCovering {
		t.Errorf("resume kick: ok=%v reason=%q, want refused with %q", ok, reason, ResumeRefusalContributorCovering)
	}

	// Once the grace has elapsed both gates open again.
	*now = now.Add(time.Hour)
	g.SetContributorCoverage(nil)
	*now = now.Add(time.Hour)
	g.SetContributorCoverage(nil)
	if !g.AgentEligibleForCELKick("quality") {
		t.Error("after the grace a CEL kick must be allowed again")
	}
	if _, reason := g.AllowResumeKickReason("quality"); reason == ResumeRefusalContributorCovering {
		t.Error("after the grace the resume gate must not cite contributor coverage")
	}
}

func TestContributorFirst_BlocksContinuousRekick(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"quality": "continuous"}},
	}}, map[string]config.AgentConfig{
		"quality": {Backend: "copilot", Enabled: true, ContributorFirst: true},
	}, slog.Default())
	g.now = func() time.Time { return now }
	g.Evaluate(0, 0, 0, 0)

	g.SetContributorCoverage(coverage("alice"))
	g.mu.Lock()
	_, _, blocked := g.continuousBlockerLocked("quality")
	g.mu.Unlock()
	if blocked != contributorCoveringPauseReason {
		t.Fatalf("continuous blocker = %q, want %q", blocked, contributorCoveringPauseReason)
	}

	now = now.Add(time.Hour)
	g.SetContributorCoverage(nil)
	now = now.Add(time.Hour)
	g.SetContributorCoverage(nil)
	g.mu.Lock()
	_, _, blocked = g.continuousBlockerLocked("quality")
	g.mu.Unlock()
	if blocked == contributorCoveringPauseReason {
		t.Fatal("after the grace the continuous blocker must not cite contributor coverage")
	}
}

func TestContributorFirst_NormalisesReport(t *testing.T) {
	g, _ := contributorFirstGovernor(t, 5*time.Minute)
	g.SetContributorCoverage(map[string]ContributorCoverage{
		" Quality ":  {Contributors: []string{" bob", "alice", "alice", ""}},
		"":           {Contributors: []string{"nobody"}},
		"unknown":    {Contributors: []string{"zed"}},
		"emptyLogin": {Contributors: []string{"", "  "}},
	})
	dueSet(g)
	cov := g.GetState().CoveredLanes["quality"]
	if len(cov.Contributors) != 2 || cov.Contributors[0] != "alice" || cov.Contributors[1] != "bob" {
		t.Fatalf("contributors = %v, want [alice bob] (trimmed, de-duplicated, sorted)", cov.Contributors)
	}
	if len(g.GetState().CoveredLanes) != 1 {
		t.Errorf("only quality is a contributor-first agent, got %+v", g.GetState().CoveredLanes)
	}
}

func TestContributorFirst_GetStateIsACopy(t *testing.T) {
	g, _ := contributorFirstGovernor(t, 5*time.Minute)
	g.SetContributorCoverage(coverage("alice"))
	dueSet(g)
	first := g.GetState().CoveredLanes["quality"]
	first.Contributors[0] = "mallory"
	if got := g.GetState().CoveredLanes["quality"].Contributors[0]; got != "alice" {
		t.Fatalf("GetState must return a copy; mutation leaked: %q", got)
	}
}
