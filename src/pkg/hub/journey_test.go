package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseTime is a fixed reference so every time-window test is deterministic.
var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// intPtr is a helper for the *int heartbeat signals.
func intPtr(v int) *int { return &v }

// hive builds a RegistryEntry registered `age` before baseTime, with every
// stage satisfied by default. Individual tests then break one thing.
func hive(age time.Duration, mut ...func(*RegistryEntry)) *RegistryEntry {
	h := &RegistryEntry{
		ID:              "hive-1",
		Name:            "acme/widget",
		Org:             "acme",
		RegisteredAt:    baseTime.Add(-age).Format(time.RFC3339),
		ACMMLevel:       maxACMMLevel, // top level → no ACMM nudge unless a test lowers it
		AgentsWithModel: intPtr(1),    // stage 2 satisfied by assignment
	}
	for _, m := range mut {
		m(h)
	}
	return h
}

// ── Stage detection ─────────────────────────────────────────────────────────

func TestGitHubAppInstalled(t *testing.T) {
	tests := []struct {
		name  string
		entry *RegistryEntry
		want  bool
	}{
		{"nil entry is treated as satisfied", nil, true},
		{"clean hive", &RegistryEntry{}, true},
		{"pending install is not satisfied", &RegistryEntry{PendingGitHubAppInstall: true}, false},
		{"app required with a permission issue is not satisfied",
			&RegistryEntry{GitHubAppRequired: true, GitHubAppPermIssue: "missing contents:write"}, false},
		{"app required with no permission issue is satisfied",
			&RegistryEntry{GitHubAppRequired: true}, true},
		{"permission issue without app required is satisfied (app auth not in use)",
			&RegistryEntry{GitHubAppPermIssue: "stale"}, true},
		{"whitespace-only permission issue does not count as a problem",
			&RegistryEntry{GitHubAppRequired: true, GitHubAppPermIssue: "   "}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := githubAppInstalled(tc.entry); got != tc.want {
				t.Errorf("githubAppInstalled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMethodModelAssigned(t *testing.T) {
	tests := []struct {
		name         string
		entry        *RegistryEntry
		wantAssigned bool
		wantKnown    bool
	}{
		{"nil entry is unknown", nil, false, false},
		{"nil pointer means an old spoke, so unknown", &RegistryEntry{}, false, false},
		{"zero agents with a model is a known negative",
			&RegistryEntry{AgentsWithModel: intPtr(0)}, false, true},
		{"one agent with a model is assigned",
			&RegistryEntry{AgentsWithModel: intPtr(1)}, true, true},
		{"many agents with a model is assigned",
			&RegistryEntry{AgentsWithModel: intPtr(9)}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assigned, known := methodModelAssigned(tc.entry)
			if assigned != tc.wantAssigned || known != tc.wantKnown {
				t.Errorf("methodModelAssigned() = (%v,%v), want (%v,%v)",
					assigned, known, tc.wantAssigned, tc.wantKnown)
			}
		})
	}
}

func TestRelayInUse(t *testing.T) {
	tests := []struct {
		name  string
		entry *RegistryEntry
		want  bool
	}{
		{"nil entry", nil, false},
		{"no contributors at all", &RegistryEntry{}, false},
		{"registered contributors count as in use (durable across idle gaps)",
			&RegistryEntry{ContributorCount: 3}, true},
		{"live contributors count as in use",
			&RegistryEntry{ActiveContributors: 1}, true},
		{"a contributor working a task counts even with no counters",
			&RegistryEntry{Leaderboard: []LeaderboardEntry{{CurrentTask: "fix #12"}}}, true},
		{"leaderboard entries with no current task do not count",
			&RegistryEntry{Leaderboard: []LeaderboardEntry{{GitHubUsername: "ada"}}}, false},
		{"whitespace current task does not count",
			&RegistryEntry{Leaderboard: []LeaderboardEntry{{CurrentTask: "  "}}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayInUse(tc.entry); got != tc.want {
				t.Errorf("relayInUse() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNextNudgeStageOrdering verifies the current stage is the FIRST
// unsatisfied one — an earlier unsatisfied stage always wins.
func TestNextNudgeStageOrdering(t *testing.T) {
	// Old enough that all three stages would otherwise have something to say.
	const age = 60 * 24 * time.Hour
	tests := []struct {
		name  string
		entry *RegistryEntry
		want  JourneyStage
	}{
		{
			"all stages unsatisfied yields stage 1",
			hive(age, func(h *RegistryEntry) {
				h.PendingGitHubAppInstall = true
				h.AgentsWithModel = intPtr(0)
				h.ACMMLevel = 2
			}),
			StageGitHubApp,
		},
		{
			"app installed, no method/model yields stage 2",
			hive(age, func(h *RegistryEntry) {
				h.AgentsWithModel = intPtr(0)
				h.ACMMLevel = 2
			}),
			StageMethodModel,
		},
		{
			"stages 1 and 2 satisfied yields stage 3",
			hive(age, func(h *RegistryEntry) { h.ACMMLevel = 2 }),
			StageACMMLevel,
		},
		{
			"everything satisfied at max ACMM yields no stage",
			hive(age),
			StageNone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextNudge(tc.entry, baseTime)
			if got.Stage != tc.want {
				t.Errorf("stage = %v, want %v", got.Stage, tc.want)
			}
		})
	}
}

// ── Escalation timing — boundaries of every window ─────────────────────────

// TestStage1EscalationBoundaries walks the urgent GitHub App ladder, checking
// the exact instant each threshold takes effect and the instant before it.
func TestStage1EscalationBoundaries(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want JourneySeverity
	}{
		{"brand new hive says nothing", 0, SeverityNone},
		{"just before the gentle threshold", stage1GentleAfter - time.Second, SeverityNone},
		{"exactly at the gentle threshold", stage1GentleAfter, SeverityGentle},
		{"just before firm", stage1FirmAfter - time.Second, SeverityGentle},
		{"exactly at firm", stage1FirmAfter, SeverityFirm},
		{"just before the de-provision warning", stage1WarnAfter - time.Second, SeverityFirm},
		{"exactly at the de-provision warning", stage1WarnAfter, SeverityDeprovisionWarning},
		{"well past the warning stays at the warning", stage1WarnAfter * 3, SeverityDeprovisionWarning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := hive(tc.age, func(h *RegistryEntry) { h.PendingGitHubAppInstall = true })
			got := nextNudge(h, baseTime)
			if got.Severity != tc.want {
				t.Errorf("severity at %s = %v, want %v", tc.age, got.Severity, tc.want)
			}
			if tc.want != SeverityNone && got.Stage != StageGitHubApp {
				t.Errorf("stage = %v, want %v", got.Stage, StageGitHubApp)
			}
		})
	}
}

// TestStage2EscalationBoundaries walks the medium-urgency method/model window.
func TestStage2EscalationBoundaries(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want JourneySeverity
	}{
		{"inside the grace period says nothing", stage2GraceAfter - time.Second, SeverityNone},
		{"exactly at the grace threshold becomes firm", stage2GraceAfter, SeverityFirm},
		{"just before the warning threshold", stage2WarnAfter - time.Second, SeverityFirm},
		{"exactly at the warning threshold", stage2WarnAfter, SeverityDeprovisionWarning},
		{"long past the warning stays at the warning", stage2WarnAfter * 2, SeverityDeprovisionWarning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := hive(tc.age, func(h *RegistryEntry) { h.AgentsWithModel = intPtr(0) })
			got := nextNudge(h, baseTime)
			if got.Severity != tc.want {
				t.Errorf("severity at %s = %v, want %v", tc.age, got.Severity, tc.want)
			}
		})
	}
}

// TestStage3SuggestBoundary checks the monthly ACMM suggestion boundary.
func TestStage3SuggestBoundary(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want JourneyStage
	}{
		{"just before the monthly mark says nothing", stage3SuggestAfter - time.Second, StageNone},
		{"exactly at the monthly mark suggests", stage3SuggestAfter, StageACMMLevel},
		{"months later still suggests", stage3SuggestAfter * 5, StageACMMLevel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := hive(tc.age, func(h *RegistryEntry) { h.ACMMLevel = 3 })
			got := nextNudge(h, baseTime)
			if got.Stage != tc.want {
				t.Errorf("stage at %s = %v, want %v", tc.age, got.Stage, tc.want)
			}
		})
	}
}

// ── The relay substitution ─────────────────────────────────────────────────

// TestRelaySubstitutesForMethodModel is the product's key stage-2 rule: the
// contributor relay fully satisfies the stage, and a hive on the relay is
// never nudged or threatened for lacking a method/model — at any age.
func TestRelaySubstitutesForMethodModel(t *testing.T) {
	// Far past even the de-provision threshold, to prove age cannot override
	// the substitution.
	const age = stage2WarnAfter * 4

	tests := []struct {
		name         string
		mut          func(*RegistryEntry)
		wantStage    JourneyStage
		wantViaRelay bool
	}{
		{
			"no model and no relay is nudged to a de-provision warning",
			func(h *RegistryEntry) { h.AgentsWithModel = intPtr(0) },
			StageMethodModel, false,
		},
		{
			"registered contributors satisfy the stage instead",
			func(h *RegistryEntry) { h.AgentsWithModel = intPtr(0); h.ContributorCount = 2 },
			StageNone, true,
		},
		{
			"an active contributor satisfies the stage",
			func(h *RegistryEntry) { h.AgentsWithModel = intPtr(0); h.ActiveContributors = 1 },
			StageNone, true,
		},
		{
			"a contributor working a task satisfies the stage",
			func(h *RegistryEntry) {
				h.AgentsWithModel = intPtr(0)
				h.Leaderboard = []LeaderboardEntry{{CurrentTask: "triage #7"}}
			},
			StageNone, true,
		},
		{
			"an assigned model satisfies the stage without the relay",
			func(h *RegistryEntry) { h.AgentsWithModel = intPtr(1) },
			StageNone, false,
		},
		{
			"both a model and the relay still reports the relay flag",
			func(h *RegistryEntry) { h.AgentsWithModel = intPtr(1); h.ContributorCount = 5 },
			StageNone, true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := hive(age, tc.mut)
			got := nextNudge(h, baseTime)
			if got.Stage != tc.wantStage {
				t.Errorf("stage = %v, want %v", got.Stage, tc.wantStage)
			}
			if got.ViaRelay != tc.wantViaRelay {
				t.Errorf("viaRelay = %v, want %v", got.ViaRelay, tc.wantViaRelay)
			}
			// The relay must never be threatened out of existence.
			if tc.wantViaRelay && got.Severity == SeverityDeprovisionWarning {
				t.Error("a hive satisfying stage 2 via the relay must never get a de-provision warning")
			}
		})
	}
}

// TestRelayHiveIsNotReportedAsAssigned guards the product's display rule: a
// hive on the relay must be visibly distinct, not silently folded into
// "assigned".
func TestRelayHiveIsNotReportedAsAssigned(t *testing.T) {
	h := hive(stage2WarnAfter*2, func(h *RegistryEntry) {
		h.AgentsWithModel = intPtr(0)
		h.ContributorCount = 4
	})
	st := JourneyStatusFor(h, nil, baseTime)
	if !st.ViaRelay {
		t.Fatal("a relay-satisfied hive must carry the viaRelay indicator")
	}
	if st.Stage != StageNone.String() {
		t.Errorf("stage = %q, want %q", st.Stage, StageNone.String())
	}
}

// ── "Never threaten ACMM" ──────────────────────────────────────────────────

// TestACMMNeverThreatensDeprovision is the strongest product rule: no matter
// how long a hive rests at a level, an ACMM nudge is always a gentle
// suggestion and never mentions de-provisioning.
func TestACMMNeverThreatensDeprovision(t *testing.T) {
	// Every level below the top, at ages spanning years.
	ages := []time.Duration{
		stage3SuggestAfter,
		stage3SuggestAfter * 2,
		stage3SuggestAfter * 12,
		stage3SuggestAfter * 60,
		365 * 24 * time.Hour * 5,
	}
	for level := 1; level < maxACMMLevel; level++ {
		for _, age := range ages {
			h := hive(age, func(h *RegistryEntry) { h.ACMMLevel = level })
			got := nextNudge(h, baseTime)
			if got.Stage != StageACMMLevel {
				t.Fatalf("level %d age %s: stage = %v, want %v", level, age, got.Stage, StageACMMLevel)
			}
			if got.Severity != SeverityGentle {
				t.Errorf("level %d age %s: severity = %v, want gentle — ACMM must never escalate",
					level, age, got.Severity)
			}
			lower := strings.ToLower(got.Message)
			for _, banned := range []string{"de-provision", "deprovision", "reclaim", "shut down", "removed"} {
				if strings.Contains(lower, banned) {
					t.Errorf("level %d age %s: ACMM copy must never threaten; found %q in: %s",
						level, age, banned, got.Message)
				}
			}
		}
	}
}

// TestACMMSuggestionIsLevelSpecific checks the copy names the hive's ACTUAL
// next level and what it entails, rather than nagging generically.
func TestACMMSuggestionIsLevelSpecific(t *testing.T) {
	tests := []struct {
		level       int
		wantCurrent string
		wantNext    string
		wantEntails string
	}{
		{1, "L1 Inception", "L2 Advisory", "advisory"},
		{2, "L2 Advisory", "L3 Quality-Gated", "hold-gated"},
		{3, "L3 Quality-Gated", "L4 Security-Aware", "issues"},
		{4, "L4 Security-Aware", "L5 Semi-Autonomous", "pull requests"},
		{5, "L5 Semi-Autonomous", "L6 Fully Autonomous", "auto-merge"},
	}
	for _, tc := range tests {
		t.Run(tc.wantCurrent, func(t *testing.T) {
			h := hive(stage3SuggestAfter, func(h *RegistryEntry) { h.ACMMLevel = tc.level })
			msg := nextNudge(h, baseTime).Message
			if !strings.Contains(msg, tc.wantCurrent) {
				t.Errorf("copy should name the current level %q: %s", tc.wantCurrent, msg)
			}
			if !strings.Contains(msg, tc.wantNext) {
				t.Errorf("copy should name the next level %q: %s", tc.wantNext, msg)
			}
			if !strings.Contains(strings.ToLower(msg), tc.wantEntails) {
				t.Errorf("copy should say what the next level entails (%q): %s", tc.wantEntails, msg)
			}
		})
	}
}

func TestACMMLevelCopyDoesNotAdvertiseOperabilityBelowL5(t *testing.T) {
	for _, level := range []int{1, 2, 3, 4} {
		info := acmmLevels[level]
		lower := strings.ToLower(info.Summary)
		for _, banned := range []string{"telemetry", "operations"} {
			if strings.Contains(lower, banned) {
				t.Errorf("L%d copy advertises %s below L5: %q", level, banned, info.Summary)
			}
		}
	}
}

// TestMaxACMMLevelGetsNoNudge — a hive at the top has nowhere to graduate to.
func TestMaxACMMLevelGetsNoNudge(t *testing.T) {
	h := hive(stage3SuggestAfter*10, func(h *RegistryEntry) { h.ACMMLevel = maxACMMLevel })
	if got := nextNudge(h, baseTime); got.Stage != StageNone {
		t.Errorf("stage = %v, want %v for a hive at the maximum level", got.Stage, StageNone)
	}
}

// ── De-provision copy is present exactly where it should be ────────────────

func TestDeprovisionWarningCopy(t *testing.T) {
	tests := []struct {
		name        string
		entry       *RegistryEntry
		wantWarning bool
	}{
		{"stage 1 past the window warns",
			hive(stage1WarnAfter, func(h *RegistryEntry) { h.PendingGitHubAppInstall = true }), true},
		{"stage 2 past the window warns",
			hive(stage2WarnAfter, func(h *RegistryEntry) { h.AgentsWithModel = intPtr(0) }), true},
		{"stage 1 before the window does not warn",
			hive(stage1FirmAfter, func(h *RegistryEntry) { h.PendingGitHubAppInstall = true }), false},
		{"stage 3 never warns",
			hive(stage3SuggestAfter*10, func(h *RegistryEntry) { h.ACMMLevel = 2 }), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextNudge(tc.entry, baseTime)
			mentions := strings.Contains(strings.ToLower(got.Message), "de-provision")
			if mentions != tc.wantWarning {
				t.Errorf("de-provision mentioned = %v, want %v; copy: %s",
					mentions, tc.wantWarning, got.Message)
			}
			isWarn := got.Severity == SeverityDeprovisionWarning
			if isWarn != tc.wantWarning {
				t.Errorf("severity warning = %v, want %v", isWarn, tc.wantWarning)
			}
		})
	}
}

