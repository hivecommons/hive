package attention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenInvalidLedgers(t *testing.T) {
	for _, body := range []string{
		`{"version":0,"items":{},"episodes":{}}`,
		`{"version":1,"episodes":{}}`,
		`{"version":1,"items":{}}`,
		`{"version":1,"items":{},"episodes":{"key":null}}`,
	} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			s := openStore(t, path)
			f := &forge{history: []string{observation().Key.Hash()}}
			es := report(t, s, f, "owner", observation())
			if len(es) != 1 || !es[0].Baseline || len(f.posts) != 0 {
				t.Fatalf("replayed invalid ledger: %+v", es)
			}
		})
	}
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("accepted unreadable ledger directory")
	}
	for _, items := range []bool{true, false} {
		d := disk{Version: 1, Items: map[string]bool{}, Episodes: map[string]*Episode{}}
		for i := 0; i <= MaxEpisodes; i++ {
			key := fmt.Sprint(i)
			if items {
				d.Items[key] = true
			} else {
				d.Episodes[key] = &Episode{}
			}
		}
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "state")
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err == nil {
			t.Fatal("accepted oversized ledger")
		}
	}
}

func TestReportRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	for _, mutate := range []func(*Observation){
		func(o *Observation) { o.Key.Repo = "other/repo" },
		func(o *Observation) { o.Key.Item = 2 },
		func(o *Observation) { o.Key.Action = "" },
		func(o *Observation) { o.Key.Recipient = "" },
		func(o *Observation) { o.Key.Subject = "" },
	} {
		s := openStore(t, "")
		o := observation()
		mutate(&o)
		if _, err := s.Report(ctx, testConfig, "owner", "org/repo", 1, []Observation{o}, &forge{}); err == nil {
			t.Fatal("accepted invalid observation")
		}
		if len(s.state.Items) != 0 || len(s.state.Episodes) != 0 {
			t.Fatal("invalid input mutated ledger")
		}
	}
	s := openStore(t, "")
	if _, err := s.Report(ctx, Config{FlapWindow: -1}, "owner", "org/repo", 1, nil, &forge{}); err == nil {
		t.Fatal("accepted invalid config")
	}
	if _, err := s.Report(ctx, testConfig, "owner", "org/repo", 0, nil, &forge{}); err == nil {
		t.Fatal("accepted invalid item")
	}
	if _, err := s.Report(ctx, testConfig, "owner", "org/repo", 1, nil, nil); err == nil {
		t.Fatal("accepted nil delivery")
	}
}

func TestRecoveryFailuresFenceDelivery(t *testing.T) {
	for _, mode := range []string{"history", "items", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			s := openStore(t, "")
			f := &forge{}
			switch mode {
			case "history":
				f.historyErr = errors.New("history unavailable")
			case "items":
				for i := 0; i < MaxItems; i++ {
					s.state.Items[fmt.Sprintf("org/other#%d", i+1)] = true
					s.state.Episodes[fmt.Sprint(i)] = &Episode{Key: Key{Repo: "org/other", Item: i + 1}}
				}
			case "recovery":
				for i := 0; i < MaxEpisodes; i++ {
					s.state.Episodes[fmt.Sprint(i)] = &Episode{}
				}
				f.history = []string{observation().Key.Hash()}
			}
			if _, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f); err == nil {
				t.Fatal("accepted failed recovery")
			}
			if len(f.posts) != 0 || s.state.Items["org/repo#1"] {
				t.Fatal("failed recovery allowed delivery or established baseline")
			}
		})
	}
}

func TestSaveFailsOnInvalidTimeAndParent(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "state"))
	s.state.Episodes["invalid"] = &Episode{FirstSeen: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := s.save(); err == nil {
		t.Fatal("saved unencodable timestamp")
	}
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(parent, "state")
	s.state.Episodes = map[string]*Episode{}
	if err := s.save(); err == nil {
		t.Fatal("saved beneath a regular file")
	}
}

