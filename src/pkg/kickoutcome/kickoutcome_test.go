package kickoutcome

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func TestDeriveOnlyFromRealSignals(t *testing.T) {
	r := Record{StartedAt: t0}
	if o := Derive(r, t0.Add(time.Minute), true); o.Kind != KindStillRunning {
		t.Fatalf("open latest kick: %q", o.Kind)
	}
	if o := Derive(r, t0.Add(time.Minute), false); o.Kind != KindEndedNoVisibleAction {
		t.Fatalf("a kick superseded by a later one cannot be running: %q", o.Kind)
	}
	if o := Derive(r, t0.Add(RunningCeiling+time.Minute), true); o.Kind != KindEndedNoVisibleAction {
		t.Fatalf("past the ceiling the hive cannot tell and must not guess: %q", o.Kind)
	}
	end := t0.Add(time.Minute)
	r.EndedAt, r.TurnKind = &end, "ended"
	if o := Derive(r, end, true); o.Kind != KindEndedNoVisibleAction {
		t.Fatalf("a turn end with no action: %q", o.Kind)
	}
	r.TurnKind = "question"
	if o := Derive(r, end, true); o.Kind != KindEndedNoVisibleAction || o.Quote != "" {
		t.Fatalf("a question is not a stand-down: %+v", o)
	}
	r.TurnKind, r.TurnReason = "stand-down", "no push rights"
	if o := Derive(r, end, true); o.Kind != KindStoodDown || o.Quote != "no push rights" {
		t.Fatalf("stand-down: %+v", o)
	}
	pr := end.Add(time.Minute)
	r.PRAt, r.PRNumber = &pr, 9
	if o := Derive(r, pr, true); o.Kind != KindOpenedPR || o.Quote != "" {
		t.Fatalf("a real PR outranks the agent's stand-down line: %+v", o)
	}
	r.RefusalCode = "paused"
	if o := Derive(r, pr, true); o.Kind != KindRefused {
		t.Fatalf("refusal: %q", o.Kind)
	}
}

func TestQuoteRedactsCapsAndCleans(t *testing.T) {
	in := "stand down: token ghp_abcdefghijklmnopqrstuvwxyz0123456789 and sk-ant-abcdef123456 and Bearer abcdefghijklmnop.qrstuvwxyz\x1b[31m\n\tred " + strings.Repeat("x", 500)
	q := Quote(in)
	for _, leak := range []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789", "sk-ant-abcdef123456", "\x1b", "\n", "\t"} {
		if strings.Contains(q, leak) {
			t.Fatalf("quote kept %q: %q", leak, q)
		}
	}
	if n := len([]rune(q)); n > MaxQuoteRunes {
		t.Fatalf("quote is %d runes", n)
	}
}

func TestLatePullRequestUpgradesTheOutcome(t *testing.T) {
	s, _ := Open("", nil)
	s.Delivered("a", t0)
	s.TurnEnded("a", "ended", "", t0, t0.Add(5*time.Minute))
	s.SetClock(func() time.Time { return t0.Add(6 * time.Minute) })
	if v, _ := s.LatestOf("a"); v.Kind != KindEndedNoVisibleAction {
		t.Fatalf("before the relay acts: %q", v.Kind)
	}
	s.PROpened("a", "https://github.com/o/r/pull/7", 7, t0.Add(8*time.Minute))
	if v, _ := s.LatestOf("a"); v.Kind != KindOpenedPR || v.Summary != "opened PR #7" {
		t.Fatalf("after the relay opened the PR: %+v", v)
	}
}

func TestActionOutsideAnyKickIsNotAttributed(t *testing.T) {
	s, _ := Open("", nil)
	s.Delivered("a", t0)
	s.TurnEnded("a", "ended", "", t0, t0.Add(time.Minute))
	s.PROpened("a", "https://github.com/o/r/pull/7", 7, t0.Add(time.Minute+ActionGrace+time.Minute))
	s.Commented("a", t0.Add(-time.Hour))
	s.SetClock(func() time.Time { return t0.Add(3 * time.Hour) })
	if v, _ := s.LatestOf("a"); v.Kind != KindEndedNoVisibleAction {
		t.Fatalf("an unrelated PR was claimed by the kick: %+v", v)
	}
}

