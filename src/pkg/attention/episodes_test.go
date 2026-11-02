package attention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type forge struct {
	posts   []Episode
	landed  map[string]bool
	history []string
	fail    bool
	finds   int
	findErr error
}

func (f *forge) History(context.Context, string, int) ([]string, error) { return f.history, nil }
func (f *forge) Find(_ context.Context, e Episode) (bool, error) {
	f.finds++
	return f.landed[e.Attempt], f.findErr
}
func (f *forge) Post(_ context.Context, e Episode, _ string) error {
	f.posts = append(f.posts, e)
	if f.landed == nil {
		f.landed = map[string]bool{}
	}
	f.landed[e.Attempt] = true
	if f.fail {
		return errors.New("timeout after server accepted POST")
	}
	return nil
}

var testConfig = Config{Admitter: map[string]string{"org/repo": "owner"}}

func observation() Observation {
	return Observation{Key: Key{Repo: "org/repo", Item: 1, Action: "repair-ci", Recipient: "maintainer", Subject: CISubject("required ARM64", "download-http2")}, Actionable: true, Evidence: "sha-1/run-1", Body: "Please repair the download"}
}
func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func report(t *testing.T, s *Store, f *forge, owner string, obs ...Observation) []Episode {
	t.Helper()
	es, err := s.Report(context.Background(), testConfig, owner, "org/repo", 1, obs, f)
	if err != nil {
		t.Fatal(err)
	}
	return es
}
func TestAcrossProducersPushesAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "episodes.json")
	s := openStore(t, path)
	f := &forge{}
	report(t, s, f, "owner") // Initial inventory is empty: establish recovery baseline.
	o := observation()
	report(t, s, f, "owner", o)
	for _, producer := range []string{"scanner", "runtime", "Actions"} {
		o.Body = producer + " describes the same underlying failure differently"
		o.Evidence = producer + "/new-sha/new-run"
		report(t, s, f, "owner", o)
	}
	s = openStore(t, path)
	es := report(t, s, f, "owner", o)
	if len(f.posts) != 1 || len(es) != 1 || es[0].Evidence != o.Evidence || es[0].PagedTo != "maintainer" {
		t.Fatalf("posts=%d episodes=%+v", len(f.posts), es)
	}
}
func TestEmptyAndCorruptStateNeverReplay(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "corrupt"}[corrupt], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "episodes.json")
			if corrupt {
				if err := os.WriteFile(path, []byte("truncated{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s := openStore(t, path)
			f := &forge{}
			o := observation()
			es := report(t, s, f, "owner", o)
			report(t, s, f, "owner", o)
			if len(f.posts) != 0 || !es[0].Baseline {
				t.Fatal("history replayed", es)
			}
			// A genuinely new urgent risk is independent of the baseline CI episode.
			risk := o
			risk.Key.Action = "urgent-security"
			risk.Key.Subject = "new-rule:file:location"
			report(t, s, f, "owner", o, risk)
			if len(f.posts) != 1 || f.posts[0].Key.Action != "urgent-security" {
				t.Fatal("new security risk suppressed")
			}
		})
	}
}
func TestRecoveryRetainsMarkersNotInFirstInventory(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "state"))
	o := observation()
	f := &forge{history: []string{o.Key.Hash()}}
	report(t, s, f, "owner")
	es := report(t, s, f, "owner", o)
	if len(f.posts) != 0 || !es[0].Baseline {
		t.Fatal("marker history replayed")
	}
}
func TestUncertainLandedPOSTReconcilesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	s := openStore(t, path)
	f := &forge{fail: true}
	report(t, s, f, "owner")
	_, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
	if err == nil {
		t.Fatal("expected timeout")
	}
	pending := s.Snapshot("org/repo", 1)[0]
	if !pending.Uncertain || pending.Attempt == "" {
		t.Fatal("attempt not durable")
	}
	s = openStore(t, path)
	es := report(t, s, f, "owner", observation())
	if len(f.posts) != 1 || f.finds != 1 || es[0].Uncertain || es[0].State != Notified || es[0].Attempt != pending.Attempt {
		t.Fatalf("reconciliation failed: %+v", es)
	}
}
func TestUncertainAbsentRetryKeepsAttempt(t *testing.T) {
	s := openStore(t, "")
	f := &forge{fail: true}
	report(t, s, f, "owner")
	_, _ = s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
	f.landed = map[string]bool{}
	f.fail = false
	report(t, s, f, "owner", observation())
	if len(f.posts) != 2 || f.finds != 1 || f.posts[0].Attempt != f.posts[1].Attempt {
		t.Fatal("retry minted another episode/attempt")
	}
}
func TestFailedLookupNeverRetries(t *testing.T) {
	s := openStore(t, "")
	f := &forge{fail: true}
	report(t, s, f, "owner")
	_, _ = s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
	f.findErr = errors.New("GitHub unavailable")
	_, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
	if err == nil || len(f.posts) != 1 {
		t.Fatal("retried without successful reconciliation")
	}
}
func TestFlappingAndGenuineRecurrence(t *testing.T) {
	s := openStore(t, "")
	f := &forge{}
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	report(t, s, f, "owner")
	o := observation()
	report(t, s, f, "owner", o)
	first := f.posts[0].Attempt
	o.Resolved = true
	report(t, s, f, "owner", o)
	now = now.Add(time.Hour)
	o.Resolved = false
	report(t, s, f, "owner", o)
	o.Resolved = true
	report(t, s, f, "owner", o)
	if len(f.posts) != 1 {
		t.Fatal("flap repaged")
	}
	now = now.Add(DefaultFlapWindow + time.Second)
	o.Resolved = false
	report(t, s, f, "owner", o)
	if len(f.posts) != 2 || first == f.posts[1].Attempt {
		t.Fatal("genuine recurrence did not start a new episode")
	}
}
func TestNonAdmitterAndUnset(t *testing.T) {
	s := openStore(t, "")
	f := &forge{}
	report(t, s, f, "other")
	es := report(t, s, f, "other", observation())
	if len(f.posts) != 0 || len(es) != 1 || !es[0].WouldNotify {
		t.Fatal("non-admitter posted or lost shadow decision")
	}
	_, err := s.Report(context.Background(), Config{}, "owner", "org/repo", 1, []Observation{observation()}, f)
	if err != nil || len(f.posts) != 0 {
		t.Fatal("unset admitter changed behavior")
	}
}
func TestConcurrentProducers(t *testing.T) {
	s := openStore(t, "")
	f := &forge{}
	report(t, s, f, "owner")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(f.posts) != 1 {
		t.Fatalf("posts=%d", len(f.posts))
	}
}
func TestAckAndSnoozeDoNotResetDelivery(t *testing.T) {
	s := openStore(t, "")
	f := &forge{}
	report(t, s, f, "owner")
	o := observation()
	o.Actionable = false
	report(t, s, f, "owner", o)
	if err := s.Update(o.Key, Acknowledged, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	o.Actionable = true
	report(t, s, f, "owner", o)
	if len(f.posts) != 0 || s.Snapshot("org/repo", 1)[0].State != Acknowledged {
		t.Fatal("acknowledged request paged")
	}
}
func TestCapacityFailsClosedAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	s := openStore(t, path)
	f := &forge{}
	report(t, s, f, "owner")
	for i := 0; i < MaxEpisodes; i++ {
		s.state.Episodes[string(rune(i))] = &Episode{State: Observed}
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, path)
	_, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
	if err == nil || len(f.posts) != 0 || len(s.state.Episodes) != MaxEpisodes {
		t.Fatal("capacity evicted delivery history")
	}
}
func TestPersistenceFailureNeverPosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	s := openStore(t, path)
	f := &forge{}
	report(t, s, f, "owner")
	// A directory at the ledger destination makes atomic replacement fail.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
	if err == nil || len(f.posts) != 0 {
		t.Fatal("posted without durable reservation")
	}
}
func TestIdentityDimensions(t *testing.T) {
	k := observation().Key
	same := k
	same.Repo = "ORG/REPO"
	same.Recipient = "MAINTAINER"
	if k.Hash() != same.Hash() {
		t.Fatal("case changed identity")
	}
	for _, change := range []func(*Key){func(k *Key) { k.Recipient = "security-owner" }, func(k *Key) { k.Action = "new-decision" }, func(k *Key) { k.Subject = CISubject("required ARM64", "permission-denied") }} {
		changed := k
		change(&changed)
		if changed.Hash() == k.Hash() {
			t.Fatal("material change collapsed")
		}
	}
}
