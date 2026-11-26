// Package forgesweep runs the automerge and close-on-merge sweeps for repos
// on Forgejo (#19671, part of #19494). The GitHub sweeps call the GitHub API
// directly and skip every repo config.RepoOnGitHub rejects; these sweeps take
// over for the Forgejo ones through Forgejo's own REST API
// (forge.SweepAPI). GitLab and Gitea are not implemented: config.ForgeSweepsFor
// keeps both sweeps off for them, and only the Forgejo adapter implements
// forge.SweepAPI.
package forgesweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/writeback"
)

const (
	// DefaultMaxMerges bounds the merges one automerge pass makes.
	DefaultMaxMerges = 5
	// DefaultMergeMethod is the Forgejo merge style the sweep asks for,
	// matching the GitHub sweep, which only squashes.
	DefaultMergeMethod = string(forge.MergeStrategySquash)
	// DefaultMergedPullsScanned is how many recently closed pull requests
	// one close-on-merge pass reads per repo.
	DefaultMergedPullsScanned = 50
	// DefaultCloseGrace is how long after a merge the sweep waits before it
	// acts, so Forgejo's own closing-keyword handling lands first.
	DefaultCloseGrace = 2 * time.Minute
	// DefaultCloseMaxAge is the oldest merge the sweep still acts on.
	DefaultCloseMaxAge = 7 * 24 * time.Hour

	// NeedsConfirmationMarker in a pull request body leaves the issues it
	// names open for the reporter to confirm, as on GitHub.
	NeedsConfirmationMarker = "hive: needs-confirmation"
	// closeMarkerFormat tags the comment the sweep posts when it closes an
	// issue for a pull request. Seeing it again on an open issue means a
	// person reopened it, so the sweep never closes it again for that PR.
	closeMarkerFormat = "<!-- hive-close-on-merge pr=%d -->"
)

// writeAccess are the Forgejo permission levels that may author a pull
// request whose closing keywords the sweep honours.
var writeAccess = map[string]bool{"write": true, "admin": true, "owner": true}

// releaseAccess are the levels that may release a merge with the automerge
// label. It adds forge.PermissionAssignable, the answer when the hive's
// Forgejo user has write but not admin and Forgejo refuses the permission
// API (#19914): the timeline already shows the labeller added a label to the
// pull request, which Forgejo allows only with write on pull requests. The
// close-on-merge author check has no such evidence and keeps writeAccess.
var releaseAccess = map[string]bool{"write": true, "admin": true, "owner": true, forge.PermissionAssignable: true}

// closingRefRe matches a same-repository closing keyword ("Fixes #12").
// Forgejo's keywords are close, closes, closed, fix, fixes, fixed, resolve,
// resolves and resolved. A cross-repository reference ("owner/repo#12") does
// not match, because the keyword must be followed directly by "#".
var closingRefRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#(\d+)\b`)

// Sweeper holds one pass's policy. Build it from the current config for each
// pass so a reload takes effect on the next one.
type Sweeper struct {
	// Repos are the Forgejo repos to sweep (config.ForgejoSweepRepos).
	Repos []string
	// API returns the Forgejo client for repo.
	API func(repo string) (forge.SweepAPI, error)
	// AutoMergeEnabled is the per-repo auto-merge switch
	// (config.RepoAutoMergeEnabled); nil merges nothing.
	AutoMergeEnabled func(repo string) bool
	// AutoMergeLabel is the label that releases a pull request
	// (governor.labels.automerge). Empty merges nothing.
	AutoMergeLabel string
	// BlockingLabels keep a pull request from merging (hold, the sentinel
	// label, the exempt labels).
	BlockingLabels []string
	MergeMethod    string
	MaxMerges      int
	// Mutation is the boundary merges and comments go through.
	Mutation effects.Boundary
	// Gate is the unsolicited write-back gate the close-on-merge sweep asks
	// before it comments on and closes an issue (writeback.KindIssueClose).
	// Nil allows, as a nil *writeback.Gate does; cmd/hive always sets it.
	Gate   func(repo string, item writeback.Item, kind writeback.Kind, preview string) writeback.Decision
	Logger *slog.Logger
	Now    func() time.Time
}

// MergeOutcome is one pull request's automerge verdict.
type MergeOutcome struct {
	Repo   string
	Number int
	Merged bool
	Reason string
}

