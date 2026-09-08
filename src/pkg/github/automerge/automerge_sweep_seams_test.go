package automerge

import (
	"context"
	"log/slog"
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// These tests pin the thin package-level wrappers and the nil-safe Engine
// seams that previously sat at or near 0% coverage. The automerge package
// gates merges, so its coverage floor (v2-tests.yml) is tight; leaving whole
// exported wrappers unexercised both hides regressions in the delegation
// wiring and parks the package a rounding error above the floor, where any
// unrelated churn fails every open PR's coverage gate.

// SweepQueuedAutoMerges (package wrapper) must delegate to a one-shot engine.
// With no repositories there is nothing to sweep: empty result, no error.
func TestSweepQueuedAutoMerges_PackageWrapper_EmptyTransport(t *testing.T) {
	transport := pauseBlindTransport{repos: nil}
	result, err := SweepQueuedAutoMerges(context.Background(), transport, Options{Logger: slog.New(slog.DiscardHandler)}, AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges() error = %v, want nil", err)
	}
	if result == nil {
		t.Fatal("SweepQueuedAutoMerges() result = nil, want empty result")
	}
	if len(result.Merged) != 0 {
		t.Fatalf("SweepQueuedAutoMerges() merged %v, want none", result.Merged)
	}
}

// StartSelfAuthoredAutoMergeSweep (package wrapper) must refuse to start the
// sweep goroutine when the ACMM gate is closed, for both a stated and an
// unset level.
func TestStartSelfAuthoredAutoMergeSweep_PackageWrapper_ACMMGateClosed(t *testing.T) {
	transport := pauseBlindTransport{repos: []string{"widget"}}
	level := selfMergeMinACMMLevel - 1
	// Must return without launching the ticker goroutine; nothing to observe
	// beyond "does not panic and does not block".
	StartSelfAuthoredAutoMergeSweep(context.Background(), transport, 1, false, &level, Options{Logger: slog.New(slog.DiscardHandler)})
	StartSelfAuthoredAutoMergeSweep(context.Background(), transport, 1, false, nil, Options{Logger: slog.New(slog.DiscardHandler)})
}

// consultApprovalDesk fails OPEN only when no desk is installed, and must
// substitute a non-empty reason when the desk withholds without one.
func TestConsultApprovalDesk_Branches(t *testing.T) {
	ctx := context.Background()

	var nilEngine *Engine
	if allow, reason := nilEngine.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{}); !allow || reason != "" {
		t.Fatalf("nil engine = (%v, %q), want (true, \"\")", allow, reason)
	}

	noDesk := New(pauseBlindTransport{}, Options{})
	if allow, reason := noDesk.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{}); !allow || reason != "" {
		t.Fatalf("no desk = (%v, %q), want (true, \"\")", allow, reason)
	}

	denyBlank := New(pauseBlindTransport{}, Options{
		ApprovalDesk: func(context.Context, hgithub.ApprovalDeskRequest) (bool, string) { return false, "" },
	})
	if allow, reason := denyBlank.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{}); allow || reason != "approval-desk-withheld" {
		t.Fatalf("deny w/o reason = (%v, %q), want (false, \"approval-desk-withheld\")", allow, reason)
	}

	denyStated := New(pauseBlindTransport{}, Options{
		ApprovalDesk: func(context.Context, hgithub.ApprovalDeskRequest) (bool, string) { return false, "freeze window" },
	})
	if allow, reason := denyStated.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{}); allow || reason != "freeze window" {
		t.Fatalf("deny with reason = (%v, %q), want (false, \"freeze window\")", allow, reason)
	}

	allowDesk := New(pauseBlindTransport{}, Options{
		ApprovalDesk: func(context.Context, hgithub.ApprovalDeskRequest) (bool, string) { return true, "" },
	})
	if allow, reason := allowDesk.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{}); !allow || reason != "" {
		t.Fatalf("allow = (%v, %q), want (true, \"\")", allow, reason)
	}
}

// The optional-capability setters must be nil-safe and must tolerate a
// transport that does not implement the capability (the minimum Transport
// shape every fake in this package has).
func TestOptionalCapabilitySetters_NilSafeAndCapabilityBlind(t *testing.T) {
	var nilEngine *Engine
	nilEngine.SetAttributionHooks(hgithub.AttributionHooks{})
	nilEngine.SetAutoMergeLabel("auto-merge")
	nilEngine.SetRequiredChecks(map[string]bool{"test": true})

	blind := New(pauseBlindTransport{}, Options{})
	blind.SetAttributionHooks(hgithub.AttributionHooks{})
	blind.SetAutoMergeLabel("auto-merge")
}