func TestRefusedKickNeverCollectsActions(t *testing.T) {
	s, _ := Open("", nil)
	s.Refused("a", Trigger{Kind: TriggerDashboard, Actor: "alice"}, "paused", "agent a cannot be kicked: it is paused", t0)
	s.PROpened("a", "https://github.com/o/r/pull/7", 7, t0.Add(time.Minute))
	rs := s.Records("a", 0)
	if len(rs) != 1 || rs[0].PRAt != nil {
		t.Fatalf("a refused kick collected a PR: %+v", rs)
	}
	d := s.Recent("a", 10)
	if d[0].Kind != KindRefused || d[0].RefusalCode != "paused" || d[0].TriggerActor != "alice" || d[0].Quote == "" {
		t.Fatalf("%+v", d[0])
	}
}

func TestTriggerAttributionAndOtherDefault(t *testing.T) {
	s, _ := Open("", nil)
	now := t0
	s.SetClock(func() time.Time { return now })
	s.Intend("a", Trigger{Kind: TriggerDashboard, Actor: "alice"})
	now = t0.Add(time.Second)
	s.Delivered("a", now)
	now = t0.Add(time.Minute)
	s.Delivered("a", now) // the intent was consumed by the first delivery
	now = t0.Add(10 * time.Minute)
	s.Intend("a", Trigger{Kind: "bogus"})
	s.Delivered("a", now)
	rs := s.Records("a", 0)
	if rs[2].TriggerKind != TriggerDashboard || rs[2].TriggerActor != "alice" || rs[1].TriggerKind != TriggerOther || rs[0].TriggerKind != TriggerOther {
		t.Fatalf("%+v", rs)
	}
	now = t0.Add(time.Hour)
	s.Intend("a", Trigger{Kind: TriggerGovernor})
	now = t0.Add(time.Hour + 2*IntentTTL)
	s.Delivered("a", now)
	if got := s.Records("a", 1)[0].TriggerKind; got != TriggerOther {
		t.Fatalf("a stale intent was claimed: %q", got)
	}
}

func TestTurnEndBelongsToItsOwnKick(t *testing.T) {
	s, _ := Open("", nil)
	s.Delivered("a", t0)
	s.Delivered("a", t0.Add(time.Hour))
	s.TurnEnded("a", "stand-down", "first", t0, t0.Add(time.Minute))
	rs := s.Records("a", 0)
	if rs[1].EndedAt == nil || rs[0].EndedAt != nil {
		t.Fatalf("turn end landed on the wrong kick: %+v", rs)
	}
}

func TestBoundedAndPersistent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.json")
	s, err := Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxRecordsPerAgent+5; i++ {
		s.Delivered("a", t0.Add(time.Duration(i)*time.Hour))
	}
	s2, err := Open(p, nil)
	if err != nil || len(s2.Records("a", 0)) != MaxRecordsPerAgent {
		t.Fatalf("not bounded or not durable: %v %d", err, len(s2.Records("a", 0)))
	}
}

func TestAuditLinesHoldIDsAndKindsOnly(t *testing.T) {
	var buf bytes.Buffer
	s, _ := Open("", slog.New(slog.NewTextHandler(&buf, nil)))
	s.Delivered("a", t0)
	s.TurnEnded("a", "stand-down", "SECRET-PHRASE the agent said", t0, t0.Add(time.Minute))
	s.Refused("a", Trigger{Kind: TriggerDashboard, Actor: "alice"}, "paused", "SECRET-PHRASE hive text", t0.Add(time.Hour))
	if buf.Len() == 0 || strings.Contains(buf.String(), "SECRET-PHRASE") || strings.Contains(buf.String(), "alice") {
		t.Fatalf("audit log: %q", buf.String())
	}
}

func TestOnlyStandDownAndRefusedQuote(t *testing.T) {
	end := t0.Add(time.Minute)
	pr := t0.Add(2 * time.Minute)
	r := Record{ID: "k", Agent: "a", StartedAt: t0, EndedAt: &end, TurnKind: "stand-down", TurnReason: "why", PRAt: &pr, PRNumber: 3}
	if d := DetailOf(r, pr, true); d.Quote != "" {
		t.Fatalf("an opened PR quoted the stand-down line: %+v", d)
	}
	r.PRAt = nil
	if d := DetailOf(r, pr, true); d.Quote != "why" {
		t.Fatalf("%+v", d)
	}
}