// TestNudgeCopyIsActionable checks every generated message says what to do,
// not merely that something is wrong.
func TestNudgeCopyIsActionable(t *testing.T) {
	cases := []*RegistryEntry{
		hive(stage1GentleAfter, func(h *RegistryEntry) { h.PendingGitHubAppInstall = true }),
		hive(stage1FirmAfter, func(h *RegistryEntry) { h.PendingGitHubAppInstall = true }),
		hive(stage1WarnAfter, func(h *RegistryEntry) { h.PendingGitHubAppInstall = true }),
		hive(stage2GraceAfter, func(h *RegistryEntry) { h.AgentsWithModel = intPtr(0) }),
		hive(stage2WarnAfter, func(h *RegistryEntry) { h.AgentsWithModel = intPtr(0) }),
		hive(stage3SuggestAfter, func(h *RegistryEntry) { h.ACMMLevel = 2 }),
	}
	// minActionableCopyLen is a floor that a bare "please fix this" cannot meet.
	const minActionableCopyLen = 80
	for i, h := range cases {
		got := nextNudge(h, baseTime)
		if len(got.Message) < minActionableCopyLen {
			t.Errorf("case %d: copy too terse to be actionable: %q", i, got.Message)
		}
		// Each message must point at a concrete place to go.
		lower := strings.ToLower(got.Message)
		if !strings.Contains(lower, "settings") && !strings.Contains(lower, "agents") &&
			!strings.Contains(lower, "config") && !strings.Contains(lower, "/contribute") {
			t.Errorf("case %d: copy names no destination: %s", i, got.Message)
		}
	}
}

