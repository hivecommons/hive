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
	// background counts the fact submissions spawned off a request path
	// (the webhook PR cover); tests wait on it instead of sleeping.
	background sync.WaitGroup
}

// WaitStageFacts blocks until every background fact submission the client
// spawned has returned. Test support; the hub never needs it.
func (c *Client) WaitStageFacts() {
	if c == nil {
		return
	}
	c.stageFacts.background.Wait()
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
// package's label-resolving issue deriver (the PR side is the deriver
// pkg/github/automerge registers at init) and writes through the client's
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
// number can name either; the stage families differ). An un-park fact is
// derived without the decision-park labels (unparkDerivationLabels), since
// the caller removes them in the same breath and the derivation must not
// see the item as still parked. On an issue Facts.Labels still carries
// them: it is also the writer's plan hint, and a hint without the
// stage/045 the issue wears plans nothing to remove when the issue derives
// no stage (project.issue_filter refuses it), so the writer never re-read
// the issue and the park label stayed on under an "un-parked" reply
// (projectbluefin/website#926).
func (c *Client) enrichStageFacts(ctx context.Context, f stageengine.Facts) (stageengine.Facts, error) {
	if c == nil || c.client == nil {
		return f, ErrNoGitHubClient
	}
	issue, _, err := c.client.Issues.Get(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number)
	if err != nil {
		return f, err
	}
	f.Labels = unionLabels(extractLabels(issue.Labels), f.Labels)
	f.Open = !strings.EqualFold(issue.GetState(), "closed")
	f.Merged = !issue.GetPullRequestLinks().GetMergedAt().IsZero()
	if issue.IsPullRequest() && f.Ref.Kind != stagelabels.PRItem {
		f.Ref.Kind = stagelabels.PRItem
		f.Issue = nil
		if f.PR == nil {
			f.PR = &stageengine.PRFacts{}
		}
	}
	if f.Ref.Kind == stagelabels.PRItem {
		// A PR's deriver reads Facts.Labels itself: it keeps the view.
		f.Labels = unparkDerivationLabels(f)
	}
	labels := unparkDerivationLabels(f)
	f.Park.Holds = stagelabels.Holds(labels)
	f.Park.NeedsHuman = f.Park.NeedsHuman || hasIssueNeedsHumanLabel(labels)
	if f.Park.DecisionLabel == "" {
		if label := hardSuppressIssueLabel(labels); label != "" && !strings.EqualFold(label, issueNeedsHumanLabel) && !strings.EqualFold(label, PossiblyExistsStageLabel) {
			f.Park.DecisionLabel = label
		}
	}
	// A fact that reports the issue actionable (un-park, claim release, a
	// covering PR closed) is re-checked against project.issue_filter the
	// way the enumeration would place it: a refused issue is filtered and
	// derives no stage, instead of a 010 / 015 the next sweep takes off
	// again. Exempt issues keep the fact's bucket, as before.
	if f.Issue != nil && f.Issue.Bucket == stageengine.BucketActionable && !c.isExempt(labels) && !c.getIssueFilter().Admits(labels) {
		f.Issue.Bucket = stageengine.BucketFiltered
		f.Issue.FilteredReason = issueFilterReason
	}
	if f.Issue != nil {
		resolveIssueLabelFacts(issueInputFrom(f), &f)
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
	// A PR fact (a human's CHANGES_REQUESTED review, an un-park) carries no
	// author kind either; left Unknown, the deriver's 07x split read every
	// contributor PR as a person's and routed hivecommons/hive#15159 — a
	// contributor's clanker PR — to 071 needs-author
	// instead of 072 returned-to-contributor. Resolve it the way the sweep
	// does: App login, agent/ stamp, body markers.
	if f.Author.Kind == authorkind.Unknown && f.Ref.Kind == stagelabels.PRItem {
		f.Author.Kind = authorkind.Detect(authorkind.Input{Author: issue.GetUser().GetLogin(), AppLogin: c.AppBotLogin(), Labels: f.Labels, Body: issue.GetBody()})
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
// of the covering PR (Record.CoveringPR), or failing that the duplicate-PR
// claim ledger's (ledgerCoveringPR), is only trusted after a fresh
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
	pr, ok := c.rememberedCoveringPR(store, f.Ref)
	partial := false
	if ok {
		// The store remembers the PR, not whether it closes the issue; the
		// ledger does (a "Refs #N" claim is Reference, hivecommons/hive#15008).
		if claim, found := c.ledgerClaim(f.Ref); found && claim.PRNumber == pr.Number {
			partial = claim.Reference && !claim.Closing
		}
	} else {
		pr, partial, ok = c.ledgerCoveringPR(f.Ref)
	}
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
	f.Issue.Claim.Partial = partial
	return f, nil
}

// rememberedCoveringPR is the store's memory of the issue's covering PR: a
// 040 record naming it.
func (c *Client) rememberedCoveringPR(store *stagelabels.Store, issue stageengine.Ref) (stageengine.Ref, bool) {
	rec, ok := store.Get(issue.Key())
	if !ok || rec.Stage != stagelabels.CoveredByPR || rec.CoveringPR == "" {
		return stageengine.Ref{}, false
	}
	return parseStoreKeyRef(rec.CoveringPR)
}

// ledgerCoveringPR is the duplicate-PR claim ledger's answer to "which open
// PR covers this issue" (SetPRClaimLookup), consulted when the store has no
// 040 record yet. That is the case for every issue whose covering PR opened
// while the issue carried a live claim: the sweep had booked 020, the PR's
// webhook delivery recorded the claim in the ledger, and the ClaimReleased
// fact that followed (lease revoked once the PR covered the work) had no
// memory to fall back on and re-derived 010 — nine issues sat at
// stage/010-actionable with bucket=claimed_by_pr for hours while cached
// cycles planned around them. Only a claim FilterClaimedIssues would
// suppress the issue for counts (IssueClaim.CoversOpenIssue); the ledger is
// keyed by the configured repo spelling, so both the bare and the
// owner-qualified name are tried.
func (c *Client) ledgerCoveringPR(issue stageengine.Ref) (pr stageengine.Ref, partial bool, ok bool) {
	claim, found := c.ledgerClaim(issue)
	if !found || !claim.CoversOpenIssue() {
		return stageengine.Ref{}, false, false
	}
	pr, ok = c.claimPRRef(claim, issue)
	return pr, claim.Reference && !claim.Closing, ok
}

// ledgerClaim is the duplicate-PR claim ledger's live claim on issue, tried
// under both the bare and the owner-qualified repo spelling.
func (c *Client) ledgerClaim(issue stageengine.Ref) (IssueClaim, bool) {
	if c == nil || c.prClaimLookup == nil || !issue.IsDefaultSource() {
		return IssueClaim{}, false
	}
	claim, ok := c.prClaimLookup(issue.Repo, issue.Number)
	if !ok && issue.Owner != "" {
		claim, ok = c.prClaimLookup(issue.Owner+"/"+issue.Repo, issue.Number)
	}
	return claim, ok
}

// SetPRClaimLookup installs the duplicate-PR claim ledger read the fact sink
// uses to keep an open covering PR in the picture for out-of-cycle facts
// (enrichCoveringPR). Resolved per call; nil disables the fallback.
func (c *Client) SetPRClaimLookup(fn func(repo string, number int) (IssueClaim, bool)) {
	if c == nil {
		return
	}
	c.prClaimLookup = fn
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

// unparkDerivationLabels is the label view an out-of-cycle fact is derived
// from: Facts.Labels, minus the decision-park labels on an un-park fact
// (unparkRemovableLabels).
func unparkDerivationLabels(f stageengine.Facts) []string {
	if !f.Park.ApprovedDirection {
		return f.Labels
	}
	return withoutLabels(f.Labels, unparkRemovableLabels)
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
