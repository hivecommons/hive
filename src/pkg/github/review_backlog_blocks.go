package github

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Review backlog items in files changed by the reviewed PR (or missing on
// its base) wait for that PR (#13833, #14473). Untouched-file findings are
// immediately actionable. Failed changed-file lookups conservatively keep
// the block. ReconcileReviewBacklogBlocks resolves parked items in their own
// destination (GitHub issue, Linear, Jira, GitHub Projects):
//
//   - the PR merged: the hive checks the finding itself first
//     (reviewBacklogSelfVerify): if its file is gone from the base, or none
//     of the symbols its title names is in that file any more, the item is
//     closed as completed with a one-line note; otherwise the block is
//     lifted with a note that the item is actionable now;
//   - the PR closed without merging: the block is dropped and a note asks for
//     re-triage against the current base. The item is never closed or
//     cancelled automatically: a closed PR is often superseded by one that
//     lands the same lines, so the finding may still apply. Normal triage and
//     dedupe decide; closing it as not planned stays a human (or `/fixed`)
//     action (maintainer decision on hivecommons/hive#13849).
//
// ReconcileBlockedIssues (issue_blocked.go) still lifts GitHub-issue blocks it
// finds through the enumeration; this sweep is the one that knows which PR an
// item came from in every destination and can tell merged from abandoned.

// Ledger block states.
const (
	reviewBacklogBlockBlocked  = "blocked"
	reviewBacklogBlockLifted   = "lifted"
	reviewBacklogBlockRetriage = "retriage"
	// reviewBacklogBlockClosed: the PR merged and the self-check found the
	// finding gone from the base, so the item was closed as completed.
	reviewBacklogBlockClosed = "closed"
)

// reviewBacklogRetriageMarker tags the note posted when an item is unblocked
// for re-triage because its PR closed unmerged.
const reviewBacklogRetriageMarker = "<!-- hive:review-backlog-retriage -->"

// reviewFollowUpsMarker opens the Follow-ups section the relay appends to a
// decided review; a revision replaces everything from the marker on.
const reviewFollowUpsMarker = "<!-- hive:review-followups v1 -->"

// reviewBacklogBlockLookups bounds the PR lookups one sweep spends.
const reviewBacklogBlockLookups = 40

// ReviewBacklogBlockResolver is an optional sink capability: a destination
// that can lift a block in its own terms. Sinks without it get the note
// appended and nothing else.
type ReviewBacklogBlockResolver interface {
	// LiftBlock removes the `blocked` signal from the item and posts note.
	LiftBlock(ctx context.Context, repo string, ref ReviewBacklogRef, note string) error
}

// blockedOnReviewedPRNote is the body section that parks a review-backlog
// item on the PR it was filed from when the cited file already exists on the
// base branch (the file-missing case keeps blockedBacklogNote's wording).
func blockedOnReviewedPRNote(repo string, pr int) string {
	var b strings.Builder
	b.WriteString(BlockedOnMarker(repo, pr))
	fmt.Fprintf(&b, "\n**Blocked on PR #%d.** This finding came from reviewing PR #%d and is about code that PR is changing. Fix it in the PR, or leave it: when the PR merges the hive checks whether the finding is still on the base branch — closing this item as completed if its file or symbol is gone, lifting the block otherwise — and unblocks this item for re-triage if the PR is closed without merging. Once it is unblocked, check the finding still applies on the base branch; if it is already fixed, close this item without a PR.\n\n", pr, pr)
	return b.String()
}

func reviewBacklogLiftNote(repo string, pr int) string {
	return fmt.Sprintf("%s\nUnblocked: %s#%d merged and the finding's file is still on the base branch. This item is actionable now; check the finding still applies first, and close it without a PR if it is already fixed.", blockedUnblockedMarker, repo, pr)
}

func reviewBacklogRetriageNote(repo string, pr int) string {
	return fmt.Sprintf("%s\n%s\nUnblocked: source PR %s#%d closed unmerged — re-triage against current base. A superseding PR often lands the same lines, so the finding may still apply; close this item only if it no longer does.", blockedUnblockedMarker, reviewBacklogRetriageMarker, repo, pr)
}

// LiftBlock removes `blocked` from the GitHub issue and posts note.
func (s githubIssueBacklogSink) LiftBlock(ctx context.Context, repo string, ref ReviewBacklogRef, note string) error {
	if ref.Number <= 0 {
		return fmt.Errorf("review backlog: GitHub item %q has no issue number", ref.ID)
	}
	if err := s.c.unblockIssue(ctx, repo, ref.Number); err != nil {
		return err
	}
	return s.c.CreateIssueCommentBudgeted(ctx, repo, ref.Number, note)
}

// ReviewBacklogBlockResult summarises one ReconcileReviewBacklogBlocks pass.
type ReviewBacklogBlockResult struct {
	Checked   int
	Lifted    int
	Retriaged int
	// Closed counts items closed as completed because the self-check found
	// the finding no longer applies on the base once its PR merged.
	Closed int
	Errors int
}

// reviewBacklogPROutcome is a tracked PR's state: still open, merged, or
// closed without merging.
type reviewBacklogPROutcome int

const (
	reviewBacklogPROpen reviewBacklogPROutcome = iota
	reviewBacklogPRMerged
	reviewBacklogPRClosed
)

// reviewBacklogPROutcomeBase reads a tracked PR's outcome plus its base
// branch, which the self-check reads the finding's file from.
func (c *Client) reviewBacklogPROutcomeBase(ctx context.Context, repo string, number int) (reviewBacklogPROutcome, string, error) {
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return reviewBacklogPROpen, "", err
	}
	return prOutcome(pr), strings.TrimSpace(pr.GetBase().GetRef()), nil
}

