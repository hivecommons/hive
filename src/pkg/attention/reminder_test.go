package attention

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestReminderIntervalDefaultAndConfigurable(t *testing.T) {
	if (Config{}).Reminder() != 24*time.Hour || DefaultReminderInterval != 24*time.Hour {
		t.Fatal("reminder default must be 24h")
	}
	if (Config{ReminderInterval: 6 * time.Hour}).Reminder() != 6*time.Hour {
		t.Fatal("configured reminder interval ignored")
	}
	if err := (Config{ReminderInterval: -time.Hour}).Validate(); err == nil {
		t.Fatal("negative reminder interval accepted")
	}
}

func pagedEpisode(paged time.Time) Episode {
	return Episode{Key: observation().Key, State: Notified, FirstSeen: paged, PagedAt: paged}
}

func TestReminderDueAtMostOncePerInterval(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	e := pagedEpisode(t0)
	if ReminderDue(e, t0.Add(23*time.Hour+59*time.Minute), 0) {
		t.Fatal("reminder before the 24h default")
	}
	if !ReminderDue(e, t0.Add(24*time.Hour), 0) {
		t.Fatal("no reminder after the 24h default")
	}
	e.RemindedAt = t0.Add(24 * time.Hour)
	if ReminderDue(e, t0.Add(47*time.Hour), 0) {
		t.Fatal("second reminder inside 24h of the first")
	}
	if !ReminderDue(e, t0.Add(48*time.Hour), 0) {
		t.Fatal("no reminder 24h after the last one")
	}
	if !ReminderDue(pagedEpisode(t0), t0.Add(2*time.Hour), 2*time.Hour) || ReminderDue(pagedEpisode(t0), t0.Add(time.Hour), 2*time.Hour) {
		t.Fatal("configured interval ignored")
	}
}

func TestReminderSkippedWhenAckedSnoozedResolvedOrUnpaged(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	later := t0.Add(72 * time.Hour)
	for name, mut := range map[string]func(*Episode){
		"acked":        func(e *Episode) { e.State, e.AckedAt = Acknowledged, t0.Add(time.Hour) },
		"acked-state":  func(e *Episode) { e.AckedAt = t0.Add(time.Hour) },
		"snoozed":      func(e *Episode) { e.SnoozedUntil = later.Add(time.Hour) },
		"resolved":     func(e *Episode) { e.State, e.ResolvedAt = Resolved, t0.Add(time.Hour) },
		"never-paged":  func(e *Episode) { e.State, e.PagedAt = Actionable, time.Time{} },
		"uncertain":    func(e *Episode) { e.Uncertain = true },
		"no-key":       func(e *Episode) { e.Key = Key{} },
		"recent-actor": func(e *Episode) { e.RecipientActedAt = later.Add(-time.Hour) },
	} {
		e := pagedEpisode(t0)
		mut(&e)
		if ReminderDue(e, later, 0) {
			t.Errorf("%s: reminder due", name)
		}
	}
	e := pagedEpisode(t0)
	e.SnoozedUntil = t0.Add(30 * time.Hour)
	if !ReminderDue(e, later, 0) {
		t.Fatal("expired snooze still suppresses reminders")
	}
}

func TestStoreReminderBookkeepingAndItemClosure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "episodes.json")
	s, e := seeded(t, path)
	at := e.PagedAt.Add(25 * time.Hour)
	got, err := s.RecordReminder(e.Key, at)
	if err != nil || got.Reminders != 1 || !got.RemindedAt.Equal(at) || !got.LastPing().Equal(at) {
		t.Fatalf("reminder: %+v err=%v", got, err)
	}
	acted := at.Add(time.Hour)
	if err := s.RecordRecipientAction(e.Key, acted); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRecipientAction(e.Key, at); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, path)
	open := s.OpenEpisodes("ORG/repo")
	if len(open) != 1 || !open[0].RecipientActedAt.Equal(acted) || open[0].Reminders != 1 || !open[0].LastPing().Equal(acted) {
		t.Fatalf("persisted: %+v", open)
	}
	if ReminderDue(open[0], acted.Add(23*time.Hour), 0) || !ReminderDue(open[0], acted.Add(24*time.Hour), 0) {
		t.Fatal("interval must run from the recipient's action")
	}
	if es, err := s.ResolveItem("org/repo", 2); err != nil || len(es) != 0 {
		t.Fatalf("other item resolved: %v %v", es, err)
	}
	resolved, err := s.ResolveItem("org/repo", 1)
	if err != nil || len(resolved) != 1 || resolved[0].State != Resolved {
		t.Fatalf("resolve: %+v err=%v", resolved, err)
	}
	if len(s.OpenEpisodes("org/repo")) != 0 {
		t.Fatal("closed item still open")
	}
	if ReminderDue(s.Snapshot("org/repo", 1)[0], acted.Add(96*time.Hour), 0) {
		t.Fatal("stale reminder after closure")
	}
	if _, err := s.RecordReminder(e.Key, acted); !errors.Is(err, ErrNoEpisode) {
		t.Fatalf("reminder on resolved episode: %v", err)
	}
	if err := s.RecordRecipientAction(e.Key, acted); !errors.Is(err, ErrNoEpisode) {
		t.Fatalf("action on resolved episode: %v", err)
	}
}

func TestRecipientLogin(t *testing.T) {
	for in, want := range map[string]string{"@Alice": "alice", " bob ": "bob", "org/team": "", "@": "", "": ""} {
		got, ok := RecipientLogin(Episode{Key: Key{Recipient: in}})
		if got != want && ok || ok != (want != "") {
			t.Errorf("%q: got %q ok=%v", in, got, ok)
		}
	}
}
