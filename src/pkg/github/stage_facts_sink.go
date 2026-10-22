package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageFactsState is the Client's stage FactSink: the one path an
// out-of-cycle writer (claim hooks, in-progress start marks, PR claim
// tracking, parking, /hive approve) may take to change an item's stage
// label (ADR-0025). Writers submit the fact they learned; the engine
// derives the stage and writes it. No code in this package adds or removes
// a `stage/*` label on its own any more.
type stageFactsState struct {
	mu   sync.Mutex
	sink stageengine.FactSink
}

// SetStageFactSink installs the FactSink the client's out-of-cycle writers
// submit facts to. nil restores the default (see StageFacts). It is the
// constructor option for tests and for a hub that already owns a
// reconciler; the hub's default wiring needs nothing here.
func (c *Client) SetStageFactSink(sink stageengine.FactSink) {
	if c == nil {
		return
	}
	c.stageFacts.mu.Lock()
	defer c.stageFacts.mu.Unlock()
	c.stageFacts.sink = sink
}

// StageFacts returns the FactSink the client's out-of-cycle writers use.
// The default is the engine's FactSink over an enforce-mode Reconciler that
// shares stagelabels.Shared() with the enumeration cycle, derives with this
// package's issue deriver (the PR side delegates to whatever cmd/hive
// registered via stageengine.SetDerivers) and writes through the client's
// one StageWriter (stage_writer.go), so a fact racing the sweep on an item
// queues behind it. Each submission re-reads the item first
// (enrichStageFacts) so the derivation and the write plan see the labels
// and state GitHub has now, not the ones the caller remembers.
func (c *Client) StageFacts() stageengine.FactSink {
	if c == nil {
		return nil
	}
	c.stageFacts.mu.Lock()
	defer c.stageFacts.mu.Unlock()
	if c.stageFacts.sink == nil {
		c.stageFacts.sink = c.defaultStageFactSink()
	}
	return c.stageFacts.sink
}

func (c *Client) defaultStageFactSink() stageengine.FactSink {
	store := stagelabels.Shared()
	r := stageengine.New(stageengine.Options{
		Mode:   stageengine.ModeEnforce,
		Store:  store,
		Writer: c.StageWriter(),
		Issue:  IssueDeriver(),
		PR:     stageengine.PRDeriver(),
		Logger: c.logger,
		// Shares the process counters with the sweeps' reconciler so
		// /metrics and /api/status count fact-driven writes too.
		Metrics: stageengine.DefaultCounters(),
	})
	return stageengine.NewFactSink(r, stageengine.FactSinkOptions{Enrich: func(ctx context.Context, f stageengine.Facts) (stageengine.Facts, error) {
		f, err := c.enrichStageFacts(ctx, f)
		if err != nil {
			return f, err
		}
		return c.enrichCoveringPR(ctx, store, f)
	}})
}

// stageRef builds the engine's Ref for an item of this client.
func (c *Client) stageRef(repo string, number int, kind stagelabels.Item) stageengine.Ref {
	owner, name := c.splitRepo(repo)
	return stageengine.Ref{Owner: owner, Repo: name, Number: number, Kind: kind}
}

// IssueStageRef is the engine Ref for an issue of repo ("owner/name", or a
// bare name under the client's org).
func (c *Client) IssueStageRef(repo string, number int) stageengine.Ref {
	return c.stageRef(repo, number, stagelabels.IssueItem)
}

// PRStageRef is the engine Ref for a pull request of repo.
func (c *Client) PRStageRef(repo string, number int) stageengine.Ref {
	return c.stageRef(repo, number, stagelabels.PRItem)
}

// enrichStageFacts is the sink's Enrich hook: a fresh read of the item tops
// the minimal Facts up with what GitHub has now — its labels (the park
// family, likely-done, the current stage), whether it is still open, and
// whether the "issue" is really a pull request (a Ref built from an issue
// number can name either; the stage families differ). An un-park fact
// drops the decision-park labels from the labels it reports, since the
// caller removes them in the same breath and the derivation must not see
// the item as still parked.
func (c *Client) enrichStageFacts(ctx context.Context, f stageengine.Facts) (stageengine.Facts, error) {
	if c == nil || c.client == nil {
		return f, ErrNoGitHubClient
	}
	issue, _, err := c.client.Issues.Get(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number)
	if err != nil {
		return f, err
	}
	fresh := extractLabels(issue.Labels)
	if f.Park.ApprovedDirection {
		fresh = withoutLabels(fresh, unparkRemovableLabels)
	}
	f.Labels = unionLabels(fresh, f.Labels)
	f.Open = !strings.EqualFold(issue.GetState(), "closed")
	f.Merged = !issue.GetPullRequestLinks().GetMergedAt().IsZero()
	if issue.IsPullRequest() && f.Ref.Kind != stagelabels.PRItem {
		f.Ref.Kind = stagelabels.PRItem
		f.Issue = nil
		if f.PR == nil {
			f.PR = &stageengine.PRFacts{}
		}
	}
	f.Park.Holds = stagelabels.Holds(f.Labels)
	f.Park.NeedsHuman = f.Park.NeedsHuman || hasIssueNeedsHumanLabel(f.Labels)
	if f.Park.DecisionLabel == "" {
		if label := hardSuppressIssueLabel(f.Labels); label != "" && !strings.EqualFold(label, issueNeedsHumanLabel) {
			f.Park.DecisionLabel = label
		}
	}
	if f.Issue != nil {
		f.Issue.LikelyDone = issueHasLabel(f.Labels, LikelyDoneLabel)
		f.Issue.Stalled = issueHasLabel(f.Labels, stagelabels.StalledLabel)
	}
	if f.Author.Login == "" {
		f.Author.Login = issue.GetUser().GetLogin()
	}
	// An out-of-cycle fact (claim release, un-park) carries no author
	// kind; without it a hive-filed issue would re-derive 010 instead of
	// 015-triage-needed until the next sweep.
	if f.Author.Kind == authorkind.Unknown && f.Issue != nil {
		switch {
		case c.isHumanAuthor(issue.GetUser()):
			f.Author.Kind = authorkind.Human
		case c.isKnownNonHumanAuthor(issue.GetUser()):
			f.Author.Kind = authorkind.Hive
		}
	}
	return f, nil
}

