package attentionbudget

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/gfi"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func fixed(c Config) func() Config { return func() Config { return c } }

func ask(recipient string, class Class, item int, at time.Time) Ask {
	return Ask{Repo: "hivecommons/hive", Item: item, Recipient: recipient, Class: class, Key: "k" + string(rune('a'+item)), Summary: "please look", At: at}
}

func TestDisabledByDefaultIsNoOp(t *testing.T) {
	var zero Config
	if zero.Enabled() || zero.Validate() != nil {
		t.Fatalf("zero config must be valid and disabled")
	}
	b := New(nil)
	for i := 0; i < 20; i++ {
		d := b.Admit(ask("alice", Reminder, i%10, t0.Add(time.Duration(i)*time.Minute)))
		if d.Outcome != Deliver {
			t.Fatalf("write %d: outcome %q with budget off; want deliver", i, d.Outcome)
		}
	}
	if got := b.Overflowed("hivecommons/hive", "", 0); len(got) != 0 {
		t.Fatalf("budget off must not record overflow, got %d", len(got))
	}
}

func TestShadowCountsWouldOverflowWithoutChangingWrites(t *testing.T) {
	b := New(fixed(Config{}))
	n := ShadowBudget + 3
	for i := 0; i < n; i++ {
		d := b.Admit(ask("Alice", Ping, i, t0.Add(time.Duration(i)*time.Minute)))
		if d.Outcome != Deliver {
			t.Fatalf("shadow mode changed write %d to %q", i, d.Outcome)
		}
		if want := i >= ShadowBudget; d.WouldOverflow != want {
			t.Fatalf("write %d: WouldOverflow=%v want %v", i, d.WouldOverflow, want)
		}
	}
	c := b.Snapshot()["alice"]
	if c.WouldOverflow != 3 || c.Delivered != n || c.Overflowed != 0 {
		t.Fatalf("shadow tally = %+v", c)
	}
	if line := b.LogLine(); !strings.Contains(line, "alice") || !strings.Contains(line, "would_overflow=3") || !strings.Contains(line, "off (shadow") {
		t.Fatalf("log line %q missing shadow counter", line)
	}
}

func TestEnabledOverflowIsRecordedNotDropped(t *testing.T) {
	cases := []struct {
		name       string
		cfg        Config
		class      Class
		want       Outcome
		wantDigest bool
	}{
		{"opted-in ping goes to digest", Config{RecipientBudget: 2, DigestRecipients: []string{"@Alice"}}, Ping, Overflow, true},
		{"opted-in reminder goes to digest", Config{RecipientBudget: 2, DigestRecipients: []string{"alice"}}, Reminder, Overflow, true},
		{"not opted-in reminder deferred to queue", Config{RecipientBudget: 2}, Reminder, Overflow, false},
		{"not opted-in new ask keeps per-episode delivery", Config{RecipientBudget: 2}, Ping, Deliver, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New(fixed(tc.cfg))
			for i := 0; i < tc.cfg.RecipientBudget; i++ {
				if d := b.Admit(ask("alice", tc.class, i, t0.Add(time.Duration(i)*time.Minute))); d.Outcome != Deliver {
					t.Fatalf("write %d within budget: %q", i, d.Outcome)
				}
			}
			extra := ask("alice", tc.class, 7, t0.Add(time.Hour))
			d := b.Admit(extra)
			if d.Outcome != tc.want || d.Digest != tc.wantDigest {
				t.Fatalf("N+1th ask: %+v; want outcome %q digest %v", d, tc.want, tc.wantDigest)
			}
			open := b.Overflowed("HiveCommons/Hive", "ALICE", 7)
			if tc.want == Overflow {
				if len(open) != 1 || open[0].Key != extra.Key {
					t.Fatalf("overflowed ask not recorded: %+v", open)
				}
				if s := RenderSection(open); !strings.Contains(s, "#7") || !strings.Contains(s, "alice") {
					t.Fatalf("status/queue section does not show the ask: %q", s)
				}
				b.Resolve("hivecommons/hive", extra.Key)
				if got := b.Overflowed("hivecommons/hive", "", 0); len(got) != 0 {
					t.Fatalf("resolve left %d entries", len(got))
				}
			} else if len(open) != 0 {
				t.Fatalf("delivered ask must not stay overflowed")
			}
		})
	}
}

