package attention

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type blockingForge struct {
	forge
	entered chan struct{}
	release chan struct{}
}

func (b *blockingForge) History(ctx context.Context, repo string, item int) ([]string, error) {
	close(b.entered)
	<-b.release
	return b.forge.History(ctx, repo, item)
}

func TestReportDoesNotHoldStoreLockAcrossDelivery(t *testing.T) {
	s := openStore(t, "")
	slow := &blockingForge{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, slow)
		done <- err
	}()
	<-slow.entered

	other := observation()
	other.Key.Item = 2
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.Snapshot("org/repo", 1)
		if _, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 2, []Observation{other}, &forge{}); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot/Report on another item blocked behind a Delivery call")
	}
	close(slow.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUncertainAbsentRetryDoesNotDeadlock(t *testing.T) {
	s := openStore(t, "")
	f := &forge{fail: true}
	_, _ = s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
	f.landed = map[string]bool{}
	f.fail = false
	done := make(chan error, 1)
	go func() {
		_, err := s.Report(context.Background(), testConfig, "owner", "org/repo", 1, []Observation{observation()}, f)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("uncertain-POST retry deadlocked on the store lock")
	}
	if len(f.posts) != 2 || f.finds != 1 {
		t.Fatalf("posts=%d finds=%d", len(f.posts), f.finds)
	}
	if s.Snapshot("org/repo", 1)[0].Uncertain {
		t.Fatal("successful retry left the episode uncertain")
	}
}

func TestItemCapEvictsResolvedItems(t *testing.T) {
	s := openStore(t, "")
	base := time.Now().UTC()
	for i := 0; i < MaxItems; i++ {
		k := fmt.Sprintf("org/other#%d", i+1)
		s.state.Items[k] = true
		s.state.ItemSeen[k] = base.Add(time.Duration(i) * time.Second)
		s.state.Episodes[fmt.Sprint(i)] = &Episode{Key: Key{Repo: "org/other", Item: i + 1}, State: Resolved, ResolvedAt: base}
	}
	f := &forge{}
	report(t, s, f, "owner", observation())
	if len(s.state.Items) != MaxItems || !s.state.Items["org/repo#1"] || s.state.Items["org/other#1"] || !s.state.Items["org/other#2"] {
		t.Fatal("oldest resolved item was not evicted in favour of the new one")
	}
	if len(f.posts) != 1 {
		t.Fatalf("posts=%d", len(f.posts))
	}
}

func TestLedgerWithoutItemSeenStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "episodes.json")
	legacy := `{"version":1,"items":{"org/repo#1":true},"episodes":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, path)
	if !s.state.Items["org/repo#1"] || s.state.ItemSeen == nil {
		t.Fatalf("legacy ledger not loaded: %+v", s.state)
	}
	report(t, s, &forge{}, "owner", observation())
}