func prOutcome(pr *gh.PullRequest) reviewBacklogPROutcome {
	switch {
	case pr.GetMerged():
		return reviewBacklogPRMerged
	case strings.EqualFold(pr.GetState(), "closed"):
		return reviewBacklogPRClosed
	default:
		return reviewBacklogPROpen
	}
}

// ReconcileReviewBacklogBlocks resolves the blocks on review-backlog items
// whose PR has merged or closed (see the file comment). Items filed to a
// destination other than the one configured now are left alone and counted
// as errors: their sink is not available to act on them.
func ReconcileReviewBacklogBlocks(ctx context.Context, c *Client, logger *slog.Logger) ReviewBacklogBlockResult {
	var res ReviewBacklogBlockResult
	if c == nil || c.client == nil {
		return res
	}
	if logger == nil {
		logger = slog.Default()
	}
	routing, _ := c.reviewBacklogRoutingConfig()
	sink, _ := c.reviewBacklogTarget(routing)
	reviewBacklogLedgerMu.Lock()
	defer reviewBacklogLedgerMu.Unlock()
	ledger, err := loadReviewBacklog("")
	if err != nil {
		logger.Warn("review backlog block sweep: ledger unreadable", "error", err)
		res.Errors++
		return res
	}
	now := time.Now().UTC()
	repos := make([]string, 0, len(ledger.Fingerprints))
	for repo := range ledger.Fingerprints {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	outcomes := map[string]reviewBacklogPROutcome{}
	bases := map[string]string{}
	lookups := 0
	changed := false
	for _, repo := range repos {
		fps := make([]string, 0, len(ledger.Fingerprints[repo]))
		for fp := range ledger.Fingerprints[repo] {
			fps = append(fps, fp)
		}
		sort.Strings(fps)
		for _, fp := range fps {
			if ctx.Err() != nil {
				break
			}
			rec := ledger.Fingerprints[repo][fp]
			if rec.BlockState != reviewBacklogBlockBlocked || rec.BlockedOnPR <= 0 {
				continue
			}
			res.Checked++
			prKey := repo + "#" + strconv.Itoa(rec.BlockedOnPR)
			outcome, seen := outcomes[prKey]
			if !seen {
				if lookups >= reviewBacklogBlockLookups {
					continue
				}
				lookups++
				o, base, err := c.reviewBacklogPROutcomeBase(ctx, repo, rec.BlockedOnPR)
				if err != nil {
					res.Errors++
					logger.Warn("review backlog block sweep: PR lookup failed", "pr", prKey, "error", err)
					continue
				}
				outcomes[prKey] = o
				bases[prKey] = base
				outcome = o
			}
			if outcome == reviewBacklogPROpen {
				continue
			}
			if sink.Destination() != rec.Destination {
				res.Errors++
				logger.Warn("review backlog block sweep: item's destination is not the configured one; left blocked", "item", rec.ItemKey, "destination", rec.Destination)
				continue
			}
			ref := rec.ref()
			resolver, canResolve := sink.(ReviewBacklogBlockResolver)
			var actErr error
			switch outcome {
			case reviewBacklogPRMerged:
				var closed bool
				closed, actErr = c.reviewBacklogSelfVerify(ctx, sink, repo, rec, bases[prKey], logger)
				if actErr == nil && closed {
					rec.BlockState = reviewBacklogBlockClosed
					res.Closed++
					break
				}
				if actErr != nil {
					break
				}
				note := reviewBacklogLiftNote(repo, rec.BlockedOnPR)
				if canResolve {
					actErr = resolver.LiftBlock(ctx, repo, ref, note)
				} else {
					actErr = sink.AppendToItem(ctx, repo, ref, note)
				}
				if actErr == nil {
					rec.BlockState = reviewBacklogBlockLifted
					res.Lifted++
				}
			case reviewBacklogPRClosed:
				if supersededBy, isSuperseded := c.pullRequestSupersededBy(ctx, repo, rec.BlockedOnPR); isSuperseded && supersededBy > 0 && supersededBy != rec.BlockedOnPR {
					yOutcome, yBase, yErr := c.reviewBacklogPROutcomeBase(ctx, repo, supersededBy)
					if yErr == nil {
						switch yOutcome {
						case reviewBacklogPRMerged:
							rec.BlockedOnPR = supersededBy
							var closed bool
							closed, actErr = c.reviewBacklogSelfVerify(ctx, sink, repo, rec, yBase, logger)
							if actErr == nil && closed {
								rec.BlockState = reviewBacklogBlockClosed
								res.Closed++
								break
							}
							if actErr != nil {
								break
							}
							note := reviewBacklogLiftNote(repo, supersededBy)
							if canResolve {
								actErr = resolver.LiftBlock(ctx, repo, ref, note)
							} else {
								actErr = sink.AppendToItem(ctx, repo, ref, note)
							}
							if actErr == nil {
								rec.BlockState = reviewBacklogBlockLifted
								res.Lifted++
							}
						case reviewBacklogPRClosed:
							note := reviewBacklogRetriageNote(repo, supersededBy)
							if canResolve {
								actErr = resolver.LiftBlock(ctx, repo, ref, note)
							} else {
								actErr = sink.AppendToItem(ctx, repo, ref, note)
							}
							if actErr == nil {
								rec.BlockState = reviewBacklogBlockRetriage
								rec.BlockedOnPR = supersededBy
								res.Retriaged++
							}
						case reviewBacklogPROpen:
							oldPR := rec.BlockedOnPR
							rec.BlockedOnPR = supersededBy
							retargetNote := fmt.Sprintf("%s\nRe-targeted: blocker #%d was superseded by #%d.", BlockedOnMarker(repo, supersededBy), oldPR, supersededBy)
							actErr = sink.AppendToItem(ctx, repo, ref, retargetNote)
						}
						if actErr != nil {
							res.Errors++
							logger.Warn("review backlog block sweep: item supersede resolution failed", "item", rec.ItemKey, "old_pr", prKey, "new_pr", repo+"#"+strconv.Itoa(supersededBy), "error", actErr)
							continue
						}
						rec.UpdatedAt = now
						ledger.Fingerprints[repo][fp] = rec
						changed = true
						logger.Info("review backlog block sweep: item supersede resolved", "item", rec.ItemKey, "old_pr", prKey, "new_pr", repo+"#"+strconv.Itoa(supersededBy), "state", rec.BlockState)
						continue
					}
				}
				// Never auto-close: unblock, comment, and leave the item to
				// normal triage (maintainer decision on #13849).
				note := reviewBacklogRetriageNote(repo, rec.BlockedOnPR)
				if canResolve {
					actErr = resolver.LiftBlock(ctx, repo, ref, note)
				} else {
					actErr = sink.AppendToItem(ctx, repo, ref, note)
				}
				if actErr == nil {
					rec.BlockState = reviewBacklogBlockRetriage
					res.Retriaged++
				}
			}
			if actErr != nil {
				res.Errors++
				logger.Warn("review backlog block sweep: item not resolved", "item", rec.ItemKey, "pr", prKey, "error", actErr)
				continue
			}
			rec.UpdatedAt = now
			ledger.Fingerprints[repo][fp] = rec
			changed = true
			logger.Info("review backlog block sweep: item resolved", "item", rec.ItemKey, "pr", prKey, "state", rec.BlockState)
		}
	}
	if changed {
		if err := ledger.save("", now); err != nil {
			res.Errors++
			logger.Warn("review backlog block sweep: ledger not saved", "error", err)
		}
	}
	return res
}

// appendReviewFollowUps lists the backlog items a decided review's findings
// map to at the end of that review, edited in place (hivecommons/hive#13833).
// It reuses the revise path's guards: only a review authored by this hive's
// App login, only a COMMENTED review, and no write when nothing changed. A
// decided verdict that posted no review (recorded only) is logged and left:
// there is no review of ours to put the list in.
func (c *Client) appendReviewFollowUps(ctx context.Context, req ReviewRequest, posted *gh.PullRequestReview, listed []reviewBacklogFiled, capped int) {
	if posted == nil || posted.GetID() == 0 {
		c.logger.Info("review backlog: decided verdict posted no review; follow-ups not listed",
			slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.Int("items", len(listed)))
		return
	}
	login := strings.TrimSpace(c.appBotLogin)
	if login == "" || !strings.EqualFold(safeGetLogin(posted.GetUser()), login) {
		c.logger.Warn("review backlog: decided review is not the hive's own; follow-ups not listed",
			slog.String("repo", req.Repo), slog.Int("number", req.Number))
		return
	}
	if !strings.EqualFold(posted.GetState(), "COMMENTED") {
		return
	}
	body := withReviewFollowUps(posted.GetBody(), listed, capped)
	if reviseBodyDigest(body) == reviseBodyDigest(posted.GetBody()) {
		return
	}
	owner, repoName := c.splitRepo(req.Repo)
	if _, _, err := c.client.PullRequests.UpdateReview(ctx, owner, repoName, req.Number, posted.GetID(), body); err != nil {
		c.logger.Warn("review backlog: follow-ups not added to the decided review",
			slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("error", err.Error()))
		return
	}
	c.recordWriteAudit(AuditActionReviewBacklogSummaryPosted, hiveWriteMeta(),
		WriteTarget{Repo: req.Repo, Number: req.Number},
		"issues", strconv.Itoa(len(listed)),
		"in_review", "true",
		"review_agent", req.Agent)
}

// withReviewFollowUps returns body with its Follow-ups section (re)written.
func withReviewFollowUps(body string, listed []reviewBacklogFiled, capped int) string {
	if i := strings.Index(body, reviewFollowUpsMarker); i >= 0 {
		body = body[:i]
	}
	body = strings.TrimRight(body, " \n")
	return body + "\n\n" + reviewFollowUpsSection(listed, capped)
}

func reviewFollowUpsSection(listed []reviewBacklogFiled, capped int) string {
	sorted := append([]reviewBacklogFiled(nil), listed...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].key != sorted[j].key {
			return sorted[i].key < sorted[j].key
		}
		return sorted[i].number < sorted[j].number
	})
	var b strings.Builder
	b.WriteString(reviewFollowUpsMarker)
	b.WriteString("\n### Follow-ups filed\n")
	b.WriteString("Non-blocking findings from this review are tracked as backlog items, so they are not filed again. Items in files changed by this PR wait on it: fix them here, or leave them and they unblock when it merges (if it closes unmerged they are unblocked for re-triage, not closed). Findings in untouched files are actionable immediately.\n\n")
	seen := map[string]bool{}
	for _, it := range sorted {
		ref := it.key
		if ref == "" && it.number > 0 {
			ref = fmt.Sprintf("#%d", it.number)
		}
		if ref == "" {
			continue
		}
		id := ref + "\x00" + it.title
		if seen[id] {
			continue
		}
		seen[id] = true
		line := "- " + ref
		if it.url != "" && it.number <= 0 {
			line = fmt.Sprintf("- [%s](%s)", ref, it.url)
		}
		fmt.Fprintf(&b, "%s — %s (%s)\n", line, it.title, it.status)
	}
	if capped > 0 {
		fmt.Fprintf(&b, "\n%d more held back by the per-PR daily cap; they are filed on a later review.\n", capped)
	}
	return b.String()
}

