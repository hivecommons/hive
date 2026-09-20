package dashboard

import (
	"context"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// verdictSettleTimeout bounds the detached GitHub work settleIssueFromVerdict
// does per completion: up to maxSettlingRefs candidates, each one or two
// round trips.
const verdictSettleTimeout = 90 * time.Second

// settleIssueFromVerdict turns a no_work_needed verdict's citation into a
// verified claim-ledger entry (hivecommons/hive#7871).
//
// The #6869 settle scan only recovers claims from PRs that REFERENCE the
// issue; a fix that landed without mentioning it is invisible, and the issue
// is re-offered every cooldown window to a contributor whose only possible
// verdict is the one the previous contributor already reached. The agent,
// however, DOES find the settling PR and says so in its reason ("already
// merged via #532", "already in upstream/main (e6d3de3)"). This parses those
// references, checks each against GitHub, and records the first that holds
// up exactly as the scan would have — a strong merged claim for a PR merged
// before dispatch or a commit on the default branch, a weak external claim
// for an open PR by someone else. Nothing matched, or no ledger/API wired:
// today's behaviour (the hub's own verdict ledger + cooldown) is unchanged.
//
// The text alone is never trusted. The API check is what turns the claim into
// a fact, and it also defuses a model fabricating "already fixed by #123" (a
// ref that does not exist — a real but unrelated ref is not detectable here
// and is bounded by the claim TTL). An open PR is recorded only when its
// author is not the reporter (hivecommons/hive#7890); a merged PR is a fact
// regardless of who merged it.
// Runs detached from the read loop; callers `go` it.
func (h *ContributeWSHub) settleIssueFromVerdict(repo string, number int, reason string, dispatchedAt time.Time, reporter string) {
	if h == nil || repo == "" || number <= 0 || reason == "" {
		return
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.RecordIssueClaim == nil {
		return
	}
	verify := h.settleVerifier
	if verify == nil {
		if h.server.deps.GHClient == nil {
			return
		}
		verify = h.server.deps.GHClient.VerifySettlingRef
	}
	refs := ghpkg.ParseSettlingRefs(reason, repo, number)
	if len(refs) == 0 {
		return
	}
	ctx := h.server.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, verdictSettleTimeout)
	defer cancel()
	for _, ref := range refs {
		res, err := verify(ctx, repo, number, ref, dispatchedAt)
		if err != nil {
			h.logger.Warn("[contribute-ws] verdict settle: reference check failed",
				"repo", repo, "number", number, "ref", ref.String(), "reason", res.Reason, "error", err.Error())
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if !res.Settled {
			h.logger.Info("[contribute-ws] verdict settle: reference does not settle the issue",
				"repo", repo, "number", number, "ref", ref.String(), "reason", res.Reason)
			continue
		}
		claim := res.Claim
		if !claim.MergedPR && reporter != "" && strings.EqualFold(strings.TrimSpace(claim.PRAuthor), reporter) {
			// hivecommons/hive#7890: an OPEN PR settles the issue only when
			// someone OTHER than the reporter is on it. The reporter citing
			// their own open PR — on anything, related or not — is not an
			// external claim; recording it would let the least-trusted write
			// path defer an issue on its own say-so. Skip to the next ref.
			h.logger.Info("[contribute-ws] verdict settle: open PR is the reporter's own, not an external claim",
				"repo", repo, "number", number, "ref", ref.String(), "pr_author", claim.PRAuthor, "reporter", reporter)
			continue
		}
		claim.Repo, claim.Issue = repo, number
		claim.Source = ghpkg.ClaimSourceVerdict
		claim.SourceReporter = reporter
		if err := h.server.deps.RecordIssueClaim(claim); err != nil {
			h.logger.Warn("[contribute-ws] verdict settle: recording claim failed",
				"repo", repo, "number", number, "ref", ref.String(), "error", err.Error())
			return
		}
		h.logger.Info("[contribute-ws] verdict settle: recorded verified claim from no_work_needed reason",
			"repo", repo, "number", number, "ref", ref.String(),
			"pr_url", claim.PRURL, "pr_author", claim.PRAuthor,
			"merged", claim.MergedPR, "merged_at", claim.MergedAt,
			"weak", claim.Reference || claim.ExternalAuthor, "reporter", reporter)
		return
	}
}