func TestWindowIsRollingAndSeeded(t *testing.T) {
	b := New(fixed(Config{RecipientBudget: 1, DigestRecipients: []string{"alice"}}))
	b.now = func() time.Time { return t0 }
	b.Seed([]Write{
		{Recipient: "Alice", At: t0.Add(-time.Hour), Class: Reminder},
		{Recipient: "alice", At: t0.Add(-2 * time.Hour), Class: Security},
	})
	if d := b.Admit(ask("alice", Reminder, 1, t0)); d.Outcome != Overflow || d.Count != 1 {
		t.Fatalf("seeded write should consume the budget (exempt ones not): %+v", d)
	}
	if d := b.Admit(ask("alice", Reminder, 2, t0.Add(Window))); d.Outcome != Deliver {
		t.Fatalf("budget must free up after the rolling window: %+v", d)
	}
}

func TestExemptClassesBypassBudget(t *testing.T) {
	for _, class := range []Class{Security, RequiredReview, ProtocolCommand, Hold} {
		t.Run(string(class), func(t *testing.T) {
			b := New(fixed(Config{RecipientBudget: 1, DigestRecipients: []string{"alice"}}))
			b.Admit(ask("alice", Ping, 1, t0))
			for i := 0; i < 5; i++ {
				d := b.Admit(ask("alice", class, 2+i, t0.Add(time.Duration(i+1)*time.Minute)))
				if d.Outcome != Deliver {
					t.Fatalf("exempt %s write %d was %q", class, i, d.Outcome)
				}
			}
			if d := b.Admit(ask("alice", Reminder, 9, t0.Add(time.Hour))); d.Count != 1 {
				t.Fatalf("exempt writes consumed budget: count=%d", d.Count)
			}
			if c := b.Snapshot()["alice"]; c.Exempt != 5 {
				t.Fatalf("exempt tally = %+v", c)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		cfg Config
		ok  bool
	}{
		{Config{}, true},
		{Config{RecipientBudget: 5, DigestRecipients: []string{"alice", "@bob"}}, true},
		{Config{RecipientBudget: -1}, false},
		{Config{RecipientBudget: MaxRecipientBudget + 1}, false},
		{Config{DigestRecipients: []string{" "}}, false},
		{Config{DigestRecipients: []string{"org/team"}}, false},
		{Config{DigestRecipients: []string{"alice", "@Alice"}}, false},
	}
	for _, tc := range cases {
		if err := tc.cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("Validate(%+v) = %v; want ok=%v", tc.cfg, err, tc.ok)
		}
	}
}

type fakeForge struct {
	comments []string
	creates  int
	edits    int
}

func (f *fakeForge) UpsertIssueMarkerComment(_ context.Context, _ string, _ int, marker, body string, create bool) (gfi.MarkerComment, error) {
	for i, c := range f.comments {
		if !strings.Contains(c, marker) {
			continue
		}
		res := gfi.MarkerComment{Found: true, Previous: c}
		if body != "" && body != c {
			f.comments[i] = body
			f.edits++
		}
		return res, nil
	}
	if !create || body == "" {
		return gfi.MarkerComment{}, nil
	}
	f.comments = append(f.comments, body)
	f.creates++
	return gfi.MarkerComment{Created: true}, nil
}

func TestDigestCreatedOnceThenEditedInPlace(t *testing.T) {
	ctx := context.Background()
	f := &fakeForge{}
	if _, err := UpsertDigest(ctx, f, "hivecommons/hive", 42, "alice", nil); err != nil || f.creates != 0 {
		t.Fatalf("empty digest must not create a comment (creates=%d err=%v)", f.creates, err)
	}
	day1 := []Ask{ask("alice", Reminder, 1, t0)}
	day2 := append(day1, ask("alice", Ping, 2, t0.Add(24*time.Hour)))
	for _, asks := range [][]Ask{day1, day2, day2, nil} {
		if _, err := UpsertDigest(ctx, f, "hivecommons/hive", 42, "@Alice", asks); err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 1 || len(f.comments) != 1 {
		t.Fatalf("digest created %d times (%d comments); want exactly once", f.creates, len(f.comments))
	}
	if f.edits != 2 {
		t.Fatalf("digest edits = %d; want 2 (unchanged body is not rewritten)", f.edits)
	}
	if !strings.Contains(f.comments[0], "Nothing outstanding") {
		t.Fatalf("cleared digest body = %q", f.comments[0])
	}
	if _, err := UpsertDigest(ctx, f, "hivecommons/hive", 42, "bob", day1); err != nil || len(f.comments) != 2 {
		t.Fatalf("each recipient gets its own digest comment (comments=%d err=%v)", len(f.comments), err)
	}
}

func TestDigestMarkerDoesNotMatchPrefixLogin(t *testing.T) {
	if strings.Contains(DigestMarker("alice"), DigestMarker("al")) {
		t.Fatalf("marker for a prefix login must not match another recipient's digest")
	}
}
