package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type ciFailureReactorHarness struct {
	r       *ciFailureReactor
	cfg     *config.Config
	clock   time.Time
	kicker  *fakeKicker
	nudges  []string
	notes   []string
	paused  map[string]bool
	breaker bool
}

func newCIFailureReactorHarness() *ciFailureReactorHarness {
	h := &ciFailureReactorHarness{
		cfg:    &config.Config{},
		clock:  time.Date(2026, 10, 29, 4, 0, 0, 0, time.UTC),
		kicker: &fakeKicker{},
		paused: map[string]bool{},
	}
	h.cfg.Project.Org = "acme"
	h.r = &ciFailureReactor{
		now: func() time.Time { return h.clock },
		cfg: func() *config.Config { return h.cfg },
		owners: func(*config.Config) map[string]string {
			return map[string]string{"acme/widgets#7": "scanner"}
		},
		available: func(*config.Config) func(string) bool {
			return func(agent string) bool { return !h.paused[agent] }
		},
		breaker: func() bool { return h.breaker },
		kicker:  h.kicker,
		nudge: func(repo string, number int) {
			h.nudges = append(h.nudges, repo+"#"+strconv.Itoa(number))
		},
		note: func(_ string, _ int, _ string, line string) { h.notes = append(h.notes, line) },
	}
	return h
}

func ciSignal(number int, head, check string) github.CIFailureSignal {
	return github.CIFailureSignal{Repo: "acme/widgets", Number: number, HeadSHA: head, Check: check,
		DetailsURL: "https://ci.example/run/1", At: time.Date(2026, 10, 29, 4, 0, 0, 0, time.UTC)}
}

// A failed check on a hive-authored PR kicks its owner at once with the
// FIX-BEFORE-NEW repair instructions, the check name and the log pointer,
// logs one activity line, and does not react again to the same head inside
// governor.ci_failure_kick_cooldown_s (hivecommons/hive#15291).
func TestCIFailureReactorKicksOwnerOncePerHeadWithinCooldown(t *testing.T) {
	h := newCIFailureReactorHarness()

	if got := h.r.React(ciSignal(7, "sha-a", "unit")); got != ciFailureKicked {
		t.Fatalf("first signal outcome = %q, want %q", got, ciFailureKicked)
	}
	if len(h.kicker.kicks) != 1 || h.kicker.kicks[0].agent != "scanner" {
		t.Fatalf("kicks = %+v, want one to scanner", h.kicker.kicks)
	}
	msg := h.kicker.kicks[0].message
	for _, want := range []string{"FIX-BEFORE-NEW", "acme/widgets#7 just failed CI (unit)", "failing: unit", "log: https://ci.example/run/1", "head: sha-a", "do not watch, poll, or sleep on CI"} {
		if !strings.Contains(msg, want) {
			t.Errorf("kick lacks %q:\n%s", want, msg)
		}
	}
	if len(h.notes) != 1 || h.notes[0] != "CI failed on #7 (unit) - kicked agent scanner" {
		t.Fatalf("activity lines = %q", h.notes)
	}

	h.clock = h.clock.Add(time.Minute)
	if got := h.r.React(ciSignal(7, "sha-a", "lint")); got != ciFailureCooldown {
		t.Fatalf("second check on the same head = %q, want %q", got, ciFailureCooldown)
	}
	if got := h.r.React(ciSignal(7, "sha-b", "unit")); got != ciFailureKicked {
		t.Fatalf("a new head = %q, want %q", got, ciFailureKicked)
	}
	h.clock = h.clock.Add(config.DefaultCIFailureKickCooldownS * time.Second)
	if got := h.r.React(ciSignal(7, "sha-a", "lint")); got != ciFailureKicked {
		t.Fatalf("after the cooldown = %q, want %q", got, ciFailureKicked)
	}
	if len(h.kicker.kicks) != 3 {
		t.Fatalf("kicks = %d, want 3", len(h.kicker.kicks))
	}

	h.cfg.Governor.CIFailureKickCooldownS = 30
	h.clock = h.clock.Add(31 * time.Second)
	if got := h.r.React(ciSignal(7, "sha-a", "e2e")); got != ciFailureKicked {
		t.Fatalf("a configured 30s cooldown = %q, want %q", got, ciFailureKicked)
	}
}