func TestUpdateTransitionsAndQuietAcknowledgedReopen(t *testing.T) {
	s := openStore(t, "")
	o := observation()
	f := &forge{history: []string{o.Key.Hash()}} // Seeded baseline: no page, so ack/snooze transitions are exercised from actionable.
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := s.Update(o.Key, Acknowledged, time.Time{}); err == nil {
		t.Fatal("updated unknown episode")
	}
	report(t, s, f, "owner", o)
	if err := s.Update(o.Key, Notified, time.Time{}); err == nil {
		t.Fatal("accepted unsupported transition")
	}
	snooze := now.Add(time.Hour)
	if err := s.Update(o.Key, "", snooze); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot("org/repo", 1)[0]; !got.SnoozedUntil.Equal(snooze) || got.State != Actionable {
		t.Fatalf("snooze changed state: %+v", got)
	}
	if err := s.Update(o.Key, Acknowledged, snooze); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := s.Update(o.Key, Acknowledged, snooze); err != nil {
		t.Fatal(err)
	}
	acked := s.Snapshot("org/repo", 1)[0].AckedAt
	if acked.Equal(now) {
		t.Fatal("duplicate acknowledgement reset timestamp")
	}
	if err := s.Update(o.Key, Resolved, time.Time{}); err != nil {
		t.Fatal(err)
	}
	resolved := s.Snapshot("org/repo", 1)[0].ResolvedAt
	now = now.Add(time.Minute)
	if err := s.Update(o.Key, Resolved, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !s.Snapshot("org/repo", 1)[0].ResolvedAt.Equal(resolved) {
		t.Fatal("duplicate resolution reset timestamp")
	}
	if err := s.Update(o.Key, Acknowledged, time.Time{}); err == nil {
		t.Fatal("acknowledged resolved episode")
	}
	es := report(t, s, f, "owner", o)
	if es[0].State != Acknowledged || !es[0].QuietReopen || !es[0].AckedAt.Equal(acked) || len(f.posts) != 0 {
		t.Fatalf("acknowledged flap lost history: %+v", es)
	}
}

func TestHandoffDoesNotRetryPreviousOwnersAttempt(t *testing.T) {
	s := openStore(t, "")
	f := &forge{fail: true}
	if _, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f); err == nil {
		t.Fatal("expected uncertain POST")
	}
	cfg := Config{Admitter: map[string]string{"org/repo": "replacement"}}
	es, err := s.Report(context.Background(), cfg, "replacement", "org/repo", 1, []Observation{observation()}, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 || f.finds != 0 || !es[0].Uncertain || es[0].AttemptOwner != "owner" {
		t.Fatalf("handoff retried old attempt: %+v", es)
	}
}

func TestDecisionSubjectTrimsIdentity(t *testing.T) {
	if got := DecisionSubject(" decision-42 \n"); got != "decision:decision-42" {
		t.Fatal(got)
	}
}

func TestUncertainResolvedEpisodeReconcilesAndPrunes(t *testing.T) {
	s := openStore(t, "")
	f := &forge{fail: true}
	ctx := context.Background()
	if _, err := s.Report(ctx, testConfig, "owner", "org/repo", 1, []Observation{observation()}, f); err == nil {
		t.Fatal("expected uncertain POST")
	}
	o := observation()
	o.Resolved = true
	es, err := s.Report(ctx, testConfig, "owner", "org/repo", 1, []Observation{o}, f)
	if err != nil {
		t.Fatal(err)
	}
	if f.finds != 1 || es[0].Uncertain || es[0].PagedAt.IsZero() {
		t.Fatalf("uncertain POST not reconciled on resolve: %+v", es)
	}
	s.now = func() time.Time { return time.Now().Add(2 * testConfig.Window()) }
	s.prune(s.now().UTC(), testConfig.Window())
	if len(s.state.Episodes) != 0 || len(s.state.Items) != 0 {
		t.Fatal("resolved episode and item not pruned")
	}
}

func TestFirstActionableReopenOfNeverPagedEpisodePages(t *testing.T) {
	s := openStore(t, "")
	f := &forge{}
	ctx := context.Background()
	o := observation()
	o.Actionable = false
	if _, err := s.Report(ctx, testConfig, "owner", "org/repo", 1, []Observation{o}, f); err != nil {
		t.Fatal(err)
	}
	o.Resolved = true
	if _, err := s.Report(ctx, testConfig, "owner", "org/repo", 1, []Observation{o}, f); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Report(ctx, testConfig, "owner", "org/repo", 1, []Observation{observation()}, f); err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 {
		t.Fatalf("never-paged reopen not paged: %d", len(f.posts))
	}
}
