package scheduler

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// forkAnnotation is the inline marker every PR list carries for a PR whose
// head lives in a fork (hivecommons/hive#7386): the agent learns "comment
// only" from the work list, not from a failed push. Empty for same-repo PRs.
func forkAnnotation(pr github.PullRequest) string {
	if !pr.FromFork {
		return ""
	}
	head := pr.HeadRepo
	if head == "" {
		head = "fork deleted"
	}
	return " [fork: " + head + " — comment only, cannot push]"
}

func (s *Scheduler) prReviewAnnotation(pr github.PullRequest, agentName string) (string, bool) {
	baseName := s.cfg.BaseAgentName(agentName)
	agentCfg, ok := s.cfg.Agents[baseName]
	if !ok || !agentCfg.ReviewModels.Configured() {
		return "", false
	}
	authorModel := github.NormalizeAttributionModel(pr.HiveModel)
	backend, model, fallback := s.selectReviewModel(agentCfg, pr)
	if model == "" {
		s.logger.Info("review model fallback skipped PR", "agent", agentName, "repo", pr.Repo, "number", pr.Number, "author_model", pr.HiveModel)
		if s.auditFunc != nil {
			s.auditFunc(github.AuditActionReviewModelFallback, fmt.Sprintf("repo=%s number=%d fallback=skip author_model=%s", pr.Repo, pr.Number, authorModel), agentName)
		}
		return "", true
	}
	fallbackMode := agentCfg.ReviewModels.EffectiveFallback()
	if fallback && s.auditFunc != nil {
		detail := fmt.Sprintf("repo=%s number=%d fallback=%s author_model=%s", pr.Repo, pr.Number, fallbackMode, authorModel)
		if fallbackMode != config.ReviewModelsFallbackRequiresHuman {
			detail += fmt.Sprintf(" review_model=%s", model)
		}
		s.auditFunc(github.AuditActionReviewModelFallback, detail, agentName)
	}
	if fallback && fallbackMode == config.ReviewModelsFallbackRequiresHuman {
		return fmt.Sprintf(" [author=%s] [needs human review — no independent model available]", authorModel), false
	}
	marker := fmt.Sprintf(" [author=%s review_with=%s/%s]", authorModel, backend, model)
	return marker, false
}

// loadReviewVerdicts reads the review-verdicts artifact for ${PR_LIST}
// annotation. A missing or unreadable artifact is an empty one: the list is
// still correct without it, just unannotated.
func (s *Scheduler) loadReviewVerdicts() review.Artifact {
	art, err := review.LoadArtifact("")
	if err != nil {
		return review.Artifact{}
	}
	return art
}

// reviewedAnnotation marks a PR whose CURRENT head already carries a hive
// review verdict: " [hive-reviewed: <verdict>@<sha7>]", or "" when it does
// not. The cadence reviewer works ${PR_LIST} on every kick and, with no such
// mark, re-read and re-commented the same PR each cycle — actions#548 collected
// six "duplicate of #554" notices in two hours. The head SHA is part of the
// key so a PR whose author pushed since the verdict reads as unreviewed again,
// which is the behaviour the dispatch lane already has. Verdict repos are
// always owner/name while the enumeration may carry the bare governor name;
// qualify before comparing, as applyHumanDecisionLabels does (#8133).
//
// Two sources, either suffices. The verdict artifact gives the verdict word;
// the review-links ledger (what the relay actually POSTED, with the head it
// posted at) covers a review whose verdict was discarded — a PR the fan-out
// lane never dispatched, so the relay had nothing to bind the verdict to.
// Without the second source such a PR stayed unmarked and was re-reviewed on
// every kick exactly like the malformed-verdict case.
func reviewedAnnotation(verdicts review.Artifact, links map[string]github.ReviewLink, pr github.PullRequest, org string) string {
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		return ""
	}
	full := config.QualifyRepo(org, pr.Repo)
	const shortSHA = 7
	short := head
	if len(short) > shortSHA {
		short = short[:shortSHA]
	}
	if agg, ok := verdicts.AggregateFor(full, pr.Number, head); ok {
		return fmt.Sprintf(" [hive-reviewed: %s@%s]", agg.Verdict, short)
	}
	if link, ok := links[github.ReviewLinkKey(full, pr.Number)]; ok && link.HeadSHA != "" && strings.EqualFold(link.HeadSHA, head) {
		state := strings.TrimSpace(link.State)
		if state == "" {
			state = "commented"
		}
		return fmt.Sprintf(" [hive-reviewed: %s@%s]", state, short)
	}
	return ""
}

// loadReviewLinks reads the relay's posted-review ledger for ${PR_LIST}
// annotation; unreadable is empty, same as loadReviewVerdicts.
func loadReviewLinks() map[string]github.ReviewLink {
	links, err := github.LoadReviewLinks("")
	if err != nil {
		return map[string]github.ReviewLink{}
	}
	return links
}

func prKickAnnotation(pr github.PullRequest, agentName string) string {
	lane := prOwningLane(pr)
	mergeableState := strings.TrimSpace(pr.MergeableState)
	if mergeableState == "" && prHasLabel(pr.Labels, "needs-rebase") {
		mergeableState = "needs-rebase"
	}
	if mergeableState == "" {
		switch pr.Mergeable {
		case github.MergeableYes:
			mergeableState = "mergeable"
		case github.MergeableNo:
			mergeableState = "not-mergeable"
		default:
			mergeableState = "unknown"
		}
	}
	parts := []string{"mergeable_state=" + mergeableState}
	if lane != "" {
		parts = append(parts, "lane="+lane)
	} else {
		parts = append(parts, "lane=unknown")
	}
	annotation := "[" + strings.Join(parts, ", ") + "]"
	if prIsAppAuthored(pr) && prHasConflictSignal(pr, mergeableState) && lane != "" && strings.EqualFold(lane, agentName) {
		annotation += " [CONFLICT, yours]"
	}
	return annotation
}

func prOwningLane(pr github.PullRequest) string {
	for _, label := range pr.Labels {
		if lane, ok := strings.CutPrefix(strings.TrimSpace(label), "agent/"); ok && lane != "" {
			return lane
		}
	}
	if head := strings.TrimSpace(pr.HeadRef); head != "" {
		if lane, _, ok := strings.Cut(head, "/"); ok && lane != "" {
			return lane
		}
	}
	return ""
}

func prHasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return false
}

func prHasConflictSignal(pr github.PullRequest, mergeableState string) bool {
	if prHasLabel(pr.Labels, "needs-rebase") {
		return true
	}
	return strings.EqualFold(mergeableState, "dirty") || strings.EqualFold(mergeableState, "conflicting")
}

func prIsAppAuthored(pr github.PullRequest) bool {
	author := strings.TrimSpace(pr.Author)
	return pr.AppAuthored || strings.HasPrefix(strings.ToLower(author), "app/")
}
