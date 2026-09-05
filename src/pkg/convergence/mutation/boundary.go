package mutation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

const defaultBoundaryTTL = 10 * time.Minute

// Boundary adapts Executor to the narrow pkg/effects seam used by GitHub and
// push call sites. It acquires a per-(repo, kind, target) claim for each
// external mutation, journals the operation, and releases the claim after the
// boundary returns.
type Boundary struct {
	Executor Executor
	Holder   string
	TTL      time.Duration
	Logger   *slog.Logger
	Stats    *effects.Recorder
	Mode     func() string
}

func (b *Boundary) Execute(ctx context.Context, claim effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	if b == nil {
		return effect(ctx)
	}
	executor := b.Executor
	if b.Mode != nil {
		executor.Mode = b.Mode()
	}
	if b.Stats != nil {
		b.Stats.SetMode(executor.Mode)
	}
	if !JournalingEnabled(executor.Mode) {
		return effect(ctx)
	}
	if err := claim.Validate(); err != nil {
		return effects.Result{}, fmt.Errorf("%w: invalid mutation claim: %v", effects.ErrDenied, err)
	}
	if executor.Ledger == nil || executor.Journal == nil {
		return effects.Result{}, fmt.Errorf("mutation boundary requires a ledger and journal when mode is enabled")
	}
	_ = RepoClaim(claim.Repo).Key()
	holder := strings.TrimSpace(claim.Actor)
	if holder == "" {
		holder = strings.TrimSpace(b.Holder)
	}
	if holder == "" {
		holder = "hive"
	}
	ttl := b.TTL
	if ttl <= 0 {
		ttl = defaultBoundaryTTL
	}
	now := executor.now()
	mclaim := TaskClaim(claim.Repo, claimSubject(claim))
	entry, acquireErr := executor.Ledger.Acquire(mclaim, holder, ttl, now)
	if acquireErr != nil {
		if FencingEnabled(executor.Mode) {
			if b.Stats != nil {
				b.Stats.IncDenied()
			}
			return effects.Result{}, fmt.Errorf("%w: %v", effects.ErrDenied, acquireErr)
		}
		if b.Logger != nil {
			b.Logger.Warn("shadow fence would have denied external mutation",
				"repo", claim.Repo, "kind", claim.Kind, "target", claim.Target, "error", acquireErr)
		}
		if b.Stats != nil {
			b.Stats.IncFenced()
		}
		entry = Entry{Claim: mclaim, Holder: holder, Epoch: 0}
	}
	inputs := map[string]string{
		"repo":   claim.Repo,
		"kind":   claim.Kind,
		"target": claim.Target,
	}
	for k, v := range claim.Inputs {
		inputs[k] = v
	}
	mutEffect := Effect{
		OutcomeKey:        claim.Repo + "@mutation",
		DesiredGeneration: 1,
		Transition:        "external." + claim.Kind,
		Subject:           mclaim.Subject,
		ClaimKey:          mclaim.Key(),
		Kind:              claim.Kind,
		Inputs:            inputs,
	}
	var out effects.Result
	op, execErr := executor.Execute(mutEffect, entry.Epoch, holder, func() (string, error) {
		res, err := effect(ctx)
		out = res
		return res.Provenance, err
	})
	if execErr != nil {
		if errors.Is(execErr, ErrClaimHeld) || errors.Is(execErr, ErrStaleEpoch) {
			if b.Stats != nil {
				b.Stats.IncDenied()
			}
			return out, fmt.Errorf("%w: %v", effects.ErrDenied, execErr)
		}
		return out, execErr
	}
	if b.Stats != nil && op.LogicalID != "" {
		b.Stats.IncJournaled()
	}
	if entry.Epoch != 0 {
		if _, err := executor.Ledger.Release(mclaim.Key(), entry.Epoch, executor.now()); err != nil && b.Logger != nil {
			b.Logger.Warn("mutation claim release failed", "repo", claim.Repo, "kind", claim.Kind, "target", claim.Target, "error", err)
		}
	}
	return out, nil
}

func claimSubject(claim effects.Claim) string {
	return claim.Repo + "#" + claim.Kind + "/" + claim.Target
}
