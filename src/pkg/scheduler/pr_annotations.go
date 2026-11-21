package scheduler

import (
	"fmt"
	"strings"
	"time"

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

// reviewDispatchHolder carries the review planner's live pending rows into
// the scheduler (SetReviewDispatchPending), with the pending TTL that ages
// them out.
type reviewDispatchHolder struct {
	pending []review.PendingReview
	ttl     time.Duration
}

// SetReviewDispatchPending hands the scheduler the reviews the review swarm
// has in flight this eval cycle (review.DispatchPlan.LiveReviews): heads it
// asked for and has no verdict on yet, including the kicks planned this cycle
// and not yet delivered. The eval cycle calls it after planning and before it
// builds the cadence kicks, so a reviewer's ${PR_LIST} marks those heads, and
// calls it with nil when review dispatch is off. pendingTTL is the effective
// review.pending_ttl (review.EffectivePendingReviewTTL; 0 never expires): a
// row older than that stops marking its head even if no later cycle replaces
// the set.
func (s *Scheduler) SetReviewDispatchPending(pending []review.PendingReview, pendingTTL time.Duration) {
	if s == nil {
		return
	}
	s.reviewDispatch.Store(&reviewDispatchHolder{pending: append([]review.PendingReview(nil), pending...), ttl: pendingTTL})
}

func (s *Scheduler) reviewDispatchPending(now time.Time) []review.PendingReview {
	if s == nil {
		return nil
	}
	h := s.reviewDispatch.Load()
	if h == nil {
		return nil
	}
	if h.ttl <= 0 {
		return h.pending
	}
	var live []review.PendingReview
	for _, p := range h.pending {
		if !p.Dispatched.IsZero() && now.Sub(p.Dispatched) < h.ttl {
			live = append(live, p)
		}
	}
	return live
}

// reviewMark is the ${PR_LIST} review mark for pr: the verdict mark when its
// head carries one (reviewedAnnotation), else the dispatched mark.
func (s *Scheduler) reviewMark(verdicts review.Artifact, links map[string]github.ReviewLink, pr github.PullRequest) string {
	if mark := reviewedAnnotation(verdicts, links, pr, s.cfg.Project.Org); mark != "" {
		return mark
	}
	return dispatchedAnnotation(s.reviewDispatchPending(time.Now()), pr, s.cfg.Project.Org)
}

// dispatchedAnnotation marks a PR whose CURRENT head the review swarm has
// asked for and has no verdict on yet: " [hive-reviewed: dispatched@<sha7>]",
// or "" when nothing is in flight for that head. Without it the cadence
// reviewer and the swarm reviewed the same head independently, and the run
// that finished last decided the verdict: on projectbluefin/bonedigger#116
// (2026-11-21) a queue-mode run listed the PR before the swarm's kick for it
// was delivered and recorded requires_human on a head the swarm had just
// approved; on projectbluefin/dakota-iso#376 the queue run reviewed a head
// whose swarm kick was planned in the same cycle, and that kick then ran a
// second review. The mark reuses the "[hive-reviewed: …]" prefix the queue
// template already tells the reviewer to skip.
func dispatchedAnnotation(pending []review.PendingReview, pr github.PullRequest, org string) string {
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		return ""
	}
	full := config.QualifyRepo(org, pr.Repo)
	for _, p := range pending {
		if p.Number != pr.Number || !strings.EqualFold(strings.TrimSpace(p.Repo), full) || !review.SameHeadSHA(p.HeadSHA, head) {
			continue
		}
		const shortSHA = 7
		short := head
		if len(short) > shortSHA {
			short = short[:shortSHA]
		}
		return fmt.Sprintf(" [hive-reviewed: dispatched@%s]", short)
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
