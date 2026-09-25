package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// verdictSettleTimeout bounds the detached GitHub work settleIssueFromVerdict
// does per completion: up to maxSettlingRefs candidates, each one or two
// round trips.
const verdictSettleTimeout = 90 * time.Second

const (
	verdictReasonKindAlreadyDone = "already_done"

	verdictDispositionAlreadyDoneVerified   = "already-done-verified"
	verdictDispositionAlreadyDoneUnverified = "already-done-unverified"
	verdictDispositionAlreadyDoneLabeled    = "already-done-labeled"
	verdictDispositionAlreadyDoneClosed     = "already-done-closed"
)

var (
	mergedPRAlreadyDonePattern = regexp.MustCompile(`(?i)\bmerged\s+(?:pull\s+request|PR)\s*#\d+\b`)
	alreadyDoneReasonPattern   = regexp.MustCompile(`(?i)\b(?:already\s+(?:resolved|fixed|implemented|merged|done)|no[-\s]?op)\b`)
)

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
func (h *ContributeWSHub) settleIssueFromVerdict(repo string, number int, reason string, dispatchedAt time.Time, reporter string) string {
	if h == nil || repo == "" || number <= 0 || reason == "" {
		return ""
	}
	return h.settleIssueFromVerdictWithEvidence(repo, number, reason, "", nil, dispatchedAt, reporter)
}

func (h *ContributeWSHub) settleIssueFromVerdictWithEvidence(repo string, number int, reason, reasonKind string, evidence *VerdictEvidence, dispatchedAt time.Time, reporter string) string {
	if h == nil || repo == "" || number <= 0 {
		return ""
	}
	refs, alreadyDone := alreadyDoneVerdictRefs(repo, number, reason, reasonKind, evidence)
	if len(refs) == 0 {
		refs = ghpkg.ParseSettlingRefs(reason, repo, number)
	}
	if len(refs) == 0 {
		if alreadyDone {
			h.markAlreadyDoneUnverified(repo, number, reporter, reason, evidence)
			return verdictDispositionAlreadyDoneUnverified
		}
		return ""
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.RecordIssueClaim == nil {
		if alreadyDone {
			h.markAlreadyDoneUnverified(repo, number, reporter, reason, evidence)
			return verdictDispositionAlreadyDoneUnverified
		}
		return ""
	}
	verify := h.settleVerifier
	if verify == nil {
		if h.server.deps.GHClient == nil {
			if alreadyDone {
				h.markAlreadyDoneUnverified(repo, number, reporter, reason, evidence)
				return verdictDispositionAlreadyDoneUnverified
			}
			return ""
		}
		verify = h.server.deps.GHClient.VerifySettlingRef
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
				if alreadyDone {
					h.markAlreadyDoneUnverified(repo, number, reporter, reason, evidence)
					return verdictDispositionAlreadyDoneUnverified
				}
				return ""
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
			return ""
		}
		h.logger.Info("[contribute-ws] verdict settle: recorded verified claim from no_work_needed reason",
			"repo", repo, "number", number, "ref", ref.String(),
			"pr_url", claim.PRURL, "pr_author", claim.PRAuthor,
			"merged", claim.MergedPR, "merged_at", claim.MergedAt,
			"weak", claim.Reference || claim.ExternalAuthor, "reporter", reporter)
		if err := h.markIssuePRClaim(ctx, repo, number, claim); err != nil && err != ghpkg.ErrNoGitHubClient {
			h.logger.Warn("[contribute-ws] verified PR-claim label failed",
				"repo", repo, "number", number, "ref", ref.String(), "error", err.Error())
		}
		// Pending PR evidence must not inherit the generic no_work_needed offer
		// suppression booked before async verification completed (#8876).
		// Commit-only settlements have no linked PR to verify/confirm from the card,
		// so keep their existing no-work hold behavior.
		if claim.PRNumber > 0 {
			h.clearNoWorkVerdict(repo, number)
		}
		if alreadyDone {
			// #8876: a verified merged PR is still pending evidence, not resolved,
			// unless GitHub itself would close the issue or an operator confirms it.
			shouldClose := false
			if claim.MergedPR && h.closeAlreadyDoneAllowed(repo) && h.prClosesIssue(ctx, repo, claim.PRNumber, number) {
				shouldClose = true
			}
			if shouldClose {
				if err := h.markAlreadyDoneIssue(ctx, repo, number, claim, reporter, true); err != nil {
					h.logger.Warn("[contribute-ws] already-done verdict mark failed",
						"repo", repo, "number", number, "ref", ref.String(), "close", shouldClose, "error", err.Error())
					return verdictDispositionAlreadyDoneVerified
				}
				return verdictDispositionAlreadyDoneClosed
			}
			return verdictDispositionAlreadyDoneVerified
		}
		return ""
	}
	if alreadyDone {
		h.markAlreadyDoneUnverified(repo, number, reporter, reason, evidence)
		return verdictDispositionAlreadyDoneUnverified
	}
	return ""
}