// CloseOutcome is one issue's close-on-merge verdict.
type CloseOutcome struct {
	Repo   string
	Issue  int
	PR     int
	Closed bool
	Reason string
}

func (s *Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sweeper) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func (s *Sweeper) blockedBy(labels []string) string {
	for _, b := range s.BlockingLabels {
		if strings.TrimSpace(b) != "" && hasLabel(labels, b) {
			return b
		}
	}
	return ""
}

// AutoMerge merges the Forgejo pull requests that pass every gate: the repo
// has auto-merge on, the PR is open, not a draft and carries the automerge
// label and no blocking label, the label was last added by someone other
// than the author who has write access and no push to the PR followed it,
// Forgejo reports the PR mergeable, and the combined commit status at the
// head is success with at least one status. The merge is pinned to that head
// (head_commit_id).
func (s *Sweeper) AutoMerge(ctx context.Context) []MergeOutcome {
	if s.API == nil || s.AutoMergeEnabled == nil || strings.TrimSpace(s.AutoMergeLabel) == "" {
		return nil
	}
	maxMerges := s.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultMaxMerges
	}
	var out []MergeOutcome
	merged := 0
	for _, repo := range s.Repos {
		if merged >= maxMerges || ctx.Err() != nil {
			break
		}
		if !s.AutoMergeEnabled(repo) {
			continue
		}
		api, err := s.API(repo)
		if err != nil {
			s.log().Warn("forgejo automerge: client unavailable", "repo", repo, "error", err)
			continue
		}
		crs, err := api.ListOpenChangeRequests(ctx, repo)
		if err != nil {
			s.log().Warn("forgejo automerge: listing pull requests failed", "repo", repo, "error", err)
			continue
		}
		for _, cr := range crs {
			if merged >= maxMerges || ctx.Err() != nil {
				break
			}
			if cr.Draft || !hasLabel(cr.Labels, s.AutoMergeLabel) {
				continue
			}
			o := s.tryMerge(ctx, api, repo, cr.Number)
			if o.Merged {
				merged++
				s.log().Info("forgejo automerge: merged pull request", "repo", repo, "pr", o.Number)
			} else {
				s.log().Info("forgejo automerge: skipped pull request", "repo", repo, "pr", o.Number, "reason", o.Reason)
			}
			out = append(out, o)
		}
	}
	return out
}