// A failed check on a PR no hive agent opened schedules an automerge pass
// (the contributor-lane / stage 072 path) once per head per cooldown.
func TestCIFailureReactorSchedulesAutomergePassForContributorPR(t *testing.T) {
	h := newCIFailureReactorHarness()
	if got := h.r.React(ciSignal(12, "sha-c", "unit")); got != ciFailureAutomergePass {
		t.Fatalf("outcome = %q, want %q", got, ciFailureAutomergePass)
	}
	if got := h.r.React(ciSignal(12, "sha-c", "lint")); got != ciFailureCooldown {
		t.Fatalf("second check = %q, want %q", got, ciFailureCooldown)
	}
	if len(h.nudges) != 1 || h.nudges[0] != "acme/widgets#12" || len(h.kicker.kicks) != 0 {
		t.Fatalf("nudges = %v kicks = %d, want one pass and no kick", h.nudges, len(h.kicker.kicks))
	}
}

// Every guard skips the reaction without spending the head's cooldown slot,
// so the reaction happens once the guard lifts.
func TestCIFailureReactorGuards(t *testing.T) {
	h := newCIFailureReactorHarness()

	off := false
	h.cfg.Governor.CIFailureWebhookKick = &off
	if got := h.r.React(ciSignal(7, "sha-a", "unit")); got != ciFailureDisabled {
		t.Fatalf("disabled = %q", got)
	}
	h.cfg.Governor.CIFailureWebhookKick = nil

	h.cfg.Project.PausedRepos = []config.RepoPause{{Repo: "widgets"}}
	if got := h.r.React(ciSignal(7, "sha-a", "unit")); got != ciFailureRepoPaused {
		t.Fatalf("paused repo = %q", got)
	}
	if got := h.r.React(ciSignal(12, "sha-c", "unit")); got != ciFailureRepoPaused {
		t.Fatalf("paused repo contributor PR = %q", got)
	}
	h.cfg.Project.PausedRepos = nil

	h.breaker = true
	if got := h.r.React(ciSignal(7, "sha-a", "unit")); got != ciFailureBreaker {
		t.Fatalf("breaker = %q", got)
	}
	h.breaker = false

	h.paused["scanner"] = true
	if got := h.r.React(ciSignal(7, "sha-a", "unit")); got != ciFailureAgentUnavailable {
		t.Fatalf("paused owner = %q", got)
	}
	h.paused["scanner"] = false

	h.kicker.err = errors.New("pane gone")
	if got := h.r.React(ciSignal(7, "sha-a", "unit")); got != ciFailureKickFailed {
		t.Fatalf("undeliverable = %q", got)
	}
	h.kicker.err = nil

	if len(h.kicker.kicks) != 0 || len(h.nudges) != 0 || len(h.notes) != 0 {
		t.Fatalf("guards must not react: kicks=%d nudges=%v notes=%q", len(h.kicker.kicks), h.nudges, h.notes)
	}
	if got := h.r.React(ciSignal(7, "sha-a", "unit")); got != ciFailureKicked {
		t.Fatalf("once every guard lifts = %q, want %q", got, ciFailureKicked)
	}
}

func TestCIFailureConfigDefaults(t *testing.T) {
	var cfg *config.Config
	if !cfg.CIFailureWebhookKickEnabled() || cfg.CIFailureKickCooldown() != 600*time.Second {
		t.Fatalf("defaults: enabled=%v cooldown=%v, want true and 600s", cfg.CIFailureWebhookKickEnabled(), cfg.CIFailureKickCooldown())
	}
}