// newReviewBacklogChangedFilesProbe caches a bounded, paginated lookup per
// filing pass. A partial list cannot prove a file untouched, so errors and
// exhausted budgets report unknown rather than releasing potentially blocked work.
// Paths are normalised on both sides so a finding citing "./pkg/a.go" or
// "/pkg/a.go" still matches GitHub's repo-root-relative "pkg/a.go".
func (c *Client) newReviewBacklogChangedFilesProbe(repo string, pr int) func(context.Context, string) (bool, bool) {
	loaded, complete := false, false
	files := map[string]bool{}
	return func(ctx context.Context, file string) (bool, bool) {
		if !loaded {
			loaded = true
			owner, name := c.splitRepo(repo)
			opts := &gh.ListOptions{PerPage: 100}
			count := 0
			for n := 0; n < reviewBacklogBlockLookups; n++ {
				changed, resp, err := c.client.PullRequests.ListFiles(ctx, owner, name, pr, opts)
				if err != nil {
					break
				}
				count += len(changed)
				for _, f := range changed {
					if p := normaliseFingerprintPath(f.GetFilename()); p != "" {
						files[p] = true
					}
					if old := normaliseFingerprintPath(f.GetPreviousFilename()); old != "" {
						files[old] = true
					}
				}
				if resp.NextPage == 0 {
					// GitHub caps this endpoint at 3000 files.
					complete = count < 3000
					break
				}
				opts.Page = resp.NextPage
			}
		}
		file = normaliseFingerprintPath(file)
		if files[file] {
			return true, true
		}
		return false, complete && file != ""
	}
}
