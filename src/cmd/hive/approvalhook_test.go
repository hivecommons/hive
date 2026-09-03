package main

// The #4000 ↔ #4001 seam: a hook's `enqueue-approval` action landing in the
// desk's durable operator inbox.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/toolapprove"
)

func hookApprovalRequest() hooks.ApprovalRequest {
	return hooks.ApprovalRequest{
		Kind:       "review_rejected",
		Agent:      "reviewer",
		Repo:       "hivecommons/hive",
		Summary:    "review_rejected requires approval (hook needs-human)",
		Transition: hooks.Transition("review_rejected"),
		HookName:   "needs-human",
	}
}

// TestHookApprovalAdapterEnqueues is the end-to-end seam: a hook firing places
// a real row in the desk's inbox, visible to the same panel and resolvable
// through the same path as a sweep-produced one.
func TestHookApprovalAdapterEnqueues(t *testing.T) {
	inbox := toolapprove.NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	a := newHookApprovalAdapter(inbox, 4)
	if a == nil {
		t.Fatal("adapter was nil for a non-nil inbox")
	}

	if err := a.EnqueueApproval(context.Background(), hookApprovalRequest()); err != nil {
		t.Fatalf("EnqueueApproval: %v", err)
	}

	pending := inbox.List()
	if len(pending) != 1 {
		t.Fatalf("inbox holds %d items, want 1", len(pending))
	}
	item := pending[0]
	if item.Verdict.Decision != toolapprove.DecisionOperatorApprove {
		t.Errorf("queued verdict is %s, want operator-approve", item.Verdict.Decision)
	}
	if item.Verdict.Rule != "needs-human" {
		t.Errorf("verdict lost the hook name as provenance: %q", item.Verdict.Rule)
	}
	if item.ACMMLevel != 4 {
		t.Errorf("queued at level %d, want 4 — the panel needs this to flag L6 policy bugs", item.ACMMLevel)
	}
}

// TestHookApprovalAdapterIsIdempotent pins the property that matters for a
// flapping transition: repeated firings of the same hook on the same subject
// produce ONE pending row, not one per firing.
func TestHookApprovalAdapterIsIdempotent(t *testing.T) {
	inbox := toolapprove.NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	a := newHookApprovalAdapter(inbox, 4)

	for i := 0; i < 5; i++ {
		if err := a.EnqueueApproval(context.Background(), hookApprovalRequest()); err != nil {
			t.Fatalf("firing %d: %v", i, err)
		}
	}
	if got := inbox.Count(); got != 1 {
		t.Fatalf("five identical hook firings produced %d rows, want 1", got)
	}

	// A DIFFERENT subject must still queue separately, or the dedupe would be
	// swallowing genuine approvals.
	other := hookApprovalRequest()
	other.Repo = "kubestellar/other"
	if err := a.EnqueueApproval(context.Background(), other); err != nil {
		t.Fatalf("distinct subject: %v", err)
	}
	if got := inbox.Count(); got != 2 {
		t.Errorf("a distinct subject did not queue separately: count=%d, want 2", got)
	}
}

// TestHookApprovalAdapterDoesNotReAskAfterResolution pins that once an operator
// has decided, a repeat firing does not re-raise the same approval.
func TestHookApprovalAdapterDoesNotReAskAfterResolution(t *testing.T) {
	inbox := toolapprove.NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	a := newHookApprovalAdapter(inbox, 4)

	if err := a.EnqueueApproval(context.Background(), hookApprovalRequest()); err != nil {
		t.Fatalf("EnqueueApproval: %v", err)
	}
	id := inbox.List()[0].ID
	if _, err := inbox.Resolve(id, true, "operator", "approved"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The transition fires again after the operator already decided.
	if err := a.EnqueueApproval(context.Background(), hookApprovalRequest()); err != nil {
		t.Fatalf("re-firing after resolution returned an error: %v — an already-decided "+
			"approval should be a no-op, not a failure", err)
	}
	if got := inbox.Count(); got != 0 {
		t.Errorf("a resolved approval was re-queued: %d pending, want 0", got)
	}
}

// TestHookApprovalAdapterNilInbox pins that a disabled desk yields a nil
// adapter, so the hooks dispatcher keeps reporting "no approval queue wired"
// rather than being handed an adapter that fails on every firing.
func TestHookApprovalAdapterNilInbox(t *testing.T) {
	if a := newHookApprovalAdapter(nil, 6); a != nil {
		t.Error("a nil inbox produced a non-nil adapter — the dispatcher's unwired-sink error would be masked")
	}
}
