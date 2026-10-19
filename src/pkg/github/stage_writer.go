package github

import (
	"context"
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageWriter is the ONLY stageengine.StageWriter (ADR-0025): every stage/*
// label the hub writes — PR sweep, issue sweep, FactSink submissions —
// goes through Apply. It wraps stagelabels.Enforce over a fresh
// stageLabeler per call, so a write is planned against a re-read of the
// item and journaled against that read's updated_at (stage_labeler.go),
// and serialises writes per Ref so a relay submission racing the sweep on
// one item cannot leave two stages behind. pkg/stageengine's admission
// test allows exactly this implementation.
type stageWriter struct {
	c     *Client
	locks sync.Map // Ref.Key() → *sync.Mutex
}

// Apply implements stageengine.StageWriter.
func (w *stageWriter) Apply(ctx context.Context, ref stageengine.Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
	if w == nil || w.c == nil || w.c.client == nil {
		return stagelabels.Outcome{}, ErrNoGitHubClient
	}
	m, _ := w.locks.LoadOrStore(ref.Key(), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return stagelabels.Enforce(ctx, w.c.StageLabeler(), ref.Kind, ref.Owner, ref.Repo, ref.Number, hint, want)
}

// StageWriter returns the client's stage writer. One per client, so the
// per-Ref locks are shared by every reconciler built over it.
func (c *Client) StageWriter() stageengine.StageWriter {
	if c == nil {
		return nil
	}
	c.stageEngineMu.Lock()
	defer c.stageEngineMu.Unlock()
	if c.stageWriter == nil {
		c.stageWriter = &stageWriter{c: c}
	}
	return c.stageWriter
}

// StageReconciler returns the reconciler the sweeps route their stage
// writes through, built once per Store over this client's StageWriter,
// and the LegacyTable the sweep fills with its own derivation before each
// ReconcileBatch. opts may be nil: the default is stageengine.DefaultMode
// (shadow) with the table as comparator, which writes exactly what the
// sweep derived — byte-identical to the pre-engine sweep. A caller's opts
// are honoured field by field; nil fields take the same defaults. The
// first call for a Store fixes its reconciler.
func (c *Client) StageReconciler(store *stagelabels.Store, opts *stageengine.Options, logger *slog.Logger) (stageengine.Reconciler, *stageengine.LegacyTable) {
	if c == nil {
		return nil, nil
	}
	if store == nil {
		store = stagelabels.Shared()
	}
	c.stageEngineMu.Lock()
	defer c.stageEngineMu.Unlock()
	if r, ok := c.stageEngines[store]; ok {
		return r, &c.stageLegacy
	}
	var o stageengine.Options
	if opts != nil {
		o = *opts
	}
	o.Store = store
	if o.Writer == nil {
		if c.stageWriter == nil {
			c.stageWriter = &stageWriter{c: c}
		}
		o.Writer = c.stageWriter
	}
	if o.Mode == "" {
		o.Mode = stageengine.DefaultMode
	}
	if o.Legacy == nil && o.Mode == stageengine.ModeShadow {
		o.Legacy = c.stageLegacy.Lookup
	}
	if o.Logger == nil && logger != nil {
		o.Logger = logger
	}
	r := stageengine.New(o)
	if c.stageEngines == nil {
		c.stageEngines = map[*stagelabels.Store]stageengine.Reconciler{}
	}
	c.stageEngines[store] = r
	return r, &c.stageLegacy
}
