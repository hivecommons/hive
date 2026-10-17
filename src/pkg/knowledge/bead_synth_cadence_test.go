package knowledge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

type beadPublisherFunc func(context.Context, []ExtractedFact) error

func (f beadPublisherFunc) PublishFacts(ctx context.Context, facts []ExtractedFact) error {
	return f(ctx, facts)
}

func TestBeadPublishCadenceDaily(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	var batches [][]ExtractedFact
	batch, err := newBeadPublishBatch(beadPublisherFunc(func(_ context.Context, facts []ExtractedFact) error {
		batches = append(batches, facts)
		return nil
	}), "daily", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	flush := func() {
		t.Helper()
		if err := batch.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	batch.enqueue(ExtractedFact{Title: "Beta", Body: "old"})
	flush()
	now = now.Add(23 * time.Hour)
	batch.enqueue(ExtractedFact{Title: "Alpha"})
	batch.enqueue(ExtractedFact{Title: "Beta", Body: "updated"})
	flush()
	if len(batches) != 0 {
		t.Fatal("daily batch published before deadline")
	}
	now = now.Add(time.Hour)
	flush()
	flush()
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("want one two-fact batch, got %+v", batches)
	}
	if batches[0][0].Title != "Alpha" || batches[0][1].Body != "updated" {
		t.Fatalf("batch did not preserve stable ordering/newest slug: %+v", batches[0])
	}
	batch.enqueue(ExtractedFact{Title: "Next day"})
	flush()
	if len(batches) != 1 {
		t.Fatal("new daily window flushed immediately")
	}
	now = now.Add(24 * time.Hour)
	flush()
	if len(batches) != 2 {
		t.Fatal("next daily window did not flush")
	}
}

func TestBeadPublishCadencePerRun(t *testing.T) {
	for _, cadence := range []string{"", "per_run"} {
		t.Run(cadence, func(t *testing.T) {
			calls := 0
			batch, err := newBeadPublishBatch(beadPublisherFunc(func(_ context.Context, facts []ExtractedFact) error {
				calls++
				if len(facts) != 2 {
					t.Fatalf("want a single batch of two facts, got %d", len(facts))
				}
				return nil
			}), cadence, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			batch.enqueue(ExtractedFact{Title: "One"})
			batch.enqueue(ExtractedFact{Title: "Two"})
			if err := batch.flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := batch.flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || batch.hasPending() {
				t.Fatal("batch not cleared after successful publish")
			}
		})
	}
}

func TestBeadPublishCadenceRetry(t *testing.T) {
	now := time.Now()
	calls := 0
	batch, err := newBeadPublishBatch(beadPublisherFunc(func(_ context.Context, facts []ExtractedFact) error {
		calls++
		if facts[0].Tags[0] != "original" {
			t.Fatal("publisher mutated retained batch")
		}
		facts[0].Tags[0] = "mutated"
		if calls == 1 {
			return errors.New("publish unavailable")
		}
		return nil
	}), "daily", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	batch.enqueue(ExtractedFact{Title: "Retry", Tags: []string{"original"}})
	now = now.Add(24 * time.Hour)
	if err := batch.flush(context.Background()); err == nil || !batch.hasPending() {
		t.Fatal("failed publish must retain pending facts")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := batch.flush(ctx); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("cancelled flush must not publish")
	}
	if err := batch.flush(context.Background()); err != nil || batch.hasPending() || calls != 2 {
		t.Fatalf("retry failed: calls=%d err=%v", calls, err)
	}
}

func TestSetBeadPublisherValidation(t *testing.T) {
	s := &BeadSynthesizer{}
	if err := s.SetFactPublisher(nil, "weekly"); err == nil {
		t.Fatal("invalid cadence accepted")
	}
	if err := s.SetFactPublisher(nil, "daily"); err != nil {
		t.Fatal(err)
	}
	s.publisher.enqueue(ExtractedFact{Title: "Disabled"})
	if s.publisher.hasPending() {
		t.Fatal("nil publisher must not accumulate work")
	}
	s.running = true
	if err := s.SetFactPublisher(nil, "daily"); err == nil {
		t.Fatal("running publisher replaced")
	}
}

func TestRunSynthesisPublishesOnEmptyCycle(t *testing.T) {
	for _, cadence := range []string{"per_run", "daily"} {
		t.Run(cadence, func(t *testing.T) {
			srv, _ := newMockWikiServer(t)
			store := newTestStore(t)
			b, err := store.Create("Publish this pattern", beads.TypeBug, beads.PriorityHigh, "scanner", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Update(b.ID, func(b *beads.Bead) { b.Notes = "Always validate a completed fact before publishing." }); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(b.ID); err != nil {
				t.Fatal(err)
			}
			s := newTestSynthesizer(t, map[string]*beads.Store{"scanner": store}, srv.URL)
			now := time.Now()
			calls := 0
			if err := s.SetFactPublisher(beadPublisherFunc(func(_ context.Context, facts []ExtractedFact) error {
				calls++
				if len(facts) != 1 || facts[0].Title != b.Title {
					t.Fatalf("unexpected batch: %+v", facts)
				}
				if cadence == "per_run" && calls == 1 {
					return errors.New("temporary failure")
				}
				return nil
			}), cadence); err != nil {
				t.Fatal(err)
			}
			s.publisher.now = func() time.Time { return now }
			if count, err := s.RunSynthesis(context.Background()); err != nil || count != 1 {
				t.Fatalf("local synthesis affected by publisher: count=%d err=%v", count, err)
			}
			if err := s.SetFactPublisher(nil, cadence); err == nil {
				t.Fatal("pending batch discarded by replacement")
			}
			now = now.Add(24 * time.Hour)
			if count, err := s.RunSynthesis(context.Background()); err != nil || count != 0 {
				t.Fatalf("expected empty cycle: count=%d err=%v", count, err)
			}
			wantCalls := 1
			if cadence == "per_run" {
				wantCalls = 2
			}
			if calls != wantCalls || s.publisher.hasPending() {
				t.Fatalf("empty cycle did not flush: calls=%d", calls)
			}
		})
	}
}

func TestBeadPublishLocalVaultIsolation(t *testing.T) {
	for _, failLocal := range []bool{false, true} {
		t.Run(fmt.Sprint(failLocal), func(t *testing.T) {
			store := newTestStore(t)
			b, err := store.Create("Vault isolation", beads.TypeBug, beads.PriorityHigh, "scanner", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Update(b.ID, func(b *beads.Bead) { b.Notes = "Always retain local knowledge when publishing fails." }); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(b.ID); err != nil {
				t.Fatal(err)
			}
			s := newTestSynthesizer(t, map[string]*beads.Store{"scanner": store}, "")
			if failLocal {
				blocker := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(blocker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				s.vaultBaseDir = blocker
			}
			calls := 0
			if err := s.SetFactPublisher(beadPublisherFunc(func(context.Context, []ExtractedFact) error {
				calls++
				return errors.New("publish failed")
			}), "per_run"); err != nil {
				t.Fatal(err)
			}
			count, err := s.RunSynthesis(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if failLocal {
				if count != 0 || calls != 0 || s.publisher.hasPending() {
					t.Fatal("failed local ingestion was published")
				}
				return
			}
			if count != 1 || calls != 1 || !s.publisher.hasPending() {
				t.Fatal("publish failure affected local synthesis or lost pending fact")
			}
			if _, err := os.Stat(filepath.Join(s.vaultBaseDir, slugify(b.Title)+".md")); err != nil {
				t.Fatalf("local vault write missing: %v", err)
			}
		})
	}
}
