package review

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
)

// hivecommons/hive#15744: the fixer's standing on a head, read back from
// the persisted dispatch state the way #15308 recorded it (six attempts,
// a fix-attempts-exhausted human hold, no pending fix).
func TestFixDispatchStatusFor(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	state := DispatchState{
		FixAttempts: []FixAttempt{
			{Repo: "hivecommons/hive", Number: 15308, HeadSHA: "5b62304aaaaaaaa", Agent: "fixer", Attempts: escalation.MaxReEngagements},
			{Repo: "hivecommons/hive", Number: 15310, HeadSHA: "abc", Agent: "fixer", Attempts: 2},
		},
		Human: []HumanReviewHold{
			{Repo: "hivecommons/hive", Number: 15308, HeadSHA: "5b62304aaaaaaaa", Reason: FixAttemptsExhaustedHumanDecisionReason, UpdatedAt: now},
			{Repo: "hivecommons/hive", Number: 15311, HeadSHA: "def", Reason: "no enabled kick-capable fixer agent is available", UpdatedAt: now},
		},
		Fixes: []PendingFix{{Repo: "hivecommons/hive", Number: 15312, HeadSHA: "fff", Agent: "fixer", Attempts: 1, Dispatched: now}},
	}

	got := FixDispatchStatusFor(state, "hive", 15308, "5b62304")
	if !got.Exhausted() || got.Attempts != escalation.MaxReEngagements || got.Active {
		t.Fatalf("exhausted PR (bare repo, short head) = %+v", got)
	}
	if text := FixDispatchExclusionText(got); text != "fixer gave up after 6 attempts" {
		t.Fatalf("exclusion text = %q", text)
	}
	if got := FixDispatchStatusFor(state, "hivecommons/hive", 15310, "abc"); got.Excluded() || got.Attempts != 2 {
		t.Fatalf("PR with attempts left = %+v, want not excluded", got)
	}
	if got := FixDispatchStatusFor(state, "hivecommons/hive", 15311, "def"); got.Reason != FixDispatchExcludedNoEnabledFixer || !got.Excluded() {
		t.Fatalf("no-fixer hold = %+v", got)
	}
	if got := FixDispatchStatusFor(state, "hivecommons/hive", 15311, "newhead"); got.Excluded() {
		t.Fatalf("hold on an older head must not exclude the new one: %+v", got)
	}
	if got := FixDispatchStatusFor(state, "hivecommons/hive", 15312, "fff"); !got.Active || got.Agent != "fixer" || got.Excluded() {
		t.Fatalf("pending fix = %+v, want active", got)
	}
	if got := FixDispatchStatusFor(DispatchState{}, "hivecommons/hive", 1, "x"); got.Excluded() || got.Active {
		t.Fatalf("unknown PR = %+v, want zero", got)
	}
}
