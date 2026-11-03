package knowledge

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// BeadFactPublisher publishes one batch of synthesized facts. Implementations
// must enforce sensitivity/scope filters and the ACMM/tool-approval write gate;
// successful local ingestion alone is NOT authorization to publish a fact.
// PublishFacts must be idempotent: a failed batch is retained and retried.
// No repository publisher is installed by default.
type BeadFactPublisher interface {
	PublishFacts(context.Context, []ExtractedFact) error
}

// SetFactPublisher installs an optional publishing sink. Configure it before
// starting synthesis. Cadence is per_run (also the empty default) or daily.
// Daily batches wait 24 hours from the first queued fact, then flush on the
// next synthesis cycle, including cycles with no new facts. Pending batches
// are in memory only; callers must not replace a sink with pending work.
func (s *BeadSynthesizer) SetFactPublisher(p BeadFactPublisher, cadence string) error {
	batch, err := newBeadPublishBatch(p, cadence, time.Now)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return fmt.Errorf("cannot replace bead publisher while synthesizer is running")
	}
	if s.publisher != nil && s.publisher.hasPending() {
		return fmt.Errorf("cannot replace bead publisher with pending facts")
	}
	s.publisher = batch
	return nil
}

type beadPublishBatch struct {
	mu        sync.Mutex
	publisher BeadFactPublisher
	cadence   string
	now       func() time.Time
	pending   map[string]ExtractedFact
	due       time.Time
}

func newBeadPublishBatch(p BeadFactPublisher, cadence string, now func() time.Time) (*beadPublishBatch, error) {
	if cadence == "" {
		cadence = "per_run"
	}
	if cadence != "per_run" && cadence != "daily" {
		return nil, fmt.Errorf("invalid bead publish cadence %q", cadence)
	}
	return &beadPublishBatch{publisher: p, cadence: cadence, now: now, pending: make(map[string]ExtractedFact)}, nil
}

func (b *beadPublishBatch) hasPending() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending) > 0
}

func (b *beadPublishBatch) enqueue(f ExtractedFact) {
	if b == nil || b.publisher == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		b.due = b.now().Add(24 * time.Hour)
	}
	// Match the vault's stable-slug update semantics, retaining the newest fact.
	f.Tags = append([]string(nil), f.Tags...)
	f.Related = append([]string(nil), f.Related...)
	b.pending[slugify(f.Title)] = f
}

func (b *beadPublishBatch) flush(ctx context.Context) error {
	if b == nil || b.publisher == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 || (b.cadence == "daily" && b.now().Before(b.due)) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	keys := make([]string, 0, len(b.pending))
	for key := range b.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	facts := make([]ExtractedFact, 0, len(keys))
	for _, key := range keys {
		f := b.pending[key]
		f.Tags = append([]string(nil), f.Tags...)
		f.Related = append([]string(nil), f.Related...)
		facts = append(facts, f)
	}
	if err := b.publisher.PublishFacts(ctx, facts); err != nil {
		return err
	}
	clear(b.pending)
	b.due = time.Time{}
	return nil
}
