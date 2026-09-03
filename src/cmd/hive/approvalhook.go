package main

// Adapter joining #4001's `enqueue-approval` hook action to #4000's operator
// inbox.
//
// #4001 shipped `hooks.ApprovalQueue` as a deliberately narrow interface with a
// nil sink, so that an `enqueue-approval` hook reported a loud unwired-sink
// error rather than silently dropping an approval. This is the adapter that
// interface anticipated: it maps a `hooks.ApprovalRequest` onto a
// `toolapprove.Request` plus a pending verdict, so a hook-produced approval
// lands in the SAME durable inbox, is rendered by the SAME Approvals panel, and
// is resolved through the SAME idempotent path as one produced by the sweep.
//
// Deliberately NOT re-deciding here: a hook firing `enqueue-approval` has
// already expressed the operator's intent that this transition needs a human,
// so the adapter enqueues directly at `operator-approve` rather than running
// the request back through the desk. Sending it through `Desk.Resolve` would
// let an auto-approve rule silently discard an approval the operator explicitly
// asked for — and `Inbox.Enqueue` refuses anything that is not
// `operator-approve` anyway, so re-deciding could only ever drop the request.

import (
	"context"
	"errors"
	"fmt"

	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/toolapprove"
)

// hookApprovalAdapter satisfies hooks.ApprovalQueue over toolapprove.Inbox.
type hookApprovalAdapter struct {
	inbox *toolapprove.Inbox
	// acmmLevel is the hive's level, recorded on the queued item so the panel
	// can flag an approval parked on a full-autonomy hive as a probable policy
	// bug (throughput-contract requirement 4) exactly as it does for the
	// sweep-produced ones.
	acmmLevel int
}

// newHookApprovalAdapter returns nil when there is no inbox, which keeps the
// hooks dispatcher's "no approval queue wired" error accurate rather than
// handing it an adapter that would fail on every firing.
func newHookApprovalAdapter(inbox *toolapprove.Inbox, acmmLevel int) hooks.ApprovalQueue {
	if inbox == nil {
		return nil
	}
	return &hookApprovalAdapter{inbox: inbox, acmmLevel: acmmLevel}
}

// EnqueueApproval places a hook-produced approval in the operator inbox.
//
// Idempotency: the key is derived from the hook name, transition and scope
// rather than left to the default content hash, so a transition that fires
// repeatedly (a flapping review, a retried turn) produces ONE pending row
// instead of one per firing. An already-resolved request is not re-queued —
// that is reported as success, because the operator's decision already exists
// and re-raising it would be the double-ask the journal exists to prevent.
func (a *hookApprovalAdapter) EnqueueApproval(ctx context.Context, req hooks.ApprovalRequest) error {
	if a == nil || a.inbox == nil {
		return fmt.Errorf("enqueue-approval: no approval queue wired")
	}

	tr := toolapprove.Request{
		Kind:   req.Kind,
		Repo:   req.Repo,
		Title:  req.Summary,
		Author: req.Agent,
		Agent:  toolapprove.AgentIdentity{Name: req.Agent},
		Tool:   toolapprove.ToolRequest{Tool: string(req.Transition)},
		// Stable across repeated firings of the same hook on the same subject.
		IdempotencyKey: fmt.Sprintf("hook:%s:%s:%s:%s",
			req.HookName, req.Transition, req.Agent, req.Repo),
	}

	v := toolapprove.Verdict{
		Decision:  toolapprove.DecisionOperatorApprove,
		Kind:      req.Kind,
		Tool:      string(req.Transition),
		Agent:     req.Agent,
		ACMMLevel: a.acmmLevel,
		Rule:      req.HookName,
		Rationale: req.Summary,
	}

	if _, err := a.inbox.Enqueue(tr, v); err != nil {
		// Already resolved: the operator has decided; do not re-ask.
		if errors.Is(err, toolapprove.ErrAlreadyResolved) {
			return nil
		}
		return err
	}
	return nil
}
