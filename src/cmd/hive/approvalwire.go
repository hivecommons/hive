package main

// Approval-desk startup wiring (RFC #4000).
//
// Mirrors celwire.go: one construction helper called during startup, fail-closed
// on malformed rules, and a total no-op when the feature block is absent.
//
// The desk is DEFAULT OFF (`tool_approval.enabled: false`). With it off this
// file returns nil for both the desk and the inbox, no hook is installed on the
// GitHub client, and the dashboard's Approvals panel reports "not enabled". A
// hive that upgrades onto this build and changes no config behaves exactly as
// it did before.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/kubestellar/hive/pkg/config"
	ghpkg "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/toolapprove"
)

// buildApprovalDesk constructs the desk and its durable operator inbox from
// config.
//
// Fail-closed on a malformed rule set: the error is logged and BOTH return
// values are nil, so the hive runs with the desk disabled and every legacy gate
// still authoritative. That is strictly safer than running with a half-applied
// rule set, and it matches celEngineFor's posture — a bad rule must never crash
// the fleet nor silently change what is permitted.
func buildApprovalDesk(cfg *config.Config, logger *slog.Logger) (*toolapprove.Desk, *toolapprove.Inbox) {
	desk, inbox, err := toolapprove.DeskFromConfig(cfg, nil, logger)
	if err != nil {
		if logger != nil {
			logger.Error("tool-approval: rejecting malformed approval rules; approval desk disabled until fixed",
				"error", err)
		}
		return nil, nil
	}
	if desk != nil && logger != nil {
		logger.Info("tool-approval: approval desk enabled",
			"rules", len(desk.RuleNames()), "acmm_level", toolapprove.ACMMLevelOf(cfg))
	}
	return desk, inbox
}

// approvalDeskAllowsLegacyOperation wraps an existing legacy gate result in the
// RFC #4000 decision point. Callers invoke it only after their legacy gate has
// allowed the operation, so a nil/disabled desk preserves old behavior and an
// enabled desk may record, audit, or withhold but never widen authority.
func approvalDeskAllowsLegacyOperation(
	ctx context.Context,
	desk *toolapprove.Desk,
	cfg *config.Config,
	kind string,
	tool string,
	agentName string,
	logger *slog.Logger,
) bool {
	if desk == nil {
		return true
	}
	req := toolapprove.Request{
		Kind: kind,
		Tool: toolapprove.ToolRequest{
			Tool: tool,
		},
		Agent: toolapprove.AgentIdentity{
			Name: agentName,
		},
		LegacyAllowed:    true,
		HasLegacyAllowed: true,
	}
	acmmLevel := toolapprove.ACMMLevelOf(cfg)
	v := desk.Resolve(ctx, req, acmmLevel)
	if v.Decision == toolapprove.DecisionAutoApprove {
		return true
	}
	if logger != nil {
		logger.Info("tool-approval: legacy operation withheld",
			"kind", kind, "tool", tool, "agent", agentName,
			"decision", string(v.Decision), "rationale", v.Rationale)
	}
	return false
}

// newSelfMergeDeskHook adapts the desk to the GitHub client's narrow
// ApprovalDeskHook signature, for the self-authored auto-merge sweep — the one
// real producer wired in this slice.
//
// Behavior per PR:
//
//   - auto-approve  → allow the merge, in-loop, nothing persisted. This is the
//     L6 path and it must stay allocation-light: no queue, no disk.
//   - operator-approve → withhold the merge and park the request in the durable
//     inbox for an operator. Enqueue is idempotent, so a PR the sweep re-sees
//     every tick produces ONE inbox row, not one per tick.
//   - deny → withhold, nothing queued.
//
// Idempotency across a granted verdict: before withholding, the hook checks the
// resolved journal. An operator who already granted this exact request gets the
// merge on the next sweep tick, and the journal's Executed flag keeps a
// re-delivery from merging twice.
func newSelfMergeDeskHook(
	desk *toolapprove.Desk,
	inbox *toolapprove.Inbox,
	cfg *config.Config,
	logger *slog.Logger,
) ghpkg.ApprovalDeskHook {
	acmmLevel := toolapprove.ACMMLevelOf(cfg)

	return func(ctx context.Context, in ghpkg.ApprovalDeskRequest) (bool, string) {
		req := toolapprove.Request{
			Kind:        toolapprove.KindSelfMerge,
			Repo:        in.Repo,
			Number:      in.Number,
			Author:      in.Author,
			Title:       in.Title,
			Labels:      in.Labels,
			ChecksGreen: in.ChecksGreen,
			// The GitHub sweep calls this hook only after its legacy
			// eligibility checks have passed. Preserve that result exactly; the
			// desk adds policy/audit/queue semantics and may only withhold.
			LegacyAllowed:    true,
			HasLegacyAllowed: true,
			Tool: toolapprove.ToolRequest{
				Tool:   "hive-merge",
				Target: in.HeadSHA,
			},
		}
		if in.Kind != "" {
			req.Kind = in.Kind
		}

		// A verdict this operator already granted must merge, and must merge
		// only once. Consult the journal before the desk so a granted request
		// is not re-queued for approval it already has.
		id := toolapprove.DeriveIdempotencyKey(req)
		if rec, ok := inbox.ResolvedRecordFor(id); ok {
			if !rec.Approved {
				return false, "approval-desk-rejected"
			}
			if rec.Executed {
				// Already merged under this grant. Withhold rather than merge a
				// second time — the sweep's own "closed"/"gone" checks would
				// usually catch this first, but the journal is the authority.
				return false, "approval-desk-already-executed"
			}
			if err := inbox.MarkExecuted(id); err != nil && logger != nil {
				logger.Warn("tool-approval: could not mark approval executed",
					"id", id, "error", err)
			}
			return true, ""
		}

		v := desk.Resolve(ctx, req, acmmLevel)

		switch v.Decision {
		case toolapprove.DecisionAutoApprove:
			// THROUGHPUT CONTRACT: resolved synchronously, in-loop, nothing
			// enqueued and nothing persisted. Do not add I/O to this branch.
			return true, ""

		case toolapprove.DecisionOperatorApprove:
			if _, err := inbox.Enqueue(req, v); err != nil {
				// An already-resolved request is not an error worth logging as
				// one: it means the operator resolved it between the journal
				// read above and here. Withhold; the next tick reads the
				// journal and proceeds.
				if !errors.Is(err, toolapprove.ErrAlreadyResolved) && logger != nil {
					logger.Warn("tool-approval: could not enqueue pending approval",
						"repo", in.Repo, "pr", in.Number, "error", err)
				}
			} else if logger != nil {
				logger.Info("tool-approval: self-merge parked for operator approval",
					"repo", in.Repo, "pr", in.Number, "rule", v.Rule,
					"acmm_level", acmmLevel, "rationale", v.Rationale)
			}
			return false, "approval-desk-pending-operator"

		default: // DecisionDeny, and DecisionSecurityScan which Resolve never returns
			if logger != nil {
				logger.Info("tool-approval: self-merge withheld",
					"repo", in.Repo, "pr", in.Number, "decision", string(v.Decision),
					"rationale", v.Rationale)
			}
			return false, "approval-desk-denied"
		}
	}
}