// ── Unknown / missing signals never produce a nudge ────────────────────────

func TestUnknownSignalsNeverNudge(t *testing.T) {
	tests := []struct {
		name  string
		entry *RegistryEntry
	}{
		{"nil entry", nil},
		{"unparseable registration timestamp",
			&RegistryEntry{RegisteredAt: "not-a-time", PendingGitHubAppInstall: true}},
		{"empty registration timestamp",
			&RegistryEntry{PendingGitHubAppInstall: true}},
		{"old spoke that never reported the model signal",
			hive(stage2WarnAfter*3, func(h *RegistryEntry) { h.AgentsWithModel = nil })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextNudge(tc.entry, baseTime); got.Stage != StageNone {
				t.Errorf("stage = %v, want %v — never nudge on a signal we do not have",
					got.Stage, StageNone)
			}
		})
	}
}

// TestFutureRegistrationClampsToZero guards against a clock skew producing a
// negative age and a nonsense escalation.
func TestFutureRegistrationClampsToZero(t *testing.T) {
	h := &RegistryEntry{
		RegisteredAt:            baseTime.Add(48 * time.Hour).Format(time.RFC3339),
		PendingGitHubAppInstall: true,
		AgentsWithModel:         intPtr(1),
	}
	if got := nextNudge(h, baseTime); got.Severity != SeverityNone {
		t.Errorf("severity = %v, want none for a hive registered in the future", got.Severity)
	}
}