// enrichCoveringPR keeps an open covering PR in the picture for a fact that
// does not mention it. The sweep ranks an open PR that fixes the issue
// above a live claim or a start mark (applyDuplicatePRGuard runs the
// PR-claim guard first; hivecommons/hive#13174), so a claim on a covered
// issue stays 040 and its release leaves 040 standing. The out-of-cycle
// facts — Claimed, ClaimReleased, InProgress, Unparked — carry only the
// claim side of the story; without this step each re-derived 020 or 010
// under the PR, the store booked a regression and the activity monitor
// showed "regressed" until the next sweep put 040 back. The store's memory
// of the covering PR (Record.CoveringPR) is only trusted after a fresh
// read says the PR is still open and unmerged; a read failure is the
// fact's error (the labels stand until the sweep converges them) rather
// than a spurious fall. An Uncovered fact names the PR that closed, and
// that PR is never resurrected as the cover.
func (c *Client) enrichCoveringPR(ctx context.Context, store *stagelabels.Store, f stageengine.Facts) (stageengine.Facts, error) {
	if c == nil || c.client == nil || store == nil || f.Issue == nil || !f.Open || f.Ref.Kind != stagelabels.IssueItem || f.Issue.CoveringPR != nil {
		return f, nil
	}
	switch f.Issue.Bucket {
	case stageengine.BucketActionable, stageengine.BucketClaimed, stageengine.BucketInProgress:
	default:
		return f, nil
	}
	rec, ok := store.Get(f.Ref.Key())
	if !ok || rec.Stage != stagelabels.CoveredByPR || rec.CoveringPR == "" {
		return f, nil
	}
	pr, ok := parseStoreKeyRef(rec.CoveringPR)
	if !ok || (f.Issue.ClosedPR != nil && f.Issue.ClosedPR.Key() == pr.Key()) {
		return f, nil
	}
	live, _, err := c.client.PullRequests.Get(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return f, fmt.Errorf("covering PR %s: %w", pr, err)
	}
	if strings.EqualFold(live.GetState(), "closed") || live.GetMerged() || !live.GetMergedAt().IsZero() {
		return f, nil
	}
	f.Issue.Bucket = stageengine.BucketCoveredByPR
	f.Issue.CoveringPR = &pr
	if prRec, ok := store.Get(pr.Key()); ok {
		f.Issue.CoveringPRStage = prRec.Stage
	}
	if f.Issue.Claim == nil {
		f.Issue.Claim = &stageengine.ClaimFact{}
	}
	f.Issue.Claim.PRRepo, f.Issue.Claim.PRNumber = pr.FullRepo(), pr.Number
	return f, nil
}

// parseStoreKeyRef turns a default-source store key ("owner/repo#n") back
// into a PR Ref. Keys of another forge ("source:owner/repo#n") and
// malformed keys report false.
func parseStoreKeyRef(key string) (stageengine.Ref, bool) {
	repo, num, ok := strings.Cut(key, "#")
	if !ok || strings.Contains(repo, ":") {
		return stageengine.Ref{}, false
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return stageengine.Ref{}, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return stageengine.Ref{}, false
	}
	return stageengine.Ref{Owner: owner, Repo: name, Number: n, Kind: stagelabels.PRItem}, true
}

// unionLabels is fresh followed by the extra labels it lacks. Stage
// spellings among the extras are dropped: the fact names the stage the
// engine should derive (a park label such as stage/045-needs-decision), not
// a stage the item already wears — GitHub's labels are the only current
// stage.
func unionLabels(fresh, extra []string) []string {
	out := append([]string(nil), fresh...)
	for _, l := range extra {
		l = strings.TrimSpace(l)
		if l == "" || stagelabels.IsStageLabel(strings.ToLower(l)) {
			continue
		}
		if !issueHasLabel(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// withoutLabels is labels minus any (case-insensitive) member of drop.
func withoutLabels(labels, drop []string) []string {
	out := labels[:0:0]
	for _, l := range labels {
		if !issueHasLabel(drop, l) {
			out = append(out, l)
		}
	}
	return out
}