func (s *Sweeper) tryMerge(ctx context.Context, api forge.SweepAPI, repo string, number int) MergeOutcome {
	o := MergeOutcome{Repo: repo, Number: number}
	skip := func(reason string) MergeOutcome { o.Reason = reason; return o }
	pr, err := api.PullFacts(ctx, repo, number)
	if err != nil {
		return skip("read-failed")
	}
	switch {
	case pr.Merged:
		return skip("already-merged")
	case !strings.EqualFold(pr.State, "open"):
		return skip("not-open")
	case pr.Draft:
		return skip("draft")
	case !hasLabel(pr.Labels, s.AutoMergeLabel):
		return skip("not-labelled")
	case strings.TrimSpace(pr.HeadSHA) == "":
		return skip("no-head")
	}
	if b := s.blockedBy(pr.Labels); b != "" {
		return skip("blocked-by-label:" + b)
	}
	if !pr.Mergeable {
		return skip("not-mergeable")
	}
	// The timeline is read after PullFacts, so a push that lands between
	// the two shows up as a pull_push here, and one that lands later moves
	// the head off the pinned HeadSHA and Forgejo refuses the merge.
	add, ok, err := api.LabelLastAdded(ctx, repo, number, s.AutoMergeLabel)
	if err != nil {
		return skip("timeline-read-failed")
	}
	if !ok {
		return skip("label-adder-unknown")
	}
	labeler := add.Login
	if strings.EqualFold(labeler, pr.Author) {
		return skip("self-approved")
	}
	// The label approves the head its adder saw. A push after it, by the
	// author or anyone else, leaves the new commits unreviewed: a writer
	// must add the label again.
	if add.PushedAfter {
		return skip("pushed-after-label")
	}
	perm, err := api.Permission(ctx, repo, labeler)
	if err != nil {
		return skip("permission-read-failed")
	}
	if !releaseAccess[perm] {
		return skip("labeler-lacks-write")
	}
	state, total, err := api.CombinedStatus(ctx, repo, pr.HeadSHA)
	if err != nil {
		return skip("status-read-failed")
	}
	if total == 0 {
		return skip("no-ci")
	}
	if state != "success" {
		return skip("ci-" + nonEmpty(state, "unknown"))
	}
	method := s.MergeMethod
	if strings.TrimSpace(method) == "" {
		method = DefaultMergeMethod
	}
	_, err = effects.Execute(ctx, s.Mutation, effects.Claim{
		Repo:   repo,
		Kind:   effects.KindPullRequestMerge,
		Target: strconv.Itoa(number),
		Inputs: map[string]string{"method": method, "expect_sha": pr.HeadSHA},
	}, func(ctx context.Context) (effects.Result, error) {
		err := api.Merge(ctx, repo, number, forge.MergeOptions{Strategy: method, HeadSHA: pr.HeadSHA})
		if errors.Is(err, forge.ErrAlreadyMerged) {
			return effects.Result{Provenance: pr.HeadSHA}, nil
		}
		if errors.Is(err, forge.ErrConflict) || errors.Is(err, forge.ErrNotMergeable) || errors.Is(err, forge.ErrHoldActive) {
			return effects.Result{}, effects.NotApplied(err)
		}
		return effects.Result{Provenance: pr.HeadSHA}, err
	})
	switch {
	case err == nil:
		o.Merged = true
		return o
	case errors.Is(err, forge.ErrConflict):
		return skip("head-moved-or-conflict")
	case errors.Is(err, forge.ErrNotMergeable):
		return skip("forge-refused")
	case errors.Is(err, forge.ErrHoldActive):
		return skip("held")
	case errors.Is(err, effects.ErrDenied):
		return skip("mutation-denied")
	}
	s.log().Warn("forgejo automerge: merge failed", "repo", repo, "pr", number, "error", err)
	return skip("merge-failed")
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// ClosingRefs returns the same-repository issue numbers text names with a
// closing keyword, in order and without duplicates.
func ClosingRefs(text string) []int {
	var out []int
	seen := map[int]bool{}
	for _, m := range closingRefRe.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// CloseOnMerge covers the gap Forgejo's closing keywords leave. Forgejo
// closes an issue a merged pull request names with a closing keyword only
// when the pull request merged into the default branch; this sweep closes
// the ones still open afterwards (a merge into another branch, or a missed
// close) for recently merged pull requests whose author is the hive's own
// Forgejo user or has write access. It waits DefaultCloseGrace after a merge
// so Forgejo acts first, ignores merges older than DefaultCloseMaxAge, honours
// the needs-confirmation marker, and never closes again an issue a person
// reopened after the sweep closed it.
func (s *Sweeper) CloseOnMerge(ctx context.Context) []CloseOutcome {
	if s.API == nil {
		return nil
	}
	var out []CloseOutcome
	now := s.now()
	for _, repo := range s.Repos {
		if ctx.Err() != nil {
			break
		}
		api, err := s.API(repo)
		if err != nil {
			s.log().Warn("forgejo close-on-merge: client unavailable", "repo", repo, "error", err)
			continue
		}
		self, err := api.Self(ctx)
		if err != nil {
			s.log().Warn("forgejo close-on-merge: token user unknown (a token limited to specific repositories cannot read /user; set forgejo.login)", "repo", repo, "error", err)
			continue
		}
		pulls, err := api.RecentlyMergedPulls(ctx, repo, DefaultMergedPullsScanned)
		if err != nil {
			s.log().Warn("forgejo close-on-merge: listing merged pull requests failed", "repo", repo, "error", err)
			continue
		}
		// self skips the permission check for its own pull requests and marks
		// its own issues hive-authored, so it must hold write on the repo
		// before it is trusted. Forgejo answers the permission API about
		// another user only for an admin, so for a write-but-not-admin token
		// a forgejo.login that names someone else fails here (#19994). It is
		// checked once per repo, at the first pull request that would close
		// something.
		selfChecked, selfTrusted := false, false
		for _, pr := range pulls {
			age := now.Sub(pr.MergedAt)
			if age < DefaultCloseGrace || age > DefaultCloseMaxAge {
				continue
			}
			if strings.Contains(pr.Body, NeedsConfirmationMarker) {
				continue
			}
			refs := ClosingRefs(pr.Title + "\n" + pr.Body)
			if len(refs) == 0 {
				continue
			}
			if !selfChecked {
				selfChecked = true
				perm, err := api.Permission(ctx, repo, self)
				selfTrusted = err == nil && writeAccess[perm]
				if !selfTrusted {
					s.log().Warn("forgejo close-on-merge: the hive's own user lacks write on the repo; check forgejo.login", "repo", repo, "login", self, "permission", perm, "error", err)
				}
			}
			if !selfTrusted {
				break
			}
			if !s.trustedAuthor(ctx, api, repo, self, pr.Author) {
				continue
			}
			for _, issue := range refs {
				o := s.closeOne(ctx, api, repo, self, pr.Number, issue)
				if o.Closed {
					s.log().Info("forgejo close-on-merge: closed issue", "repo", repo, "issue", issue, "pr", pr.Number)
				}
				out = append(out, o)
			}
		}
	}
	return out
}

func (s *Sweeper) trustedAuthor(ctx context.Context, api forge.SweepAPI, repo, self, author string) bool {
	if author == "" {
		return false
	}
	if strings.EqualFold(author, self) {
		return true
	}
	perm, err := api.Permission(ctx, repo, author)
	return err == nil && writeAccess[perm]
}

func (s *Sweeper) closeOne(ctx context.Context, api forge.SweepAPI, repo, self string, pr, issue int) CloseOutcome {
	o := CloseOutcome{Repo: repo, Issue: issue, PR: pr}
	skip := func(reason string) CloseOutcome { o.Reason = reason; return o }
	facts, err := api.IssueFacts(ctx, repo, issue)
	if err != nil {
		return skip("issue-read-failed")
	}
	if facts.IsPull {
		return skip("is-pull-request")
	}
	if !strings.EqualFold(facts.State, "open") {
		return skip("already-closed")
	}
	// Closing someone's issue is an unsolicited write: the write-back gate
	// (ACMM level, opt-in labels, repo mode, backfill) decides, as it does
	// for the GitHub close-on-merge paths.
	item := writeback.Item{
		Number:       issue,
		Author:       facts.Author,
		Labels:       facts.Labels,
		CreatedAt:    facts.CreatedAt,
		HiveAuthored: self != "" && strings.EqualFold(facts.Author, self),
	}
	if d := s.allowed(repo, item, fmt.Sprintf("close issue named by merged #%d", pr)); !d.Allowed {
		return skip("writeback:" + nonEmpty(d.Reason, "refused"))
	}
	marker := fmt.Sprintf(closeMarkerFormat, pr)
	bodies, err := api.IssueCommentBodies(ctx, repo, issue)
	if err != nil {
		return skip("comments-read-failed")
	}
	for _, b := range bodies {
		if !strings.Contains(b, marker) {
			continue
		}
		// The sweep commented before. A reopen since then is a person's
		// decision and is final; without one, the earlier close failed and
		// is retried without a second comment.
		reopened, err := api.IssueReopened(ctx, repo, issue)
		if err != nil {
			return skip("timeline-read-failed")
		}
		if reopened {
			return skip("reopened-after-close")
		}
		return s.close(ctx, api, o)
	}
	body := fmt.Sprintf("%s\nClosing: #%d merged and names this issue with a closing keyword, but Forgejo left it open (it closes issues only for merges into the default branch).", marker, pr)
	_, err = effects.Execute(ctx, s.Mutation, effects.Claim{
		Repo:   repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(issue),
		Inputs: map[string]string{"body": effects.StableDigest(body)},
	}, func(ctx context.Context) (effects.Result, error) {
		return effects.Result{}, api.CreateIssueComment(ctx, repo, issue, body) // writeback:gated
	})
	if err != nil {
		return skip("close-comment-failed")
	}
	return s.close(ctx, api, o)
}

func (s *Sweeper) allowed(repo string, item writeback.Item, preview string) writeback.Decision {
	if s.Gate == nil {
		return writeback.Decision{Allowed: true, Reason: writeback.ReasonNoGate}
	}
	return s.Gate(repo, item, writeback.KindIssueClose, preview)
}

func (s *Sweeper) close(ctx context.Context, api forge.SweepAPI, o CloseOutcome) CloseOutcome {
	if err := api.CloseIssue(ctx, o.Repo, o.Issue); err != nil { // writeback:gated
		s.log().Warn("forgejo close-on-merge: close failed", "repo", o.Repo, "issue", o.Issue, "error", err)
		o.Reason = "close-failed"
		return o
	}
	o.Closed = true
	return o
}