// ── Idempotence / re-send intervals ────────────────────────────────────────

func TestShouldSendRespectsResendInterval(t *testing.T) {
	n := nudge{Stage: StageGitHubApp, Severity: SeverityGentle, Resend: stage1ResendInterval}
	sentAt := baseTime.Add(-stage1ResendInterval)
	state := func(offset time.Duration) *JourneyState {
		return &JourneyState{
			LastStage:    StageGitHubApp.String(),
			LastSeverity: SeverityGentle.String(),
			LastSentAt:   baseTime.Add(offset).Format(time.RFC3339),
		}
	}

	tests := []struct {
		name  string
		nudge nudge
		state *JourneyState
		want  bool
	}{
		{"no state means never sent, so send", n, nil, true},
		{"empty timestamp means never sent, so send", n, &JourneyState{}, true},
		{"just sent, do not repeat", n, state(-time.Minute), false},
		{"one second before the interval elapses, do not repeat",
			n, state(-stage1ResendInterval + time.Second), false},
		{"exactly at the interval, send again", n, state(-stage1ResendInterval), true},
		{"long past the interval, send again", n, state(-stage1ResendInterval * 5), true},
		{
			"an escalation sends immediately, ignoring the interval",
			nudge{Stage: StageGitHubApp, Severity: SeverityFirm, Resend: stage1ResendInterval},
			state(-time.Minute),
			true,
		},
		{
			"a stage change sends immediately",
			nudge{Stage: StageMethodModel, Severity: SeverityGentle, Resend: stage2ResendInterval},
			state(-time.Minute),
			true,
		},
		{
			"an unparseable timestamp re-sends once rather than going silent",
			n,
			&JourneyState{LastStage: StageGitHubApp.String(), LastSeverity: SeverityGentle.String(), LastSentAt: "bogus"},
			true,
		},
		{"nothing to say is never sent", nudge{}, nil, false},
		{
			"a stage with no severity is never sent",
			nudge{Stage: StageGitHubApp, Severity: SeverityNone},
			nil,
			false,
		},
	}
	_ = sentAt
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSend(tc.nudge, tc.state, baseTime); got != tc.want {
				t.Errorf("shouldSend() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResendIntervalsMatchUrgency locks in the product's cadence ordering:
// urgent nudges repeat more often than the monthly ACMM suggestion.
func TestResendIntervalsMatchUrgency(t *testing.T) {
	if stage1ResendInterval >= stage2ResendInterval || stage2ResendInterval >= stage3ResendInterval {
		t.Errorf("re-send intervals must increase with decreasing urgency: %s, %s, %s",
			stage1ResendInterval, stage2ResendInterval, stage3ResendInterval)
	}
	if stage3ResendInterval < 28*24*time.Hour {
		t.Errorf("the ACMM suggestion must be about monthly, got %s", stage3ResendInterval)
	}
}

// TestJourneyBannerIDChangesOnEscalation ensures each escalation step is a NEW
// banner, so an owner who dismissed the gentle one still sees the firm one.
func TestJourneyBannerIDChangesOnEscalation(t *testing.T) {
	gentle := journeyBannerID(nudge{Stage: StageGitHubApp, Severity: SeverityGentle}, 0)
	firm := journeyBannerID(nudge{Stage: StageGitHubApp, Severity: SeverityFirm}, 0)
	warn := journeyBannerID(nudge{Stage: StageGitHubApp, Severity: SeverityDeprovisionWarning}, 0)
	ids := map[string]bool{gentle: true, firm: true, warn: true}
	if len(ids) != 3 {
		t.Errorf("each escalation step needs a distinct banner ID, got %v", ids)
	}
	for _, id := range []string{gentle, firm, warn} {
		if !isJourneyBanner(id) {
			t.Errorf("%q should be recognised as a journey banner", id)
		}
	}
	// ACMM IDs embed the level so a new suggestion appears after graduating.
	l2 := journeyBannerID(nudge{Stage: StageACMMLevel, Severity: SeverityGentle}, 2)
	l3 := journeyBannerID(nudge{Stage: StageACMMLevel, Severity: SeverityGentle}, 3)
	if l2 == l3 {
		t.Error("ACMM banner IDs must differ per level so a graduated hive gets a fresh suggestion")
	}
	if isJourneyBanner(bannerIDPrefix + "123") {
		t.Error("an admin banner must not be mistaken for a journey banner")
	}
}

// ── Snooze ─────────────────────────────────────────────────────────────────

func TestSnoozeActive(t *testing.T) {
	tests := []struct {
		name  string
		state *JourneyState
		want  bool
	}{
		{"nil state", nil, false},
		{"no snooze set", &JourneyState{}, false},
		{"snoozed into the future",
			&JourneyState{SnoozedUntil: baseTime.Add(time.Hour).Format(time.RFC3339)}, true},
		{"snooze expired",
			&JourneyState{SnoozedUntil: baseTime.Add(-time.Hour).Format(time.RFC3339)}, false},
		{"snooze expiring exactly now is no longer active",
			&JourneyState{SnoozedUntil: baseTime.Format(time.RFC3339)}, false},
		{"corrupt timestamp fails toward nudging, not toward silence",
			&JourneyState{SnoozedUntil: "nonsense"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.snoozeActive(baseTime); got != tc.want {
				t.Errorf("snoozeActive() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseSnoozeUntil(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantZero bool
		wantErr  bool
	}{
		{"empty clears the snooze", "", true, false},
		{"zero clears the snooze", "0", true, false},
		{"negative clears the snooze", "-5h", true, false},
		{"a week is fine", "168h", false, false},
		{"the maximum is accepted", maxJourneySnoozeDuration.String(), false, false},
		{"beyond the maximum is rejected", (maxJourneySnoozeDuration + time.Hour).String(), true, true},
		{"garbage is rejected", "soon", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSnoozeUntil(tc.raw, baseTime)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got.IsZero() != tc.wantZero {
				t.Errorf("zero = %v, want %v", got.IsZero(), tc.wantZero)
			}
		})
	}
}

// TestSnoozeSuppressesStatus verifies a snoozed hive is displayed as snoozed
// even while a stage is genuinely outstanding.
func TestSnoozeSuppressesStatus(t *testing.T) {
	h := hive(stage1WarnAfter*2, func(h *RegistryEntry) { h.PendingGitHubAppInstall = true })
	st := &JourneyState{SnoozedUntil: baseTime.Add(24 * time.Hour).Format(time.RFC3339)}
	got := JourneyStatusFor(h, st, baseTime)
	if !got.Snoozed {
		t.Error("a snoozed hive must report Snoozed")
	}
	if got.SnoozedUntil != st.SnoozedUntil {
		t.Errorf("SnoozedUntil = %q, want %q", got.SnoozedUntil, st.SnoozedUntil)
	}
	// The underlying stage is still reported so an admin can see what is
	// being suppressed.
	if got.Stage != StageGitHubApp.String() {
		t.Errorf("stage = %q, want %q", got.Stage, StageGitHubApp.String())
	}
}

// ── Store persistence ──────────────────────────────────────────────────────

// withTempStatePath redirects journeyStatePath at a temp file for the duration
// of a test.
func withTempStatePath(t *testing.T) {
	t.Helper()
	orig := journeyStatePath
	journeyStatePath = filepath.Join(t.TempDir(), "journey-state.json")
	t.Cleanup(func() { journeyStatePath = orig })
}

func TestJourneyStoreRoundTrip(t *testing.T) {
	withTempStatePath(t)

	store := newJourneyStore()
	n := nudge{Stage: StageGitHubApp, Severity: SeverityFirm}
	store.recordSent("hive-1", n, 0, baseTime)
	store.setSnooze("hive-2", baseTime.Add(24*time.Hour), "admin", "vendor pilot")
	if err := store.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded := newJourneyStore()
	if err := reloaded.load(baseTime); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.get("hive-1")
	if got == nil || got.LastStage != StageGitHubApp.String() || got.LastSeverity != SeverityFirm.String() {
		t.Errorf("hive-1 state did not survive the round trip: %+v", got)
	}
	sn := reloaded.get("hive-2")
	if sn == nil || !sn.snoozeActive(baseTime) {
		t.Errorf("hive-2 snooze did not survive the round trip: %+v", sn)
	}
	if sn.SnoozedBy != "admin" || sn.SnoozeReason != "vendor pilot" {
		t.Errorf("snooze audit fields lost: %+v", sn)
	}
}

// TestJourneyStoreDropsExpiredSnoozeOnLoad — a forgotten exemption must lapse.
func TestJourneyStoreDropsExpiredSnoozeOnLoad(t *testing.T) {
	withTempStatePath(t)

	store := newJourneyStore()
	store.setSnooze("hive-1", baseTime.Add(time.Hour), "admin", "short pilot")
	if err := store.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded := newJourneyStore()
	// Load well after the snooze expired.
	if err := reloaded.load(baseTime.Add(48 * time.Hour)); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.get("hive-1")
	if got == nil {
		t.Fatal("state should still exist")
	}
	if got.SnoozedUntil != "" {
		t.Errorf("expired snooze should be cleared on load, got %q", got.SnoozedUntil)
	}
}

func TestJourneyStoreMissingFileIsNotAnError(t *testing.T) {
	withTempStatePath(t)
	store := newJourneyStore()
	if err := store.load(baseTime); err != nil {
		t.Errorf("a missing state file must not be an error, got %v", err)
	}
}

func TestJourneyStoreGetReturnsACopy(t *testing.T) {
	withTempStatePath(t)
	store := newJourneyStore()
	store.recordSent("hive-1", nudge{Stage: StageGitHubApp, Severity: SeverityGentle}, 0, baseTime)
	got := store.get("hive-1")
	got.LastStage = "tampered"
	if again := store.get("hive-1"); again.LastStage != StageGitHubApp.String() {
		t.Error("get() must return a copy so callers cannot mutate shared state")
	}
	if store.get("missing") != nil {
		t.Error("get() on an unknown hive should return nil")
	}
}

func TestJourneyStoreSaveIsSkippedWhenClean(t *testing.T) {
	withTempStatePath(t)
	store := newJourneyStore()
	// Nothing recorded → nothing to write.
	if err := store.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(journeyStatePath); !os.IsNotExist(err) {
		t.Error("a clean store must not write a file")
	}
}

// TestRecordSentTracksACMMLevel verifies the ACMM level is remembered so a
// graduated hive gets a fresh suggestion.
func TestRecordSentTracksACMMLevel(t *testing.T) {
	withTempStatePath(t)
	store := newJourneyStore()
	store.recordSent("hive-1", nudge{Stage: StageACMMLevel, Severity: SeverityGentle}, 3, baseTime)
	if got := store.get("hive-1"); got.LastACMMLevel != 3 {
		t.Errorf("LastACMMLevel = %d, want 3", got.LastACMMLevel)
	}
	// A non-ACMM nudge must not overwrite the recorded level.
	store.recordSent("hive-1", nudge{Stage: StageGitHubApp, Severity: SeverityFirm}, 5, baseTime)
	if got := store.get("hive-1"); got.LastACMMLevel != 3 {
		t.Errorf("LastACMMLevel = %d, want it unchanged at 3", got.LastACMMLevel)
	}
}

// ── Presentation helpers ───────────────────────────────────────────────────

func TestSeverityBannerColorIsAlwaysValid(t *testing.T) {
	for _, s := range []JourneySeverity{SeverityNone, SeverityGentle, SeverityFirm, SeverityDeprovisionWarning} {
		if !validBannerColors[s.bannerColor()] {
			t.Errorf("severity %v maps to invalid banner color %q", s, s.bannerColor())
		}
	}
}

func TestStageAndSeverityStrings(t *testing.T) {
	if StageNone.String() != "none" || StageGitHubApp.String() != "github-app" ||
		StageMethodModel.String() != "method-model" || StageACMMLevel.String() != "acmm-level" {
		t.Error("stage tokens must stay stable — the UI keys off them")
	}
	if SeverityNone.String() != "none" || SeverityGentle.String() != "gentle" ||
		SeverityFirm.String() != "firm" || SeverityDeprovisionWarning.String() != "deprovision-warning" {
		t.Error("severity tokens must stay stable — the UI keys off them")
	}
}

func TestACMMLevelLabel(t *testing.T) {
	tests := []struct {
		level int
		want  string
	}{
		{1, "L1 Inception"},
		{6, "L6 Fully Autonomous"},
		{99, "L99"}, // unknown levels degrade gracefully, never "L99 <nil>"
	}
	for _, tc := range tests {
		if got := acmmLevelLabel(tc.level); got != tc.want {
			t.Errorf("acmmLevelLabel(%d) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "under an hour"},
		{time.Hour, "under an hour"},
		{5 * time.Hour, "5 hours"},
		{25 * time.Hour, "1 day"},
		{72 * time.Hour, "3 days"},
	}
	for _, tc := range tests {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestOrgLabelFallbacks(t *testing.T) {
	tests := []struct {
		name  string
		entry *RegistryEntry
		want  string
	}{
		{"nil", nil, "your organization"},
		{"org wins", &RegistryEntry{Org: "acme", Name: "acme/widget"}, "acme"},
		{"name is the fallback", &RegistryEntry{Name: "acme/widget"}, "acme/widget"},
		{"blank falls back to generic copy", &RegistryEntry{Org: "  "}, "your organization"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := orgLabel(tc.entry); got != tc.want {
				t.Errorf("orgLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJourneyStatusForNilState covers the common path where a hive has never
// been nudged.
func TestJourneyStatusForNilState(t *testing.T) {
	h := hive(stage1FirmAfter, func(h *RegistryEntry) { h.PendingGitHubAppInstall = true })
	got := JourneyStatusFor(h, nil, baseTime)
	if got.Stage != StageGitHubApp.String() || got.Severity != SeverityFirm.String() {
		t.Errorf("unexpected status: %+v", got)
	}
	if got.StageNum != int(StageGitHubApp) {
		t.Errorf("StageNum = %d, want %d", got.StageNum, int(StageGitHubApp))
	}
	if got.StalledFor == "" {
		t.Error("a stalled hive should report how long it has been stalled")
	}
	if got.Snoozed {
		t.Error("a hive with no state must not report as snoozed")
	}
}
