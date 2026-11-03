package attention

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		body   string
		ok     bool
		bad    bool
		kind   CommandKind
		snooze time.Duration
		target string
	}{
		{body: "/hive ack", ok: true, kind: CommandAck},
		{body: "\n  /HIVE Ack abc123\nthanks", ok: true, kind: CommandAck, target: "abc123"},
		{body: "/hive snooze 2d", ok: true, kind: CommandSnooze, snooze: 48 * time.Hour},
		{body: "/hive snooze 90m `ABCDEF`", ok: true, kind: CommandSnooze, snooze: 90 * time.Minute, target: "abcdef"},
		{body: "/hive snooze 1w", ok: true, kind: CommandSnooze, snooze: 7 * 24 * time.Hour},
		{body: "/hive snooze", ok: true, bad: true},
		{body: "/hive snooze 31d", ok: true, bad: true},
		{body: "/hive snooze 10s", ok: true, bad: true},
		{body: "/hive snooze forever", ok: true, bad: true},
		{body: "/hive ack not-a-key", ok: true, bad: true},
		{body: "> /hive ack", ok: false},
		{body: "please /hive ack", ok: false},
		{body: "/hive approve", ok: false},
	} {
		cmd, ok, err := ParseCommand(tc.body)
		if ok != tc.ok || (err != nil) != tc.bad {
			t.Fatalf("%q: ok=%v err=%v", tc.body, ok, err)
		}
		if !tc.ok || tc.bad {
			continue
		}
		if cmd.Kind != tc.kind || cmd.Snooze != tc.snooze || cmd.Target != tc.target {
			t.Fatalf("%q: got %+v", tc.body, cmd)
		}
	}
}

func seeded(t *testing.T, path string) (*Store, Episode) {
	t.Helper()
	s := openStore(t, path)
	f := &forge{}
	es := report(t, s, f, "owner", observation())
	if len(es) != 1 || es[0].State != Notified {
		t.Fatalf("seed: %+v", es)
	}
	return s, es[0]
}

func TestAckAndSnoozeRecordedOnEpisodeByRecipientOrMaintainer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "episodes.json")
	s, _ := seeded(t, path)
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	es, err := s.Apply("org/repo", 1, "@Maintainer", false, Command{Kind: CommandAck})
	if err != nil || len(es) != 1 {
		t.Fatalf("recipient ack: %v %+v", err, es)
	}
	if es[0].State != Acknowledged || !es[0].AckedAt.Equal(now) || es[0].AckedBy != "Maintainer" {
		t.Fatalf("ack not recorded: %+v", es[0])
	}
	es, err = s.Apply("org/repo", 1, "repo-admin", true, Command{Kind: CommandSnooze, Snooze: 4 * time.Hour})
	if err != nil || len(es) != 1 {
		t.Fatalf("maintainer snooze: %v %+v", err, es)
	}
	if !es[0].SnoozedUntil.Equal(now.Add(4*time.Hour)) || es[0].SnoozedBy != "repo-admin" || es[0].State != Acknowledged {
		t.Fatalf("snooze not recorded: %+v", es[0])
	}
	// Persisted for the reminder scheduler, across a restart.
	got := openStore(t, path).Snapshot("org/repo", 1)
	if len(got) != 1 || got[0].AckedBy != "Maintainer" || got[0].SnoozedBy != "repo-admin" || !got[0].SnoozedUntil.Equal(now.Add(4*time.Hour)) {
		t.Fatalf("not persisted: %+v", got)
	}
}

func TestAckAndSnoozeRejectedForOtherCallers(t *testing.T) {
	s, before := seeded(t, filepath.Join(t.TempDir(), "episodes.json"))
	for _, cmd := range []Command{{Kind: CommandAck}, {Kind: CommandSnooze, Snooze: time.Hour}} {
		if _, err := s.Apply("org/repo", 1, "drive-by", false, cmd); !errors.Is(err, ErrNotPermitted) {
			t.Fatalf("%s by non-recipient non-maintainer: err=%v", cmd.Kind, err)
		}
	}
	after := s.Snapshot("org/repo", 1)
	if len(after) != 1 || after[0] != before {
		t.Fatalf("rejected command changed the episode:\n%+v\n%+v", before, after)
	}
	if _, err := s.Apply("org/repo", 2, "maintainer", true, Command{Kind: CommandAck}); !errors.Is(err, ErrNoEpisode) {
		t.Fatalf("other item: %v", err)
	}
	other := "0"
	if strings.HasPrefix(before.Key.Hash(), other) {
		other = "1"
	}
	if _, err := s.Apply("org/repo", 1, "maintainer", true, Command{Kind: CommandAck, Target: other}); !errors.Is(err, ErrNoEpisode) {
		t.Fatalf("unmatched target: %v", err)
	}
	if _, err := s.Apply("org/repo", 1, "maintainer", false, Command{Kind: CommandAck, Target: before.Key.Hash()[:12]}); err != nil {
		t.Fatalf("status-card key prefix: %v", err)
	}
}

func TestAckAndSnoozeChangeOnlyReminderState(t *testing.T) {
	s, before := seeded(t, filepath.Join(t.TempDir(), "episodes.json"))
	if _, err := s.Apply("org/repo", 1, "maintainer", false, Command{Kind: CommandAck}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply("org/repo", 1, "maintainer", false, Command{Kind: CommandSnooze, Snooze: time.Hour}); err != nil {
		t.Fatal(err)
	}
	after := s.Snapshot("org/repo", 1)[0]
	after.State, after.AckedAt, after.AckedBy, after.SnoozedUntil, after.SnoozedBy = before.State, before.AckedAt, before.AckedBy, before.SnoozedUntil, before.SnoozedBy
	if after != before {
		t.Fatalf("ack/snooze changed non-reminder state:\n%+v\n%+v", before, after)
	}
}

func TestEventRenderEmitsEveryContractField(t *testing.T) {
	ev := Event{Who: "@alice", WhyNow: "required check failed on the new head", Action: "re-run or fix the ARM64 job", Evidence: "run 42", Tried: "two automatic re-runs", Next: "Hive re-checks on the next push", Resolve: "the required check passes"}
	body := ev.Render()
	for _, want := range []string{
		"**Who:** @alice", "**What changed / why now:** required check", "**Requested action:** re-run", "**Evidence:** run 42",
		"**What Hive tried:** two automatic", "**What happens next:** Hive re-checks", "**How to ack, snooze or resolve:**",
		"`/hive ack`", "`/hive snooze <duration>`", "Resolve: the required check passes",
		"Acknowledgement is not approval", "release no security, merge or review hold",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("render missing %q:\n%s", want, body)
		}
	}
	key := observation().Key
	o, err := NewObservation(key, true, "run 42", ev)
	if err != nil || o.Body != body || o.Key != key || !o.Actionable {
		t.Fatalf("observation: %v %+v", err, o)
	}
	ev.Tried = " "
	if _, err := NewObservation(key, true, "run 42", ev); err == nil || !strings.Contains(err.Error(), "what Hive tried") {
		t.Fatalf("incomplete event admitted: %v", err)
	}
}