func alreadyDoneVerdictRefs(repo string, number int, reason, reasonKind string, evidence *VerdictEvidence) ([]ghpkg.SettlingRef, bool) {
	kind := strings.ToLower(strings.TrimSpace(reasonKind))
	structured := kind == verdictReasonKindAlreadyDone
	alreadyDone := structured || mergedPRAlreadyDonePattern.MatchString(reason) || alreadyDoneReasonPattern.MatchString(reason)
	if !alreadyDone {
		return nil, false
	}
	var refs []ghpkg.SettlingRef
	if evidence != nil {
		if evidence.PR > 0 {
			refs = append(refs, ghpkg.SettlingRef{Repo: repo, Number: evidence.PR})
		}
		if commit := strings.TrimSpace(evidence.Commit); commit != "" {
			refs = append(refs, ghpkg.SettlingRef{Repo: repo, SHA: commit})
		}
	}
	if len(refs) == 0 {
		refs = ghpkg.ParseSettlingRefs(reason, repo, number)
	}
	return refs, true
}

func (h *ContributeWSHub) closeAlreadyDoneAllowed(repo string) bool {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return false
	}
	cfg := h.server.deps.Config
	if !cfg.Hub.IsContributeCloseAlreadyDone() {
		return false
	}
	return cfg.EffectiveACMMLevelForRepo(repo) >= config.SelfMergeMinACMMLevel
}

func (h *ContributeWSHub) markIssuePRClaim(ctx context.Context, repo string, number int, claim ghpkg.IssueClaim) error {
	if h != nil && h.issuePRClaimMarker != nil {
		return h.issuePRClaimMarker(ctx, repo, number, claim)
	}
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	label := ghpkg.CoveredByPRLabel
	color := "1d76db"
	desc := "Hive verified that an open PR references or claims this issue; still actionable until confirmed"
	if claim.MergedPR {
		label = ghpkg.LikelyDoneLabel
		color = "0e8a16"
		desc = "Hive verified that a merged PR references or claims this issue; pending confirmation"
	}
	if err := h.server.deps.GHClient.EnsureIssueLabel(ctx, repo, label, color, desc); err != nil {
		return err
	}
	return h.server.deps.GHClient.AddLabels(ctx, repo, number, []string{label})
}

func (h *ContributeWSHub) prClosesIssue(ctx context.Context, repo string, prNumber, issueNumber int) bool {
	if prNumber <= 0 || issueNumber <= 0 || h == nil {
		return false
	}
	if h.prClosingVerifier != nil {
		ok, err := h.prClosingVerifier(ctx, repo, prNumber, issueNumber)
		if err != nil {
			if h.logger != nil {
				h.logger.Warn("[contribute-ws] closing relationship check failed", "repo", repo, "pr", prNumber, "issue", issueNumber, "error", err.Error())
			}
			return false
		}
		return ok
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return false
	}
	ok, err := h.server.deps.GHClient.PRClosesIssue(ctx, repo, prNumber, issueNumber)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("[contribute-ws] closing relationship check failed", "repo", repo, "pr", prNumber, "issue", issueNumber, "error", err.Error())
		}
		return false
	}
	return ok
}

func (h *ContributeWSHub) markAlreadyDoneIssue(ctx context.Context, repo string, number int, claim ghpkg.IssueClaim, reporter string, closeIssue bool) error {
	if h != nil && h.alreadyDoneMarker != nil {
		return h.alreadyDoneMarker(ctx, repo, number, claim, reporter, closeIssue)
	}
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	label := config.HubConfig{}.ContributeAlreadyDoneLabelOrDefault()
	if h.server.deps.Config != nil {
		label = h.server.deps.Config.Hub.ContributeAlreadyDoneLabelOrDefault()
	}
	evidence := alreadyDoneEvidenceText(claim)
	action := fmt.Sprintf("added the `%s` label so this is withheld from contributor offers", label)
	if closeIssue {
		action = "closing"
	}
	body := fmt.Sprintf("Hive: already resolved by %s (found by contributor %s); %s.", evidence, strings.TrimSpace(reporter), action)
	if strings.TrimSpace(reporter) == "" {
		body = fmt.Sprintf("Hive: already resolved by %s; %s.", evidence, action)
	}
	if ok, err := h.server.deps.GHClient.IssueCommentsContain(ctx, repo, number, body); err == nil && !ok {
		if err := h.server.deps.GHClient.CreateIssueComment(ctx, repo, number, body); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := h.server.deps.GHClient.EnsureIssueLabel(ctx, repo, label, "8250df", "Hive contributor found this issue already resolved; remove if work remains"); err != nil {
		return err
	}
	if err := h.server.deps.GHClient.AddLabels(ctx, repo, number, []string{label}); err != nil {
		return err
	}
	if !closeIssue {
		return nil
	}
	return h.server.deps.GHClient.CloseIssue(ctx, repo, number, ghpkg.IssueCloseOptions{
		OverrideReason:          "verified already-done contributor verdict: " + evidence,
		SuppressOverrideComment: true,
	})
}

func alreadyDoneEvidenceText(claim ghpkg.IssueClaim) string {
	if claim.PRNumber > 0 {
		return fmt.Sprintf("#%d", claim.PRNumber)
	}
	if claim.PRURL != "" {
		parts := strings.Split(strings.TrimRight(claim.PRURL, "/"), "/")
		if len(parts) > 0 {
			last := parts[len(parts)-1]
			if len(last) > 12 {
				last = last[:12]
			}
			return last
		}
		return claim.PRURL
	}
	return "the contributor's already-done verdict"
}
