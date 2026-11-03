package attention

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/attentionbudget"
)

func budgetConfig(n int, digest ...string) Config {
	return Config{Admitter: testConfig.Admitter, Config: attentionbudget.Config{RecipientBudget: n, DigestRecipients: digest}}
}

// asks returns n distinct actionable observations for the same recipient.
func asks(n int, class attentionbudget.Class) []Observation {
	var out []Observation
	for i := 0; i < n; i++ {
		o := observation()
		o.Key.Subject = fmt.Sprintf("%s-%d", class, i)
		o.Class = class
		out = append(out, o)
	}
	return out
}

func reportWith(t *testing.T, s *Store, cfg Config, f *forge, obs ...Observation) []Episode {
	t.Helper()
	es, err := s.Report(context.Background(), cfg, "owner", "org/repo", 1, obs, f)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestBudgetDisabledChangesNothing(t *testing.T) {
	s := openStore(t, "")
	f := &forge{}
	es := report(t, s, f, "owner", asks(attentionbudget.ShadowBudget+2, "")...)
	if len(f.posts) != attentionbudget.ShadowBudget+2 {
		t.Fatalf("disabled budget held back asks: posts=%d", len(f.posts))
	}
	for _, e := range es {
		if e.Overflowed || e.State != Notified {
			t.Fatalf("disabled budget changed an episode: %+v", e)
		}
	}
	if c := s.Budget().Snapshot()["maintainer"]; c.Delivered != len(f.posts) || c.WouldOverflow != 2 || c.Overflowed != 0 {
		t.Fatalf("shadow counts = %+v", c)
	}
	if line := s.Budget().LogLine(); !strings.HasPrefix(line, "attention budget: off") || !strings.Contains(line, "would_overflow=2") {
		t.Fatalf("shadow log line = %q", line)
	}
}

func TestBudgetOverflowIsRecordedNotDropped(t *testing.T) {
	s := openStore(t, "")
	f := &forge{}
	cfg := budgetConfig(2, "maintainer")
	obs := asks(3, "")
	es := reportWith(t, s, cfg, f, obs...)
	if len(f.posts) != 2 {
		t.Fatalf("(N+1)th ask posted: posts=%d", len(f.posts))
	}
	held := obs[2].Key.Hash()
	var found bool
	for _, e := range es {
		if e.Key.Hash() != held {
			continue
		}
		found = true
		if !e.Overflowed || e.State != Actionable || !e.WouldNotify || !e.PagedAt.IsZero() {
			t.Fatalf("overflowed ask not kept open: %+v", e)
		}
	}
	if !found {
		t.Fatal("overflowed ask dropped from the ledger")
	}
	open := s.OpenEpisodes("org/repo")
	if len(open) != 3 {
		t.Fatalf("overflowed ask missing from the action queue: %d open", len(open))
	}
	over := s.Budget().Overflowed("org/repo", "maintainer", 1)
	if len(over) != 1 || over[0].Key != held {
		t.Fatalf("overflowed ask not visible: %+v", over)
	}
	if !strings.Contains(attentionbudget.RenderSection(over), "#1") || !strings.Contains(attentionbudget.RenderDigest("maintainer", over), "#1") {
		t.Fatal("overflowed ask not rendered for the status card and digest")
	}
	// The next report retries, still held, and never double-posts.
	reportWith(t, s, cfg, f, obs...)
	if len(f.posts) != 2 {
		t.Fatalf("held ask posted while over budget: posts=%d", len(f.posts))
	}
	// Resolving the episode clears the overflowed entry.
	resolved := obs[2]
	resolved.Resolved = true
	reportWith(t, s, cfg, f, obs[0], obs[1], resolved)
	if over := s.Budget().Overflowed("org/repo", "", 0); len(over) != 0 {
		t.Fatalf("resolved ask still listed: %+v", over)
	}
}

func TestBudgetNotOptedInKeepsNewAsks(t *testing.T) {
	s := openStore(t, "")
	f := &forge{}
	reportWith(t, s, budgetConfig(1), f, asks(3, "")...)
	if len(f.posts) != 3 {
		t.Fatalf("recipient without digest lost a new ask: posts=%d", len(f.posts))
	}
}

func TestBudgetExemptClassesAlwaysDeliver(t *testing.T) {
	for _, class := range []attentionbudget.Class{attentionbudget.Security, attentionbudget.RequiredReview, attentionbudget.ProtocolCommand, attentionbudget.Hold} {
		t.Run(string(class), func(t *testing.T) {
			s := openStore(t, "")
			f := &forge{}
			cfg := budgetConfig(1, "maintainer")
			obs := append(asks(1, ""), asks(3, class)...)
			es := reportWith(t, s, cfg, f, obs...)
			if len(f.posts) != 4 {
				t.Fatalf("exempt %s ask held by budget: posts=%d", class, len(f.posts))
			}
			for _, e := range es {
				if e.Overflowed {
					t.Fatalf("exempt episode overflowed: %+v", e)
				}
			}
			for _, e := range es {
				if e.Class != class {
					continue
				}
				if d, err := s.AdmitReminder(cfg, e, time.Now()); err != nil || d.Outcome != attentionbudget.Deliver {
					t.Fatalf("exempt %s reminder held: %+v %v", class, d, err)
				}
			}
		})
	}
}

func TestBudgetReminderOverflowAndSeedFromLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "episodes.json")
	s := openStore(t, path)
	f := &forge{}
	cfg := budgetConfig(1, "maintainer")
	es := reportWith(t, s, cfg, f, asks(1, "")...)
	if len(f.posts) != 1 || len(es) != 1 {
		t.Fatalf("posts=%d", len(f.posts))
	}
	// A restart reseeds the window from paged_to/paged_at.
	s = openStore(t, path)
	d, err := s.AdmitReminder(cfg, es[0], time.Now())
	if err != nil || d.Outcome != attentionbudget.Overflow || !d.Digest {
		t.Fatalf("reminder over a seeded budget = %+v %v", d, err)
	}
	if got := s.Snapshot("org/repo", 1); len(got) != 1 || !got[0].Overflowed {
		t.Fatalf("held reminder not recorded: %+v", got)
	}
	if over := s.Budget().Overflowed("org/repo", "maintainer", 0); len(over) != 1 || over[0].Class != attentionbudget.Reminder {
		t.Fatalf("held reminder not visible: %+v", over)
	}
	if _, err := s.Apply("org/repo", 1, "maintainer", false, Command{Kind: CommandAck, Target: es[0].Key.Hash()[:12]}); err != nil {
		t.Fatal(err)
	}
	if over := s.Budget().Overflowed("org/repo", "", 0); len(over) != 0 {
		t.Fatalf("acknowledged reminder still listed: %+v", over)
	}
	// Disabled, the same reminder is delivered.
	if d, err := s.AdmitReminder(testConfig, es[0], time.Now()); err != nil || d.Outcome != attentionbudget.Deliver {
		t.Fatalf("disabled budget held a reminder: %+v %v", d, err)
	}
}

func TestConfigValidatesBudget(t *testing.T) {
	for _, cfg := range []Config{budgetConfig(-1), budgetConfig(attentionbudget.MaxRecipientBudget + 1), budgetConfig(1, "a", "A")} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted invalid budget config: %+v", cfg.Config)
		}
	}
	if err := budgetConfig(3, "alice").Validate(); err != nil {
		t.Fatal(err)
	}
}
